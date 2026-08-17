package engine_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
)

// captureLogs redirects the default logger for one test so a checkpoint
// failure's slog output can be asserted on directly. Level is Warn so both
// the mid-step warnings and finish's error-level entry are captured.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return buf
}

type fakeStep struct {
	phase    engine.Phase
	requires []string
	err      error
	ran      chan struct{}
	// components, when set, is appended to r.Components before Run returns
	// -- opt-in and nil by default, so every existing caller of newFakeStep
	// is unaffected. Lets a test drive Apply's kind of partial-progress
	// write (r.Components, not just r.Artifacts) through the real engine,
	// including on the error path (err set alongside components), which is
	// what runStep's merge-back on failure (Ruling 14) needs covered.
	components []engine.ComponentState
}

func newFakeStep(p engine.Phase, requires ...string) *fakeStep {
	return &fakeStep{phase: p, requires: requires, ran: make(chan struct{}, 4)}
}

func (f *fakeStep) Phase() engine.Phase { return f.phase }
func (f *fakeStep) Requires() []string  { return f.requires }
func (f *fakeStep) Run(_ context.Context, r *engine.Run, emit engine.Emit) error {
	f.ran <- struct{}{}
	emit(bus.Event{Kind: bus.KindLog, Message: string(f.phase) + " ran"})
	r.Artifacts[string(f.phase)] = []byte("done")
	r.Components = append(r.Components, f.components...)
	return f.err
}

func waitState(t *testing.T, e *engine.Engine, id string, want engine.State) *engine.Run {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			got, _ := e.Get(context.Background(), id)
			t.Fatalf("timed out waiting for state %q, last state %q", want, got.State)
		default:
		}
		r, err := e.Get(context.Background(), id)
		if err == nil && r.State == want {
			return r
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRunCompletesAllSteps(t *testing.T) {
	b := bus.New(64)
	a := newFakeStep(engine.PhaseDiscover)
	c := newFakeStep(engine.PhaseRecommend)
	e := engine.New(b, engine.NewMemoryStore(), a, c)

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	final := waitState(t, e, run.ID, engine.StateDone)
	if final.Phase != engine.PhaseRecommend {
		t.Errorf("final phase = %q, want %q", final.Phase, engine.PhaseRecommend)
	}
	if len(a.ran) != 1 || len(c.ran) != 1 {
		t.Errorf("step run counts = (%d, %d), want (1, 1)", len(a.ran), len(c.ran))
	}
}

func TestRunParksForDecisions(t *testing.T) {
	b := bus.New(64)
	a := newFakeStep(engine.PhaseDiscover)
	c := newFakeStep(engine.PhaseRecommend, "intent", "platform")
	e := engine.New(b, engine.NewMemoryStore(), a, c)

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	waitState(t, e, run.ID, engine.StateAwaitingDecision)
	if len(c.ran) != 0 {
		t.Fatal("gated step ran before decisions were supplied")
	}

	if err := e.Decide(context.Background(), run.ID, map[string]string{"intent": "training"}); err == nil {
		t.Error("Decide() with a missing key should error")
	}

	if err := e.Decide(context.Background(), run.ID, map[string]string{"intent": "training", "platform": "kubeflow"}); err != nil {
		t.Fatalf("Decide() error = %v", err)
	}

	final := waitState(t, e, run.ID, engine.StateDone)
	if final.Decisions["platform"] != "kubeflow" {
		t.Errorf("decisions not recorded: %v", final.Decisions)
	}
}

// TestDecideRejectsKeysNotCurrentlyPending closes the gap the pre-fix Decide
// left open: a single call supplying every key a later gate will ever
// require -- sent at the FIRST gate the run parks on -- must not pre-satisfy
// a downstream Requires() before that step's own gate is ever reached. The
// pre-fix Decide merged every key the client sent, checking only that the
// currently-pending keys were present, so
// {"intent":"training","platform":"kubeflow","apply":"yes"} answered at the
// Recommend gate would have satisfied steps.Apply.Requires() (steps/apply.go)
// without the confirm gate it names ever firing.
func TestDecideRejectsKeysNotCurrentlyPending(t *testing.T) {
	b := bus.New(64)
	a := newFakeStep(engine.PhaseDiscover)
	c := newFakeStep(engine.PhaseRecommend, "intent", "platform")
	d := newFakeStep(engine.PhaseApply, "apply")
	e := engine.New(b, engine.NewMemoryStore(), a, c, d)

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitState(t, e, run.ID, engine.StateAwaitingDecision)

	if decideErr := e.Decide(context.Background(), run.ID, map[string]string{
		"intent": "training", "platform": "kubeflow", "apply": "yes",
	}); decideErr == nil {
		t.Fatal("Decide() with a key not currently pending ('apply') should error")
	}

	got, err := e.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if _, ok := got.Decisions["apply"]; ok {
		t.Error("apply decision was recorded even though the rejected call must not merge anything")
	}
	if len(d.ran) != 0 {
		t.Fatal("Apply ran before its own confirm gate was ever satisfied")
	}

	// The legitimate two-key answer at this gate still works.
	if decideErr := e.Decide(context.Background(), run.ID, map[string]string{"intent": "training", "platform": "kubeflow"}); decideErr != nil {
		t.Fatalf("Decide() with only the pending keys error = %v", decideErr)
	}

	// The run now parks on Apply's own gate, proving "apply" is still
	// genuinely pending rather than having been silently pre-satisfied.
	waitState(t, e, run.ID, engine.StateAwaitingDecision)
	got, err = e.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if len(got.Pending) != 1 || got.Pending[0] != "apply" {
		t.Fatalf("Pending = %v, want [apply]", got.Pending)
	}
	if len(d.ran) != 0 {
		t.Fatal("Apply ran before its confirm gate was answered")
	}

	if decideErr := e.Decide(context.Background(), run.ID, map[string]string{"apply": "yes"}); decideErr != nil {
		t.Fatalf("Decide() error = %v", decideErr)
	}
	waitState(t, e, run.ID, engine.StateDone)
	if len(d.ran) != 1 {
		t.Errorf("Apply ran %d times, want 1", len(d.ran))
	}
}

func TestStepFailureStopsRun(t *testing.T) {
	b := bus.New(64)
	boom := errors.New("boom")
	a := newFakeStep(engine.PhaseDiscover)
	a.err = boom
	c := newFakeStep(engine.PhaseRecommend)
	e := engine.New(b, engine.NewMemoryStore(), a, c)

	run, _ := e.Start(context.Background())
	final := waitState(t, e, run.ID, engine.StateFailed)

	if final.Err == "" {
		t.Error("failed run carries no error message")
	}
	if len(c.ran) != 0 {
		t.Error("step after a failure ran anyway")
	}
}

func TestGetReturnsCopy(t *testing.T) {
	b := bus.New(64)
	e := engine.New(b, engine.NewMemoryStore(), newFakeStep(engine.PhaseDiscover))
	run, _ := e.Start(context.Background())
	waitState(t, e, run.ID, engine.StateDone)

	got, _ := e.Get(context.Background(), run.ID)
	got.Decisions["tamper"] = "yes"
	again, _ := e.Get(context.Background(), run.ID)
	if _, ok := again.Decisions["tamper"]; ok {
		t.Error("Get() returned a live reference, not a copy")
	}
}

// blockingStep writes to the Run while the test hammers Get concurrently. Under
// -race this fails if the engine hands steps the live *Run instead of a copy.
type blockingStep struct {
	release chan struct{}
	entered chan struct{}
}

func (b *blockingStep) Phase() engine.Phase { return engine.PhaseDiscover }
func (b *blockingStep) Requires() []string  { return nil }
func (b *blockingStep) Run(_ context.Context, r *engine.Run, _ engine.Emit) error {
	close(b.entered)
	for i := 0; i < 200; i++ {
		r.Artifacts["k"] = []byte{byte(i)}
	}
	<-b.release
	r.Artifacts["final"] = []byte("done")
	return nil
}

func TestGetDuringStepIsRaceFree(t *testing.T) {
	b := bus.New(64)
	step := &blockingStep{release: make(chan struct{}), entered: make(chan struct{})}
	e := engine.New(b, engine.NewMemoryStore(), step)

	run, _ := e.Start(context.Background())
	<-step.entered

	for i := 0; i < 200; i++ {
		if _, err := e.Get(context.Background(), run.ID); err != nil {
			t.Fatalf("Get() error = %v", err)
		}
	}
	close(step.release)

	final := waitState(t, e, run.ID, engine.StateDone)
	if string(final.Artifacts["final"]) != "done" {
		t.Error("step writes were not merged back into the run")
	}
}

func TestCurrentReturnsNilBeforeAnyRun(t *testing.T) {
	e := engine.New(bus.New(8), engine.NewMemoryStore())
	if got := e.Current(); got != nil {
		t.Errorf("Current() = %v, want nil before Start", got)
	}
}

func TestCurrentReturnsCopyOfLatestRun(t *testing.T) {
	b := bus.New(64)
	e := engine.New(b, engine.NewMemoryStore(), newFakeStep(engine.PhaseDiscover))
	run, _ := e.Start(context.Background())
	waitState(t, e, run.ID, engine.StateDone)

	got := e.Current()
	if got == nil || got.ID != run.ID {
		t.Fatalf("Current() = %v, want run %q", got, run.ID)
	}
	got.Decisions["tamper"] = "yes"
	again := e.Current()
	if _, ok := again.Decisions["tamper"]; ok {
		t.Error("Current() returned a live reference, not a copy")
	}
}

func TestPhaseEventsPublished(t *testing.T) {
	b := bus.New(64)
	e := engine.New(b, engine.NewMemoryStore(), newFakeStep(engine.PhaseDiscover))
	run, _ := e.Start(context.Background())
	waitState(t, e, run.ID, engine.StateDone)

	var phases int
	for _, ev := range b.Replay(0) {
		if ev.Kind == bus.KindPhase {
			phases++
		}
		if ev.RunID != run.ID {
			t.Errorf("event %d has RunID %q, want %q", ev.ID, ev.RunID, run.ID)
		}
	}
	if phases == 0 {
		t.Error("no phase events published")
	}
}

// A step that fails once and then succeeds -- the exact shape Retry exists
// for, since a mid-apply component failure is normal on real clusters.
type flakyStep struct {
	phase engine.Phase
	fails int
	runs  int
	mu    sync.Mutex
}

func (f *flakyStep) Phase() engine.Phase { return f.phase }
func (f *flakyStep) Requires() []string  { return nil }
func (f *flakyStep) Run(_ context.Context, _ *engine.Run, _ engine.Emit) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs++
	if f.runs <= f.fails {
		return errors.New("boom")
	}
	return nil
}

func (f *flakyStep) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs
}

func TestRetryResumesFromTheFailedStep(t *testing.T) {
	b := bus.New(64)
	first := newFakeStep(engine.PhaseDiscover)
	flaky := &flakyStep{phase: engine.PhaseApply, fails: 1}
	e := engine.New(b, engine.NewMemoryStore(), first, flaky)

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitState(t, e, run.ID, engine.StateFailed)

	if _, err := e.Retry(context.Background(), run.ID); err != nil {
		t.Fatalf("Retry() error = %v", err)
	}
	done := waitState(t, e, run.ID, engine.StateDone)

	if done.Err != "" {
		t.Errorf("Err = %q, want cleared on a successful retry", done.Err)
	}
	// The first step must NOT re-run: the cursor resumes at the step that
	// failed, so Discover's snapshot Job is not redeployed.
	if got := len(first.ran); got != 1 {
		t.Errorf("first step ran %d times, want 1", got)
	}
	if got := flaky.count(); got != 2 {
		t.Errorf("failed step ran %d times, want 2", got)
	}
}

func TestRetryRejectsARunThatIsNotFailed(t *testing.T) {
	b := bus.New(64)
	e := engine.New(b, engine.NewMemoryStore(), newFakeStep(engine.PhaseDiscover))

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitState(t, e, run.ID, engine.StateDone)

	if _, err := e.Retry(context.Background(), run.ID); err == nil {
		t.Error("Retry() error = nil, want a conflict on a completed run")
	}
}

func TestRetryRejectsAnUnknownRun(t *testing.T) {
	e := engine.New(bus.New(8), engine.NewMemoryStore(), newFakeStep(engine.PhaseDiscover))
	if _, err := e.Retry(context.Background(), "nope"); err == nil {
		t.Error("Retry() error = nil, want not-found")
	}
}

// The epoch guard. Retry is the path that makes a second execute goroutine
// reachable for the SAME run, which is exactly what Start's isLive check
// cannot see: isLive answers "is a run live", not "is THIS goroutine still
// the one driving it". A retried run must therefore reach exactly one
// terminal state and publish exactly one terminal event -- a superseded
// goroutine writing state again would produce two.
func TestRetriedRunReachesExactlyOneTerminalState(t *testing.T) {
	b := bus.New(256)
	sub, unsubscribe := b.Subscribe(0)
	defer unsubscribe()

	flaky := &flakyStep{phase: engine.PhaseApply, fails: 1}
	e := engine.New(b, engine.NewMemoryStore(), flaky)

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitState(t, e, run.ID, engine.StateFailed)

	if _, err := e.Retry(context.Background(), run.ID); err != nil {
		t.Fatalf("Retry() error = %v", err)
	}
	done := waitState(t, e, run.ID, engine.StateDone)

	if done.StepIndex != 1 {
		t.Errorf("StepIndex = %d, want 1 (all steps consumed)", done.StepIndex)
	}

	// Drain what the bus has so far and count terminal events. A superseded
	// goroutine calling finish() again is the failure this catches.
	deadline := time.After(500 * time.Millisecond)
	terminal := 0
drain:
	for {
		select {
		case ev := <-sub:
			if ev.Message == "run done" {
				terminal++
			}
		case <-deadline:
			break drain
		}
	}
	if terminal != 1 {
		t.Errorf("published %d 'run done' events, want exactly 1 -- a superseded goroutine wrote state", terminal)
	}
}

// saveFailingStore wraps a Store and can be told to fail every Save call on
// demand, to exercise Retry's rollback path when persistence fails.
type saveFailingStore struct {
	engine.Store
	mu   sync.Mutex
	fail bool
}

func (s *saveFailingStore) Save(ctx context.Context, r *engine.Run) error {
	s.mu.Lock()
	fail := s.fail
	s.mu.Unlock()
	if fail {
		return errors.New("save failed")
	}
	return s.Store.Save(ctx, r)
}

func (s *saveFailingStore) setFail(v bool) {
	s.mu.Lock()
	s.fail = v
	s.mu.Unlock()
}

// TestRetryFailedSaveLeavesRunRetryable pins that a Save failure during
// Retry does not wedge the run. Retry flips State to StateRunning before
// persisting; if Save then fails and State is left at StateRunning with no
// goroutine driving it, the run becomes unrecoverable without restarting
// the process -- Start refuses because isLive(StateRunning) is true, and
// Retry refuses because it requires StateFailed. memoryStore.Save itself
// still never errors, which is why this test wraps it in a fake that can --
// but that is no longer merely a hypothetical: 2b-ii's ConfigMap-backed
// store (internal/engine/cmstore.go) is wired into production
// (cmd/aicrme/main.go) and is precisely where Save starts failing for real.
func TestRetryFailedSaveLeavesRunRetryable(t *testing.T) {
	b := bus.New(64)
	boom := errors.New("boom")
	a := newFakeStep(engine.PhaseDiscover)
	a.err = boom
	store := &saveFailingStore{Store: engine.NewMemoryStore()}
	e := engine.New(b, store, a)

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitState(t, e, run.ID, engine.StateFailed)

	store.setFail(true)
	if _, retryErr := e.Retry(context.Background(), run.ID); retryErr == nil {
		t.Fatal("Retry() error = nil, want the store's Save error")
	}
	store.setFail(false)

	// The assertion that matters: State is still StateFailed, so a
	// subsequent Retry could still succeed. Asserting only the error above
	// would pass even with the run wedged at StateRunning.
	got, err := e.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.State != engine.StateFailed {
		t.Errorf("State = %q, want %q -- a failed Save must not wedge the run", got.State, engine.StateFailed)
	}
}

// flakyGatedStep requires a decision before it may run, and fails once then
// succeeds -- it exists to pin that Retry does not re-park a run whose
// decisions were already supplied before the failing attempt.
type flakyGatedStep struct {
	phase    engine.Phase
	requires []string
	fails    int
	runs     int
	mu       sync.Mutex
}

func (f *flakyGatedStep) Phase() engine.Phase { return f.phase }
func (f *flakyGatedStep) Requires() []string  { return f.requires }
func (f *flakyGatedStep) Run(_ context.Context, _ *engine.Run, _ engine.Emit) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs++
	if f.runs <= f.fails {
		return errors.New("boom")
	}
	return nil
}

// TestRetryDoesNotReparkForDecisions pins the path the product depends on:
// a mid-Apply failure after the user has already answered its decision
// prompt, followed by Retry, must resume straight into the step rather than
// asking the question again. Retry never touches Decisions, and
// awaitDecisions only checks key presence, so this should already hold --
// but nothing pinned it, and a future change to either would silently
// re-park a retried run.
func TestRetryDoesNotReparkForDecisions(t *testing.T) {
	b := bus.New(64)
	step := &flakyGatedStep{phase: engine.PhaseApply, requires: []string{"apply"}, fails: 1}
	e := engine.New(b, engine.NewMemoryStore(), step)

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitState(t, e, run.ID, engine.StateAwaitingDecision)

	if err := e.Decide(context.Background(), run.ID, map[string]string{"apply": "yes"}); err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	waitState(t, e, run.ID, engine.StateFailed)

	if _, err := e.Retry(context.Background(), run.ID); err != nil {
		t.Fatalf("Retry() error = %v", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			got, _ := e.Get(context.Background(), run.ID)
			t.Fatalf("timed out waiting for state %q, last state %q", engine.StateDone, got.State)
		default:
		}
		got, err := e.Get(context.Background(), run.ID)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if got.State == engine.StateAwaitingDecision {
			t.Fatal("Retry re-parked the run for a decision it already had")
		}
		if got.State == engine.StateDone {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ctxBlockingStep blocks until its context is canceled, which is what a
// canceled deploy.sh does: SIGTERM propagates through the process group,
// deploy.sh's trap runs, and Wait() returns once the tree is reaped -- here
// represented by <-ctx.Done(). Named distinctly from the existing
// blockingStep above (which blocks on a release channel, not ctx) since the
// two are unrelated fixtures that happen to share a shape.
type ctxBlockingStep struct {
	phase   engine.Phase
	entered chan struct{}
}

func (b *ctxBlockingStep) Phase() engine.Phase { return b.phase }
func (b *ctxBlockingStep) Requires() []string  { return nil }
func (b *ctxBlockingStep) Run(ctx context.Context, _ *engine.Run, _ engine.Emit) error {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestCancelAndWaitStopsAnInFlightRun(t *testing.T) {
	b := bus.New(64)
	step := &ctxBlockingStep{phase: engine.PhaseApply, entered: make(chan struct{}, 1)}
	e := engine.New(b, engine.NewMemoryStore(), step)

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-step.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("step never entered")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if cancelErr := e.CancelAndWait(ctx); cancelErr != nil {
		t.Fatalf("CancelAndWait() error = %v", cancelErr)
	}

	// CancelAndWait must not return until the terminal state is persisted --
	// a caller that returns early would let main exit before the run is done.
	got, err := e.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.State != engine.StateFailed {
		t.Errorf("State = %q, want %q", got.State, engine.StateFailed)
	}
	if !strings.Contains(got.Err, "canceled") {
		t.Errorf("Err = %q, want it to say the run was canceled", got.Err)
	}
}

// ctxCanceledStore rejects a Save whose context is already dead, the way any
// store that issues a real API call does -- client-go checks ctx.Err() and
// returns before the request ever leaves the process. It is the only way this
// package can observe finish()'s context.WithoutCancel: memoryStore.Save
// takes `_ context.Context` and ignores it, so with that double alone the
// entire terminal-save contract is unverified in both directions.
type ctxCanceledStore struct {
	engine.Store
}

func (s *ctxCanceledStore) Save(ctx context.Context, r *engine.Run) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.Store.Save(ctx, r)
}

// TestTerminalStateIsPersistedDespiteCancellation pins the branch's headline
// contract: finish() detaches the terminal write from the (by then canceled)
// run context, so shutdown records how the run ended instead of leaving it
// recorded as running with no goroutine behind it.
//
// It asserts through store.Load, never e.Get: e.Get returns
// e.current.Clone() while the run is still the current one and never touches
// the store at all, so it reports the in-memory terminal state whether or not
// the persisted one was ever written. Now that 2b-ii's ConfigMap store is
// wired into production (cmd/aicrme/main.go's newRunStore), that distinction
// is not academic: a Save issued under the canceled run context would return
// context.Canceled before ever reaching the API server, leaving the run
// wedged at `running` across a restart -- exactly what finish's detachment
// above exists to prevent.
func TestTerminalStateIsPersistedDespiteCancellation(t *testing.T) {
	t.Run("canceled mid-step", func(t *testing.T) {
		b := bus.New(64)
		step := &ctxBlockingStep{phase: engine.PhaseApply, entered: make(chan struct{}, 1)}
		store := &ctxCanceledStore{Store: engine.NewMemoryStore()}
		e := engine.New(b, store, step)

		run, err := e.Start(context.Background())
		if err != nil {
			t.Fatalf("Start() error = %v", err)
		}
		select {
		case <-step.entered:
		case <-time.After(2 * time.Second):
			t.Fatal("step never entered")
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if cancelErr := e.CancelAndWait(ctx); cancelErr != nil {
			t.Fatalf("CancelAndWait() error = %v", cancelErr)
		}

		assertPersistedTerminalState(t, store, run.ID)
	})

	t.Run("canceled parked at a decision gate", func(t *testing.T) {
		b := bus.New(64)
		store := &ctxCanceledStore{Store: engine.NewMemoryStore()}
		e := engine.New(b, store, newFakeStep(engine.PhaseRecommend, "intent"))

		run, err := e.Start(context.Background())
		if err != nil {
			t.Fatalf("Start() error = %v", err)
		}
		waitState(t, e, run.ID, engine.StateAwaitingDecision)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if cancelErr := e.CancelAndWait(ctx); cancelErr != nil {
			t.Fatalf("CancelAndWait() error = %v", cancelErr)
		}

		assertPersistedTerminalState(t, store, run.ID)
	})
}

func assertPersistedTerminalState(t *testing.T, store engine.Store, runID string) {
	t.Helper()
	saved, err := store.Load(context.Background(), runID)
	if err != nil {
		t.Fatalf("store.Load() error = %v", err)
	}
	if saved.State != engine.StateFailed {
		t.Errorf("persisted State = %q, want %q -- the terminal write must not ride the canceled run context",
			saved.State, engine.StateFailed)
	}
	if !strings.Contains(saved.Err, "canceled") {
		t.Errorf("persisted Err = %q, want it to say the run was canceled", saved.Err)
	}
}

// Both halves keep their original assertions; they need one engine each
// because CancelAndWait is now terminal for the engine it is called on --
// draining is never cleared, so the second half's Start would be refused on
// an engine the first half had already drained. That is the contract, not a
// workaround: an engine told to shut down must not accept a run afterwards.
func TestCancelAndWaitIsIdempotentAndSafeWithNoRun(t *testing.T) {
	ctx := context.Background()

	// No run has ever started.
	idle := engine.New(bus.New(8), engine.NewMemoryStore(), newFakeStep(engine.PhaseDiscover))
	if err := idle.CancelAndWait(ctx); err != nil {
		t.Fatalf("CancelAndWait() with no run error = %v", err)
	}

	e := engine.New(bus.New(8), engine.NewMemoryStore(), newFakeStep(engine.PhaseDiscover))
	run, err := e.Start(ctx)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitState(t, e, run.ID, engine.StateDone)

	// Run already finished on its own; canceling is a no-op, twice.
	if cancelErr := e.CancelAndWait(ctx); cancelErr != nil {
		t.Fatalf("first CancelAndWait() error = %v", cancelErr)
	}
	if cancelErr := e.CancelAndWait(ctx); cancelErr != nil {
		t.Fatalf("second CancelAndWait() error = %v", cancelErr)
	}
}

// TestDrainingEngineRefusesNewWork pins the flag CancelAndWait sets. Both
// entry points that install a fresh cancel/done pair have to honor it, or
// CancelAndWait goes back to being able to wait on a run that has already
// been replaced.
func TestDrainingEngineRefusesNewWork(t *testing.T) {
	t.Run("Start", func(t *testing.T) {
		e := engine.New(bus.New(8), engine.NewMemoryStore(), newFakeStep(engine.PhaseDiscover))
		if err := e.CancelAndWait(context.Background()); err != nil {
			t.Fatalf("CancelAndWait() error = %v", err)
		}
		_, err := e.Start(context.Background())
		if !errors.Is(err, engine.ErrDraining) {
			t.Fatalf("Start() error = %v, want ErrDraining", err)
		}
	})

	t.Run("Retry", func(t *testing.T) {
		failing := newFakeStep(engine.PhaseDiscover)
		failing.err = errors.New("boom")
		e := engine.New(bus.New(64), engine.NewMemoryStore(), failing)

		run, err := e.Start(context.Background())
		if err != nil {
			t.Fatalf("Start() error = %v", err)
		}
		waitState(t, e, run.ID, engine.StateFailed)

		if cancelErr := e.CancelAndWait(context.Background()); cancelErr != nil {
			t.Fatalf("CancelAndWait() error = %v", cancelErr)
		}
		if _, retryErr := e.Retry(context.Background(), run.ID); !errors.Is(retryErr, engine.ErrDraining) {
			t.Fatalf("Retry() error = %v, want ErrDraining", retryErr)
		}
	})

	// Added for consistency with Start/Retry (Ruling 12's Minor): Discard
	// mutates engine state the same way they do, so it refuses during drain
	// for the same reason -- one rule for every mutating action, rather than
	// a caller having to remember Discard is the one exception.
	t.Run("Discard", func(t *testing.T) {
		failing := newFakeStep(engine.PhaseDiscover)
		failing.err = errors.New("boom")
		e := engine.New(bus.New(64), engine.NewMemoryStore(), failing)

		run, err := e.Start(context.Background())
		if err != nil {
			t.Fatalf("Start() error = %v", err)
		}
		waitState(t, e, run.ID, engine.StateFailed)

		if cancelErr := e.CancelAndWait(context.Background()); cancelErr != nil {
			t.Fatalf("CancelAndWait() error = %v", cancelErr)
		}
		if discardErr := e.Discard(context.Background(), run.ID); !errors.Is(discardErr, engine.ErrDraining) {
			t.Fatalf("Discard() error = %v, want ErrDraining", discardErr)
		}
	})
}

// TestCancelAndWaitNeverReturnsWithARunStillLive is the interleaving nothing
// covered. CancelAndWait used to snapshot cancel/done under the lock, release
// it, and wait on that one channel: a Start landing in between installed a
// new pair, so the wait completed against the previous (already terminal)
// run's closed channel and returned nil while the new run's goroutine was
// still inside its step. main then returns, tearing down the PID namespace
// under a live deploy.sh.
//
// The assertion is deliberately synchronous -- no polling. A nil return from
// CancelAndWait means done was closed, which means execute returned, which
// means finish already wrote the terminal state. Polling here would hide the
// exact defect the test exists for.
func TestCancelAndWaitNeverReturnsWithARunStillLive(t *testing.T) {
	for i := 0; i < 200; i++ {
		step := &ctxBlockingStep{phase: engine.PhaseApply, entered: make(chan struct{}, 1)}
		e := engine.New(bus.New(8), engine.NewMemoryStore(), step)

		var (
			wg        sync.WaitGroup
			release   = make(chan struct{})
			run       *engine.Run
			startErr  error
			cancelErr error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-release
			run, startErr = e.Start(context.Background())
		}()
		go func() {
			defer wg.Done()
			<-release
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cancelErr = e.CancelAndWait(ctx)
		}()
		close(release)
		wg.Wait()

		if cancelErr != nil {
			t.Fatalf("iteration %d: CancelAndWait() error = %v", i, cancelErr)
		}
		if startErr != nil {
			// Refused as draining: there is no run to have left behind.
			if !errors.Is(startErr, engine.ErrDraining) {
				t.Fatalf("iteration %d: Start() error = %v, want ErrDraining", i, startErr)
			}
			continue
		}
		got, err := e.Get(context.Background(), run.ID)
		if err != nil {
			t.Fatalf("iteration %d: Get() error = %v", i, err)
		}
		if got.State == engine.StateRunning || got.State == engine.StateAwaitingDecision {
			t.Fatalf("iteration %d: CancelAndWait() returned nil with run %s still in state %q",
				i, run.ID, got.State)
		}
	}
}

// stuckStep ignores its context entirely -- the pathological case
// CancelAndWait's deadline exists for.
type stuckStep struct {
	phase   engine.Phase
	entered chan struct{}
	release chan struct{}
}

func (s *stuckStep) Phase() engine.Phase { return s.phase }
func (s *stuckStep) Requires() []string  { return nil }
func (s *stuckStep) Run(_ context.Context, _ *engine.Run, _ engine.Emit) error {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-s.release
	return nil
}

func TestCancelAndWaitTimesOutRatherThanBlockingForever(t *testing.T) {
	b := bus.New(64)
	stuck := &stuckStep{phase: engine.PhaseApply, entered: make(chan struct{}, 1), release: make(chan struct{})}
	e := engine.New(b, engine.NewMemoryStore(), stuck)

	if _, err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-stuck.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("step never entered")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := e.CancelAndWait(ctx); err == nil {
		t.Error("CancelAndWait() error = nil, want a timeout error")
	}
	close(stuck.release)
}

func TestCancelWhileParkedForDecisionsFinishesTheRun(t *testing.T) {
	b := bus.New(64)
	e := engine.New(b, engine.NewMemoryStore(), newFakeStep(engine.PhaseRecommend, "intent"))

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitState(t, e, run.ID, engine.StateAwaitingDecision)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if cancelErr := e.CancelAndWait(ctx); cancelErr != nil {
		t.Fatalf("CancelAndWait() error = %v", cancelErr)
	}

	// A run frozen at a gate with no goroutine is the wedge class Ruling 13
	// fixed for Save failures; cancellation must not reintroduce it.
	got, _ := e.Get(context.Background(), run.ID)
	if got.State != engine.StateFailed {
		t.Errorf("State = %q, want %q", got.State, engine.StateFailed)
	}
}

// TestArtifactReturnsACopy pins the reason Artifact exists: the observer's
// run-scope accessor calls it on the per-event path, so it must not hand out
// a reference into engine-owned state that a caller could then mutate.
func TestArtifactReturnsACopy(t *testing.T) {
	e := engine.New(bus.New(64), engine.NewMemoryStore(), newFakeStep(engine.PhaseDiscover))
	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitState(t, e, run.ID, engine.StateDone)

	key := string(engine.PhaseDiscover)
	got, ok := e.Artifact(run.ID, key)
	if !ok || string(got) != "done" {
		t.Fatalf("Artifact(%q) = %q, %v, want \"done\", true", key, got, ok)
	}

	got[0] = 'X'
	again, _ := e.Artifact(run.ID, key)
	if string(again) != "done" {
		t.Errorf("Artifact() = %q after a caller mutated an earlier result -- it handed out the live backing array", again)
	}
}

func TestArtifactReportsMisses(t *testing.T) {
	e := engine.New(bus.New(64), engine.NewMemoryStore(), newFakeStep(engine.PhaseDiscover))

	if _, ok := e.Artifact("any-run", "recipe.json"); ok {
		t.Error("Artifact() ok = true before any run has started")
	}

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitState(t, e, run.ID, engine.StateDone)

	// The run ID argument is what stops a caller that paired this with
	// CurrentID from attributing a new run's artifact to the old run's scope.
	if _, ok := e.Artifact("some-other-run", string(engine.PhaseDiscover)); ok {
		t.Error("Artifact() ok = true for a run ID that is not the current run")
	}
	if _, ok := e.Artifact(run.ID, "never-written"); ok {
		t.Error("Artifact() ok = true for an absent key")
	}
}

func TestCurrentIDDoesNotRequireAClone(t *testing.T) {
	e := engine.New(bus.New(8), engine.NewMemoryStore(), newFakeStep(engine.PhaseDiscover))

	if _, ok := e.CurrentID(); ok {
		t.Error("CurrentID() ok = true before any run, want false")
	}

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	id, ok := e.CurrentID()
	if !ok || id != run.ID {
		t.Errorf("CurrentID() = %q, %v, want %q, true", id, ok, run.ID)
	}
}

// TestStartIsRefusedWhileRecoveryIsPending pins the bootstrap contract:
// web/src/App.tsx posts /api/runs automatically on load, and Start rejected
// only isLive states -- StateFailed, which is what Recover produces, is not
// live. Without this refusal, the SPA's routine startup POST silently
// replaces a recovered run before the operator ever sees it.
func TestStartIsRefusedWhileRecoveryIsPending(t *testing.T) {
	store := engine.NewMemoryStore()
	seed := baseRun(testRunID, engine.StateRunning, engine.PhaseDiscover, 0)
	if err := store.Save(context.Background(), seed); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	e := fourStepEngine(store)

	if err := e.Recover(context.Background()); err != nil {
		t.Fatalf("Recover() error = %v", err)
	}

	_, err := e.Start(context.Background())
	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) || se.Code != aicrerrors.ErrCodeConflict {
		t.Fatalf("Start() error = %v, want a StructuredError with ErrCodeConflict while a recovered run is pending", err)
	}
	// Start must refuse before touching e.current -- the recovered run must
	// still be the one installed, not replaced and then rejected.
	if got := e.Current(); got == nil || got.ID != testRunID {
		t.Errorf("Current() = %+v, want the recovered run left in place", got)
	}
}

// TestRetryClearsRecoveryPending pins Retry as the intended resume path: once
// it accepts the recovered run and the run reaches a terminal state on its
// own, Start must behave normally again with no further operator action.
func TestRetryClearsRecoveryPending(t *testing.T) {
	store := engine.NewMemoryStore()
	seed := baseRun(testRunID, engine.StateRunning, engine.PhaseApply, 3)
	if err := store.Save(context.Background(), seed); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	e := fourStepEngine(store)

	if err := e.Recover(context.Background()); err != nil {
		t.Fatalf("Recover() error = %v", err)
	}

	if _, err := e.Retry(context.Background(), testRunID); err != nil {
		t.Fatalf("Retry() error = %v", err)
	}
	waitState(t, e, testRunID, engine.StateDone)

	if _, err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v, want nil once the run is terminal and Retry has cleared recoveredPending", err)
	}
}

// TestDiscardClearsRecoveryPendingAndDeletes pins Discard's whole contract in
// one test: without it, a recovered run blocks Start forever and the console
// can never begin a new run -- a worse wedge than the bug the block exists to
// prevent.
func TestDiscardClearsRecoveryPendingAndDeletes(t *testing.T) {
	store := engine.NewMemoryStore()
	seed := baseRun(testRunID, engine.StateFailed, engine.PhaseDiscover, 0)
	if err := store.Save(context.Background(), seed); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	e := fourStepEngine(store)

	if err := e.Recover(context.Background()); err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if got := e.Current(); got == nil {
		t.Fatal("Current() = nil, want the recovered run installed")
	}

	if err := e.Discard(context.Background(), testRunID); err != nil {
		t.Fatalf("Discard() error = %v", err)
	}
	if got := e.Current(); got != nil {
		t.Errorf("Current() = %+v, want nil after Discard", got)
	}

	_, err := store.LoadCurrent(context.Background())
	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) || se.Code != aicrerrors.ErrCodeNotFound {
		t.Errorf("LoadCurrent() error = %v, want ErrCodeNotFound -- Discard must delete the persisted record", err)
	}

	if _, err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v, want nil now that the recovered run was discarded", err)
	}
}

// TestDiscardRejectsUnknownRunID guards against a stale browser tab
// discarding a run the operator has since replaced: Discard must check the
// run ID, not just drop whatever is current.
func TestDiscardRejectsUnknownRunID(t *testing.T) {
	store := engine.NewMemoryStore()
	seed := baseRun(testRunID, engine.StateFailed, engine.PhaseDiscover, 0)
	if err := store.Save(context.Background(), seed); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	e := fourStepEngine(store)

	if err := e.Recover(context.Background()); err != nil {
		t.Fatalf("Recover() error = %v", err)
	}

	err := e.Discard(context.Background(), "not-the-current-run")
	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) || se.Code != aicrerrors.ErrCodeNotFound {
		t.Fatalf("Discard() error = %v, want a StructuredError with ErrCodeNotFound for a stale run ID", err)
	}
	if got := e.Current(); got == nil || got.ID != testRunID {
		t.Errorf("Current() = %+v, want the recovered run left untouched by a rejected Discard", got)
	}
}

// TestStartIsNormalWithoutRecovery pins the zero value: recoveredPending must
// default false so a cold start (no Recover call, as internal/api's
// zero-step-engine suite exercises) behaves exactly as it did before this
// contract existed.
func TestStartIsNormalWithoutRecovery(t *testing.T) {
	e := engine.New(bus.New(64), engine.NewMemoryStore(), newFakeStep(engine.PhaseDiscover))

	if _, err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v, want nil -- recoveredPending must default false on a cold start", err)
	}
}

// TestRetrySaveFailureRestoresRecoveryPending is the regression test for
// Ruling 11. Retry's rollback restores State and Err when its post-accept
// Save fails, but previously left recoveredPending cleared -- so a Save
// failure during Retry (an API blip is exactly the moment an operator is
// hitting Retry after a restart, per Task 8's ConfigMap store) silently
// reopened the bug this whole task exists to close: Start would stop
// returning 409, and the SPA's automatic POST /api/runs on the next load
// would destroy the recovered run.
func TestRetrySaveFailureRestoresRecoveryPending(t *testing.T) {
	inner := engine.NewMemoryStore()
	seed := baseRun(testRunID, engine.StateRunning, engine.PhaseApply, 3)
	if err := inner.Save(context.Background(), seed); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	store := &saveFailingStore{Store: inner}
	e := fourStepEngine(store)

	if err := e.Recover(context.Background()); err != nil {
		t.Fatalf("Recover() error = %v", err)
	}

	store.setFail(true)
	if _, err := e.Retry(context.Background(), testRunID); err == nil {
		t.Fatal("Retry() error = nil, want the manufactured Save failure to surface")
	}
	store.setFail(false)

	_, err := e.Start(context.Background())
	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) || se.Code != aicrerrors.ErrCodeConflict {
		t.Fatalf("Start() error = %v, want ErrCodeConflict -- Retry's rollback must restore recoveredPending along with State", err)
	}
}

// deleteFailingStore wraps a Store and fails every Delete call, to exercise
// Discard's error path. Per Ruling 1 it embeds a real store rather than
// leaving engine.Store nil, since Save/Load/LoadCurrent all fall through
// unmodified.
type deleteFailingStore struct {
	engine.Store
}

func (s *deleteFailingStore) Delete(context.Context) error {
	return errors.New("delete failed")
}

// TestDiscardSurfacesStoreDeleteFailure covers concern 2: Discard is the
// only release valve on Start's recoveredPending gate, so a Delete failure
// nobody has exercised is exactly the kind of gap that could leave the
// console wedged with no way out. It also pins the chosen failure semantics
// (see the Fix round 1 report): e.current and recoveredPending are cleared
// before the store I/O and stay cleared even when Delete fails, so Start is
// never blocked by a store outage -- the error still surfaces to the caller.
func TestDiscardSurfacesStoreDeleteFailure(t *testing.T) {
	inner := engine.NewMemoryStore()
	seed := baseRun(testRunID, engine.StateFailed, engine.PhaseDiscover, 0)
	if err := inner.Save(context.Background(), seed); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	store := &deleteFailingStore{Store: inner}
	e := fourStepEngine(store)

	if err := e.Recover(context.Background()); err != nil {
		t.Fatalf("Recover() error = %v", err)
	}

	err := e.Discard(context.Background(), testRunID)
	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) || se.Code != aicrerrors.ErrCodeUnavailable {
		t.Fatalf("Discard() error = %v, want a StructuredError with ErrCodeUnavailable when Delete fails", err)
	}
	if got := e.Current(); got != nil {
		t.Errorf("Current() = %+v, want nil even though Delete failed -- a store outage must not leave the run wedged in place", got)
	}
	if _, err := e.Start(context.Background()); err != nil {
		t.Errorf("Start() error = %v, want nil -- a failed Delete must not permanently block Start", err)
	}
}

// TestDiscardRejectsALiveRun is the regression test for Ruling 12:
// Discard previously checked only the run ID, not whether the run was
// live, so it could nil e.current out from under an in-flight execute
// goroutine. Parks a run at a decision gate (StateAwaitingDecision is
// live, same as StateRunning) and confirms Discard refuses it with
// ErrCodeConflict rather than touching it.
func TestDiscardRejectsALiveRun(t *testing.T) {
	a := newFakeStep(engine.PhaseDiscover)
	c := newFakeStep(engine.PhaseRecommend, "intent", "platform")
	e := engine.New(bus.New(64), engine.NewMemoryStore(), a, c)

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitState(t, e, run.ID, engine.StateAwaitingDecision)

	err = e.Discard(context.Background(), run.ID)
	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) || se.Code != aicrerrors.ErrCodeConflict {
		t.Fatalf("Discard() error = %v, want a StructuredError with ErrCodeConflict for a live run", err)
	}

	// Assert the run is still there, not just that an error came back --
	// a Discard that errors after already nilling e.current would still be
	// the bug this test exists to catch.
	got := e.Current()
	if got == nil || got.ID != run.ID {
		t.Fatalf("Current() = %+v, want the live run left in place", got)
	}
	if got.State != engine.StateAwaitingDecision {
		t.Errorf("State = %q, want %q -- a rejected Discard must not touch a live run's state", got.State, engine.StateAwaitingDecision)
	}

	// Let the run finish so it doesn't leak a goroutine past the test.
	if err := e.Decide(context.Background(), run.ID, map[string]string{"intent": "training", "platform": "kubeflow"}); err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	waitState(t, e, run.ID, engine.StateDone)
}

// releaseBlockingStep blocks in Run until released, then completes
// normally (not via context cancellation, unlike ctxBlockingStep). Phase is
// configurable so a test can hold whichever step Retry will resume at --
// here PhaseBundle, since Recover's bundleStepIndex() requires exactly one
// step reporting it.
type releaseBlockingStep struct {
	phase   engine.Phase
	release chan struct{}
	entered chan struct{}
}

func (b *releaseBlockingStep) Phase() engine.Phase { return b.phase }
func (b *releaseBlockingStep) Requires() []string  { return nil }
func (b *releaseBlockingStep) Run(_ context.Context, _ *engine.Run, _ engine.Emit) error {
	close(b.entered)
	<-b.release
	return nil
}

// TestDiscardCannotRaceALiveRetryIntoANilCurrent reproduces the reviewer's
// exact scenario from Ruling 12: recover a run, Retry it so it goes live
// and blocks mid-step (simulating a stale browser tab still showing the
// "recovered -- retry or discard?" prompt while another tab has already
// clicked Retry), then Discard the same run ID. Before Ruling 12's guard
// this crashed the whole process -- runStep's merge-back dereferenced a
// nil e.current once the step unblocked. Asserts Discard is rejected and
// the goroutine completes normally afterward, proving the run was left
// intact rather than merely that an error came back.
func TestDiscardCannotRaceALiveRetryIntoANilCurrent(t *testing.T) {
	store := engine.NewMemoryStore()
	seed := baseRun(testRunID, engine.StateRunning, engine.PhaseApply, 3)
	if err := store.Save(context.Background(), seed); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	bundle := &releaseBlockingStep{phase: engine.PhaseBundle, release: make(chan struct{}), entered: make(chan struct{})}
	e := engine.New(bus.New(64), store,
		newFakeStep(engine.PhaseDiscover),
		newFakeStep(engine.PhaseRecommend),
		bundle,
		newFakeStep(engine.PhaseApply),
	)

	if err := e.Recover(context.Background()); err != nil {
		t.Fatalf("Recover() error = %v", err)
	}

	if _, err := e.Retry(context.Background(), testRunID); err != nil {
		t.Fatalf("Retry() error = %v", err)
	}
	select {
	case <-bundle.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Bundle step never entered")
	}

	err := e.Discard(context.Background(), testRunID)

	// Unblock the step before asserting on err, not after: on the pre-fix
	// code Discard has already nilled e.current by this point, and this is
	// exactly what lets the superseded goroutine resume and dereference it
	// -- a process-crashing panic, not a clean test failure. Asserting
	// first (and returning via t.Fatalf on failure) would skip this line
	// entirely and never actually reproduce the crash this test exists to
	// pin.
	close(bundle.release)

	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) || se.Code != aicrerrors.ErrCodeConflict {
		t.Fatalf("Discard() error = %v, want ErrCodeConflict for a run that went live under Discard's feet", err)
	}

	waitState(t, e, testRunID, engine.StateDone)
}

// TestDiscardStillWorksOnARecoveredFailedRun confirms Ruling 12's new
// isLive guard did not over-restrict Discard's original, already-tested
// case: a recovered run that landed StateFailed (not live) must still be
// discardable exactly as before.
func TestDiscardStillWorksOnARecoveredFailedRun(t *testing.T) {
	store := engine.NewMemoryStore()
	seed := baseRun(testRunID, engine.StateFailed, engine.PhaseDiscover, 0)
	if err := store.Save(context.Background(), seed); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	e := fourStepEngine(store)

	if err := e.Recover(context.Background()); err != nil {
		t.Fatalf("Recover() error = %v", err)
	}

	if err := e.Discard(context.Background(), testRunID); err != nil {
		t.Fatalf("Discard() error = %v, want nil -- a non-live recovered run must still be discardable", err)
	}
	if got := e.Current(); got != nil {
		t.Errorf("Current() = %+v, want nil after Discard", got)
	}
}

// --- Task 5: save-failure policy ---------------------------------------

// blockingThenFailingStore blocks the Nth Save call until released, then
// fails it. Used by TestDecidePersistsBeforeAcknowledging to give a
// prematurely-sent resume signal a large, deterministic window to let the
// parked awaitDecisions goroutine react and mistakenly proceed before
// Decide's own Save call (and rollback) ever completes -- a store that
// fails instantly would only catch that ordering bug by scheduler luck, and
// this task's bite-proof step requires it to be caught reliably.
type blockingThenFailingStore struct {
	engine.Store
	mu      sync.Mutex
	calls   int
	blockAt int
	entered chan struct{}
	release chan struct{}
}

func (s *blockingThenFailingStore) Save(ctx context.Context, r *engine.Run) error {
	s.mu.Lock()
	s.calls++
	n := s.calls
	s.mu.Unlock()
	if n == s.blockAt {
		select {
		case s.entered <- struct{}{}:
		default:
		}
		<-s.release
		return errors.New("save failed")
	}
	return s.Store.Save(ctx, r)
}

// TestDecidePersistsBeforeAcknowledging pins the headline fix: Decide must
// not mutate Decisions, clear Pending, flip State to running, and signal
// resume unless the mutation was actually persisted. Before this task,
// Decide never called Save at all, so a pod dying right after a 200 lost
// the operator's choice and recovery re-parked for a decision already made.
//
// Call 1 is Start's own initial write and call 2 is awaitDecisions' single
// parking checkpoint for this one-key gate, so call 3 is guaranteed to be
// Decide's -- see blockingDecideStore's identical reasoning below.
func TestDecidePersistsBeforeAcknowledging(t *testing.T) {
	b := bus.New(64)
	step := newFakeStep(engine.PhaseApply, "apply")
	store := &blockingThenFailingStore{
		Store:   engine.NewMemoryStore(),
		blockAt: 3,
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	e := engine.New(b, store, step)

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitState(t, e, run.ID, engine.StateAwaitingDecision)

	decideErr := make(chan error, 1)
	go func() {
		decideErr <- e.Decide(context.Background(), run.ID, map[string]string{"apply": "yes"})
	}()

	select {
	case <-store.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Decide's Save never reached the blocking store")
	}

	// A prematurely-sent resume signal gets a full, generous window here to
	// wake the parked goroutine and let it wrongly proceed while Decide's
	// own Save call is still outstanding.
	time.Sleep(200 * time.Millisecond)
	if len(step.ran) != 0 {
		t.Fatal("step ran before Decide's Save even returned -- resume was signaled before the decision was durably recorded")
	}

	close(store.release)
	if decErr := <-decideErr; decErr == nil {
		t.Fatal("Decide() error = nil, want the manufactured Save failure to surface")
	}

	got, err := e.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.State != engine.StateAwaitingDecision {
		t.Errorf("State = %q, want %q -- a failed Save must roll back the state transition", got.State, engine.StateAwaitingDecision)
	}
	if _, ok := got.Decisions["apply"]; ok {
		t.Error("Decisions contains \"apply\" after a failed Save -- the mutation was not rolled back")
	}
	if len(got.Pending) != 1 || got.Pending[0] != "apply" {
		t.Errorf("Pending = %v, want [\"apply\"] restored", got.Pending)
	}

	// The step must not have advanced: a resume signal sent despite the
	// failed Save would let it proceed on a decision that was never
	// recorded.
	select {
	case <-step.ran:
		t.Error("step ran despite Decide's Save failing -- resume was signaled before the decision was durably recorded")
	case <-time.After(150 * time.Millisecond):
	}
}

// TestDecideRollbackDoesNotOverwriteATerminalStateFromConcurrentShutdown
// reproduces the narrow window a same-epoch (not superseded) rollback can
// still corrupt: awaitDecisions is still parked on <-resume while Decide's
// Save is in flight (resume is not sent until Save succeeds), so a SIGTERM
// landing in that exact window cancels the run's context, awaitDecisions
// takes the ctx.Done() branch, and finish sets StateFailed -- all via the
// SAME epoch, since only Start and Retry bump it. Without an isLive check
// alongside the identity+epoch guard, Decide's rollback (once its own Save
// finally fails) would overwrite that terminal state with
// StateAwaitingDecision: a live state with no goroutine behind it, the
// exact wedge class this whole discipline exists to prevent.
//
// Reuses blockingThenFailingStore: blockAt: 3 blocks Decide's own Save
// (call 3, same reasoning as TestDecidePersistsBeforeAcknowledging) while
// finish's terminal save (call 4, once CancelAndWait triggers it) passes
// straight through the same store unblocked.
func TestDecideRollbackDoesNotOverwriteATerminalStateFromConcurrentShutdown(t *testing.T) {
	b := bus.New(64)
	step := newFakeStep(engine.PhaseApply, "apply")
	store := &blockingThenFailingStore{
		Store:   engine.NewMemoryStore(),
		blockAt: 3,
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	e := engine.New(b, store, step)

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitState(t, e, run.ID, engine.StateAwaitingDecision)

	decideErr := make(chan error, 1)
	go func() {
		decideErr <- e.Decide(context.Background(), run.ID, map[string]string{"apply": "yes"})
	}()

	select {
	case <-store.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Decide's Save never reached the blocking store")
	}

	// Simulate SIGTERM landing while Decide's Save is still outstanding.
	// CancelAndWait blocks until the run's goroutine has exited and
	// persisted a terminal state, which requires finish's own Save (call 4)
	// to go through -- it is not the blocked call, so it does.
	cancelErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cancelErr <- e.CancelAndWait(ctx)
	}()

	waitState(t, e, run.ID, engine.StateFailed)
	if cancErr := <-cancelErr; cancErr != nil {
		t.Fatalf("CancelAndWait() error = %v", cancErr)
	}

	// Now let Decide's own Save fail and its rollback run, against a run
	// that is already terminal.
	close(store.release)
	if decErr := <-decideErr; decErr == nil {
		t.Fatal("Decide() error = nil, want the manufactured Save failure to surface")
	}

	got, err := e.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.State != engine.StateFailed {
		t.Errorf("State = %q, want %q -- the rollback overwrote a terminal state set by a concurrent shutdown with a live one", got.State, engine.StateFailed)
	}
}

// TestDecideSucceedsAndPersists is the happy-path pair to the test above:
// on success Decide saves exactly once and the persisted snapshot carries
// the new decisions.
func TestDecideSucceedsAndPersists(t *testing.T) {
	b := bus.New(64)
	step := newFakeStep(engine.PhaseApply, "apply")
	inner := engine.NewMemoryStore()
	store := &countingSaveStore{Store: inner}
	e := engine.New(b, store, step)

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitState(t, e, run.ID, engine.StateAwaitingDecision)

	before := store.count()
	if decErr := e.Decide(context.Background(), run.ID, map[string]string{"apply": "yes"}); decErr != nil {
		t.Fatalf("Decide() error = %v", decErr)
	}
	if delta := store.count() - before; delta != 1 {
		t.Errorf("Decide() issued %d Save calls, want exactly 1", delta)
	}

	persisted, err := inner.LoadCurrent(context.Background())
	if err != nil {
		t.Fatalf("LoadCurrent() error = %v", err)
	}
	if persisted.Decisions["apply"] != "yes" {
		t.Errorf("persisted Decisions[\"apply\"] = %q, want \"yes\" -- Decide's Save did not carry the new decision", persisted.Decisions["apply"])
	}
	if persisted.State != engine.StateRunning {
		t.Errorf("persisted State = %q, want %q", persisted.State, engine.StateRunning)
	}

	waitState(t, e, run.ID, engine.StateDone)
}

// ctxRespectingStore's Save fails if the context it is handed is already
// canceled. memoryStore (which it wraps) ignores context entirely, so it
// cannot distinguish a save Decide detached from one it didn't -- this is
// what makes that distinction observable from a test.
type ctxRespectingStore struct {
	engine.Store
}

func (s *ctxRespectingStore) Save(ctx context.Context, r *engine.Run) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.Store.Save(ctx, r)
}

// TestDecideSurvivesAnAlreadyCanceledCallerContext is the regression test
// for engine.go's context.WithoutCancel(ctx) inside Decide: reverting it to
// a bare ctx reintroduces "a browser tab closed the instant before Decide's
// save reaches the store silently discards the operator's decision" while
// every other Decide test stays green -- all four existing ones pass
// context.Background(), never a canceled or mid-cancel context, so none of
// them can catch this. The property asserted is the one that actually
// matters: not merely that the save observed a non-canceled context, but
// that the decision is both accepted (nil error) and durably persisted
// (read back through LoadCurrent, not just Get's in-memory copy) despite
// the caller's context being canceled before Decide is ever called.
func TestDecideSurvivesAnAlreadyCanceledCallerContext(t *testing.T) {
	b := bus.New(64)
	step := newFakeStep(engine.PhaseApply, "apply")
	inner := engine.NewMemoryStore()
	store := &ctxRespectingStore{Store: inner}
	e := engine.New(b, store, step)

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitState(t, e, run.ID, engine.StateAwaitingDecision)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // canceled before Decide is even called, not mid-flight

	if decErr := e.Decide(ctx, run.ID, map[string]string{"apply": "yes"}); decErr != nil {
		t.Fatalf("Decide() error = %v, want nil -- an already-canceled caller context must not fail the save", decErr)
	}

	persisted, err := inner.LoadCurrent(context.Background())
	if err != nil {
		t.Fatalf("LoadCurrent() error = %v", err)
	}
	if persisted.Decisions["apply"] != "yes" {
		t.Errorf("persisted Decisions[\"apply\"] = %q, want \"yes\" -- the decision did not survive an already-canceled caller context", persisted.Decisions["apply"])
	}
	if persisted.State != engine.StateRunning {
		t.Errorf("persisted State = %q, want %q", persisted.State, engine.StateRunning)
	}

	waitState(t, e, run.ID, engine.StateDone)
}

// countingSaveStore counts every Save call so a test can assert exactly how
// many happened across a narrow window.
type countingSaveStore struct {
	engine.Store
	calls atomic.Int64
}

func (s *countingSaveStore) Save(ctx context.Context, r *engine.Run) error {
	s.calls.Add(1)
	return s.Store.Save(ctx, r)
}

func (s *countingSaveStore) count() int {
	return int(s.calls.Load())
}

// blockingDecideStore blocks the Nth Save call on a channel until released,
// signaling entered first. Task 5's constraint is that no store I/O happens
// while Decide holds e.mu -- callIndex pins the block to Decide's own Save
// deterministically: call 1 is Start's initial write, call 2 is
// awaitDecisions' single parking checkpoint for a one-key gate, so call 3 is
// guaranteed to be Decide's.
type blockingDecideStore struct {
	engine.Store
	mu      sync.Mutex
	calls   int
	blockAt int
	entered chan struct{}
	release chan struct{}
}

func (s *blockingDecideStore) Save(ctx context.Context, r *engine.Run) error {
	s.mu.Lock()
	s.calls++
	n := s.calls
	s.mu.Unlock()
	if n == s.blockAt {
		select {
		case s.entered <- struct{}{}:
		default:
		}
		<-s.release
	}
	return s.Store.Save(ctx, r)
}

// TestDecideDoesNotHoldTheLockDuringIO is the constraint the observer's
// per-watch-event scope accessor depends on: CurrentID takes e.mu on a hot
// path, so a Decide call blocked inside a slow store round trip must not
// block it too.
func TestDecideDoesNotHoldTheLockDuringIO(t *testing.T) {
	b := bus.New(64)
	step := newFakeStep(engine.PhaseApply, "apply")
	store := &blockingDecideStore{
		Store:   engine.NewMemoryStore(),
		blockAt: 3,
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	e := engine.New(b, store, step)

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitState(t, e, run.ID, engine.StateAwaitingDecision)

	decideErr := make(chan error, 1)
	go func() {
		decideErr <- e.Decide(context.Background(), run.ID, map[string]string{"apply": "yes"})
	}()

	select {
	case <-store.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Decide's Save never reached the blocking store")
	}

	idDone := make(chan struct{})
	go func() {
		if _, ok := e.CurrentID(); !ok {
			t.Error("CurrentID() ok = false, want true")
		}
		close(idDone)
	}()
	select {
	case <-idDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("CurrentID blocked behind Decide's in-flight Save -- e.mu must not be held during store I/O")
	}

	close(store.release)
	if err := <-decideErr; err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	waitState(t, e, run.ID, engine.StateDone)
}

// stepIndexRecordingStore records the StepIndex and Phase of every Save
// call so a test can inspect the exact sequence a run's checkpoints took.
type stepIndexRecordingStore struct {
	engine.Store
	mu    sync.Mutex
	saves []stepIndexSave
}

type stepIndexSave struct {
	phase     engine.Phase
	stepIndex int
}

func (s *stepIndexRecordingStore) Save(ctx context.Context, r *engine.Run) error {
	s.mu.Lock()
	s.saves = append(s.saves, stepIndexSave{phase: r.Phase, stepIndex: r.StepIndex})
	s.mu.Unlock()
	return s.Store.Save(ctx, r)
}

func (s *stepIndexRecordingStore) snapshot() []stepIndexSave {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stepIndexSave(nil), s.saves...)
}

// TestStepSuccessCheckpointsCursorBeforeNextStep pins the ordering fix: the
// save that follows a step's success must carry the advanced StepIndex
// before the next step's own checkpoint is written. Before this task, the
// merge-back save ran before execute()'s own StepIndex increment, so a
// crash in that window replayed a completed step on Retry.
func TestStepSuccessCheckpointsCursorBeforeNextStep(t *testing.T) {
	b := bus.New(64)
	step1 := newFakeStep(engine.PhaseDiscover)
	step2 := newFakeStep(engine.PhaseRecommend)
	store := &stepIndexRecordingStore{Store: engine.NewMemoryStore()}
	e := engine.New(b, store, step1, step2)

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitState(t, e, run.ID, engine.StateDone)

	saves := store.snapshot()
	lastDiscoverStepIndex := -1
	sawRecommend := false
	for _, sv := range saves {
		if sv.phase == engine.PhaseDiscover {
			lastDiscoverStepIndex = sv.stepIndex
		}
		if sv.phase == engine.PhaseRecommend && !sawRecommend {
			sawRecommend = true
			if sv.stepIndex != 1 {
				t.Errorf("first Recommend-phase save has StepIndex = %d, want 1 -- step 2 began before step 1's checkpoint carried the advanced cursor", sv.stepIndex)
			}
		}
	}
	if lastDiscoverStepIndex != 1 {
		t.Errorf("last Discover-phase save carried StepIndex = %d, want 1 -- the step-success checkpoint must carry the advanced cursor", lastDiscoverStepIndex)
	}
}

// countingFailStore fails every Save call from the Nth (1-indexed) onward,
// deterministically -- independent of goroutine scheduling -- so a test can
// pin exactly which checkpoint in a call sequence is expected to fail.
type countingFailStore struct {
	engine.Store
	mu       sync.Mutex
	calls    int
	failFrom int
}

func (s *countingFailStore) Save(ctx context.Context, r *engine.Run) error {
	s.mu.Lock()
	s.calls++
	n := s.calls
	s.mu.Unlock()
	if s.failFrom > 0 && n >= s.failFrom {
		return errors.New("save failed")
	}
	return s.Store.Save(ctx, r)
}

// waitForTerminalEvent blocks until it observes the run's terminal phase
// event (the last statement finish() executes) or the deadline passes.
// Waiting on this rather than polling Get()/waitState matters for tests that
// then inspect a shared log buffer: e.current.State flips to a terminal
// value under e.mu *before* finish() attempts its Save and any logging that
// follows a failure, so a test that only waits for that state flip can read
// the log buffer concurrently with finish() still writing to it -- a real
// data race, not a flaky one. The channel receive here happens-after every
// statement finish() executed in program order (same goroutine), including
// its slog calls, which is what makes reading the buffer safe afterward.
func waitForTerminalEvent(t *testing.T, sub <-chan bus.Event, runID string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-sub:
			if ev.RunID == runID && ev.Kind == bus.KindPhase && strings.HasPrefix(ev.Message, "run ") {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for the run's terminal event")
		}
	}
}

// TestBestEffortCheckpointFailureIsLogged pins the "never silent" half of
// the policy: a failing mid-step save must not fail the run, but must warn
// -- six to thirty of these across a run is the only signal recovery has
// quietly stopped working.
func TestBestEffortCheckpointFailureIsLogged(t *testing.T) {
	buf := captureLogs(t)
	b := bus.New(64)
	sub, unsubscribe := b.Subscribe(0)
	defer unsubscribe()

	step := newFakeStep(engine.PhaseDiscover) // no Requires -- no decision gate to cross
	store := &countingFailStore{Store: engine.NewMemoryStore(), failFrom: 2}
	e := engine.New(b, store, step)

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitForTerminalEvent(t, sub, run.ID)

	// Match the composed line, not a bare substring: "run checkpoint
	// failed" is also a substring of finish's "terminal run checkpoint
	// failed..." (which this same countingFailStore also fails, at
	// LevelError), and this store's failFrom:2 makes that terminal line
	// appear in the same buffer. slog's TextHandler quotes msg exactly, so
	// `msg="run checkpoint failed"` (closing quote immediately after
	// "failed") cannot match the longer terminal message, whose quoted
	// value continues past that point -- unlike two independent
	// strings.Contains checks, which the terminal ERROR line and any one
	// mid-step WARN line can satisfy between them even if the level and
	// message of each were swapped or downgraded.
	const wantLine = `level=WARN msg="run checkpoint failed"`
	logs := buf.String()
	// Two mid-step checkpoints fail with failFrom:2 on this one-step,
	// no-Requires engine: runStep's phase-start save and its step-success
	// save. Both must warn.
	if got := strings.Count(logs, wantLine); got < 2 {
		t.Errorf("logs contain %d occurrences of %q, want at least 2\nlogs:\n%s", got, wantLine, logs)
	}

	waitState(t, e, run.ID, engine.StateDone)
}

// terminalOnlyFailStore fails a Save only when the snapshot's State is
// terminal, so a test can isolate finish()'s own checkpoint from every
// mid-run one.
type terminalOnlyFailStore struct {
	engine.Store
}

func (s *terminalOnlyFailStore) Save(ctx context.Context, r *engine.Run) error {
	if r.State == engine.StateDone || r.State == engine.StateFailed || r.State == engine.StateActive {
		return errors.New("terminal save failed")
	}
	return s.Store.Save(ctx, r)
}

// TestTerminalSaveFailureIsVisible pins the last leg of the policy:
// finish's terminal save cannot roll back the run is already terminal --
// but a failure there must not vanish silently. It must log at error level
// and publish a bus event, because the real consequence is that the next
// startup recovers a stale earlier checkpoint instead of finding nothing.
func TestTerminalSaveFailureIsVisible(t *testing.T) {
	buf := captureLogs(t)
	b := bus.New(64)
	sub, unsubscribe := b.Subscribe(0)
	defer unsubscribe()

	step := newFakeStep(engine.PhaseDiscover)
	store := &terminalOnlyFailStore{Store: engine.NewMemoryStore()}
	e := engine.New(b, store, step)

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	// Drain for the KindError event first: finish() publishes it before the
	// terminal phase event, and both happen after the failed Save and its
	// slog.Error call, in that same goroutine's program order -- receiving
	// either over the channel is what makes the subsequent buf.String() read
	// safe rather than racing a still-running finish().
	deadline := time.After(2 * time.Second)
	foundErrorEvent := false
drainTerminal:
	for !foundErrorEvent {
		select {
		case ev := <-sub:
			if ev.RunID == run.ID && ev.Kind == bus.KindError && ev.Level == bus.LevelError {
				foundErrorEvent = true
			}
		case <-deadline:
			break drainTerminal
		}
	}
	if !foundErrorEvent {
		t.Fatal("no error-level bus event published for the failed terminal checkpoint")
	}

	logs := buf.String()
	if !strings.Contains(logs, "level=ERROR") {
		t.Errorf("logs = %q, want an ERROR-level entry for the terminal checkpoint failure", logs)
	}

	waitState(t, e, run.ID, engine.StateDone)
}

// TestRunStepMergesPartialComponentStateOnFailure pins Ruling 14 (Task 6
// fix round 1): runStep's merge-back historically ran only in the step
// SUCCESS path, so a step's writes to r.Components -- Apply's per-component
// projection -- were made against a private scratch copy and silently
// discarded whenever the step returned an error, because the error branch
// returned before reaching the merge block.
//
// This is the dominant Apply failure shape, not an edge case:
// internal/applier/applier.go's Apply runs deploy.sh WITHOUT
// --best-effort, so the first component to exhaust its retries ends the
// whole run. A run that fails during Apply must still recover with the
// rows the step wrote before it failed, or a recovered run redraws as a
// bare failure again -- exactly what this task exists to prevent.
//
// Deliberately goes through e.Start(), not step.Run() directly:
// internal/steps/apply_test.go already proves the step-local upsert logic
// works in isolation, but nothing exercised whether those writes survive
// runStep's merge-back on the failure path, which is where the bug
// actually lived.
func TestRunStepMergesPartialComponentStateOnFailure(t *testing.T) {
	b := bus.New(64)
	store := engine.NewMemoryStore()
	failing := newFakeStep(engine.PhaseApply)
	failing.err = errors.New("deploy.sh failed: exit status 1")
	failing.components = []engine.ComponentState{
		{Name: "gpu-operator", Index: 1, Total: 2, Status: "installed"},
		{Name: "kai-scheduler", Index: 2, Total: 2, Status: "failed"},
	}
	e := engine.New(b, store, failing)

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	final := waitState(t, e, run.ID, engine.StateFailed)
	if len(final.Components) != 2 {
		t.Fatalf("Components = %+v, want the 2 rows the failing step wrote before returning its error", final.Components)
	}
	byName := map[string]engine.ComponentState{}
	for _, c := range final.Components {
		byName[c.Name] = c
	}
	if got := byName["gpu-operator"]; got.Status != "installed" {
		t.Errorf("gpu-operator = %+v, want status=installed", got)
	}
	if got := byName["kai-scheduler"]; got.Status != "failed" {
		t.Errorf("kai-scheduler = %+v, want status=failed", got)
	}

	// The in-memory run being right is a different claim from the
	// PERSISTED record being right, and recovery reads the record, not
	// e.Current(). The merge must land before finish's terminal save, or
	// that save captures the pre-Apply Components instead of these rows.
	persisted, err := store.LoadCurrent(context.Background())
	if err != nil {
		t.Fatalf("LoadCurrent() error = %v", err)
	}
	if len(persisted.Components) != 2 {
		t.Fatalf("persisted Components = %+v, want 2 rows -- the terminal save must capture them, not the pre-Apply state", persisted.Components)
	}
}

// TestRunStepMergesComponentStateOnSuccess is the success-path companion:
// it pins that the pre-existing merge (never broken, only the failure
// branch was) still runs, through the real engine rather than a direct
// step.Run() call.
func TestRunStepMergesComponentStateOnSuccess(t *testing.T) {
	b := bus.New(64)
	store := engine.NewMemoryStore()
	ok := newFakeStep(engine.PhaseApply)
	ok.components = []engine.ComponentState{
		{Name: "gpu-operator", Index: 1, Total: 1, Status: "installed"},
	}
	e := engine.New(b, store, ok)

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	final := waitState(t, e, run.ID, engine.StateDone)
	if len(final.Components) != 1 || final.Components[0].Status != "installed" {
		t.Fatalf("Components = %+v, want 1 row with status=installed", final.Components)
	}

	persisted, err := store.LoadCurrent(context.Background())
	if err != nil {
		t.Fatalf("LoadCurrent() error = %v", err)
	}
	if len(persisted.Components) != 1 {
		t.Fatalf("persisted Components = %+v, want 1 row", persisted.Components)
	}
}

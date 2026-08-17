package engine_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
)

type fakeStep struct {
	phase    engine.Phase
	requires []string
	err      error
	ran      chan struct{}
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
	return f.err
}

func waitState(t *testing.T, e *engine.Engine, id string, want engine.State) *engine.Run {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			got, _ := e.Get(id)
			t.Fatalf("timed out waiting for state %q, last state %q", want, got.State)
		default:
		}
		r, err := e.Get(id)
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

	if err := e.Decide(run.ID, map[string]string{"intent": "training"}); err == nil {
		t.Error("Decide() with a missing key should error")
	}

	if err := e.Decide(run.ID, map[string]string{"intent": "training", "platform": "kubeflow"}); err != nil {
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

	if decideErr := e.Decide(run.ID, map[string]string{
		"intent": "training", "platform": "kubeflow", "apply": "yes",
	}); decideErr == nil {
		t.Fatal("Decide() with a key not currently pending ('apply') should error")
	}

	got, err := e.Get(run.ID)
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
	if decideErr := e.Decide(run.ID, map[string]string{"intent": "training", "platform": "kubeflow"}); decideErr != nil {
		t.Fatalf("Decide() with only the pending keys error = %v", decideErr)
	}

	// The run now parks on Apply's own gate, proving "apply" is still
	// genuinely pending rather than having been silently pre-satisfied.
	waitState(t, e, run.ID, engine.StateAwaitingDecision)
	got, err = e.Get(run.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if len(got.Pending) != 1 || got.Pending[0] != "apply" {
		t.Fatalf("Pending = %v, want [apply]", got.Pending)
	}
	if len(d.ran) != 0 {
		t.Fatal("Apply ran before its confirm gate was answered")
	}

	if decideErr := e.Decide(run.ID, map[string]string{"apply": "yes"}); decideErr != nil {
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

	got, _ := e.Get(run.ID)
	got.Decisions["tamper"] = "yes"
	again, _ := e.Get(run.ID)
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
		if _, err := e.Get(run.ID); err != nil {
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

	if _, err := e.Retry(run.ID); err != nil {
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

	if _, err := e.Retry(run.ID); err == nil {
		t.Error("Retry() error = nil, want a conflict on a completed run")
	}
}

func TestRetryRejectsAnUnknownRun(t *testing.T) {
	e := engine.New(bus.New(8), engine.NewMemoryStore(), newFakeStep(engine.PhaseDiscover))
	if _, err := e.Retry("nope"); err == nil {
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

	if _, err := e.Retry(run.ID); err != nil {
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
// Retry refuses because it requires StateFailed. This is currently inert
// (memoryStore.Save never errors) but docs/phase-2-handoff.md's
// ConfigMap-backed store, landing in 2b-ii, is precisely where Save
// starts failing for real.
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
	if _, retryErr := e.Retry(run.ID); retryErr == nil {
		t.Fatal("Retry() error = nil, want the store's Save error")
	}
	store.setFail(false)

	// The assertion that matters: State is still StateFailed, so a
	// subsequent Retry could still succeed. Asserting only the error above
	// would pass even with the run wedged at StateRunning.
	got, err := e.Get(run.ID)
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

	if err := e.Decide(run.ID, map[string]string{"apply": "yes"}); err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	waitState(t, e, run.ID, engine.StateFailed)

	if _, err := e.Retry(run.ID); err != nil {
		t.Fatalf("Retry() error = %v", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			got, _ := e.Get(run.ID)
			t.Fatalf("timed out waiting for state %q, last state %q", engine.StateDone, got.State)
		default:
		}
		got, err := e.Get(run.ID)
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
	got, err := e.Get(run.ID)
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
// the persisted one was ever written. Once 2b-ii swaps in the ConfigMap
// store, a Save under the canceled run context returns context.Canceled
// before issuing an API call, and the run is left wedged at `running` across
// a restart.
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
		if _, retryErr := e.Retry(run.ID); !errors.Is(retryErr, engine.ErrDraining) {
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
		got, err := e.Get(run.ID)
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
	got, _ := e.Get(run.ID)
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

	if _, err := e.Retry(testRunID); err != nil {
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
	if _, err := e.Retry(testRunID); err == nil {
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
	if err := e.Decide(run.ID, map[string]string{"intent": "training", "platform": "kubeflow"}); err != nil {
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

	if _, err := e.Retry(testRunID); err != nil {
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

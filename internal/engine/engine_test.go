package engine_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestCancelAndWaitIsIdempotentAndSafeWithNoRun(t *testing.T) {
	e := engine.New(bus.New(8), engine.NewMemoryStore(), newFakeStep(engine.PhaseDiscover))
	ctx := context.Background()

	// No run has ever started.
	if err := e.CancelAndWait(ctx); err != nil {
		t.Fatalf("CancelAndWait() with no run error = %v", err)
	}

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

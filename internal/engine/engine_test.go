package engine_test

import (
	"context"
	"errors"
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

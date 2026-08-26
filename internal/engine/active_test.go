package engine

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/prove"
)

// fakeStep mirrors engine_test.go's fakeStep (package engine_test) field for
// field, but lives in this package's white-box test files: the tests below
// reference Phase/State constants unqualified (PhaseApply, StateActive),
// which only compiles from inside package engine. The two types cannot be
// shared across the package boundary, so this is a second copy of the same
// shape rather than an import -- not a parallel harness invented from
// scratch.
type fakeStep struct {
	phase      Phase
	requires   []string
	err        error
	ran        chan struct{}
	components []ComponentState
}

func (f *fakeStep) Phase() Phase       { return f.phase }
func (f *fakeStep) Requires() []string { return f.requires }
func (f *fakeStep) Run(_ context.Context, r *Run, emit Emit) error {
	// ran is nil for every fakeStep literal in this file -- none of these
	// tests need to observe invocation count, only the terminal state -- so
	// sending unconditionally would block forever on a nil channel.
	if f.ran != nil {
		f.ran <- struct{}{}
	}
	emit(bus.Event{Kind: bus.KindLog, Message: string(f.phase) + " ran"})
	r.Artifacts[string(f.phase)] = []byte("done")
	r.Components = append(r.Components, f.components...)
	return f.err
}

// fakeActiveStep is fakeStep's shape plus the ActiveStep hook this file
// exists to test. A separate type, not fakeStep embedding ActiveStep-ness
// conditionally: isActive's whole point is that implementing the interface
// is a per-type decision, so the test double for "does not implement it"
// (fakeStep) and "implements it" (fakeActiveStep) must actually be different
// types, not one type with a flag isActive would never see.
type fakeActiveStep struct {
	phase    Phase
	active   bool
	err      error
	workload Workload
}

func (f *fakeActiveStep) Phase() Phase       { return f.phase }
func (f *fakeActiveStep) Requires() []string { return nil }
func (f *fakeActiveStep) Run(_ context.Context, r *Run, emit Emit) error {
	emit(bus.Event{Kind: bus.KindLog, Message: string(f.phase) + " ran"})
	r.Artifacts[string(f.phase)] = []byte("done")
	r.Workload = f.workload
	return f.err
}
func (f *fakeActiveStep) LeavesWorkloadRunning() bool { return f.active }

// newTestEngine builds an Engine with a fresh bus and memory store -- the
// combination every test in this file needs and none of them varies.
func newTestEngine(t *testing.T, steps ...Step) *Engine {
	t.Helper()
	return New(bus.New(64), NewMemoryStore(), steps...)
}

// startAndWait starts e and blocks until the run leaves StateRunning /
// StateAwaitingDecision. It polls for any of StateDone, StateFailed, or
// StateActive rather than isTerminal(state) -- isTerminal deliberately
// excludes StateActive (see engine.go), and a helper keyed off it would hang
// forever on exactly the case this file is testing.
func startAndWait(t *testing.T, e *Engine) *Run {
	t.Helper()
	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	deadline := time.After(2 * time.Second)
	for {
		r, err := e.Get(context.Background(), run.ID)
		if err == nil && (r.State == StateDone || r.State == StateFailed || r.State == StateActive) {
			return r
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for a final state, last state %q", r.State)
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A step's Workload write must survive runStep's merge, the same as
// Artifacts, Decisions, and Components -- otherwise Prove (Task 5) can set
// run.Workload on its own scratch copy and have it silently discarded,
// leaving e.current.Workload at the zero value on a run that just went
// StateActive with a workload genuinely running in the cluster.
func TestActiveStepWorkloadSurvivesTheMerge(t *testing.T) {
	want := Workload{Namespace: "aicrme-prove", Kind: "Job", Name: "prove-run-abc"}
	e := newTestEngine(t, &fakeActiveStep{phase: PhaseProve, active: true, workload: want})
	run := startAndWait(t, e)
	if run.Workload != want {
		t.Errorf("Workload = %+v, want %+v", run.Workload, want)
	}
}

func TestRunWithActiveFinalStepEndsActive(t *testing.T) {
	e := newTestEngine(t, &fakeStep{phase: PhaseApply}, &fakeActiveStep{phase: PhaseProve, active: true})
	run := startAndWait(t, e)
	if run.State != StateActive {
		t.Errorf("State = %q, want %q", run.State, StateActive)
	}
}

// The interface is opt-IN. A step that implements it and returns false, and a
// step that does not implement it at all, must both finish at StateDone --
// otherwise every future step author has to know about this hook.
func TestRunWithNonActiveFinalStepEndsDone(t *testing.T) {
	for name, last := range map[string]Step{
		"does not implement ActiveStep": &fakeStep{phase: PhaseProve},
		"implements it, returns false":  &fakeActiveStep{phase: PhaseProve, active: false},
	} {
		t.Run(name, func(t *testing.T) {
			e := newTestEngine(t, &fakeStep{phase: PhaseApply}, last)
			if run := startAndWait(t, e); run.State != StateDone {
				t.Errorf("State = %q, want %q", run.State, StateDone)
			}
		})
	}
}

// Only the FINAL step decides. An ActiveStep in the middle leaves nothing
// running once later steps have run past it.
func TestActiveStepThatIsNotLastDoesNotMakeRunActive(t *testing.T) {
	e := newTestEngine(t, &fakeActiveStep{phase: PhaseApply, active: true}, &fakeStep{phase: PhaseProve})
	if run := startAndWait(t, e); run.State != StateDone {
		t.Errorf("State = %q, want %q", run.State, StateDone)
	}
}

// A failing run never reaches StateActive, whatever the final step claims.
func TestFailedRunNeverEndsActive(t *testing.T) {
	e := newTestEngine(t, &fakeStep{phase: PhaseApply, err: errors.New("boom")},
		&fakeActiveStep{phase: PhaseProve, active: true})
	if run := startAndWait(t, e); run.State != StateFailed {
		t.Errorf("State = %q, want %q", run.State, StateFailed)
	}
}

// A final ActiveStep that itself fails must still land StateFailed, not
// StateActive: isActive is only consulted once the step loop has run to
// completion without error (engine.go's execute), so a failing final step's
// own LeavesWorkloadRunning claim must never be reached. Task 1's review
// found that mutating runStep's failure branch to promote a failing step's
// isActive claim to StateActive left all of Task 1's tests green -- this
// pins the gap those tests missed.
func TestFailedActiveStepNeverEndsActive(t *testing.T) {
	e := newTestEngine(t, &fakeActiveStep{phase: PhaseProve, active: true, err: errors.New("boom")})
	if run := startAndWait(t, e); run.State != StateFailed {
		t.Errorf("State = %q, want %q", run.State, StateFailed)
	}
}

func TestStartRejectsWhileWorkloadActive(t *testing.T) {
	e := newTestEngine(t, &fakeActiveStep{phase: PhaseProve, active: true})
	startAndWait(t, e)

	_, err := e.Start(context.Background())
	if err == nil {
		t.Fatal("Start() succeeded over a live workload, want conflict")
	}
	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) || se.Code != aicrerrors.ErrCodeConflict {
		t.Errorf("Start() error = %v, want ErrCodeConflict", err)
	}
	// The remedy has to be in the message: the operator's only way out is
	// Stop, and a bare "conflict" leaves them guessing.
	if !strings.Contains(err.Error(), "stop") {
		t.Errorf("Start() error = %q, want it to name stopping the workload", err)
	}
}

func TestDiscardRejectsActiveRun(t *testing.T) {
	e := newTestEngine(t, &fakeActiveStep{phase: PhaseProve, active: true})
	run := startAndWait(t, e)

	err := e.Discard(context.Background(), run.ID)
	if err == nil {
		t.Fatal("Discard() succeeded on an active run -- it would orphan the workload")
	}
	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) || se.Code != aicrerrors.ErrCodeConflict {
		t.Errorf("Discard() error = %v, want ErrCodeConflict", err)
	}
	if !strings.Contains(err.Error(), "stop") {
		t.Errorf("Discard() error = %q, want it to name stopping the workload", err)
	}
	// And the run must survive: a rejected Discard that still nils e.current
	// is the bug wearing a different hat.
	if got, ok := e.CurrentID(); !ok || got != run.ID {
		t.Errorf("CurrentID() = %q, %v after rejected Discard, want %q, true", got, ok, run.ID)
	}
}

// Discard must still work for the states it exists to serve.
func TestDiscardStillAcceptsFailedRun(t *testing.T) {
	e := newTestEngine(t, &fakeStep{phase: PhaseApply, err: errors.New("boom")})
	run := startAndWait(t, e)
	if err := e.Discard(context.Background(), run.ID); err != nil {
		t.Fatalf("Discard() on a failed run error = %v, want nil", err)
	}
}

// stopTestWorkload is the Workload every Stop test's fakeActiveStep records,
// so run.Workload is non-zero the moment the run goes StateActive --
// stoppable's second arm (this file's Stop tests exercise both) depends on
// that non-zero value surviving into StateDone, exactly as
// TestActiveStepWorkloadSurvivesTheMerge already pins for the merge itself.
var stopTestWorkload = Workload{Namespace: prove.Namespace, Kind: "Job", Name: "prove-run-stop-test"}

// newStopTestEngine builds an engine whose final step is an ActiveStep,
// pins the next run's ID to id (so the caller can pre-seed a matching Job in
// cs before Start), and wires a prove.Client wrapping cs as the engine's
// Stop dependency. Every Stop test in this file shares this shape; only the
// clientset's reactors and pre-seeded objects vary between them.
func newStopTestEngine(t *testing.T, id string, cs *fake.Clientset) (*Engine, *prove.Client) {
	t.Helper()
	e := newTestEngine(t, &fakeActiveStep{phase: PhaseProve, active: true, workload: stopTestWorkload})
	e.newID = func() string { return id }
	client := prove.NewClient(cs)
	e.SetProveClient(client)
	return e, client
}

// TestStopEndsTheRunAtDone is Stop's core contract: Active -> Stop ->
// StateDone, with the workload actually gone from the cluster, not merely
// an error-free return. Asserting absence directly against the fake
// clientset (rather than trusting Stop's nil error alone) is deliberate --
// this phase already caught a WaitAbsent test pair that passed against a
// fake that never polled at all (docs/superpowers/specs' own recorded
// trap), so a Stop test that only checks err == nil would repeat it.
func TestStopEndsTheRunAtDone(t *testing.T) {
	const runID = "run-stop-ends-done"
	cs := fake.NewSimpleClientset()
	e, client := newStopTestEngine(t, runID, cs)
	ctx := context.Background()
	if err := client.EnsureNamespace(ctx); err != nil {
		t.Fatalf("EnsureNamespace() error = %v", err)
	}
	if err := client.Apply(ctx, runID); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	run := startAndWait(t, e)
	if run.State != StateActive {
		t.Fatalf("State = %q before Stop, want %q", run.State, StateActive)
	}

	if err := e.Stop(ctx, run.ID); err != nil {
		t.Fatalf("Stop() error = %v, want nil", err)
	}

	got, err := e.Get(ctx, run.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.State != StateDone {
		t.Errorf("State after Stop = %q, want %q", got.State, StateDone)
	}

	if _, getErr := cs.BatchV1().Jobs(prove.Namespace).
		Get(ctx, prove.WorkloadName(runID), metav1.GetOptions{}); !apierrors.IsNotFound(getErr) {
		t.Errorf("workload still present after Stop (Get error = %v), want NotFound", getErr)
	}
}

// TestStopIsIdempotent calls Stop twice in sequence on the same run: the
// first actually deletes a real workload and finishes the run at
// StateDone; the second targets a run that is already StateDone but still
// carries the workload identity Stop itself just recorded, and per spec §7
// ("stopping an already-stopped workload succeeds") must still return nil
// rather than the ErrCodeConflict TestStopRejectsNonActiveRun pins for a
// StateDone run that never held a workload at all.
func TestStopIsIdempotent(t *testing.T) {
	const runID = "run-stop-idempotent"
	cs := fake.NewSimpleClientset()
	e, client := newStopTestEngine(t, runID, cs)
	ctx := context.Background()
	if err := client.EnsureNamespace(ctx); err != nil {
		t.Fatalf("EnsureNamespace() error = %v", err)
	}
	if err := client.Apply(ctx, runID); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	run := startAndWait(t, e)
	if err := e.Stop(ctx, run.ID); err != nil {
		t.Fatalf("first Stop() error = %v, want nil", err)
	}
	if err := e.Stop(ctx, run.ID); err != nil {
		t.Errorf("second Stop() error = %v, want nil", err)
	}

	got, err := e.Get(ctx, run.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.State != StateDone {
		t.Errorf("State after two Stop calls = %q, want %q", got.State, StateDone)
	}
}

// TestStopClearsRecoveredPending is fix round 1's M2: without this, a Stop
// that succeeds against a recovered StateActive run (spec §9's restart ->
// recovered StateActive -> Stop flow) leaves recoveredPending set, so the
// very next Start still 409s on "a recovered run is waiting for retry or
// discard" -- naming a run that is now StateDone and gone -- forcing a
// second, differently-named click for no reason. recoveredPending is set
// here directly rather than by round-tripping through Recover: this test's
// only concern is whether Stop clears the flag on success, not how Recover
// sets it (recover_test.go already covers that, white-box in the same
// package).
func TestStopClearsRecoveredPending(t *testing.T) {
	e := newTestEngine(t, &fakeActiveStep{phase: PhaseProve, active: true, workload: stopTestWorkload})
	e.SetProveClient(prove.NewClient(fake.NewSimpleClientset()))
	run := startAndWait(t, e)

	e.mu.Lock()
	e.recoveredPending = true
	e.mu.Unlock()

	if _, err := e.Start(context.Background()); err == nil {
		t.Fatal("fixture check: Start() succeeded while recoveredPending, want conflict")
	}

	if err := e.Stop(context.Background(), run.ID); err != nil {
		t.Fatalf("Stop() error = %v, want nil", err)
	}

	if _, err := e.Start(context.Background()); err != nil {
		t.Errorf("Start() error = %v after Stop on a recovered run, want nil -- recoveredPending must be cleared", err)
	}
}

// TestStopSurvivesConcurrentDiscard is fix round 1's C1 regression: a
// second, idempotent Stop call (stoppable's StateDone arm) races a Discard
// of the same, already-finished run. Discard nils e.current WITHOUT
// bumping the epoch (deliberately -- see Discard's own doc comment), which
// used to be safe only because Discard rejected every state a live
// goroutine's finish call could still be in flight for. Stop breaks that:
// it calls finish from outside the execute machinery, for a run in
// StateDone, which Discard does NOT reject -- so before finish's own
// nil-check (this fix round), the blocked Stop below panicked with a nil
// pointer dereference the instant its WaitAbsent poll returned, taking the
// whole process down.
//
// The panic is recovered rather than left to crash the test binary, so a
// regression here reads as a normal, attributable test failure.
func TestStopSurvivesConcurrentDiscard(t *testing.T) {
	const runID = "run-stop-vs-discard"
	cs := fake.NewSimpleClientset()
	e, client := newStopTestEngine(t, runID, cs)
	ctx := context.Background()
	if err := client.EnsureNamespace(ctx); err != nil {
		t.Fatalf("EnsureNamespace() error = %v", err)
	}
	if err := client.Apply(ctx, runID); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	run := startAndWait(t, e)

	// First Stop: genuinely deletes the workload and lands the run at
	// StateDone -- the state Discard is willing to accept.
	if err := e.Stop(ctx, run.ID); err != nil {
		t.Fatalf("first Stop() error = %v, want nil", err)
	}

	// The second, idempotent Stop's WaitAbsent poll is held open here so
	// Discard has a real window to run underneath it. Once released, the
	// reactor falls through (returns false) to the real tracker, which
	// (the workload already being gone) reports absence normally -- this
	// is not simulating a cluster failure, only stretching out the timing
	// of a genuinely successful poll.
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	cs.PrependReactor("get", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		once.Do(func() { close(entered) })
		<-release
		return false, nil, nil
	})

	done := make(chan error, 1)
	go func() {
		var stopErr error
		defer func() {
			if r := recover(); r != nil {
				stopErr = fmt.Errorf("Stop panicked: %v", r)
			}
			done <- stopErr
		}()
		stopErr = e.Stop(ctx, run.ID)
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("second Stop's WaitAbsent poll was never reached")
	}

	if err := e.Discard(ctx, run.ID); err != nil {
		t.Fatalf("Discard() error = %v, want nil -- the run is StateDone, not live", err)
	}

	close(release)

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("second Stop() error = %v, want nil (and NOT a panic)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second Stop() never returned")
	}
}

// assertFailedStopLeavesRunActive is the shared body of both
// TestFailedStopLeavesRunActive and TestFailedDeleteLeavesRunActive: call
// Stop against a client wired to fail, then assert the one outcome spec §7
// most wants to avoid never happens -- a console reporting a clean stop
// over a cluster that still holds the workload.
func assertFailedStopLeavesRunActive(t *testing.T, e *Engine, runID string) {
	t.Helper()
	ctx := context.Background()

	if err := e.Stop(ctx, runID); err == nil {
		t.Fatal("Stop() succeeded though the cluster call failed, want an error")
	}

	got, err := e.Get(ctx, runID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.State != StateActive {
		t.Errorf("State after a failed Stop = %q, want %q -- the run must not silently move off the state that says something is still running", got.State, StateActive)
	}

	// The operator's only exit from this run is a retried Stop; Start must
	// still refuse, exactly as it does for any other StateActive run.
	_, startErr := e.Start(ctx)
	if startErr == nil {
		t.Fatal("Start() succeeded after a failed Stop, want ErrCodeConflict")
	}
	var se *aicrerrors.StructuredError
	if !errors.As(startErr, &se) || se.Code != aicrerrors.ErrCodeConflict {
		t.Errorf("Start() error = %v, want ErrCodeConflict", startErr)
	}
}

// TestFailedStopLeavesRunActive is the required bite-proof target: it makes
// Delete SUCCEED and WaitAbsent fail (a "get" reactor on jobs, so
// WaitAbsent's very first poll returns a non-NotFound error and fails
// immediately rather than exhausting stopWaitAbsentTimeout). That specific
// shape -- confirmed deletion issued, absence never confirmed -- is the one
// sensitive to the ordering the bite-proof mutates: moving Stop's e.finish
// call to before the WaitAbsent call cannot be caught by a Delete failure,
// since Delete's own error check returns before either position of finish
// is ever reached. A WaitAbsent failure is the only shape that actually
// exercises "finish before waiting for absence".
func TestFailedStopLeavesRunActive(t *testing.T) {
	const runID = "run-stop-wait-fails"
	cs := fake.NewSimpleClientset()
	e, client := newStopTestEngine(t, runID, cs)
	ctx := context.Background()
	if err := client.EnsureNamespace(ctx); err != nil {
		t.Fatalf("EnsureNamespace() error = %v", err)
	}
	if err := client.Apply(ctx, runID); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	// Installed after Apply, not before: Apply itself now issues a Get to
	// check for spec drift, and this reactor exists to fail WaitAbsent's poll
	// during the Stop under test below, not that unrelated setup call.
	cs.PrependReactor("get", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("get refused")
	})
	run := startAndWait(t, e)

	assertFailedStopLeavesRunActive(t, e, run.ID)
}

// TestFailedDeleteLeavesRunActive covers the other half of Stop's failure
// surface: Delete itself refused. Kept as its own test, distinct from the
// bite-proof target above, because the two exercise different branches of
// Stop's body (Delete's own error check vs. the ordering between a
// successful Delete and WaitAbsent) and a mutation that breaks one need not
// break the other.
func TestFailedDeleteLeavesRunActive(t *testing.T) {
	const runID = "run-stop-delete-fails"
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("delete refused")
	})
	e, client := newStopTestEngine(t, runID, cs)
	ctx := context.Background()
	if err := client.EnsureNamespace(ctx); err != nil {
		t.Fatalf("EnsureNamespace() error = %v", err)
	}
	if err := client.Apply(ctx, runID); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	run := startAndWait(t, e)

	assertFailedStopLeavesRunActive(t, e, run.ID)
}

// TestStopRejectsNonActiveRun covers a run that reached StateDone through
// ordinary step completion -- no ActiveStep ever ran, so it never held a
// workload -- and must not be mistaken for the idempotent
// already-stopped case TestStopIsIdempotent exercises.
func TestStopRejectsNonActiveRun(t *testing.T) {
	e := newTestEngine(t, &fakeStep{phase: PhaseApply})
	run := startAndWait(t, e)
	if run.State != StateDone {
		t.Fatalf("State = %q, want %q (fixture must never have been Active)", run.State, StateDone)
	}

	err := e.Stop(context.Background(), run.ID)
	if err == nil {
		t.Fatal("Stop() succeeded on a run that never held a workload, want conflict")
	}
	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) || se.Code != aicrerrors.ErrCodeConflict {
		t.Errorf("Stop() error = %v, want ErrCodeConflict", err)
	}
}

// TestStopRejectsFailedRunWithoutUnconfirmedCleanup is fix round 2's N3:
// the negative control for stoppable's third arm. Broadening that arm to
// r.State == StateFailed alone (dropping the CleanupUnconfirmed check)
// leaves every existing internal/engine test green -- every other
// StateFailed fixture in this file either never held a workload
// (TestStopRejectsNonActiveRun, StateDone) or genuinely has
// CleanupUnconfirmed set, so nothing here previously pinned that an
// ORDINARY StateFailed run (one that failed for a reason unrelated to
// cleanup) must still be rejected.
func TestStopRejectsFailedRunWithoutUnconfirmedCleanup(t *testing.T) {
	e := newTestEngine(t, &fakeStep{phase: PhaseProve, err: errors.New("boom -- an ordinary failure")})
	// Wired deliberately, even though a correctly-guarded Stop never
	// reaches it: without a client, a broadened guard would still be
	// caught, but only via ErrCodeUnavailable from the client-readiness
	// check below stoppable -- a confounded catch that says nothing about
	// stoppable itself. With a client wired, Delete/WaitAbsent against an
	// absent object trivially succeed (prove.Client.Delete's own contract),
	// so a broadened guard is caught cleanly: Stop() returns nil instead of
	// the conflict this test wants.
	e.SetProveClient(prove.NewClient(fake.NewSimpleClientset()))
	run := startAndWait(t, e)
	if run.State != StateFailed || run.CleanupUnconfirmed {
		t.Fatalf("fixture run = %+v, want StateFailed with CleanupUnconfirmed = false", run)
	}

	err := e.Stop(context.Background(), run.ID)
	if err == nil {
		t.Fatal("Stop() succeeded on an ordinary StateFailed run with no unconfirmed cleanup, want conflict")
	}
	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) || se.Code != aicrerrors.ErrCodeConflict {
		t.Errorf("Stop() error = %v, want ErrCodeConflict", err)
	}
}

// TestStopFailsGracefullyWithoutLiveCluster is fix round 1's I3: the
// nil/unready-client guard in Stop had no test, and deleting it entirely
// left both internal/engine and internal/api green (the reviewer's own
// demonstration) -- Stop would instead reach a nil *prove.Client and
// panic, the exact class of process-killing bug C1 (steps/prove.go's own
// Client.Ready() guard) already fixed once in this phase, at the
// equivalent call site one layer up. Deliberately does NOT call
// SetProveClient at all: newTestEngine's engine has no cluster client
// wired, matching main.go's dev-mode-without-a-pod posture.
func TestStopFailsGracefullyWithoutLiveCluster(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Stop() panicked: %v -- want a returned error instead", r)
		}
	}()
	e := newTestEngine(t, &fakeActiveStep{phase: PhaseProve, active: true, workload: stopTestWorkload})
	run := startAndWait(t, e)

	err := e.Stop(context.Background(), run.ID)
	if err == nil {
		t.Fatal("Stop() succeeded with no cluster client wired, want ErrCodeUnavailable")
	}
	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) || se.Code != aicrerrors.ErrCodeUnavailable {
		t.Errorf("Stop() error = %v, want ErrCodeUnavailable", err)
	}

	got, getErr := e.Get(context.Background(), run.ID)
	if getErr != nil {
		t.Fatalf("Get() error = %v", getErr)
	}
	if got.State != StateActive {
		t.Errorf("State after a client-less Stop = %q, want %q", got.State, StateActive)
	}
}

// unconfirmedCleanupErr wraps the same sentinel steps/prove.go's real
// cleanup helper wraps (ErrUnconfirmedCleanup), rather than reproducing its
// message text. Fix round 1's C3: the guard used to key off a substring of
// that text, and a one-character reword left it silently dead with the
// whole suite green -- this package's tests must exercise the same
// errors.Is-based mechanism runStep now uses, or they would still pass
// against a version of engine.go that reintroduces exactly that defect.
// internal/steps/prove_test.go's TestCleanupFailureWrapsErrUnconfirmedCleanup
// is the other half: it drives the REAL steps.NewProve cleanup path and
// asserts engine's own predicate against the error it actually produces, so
// the two sides of this cross-package contract are each pinned once.
func unconfirmedCleanupErr(runID string) error {
	return fmt.Errorf("run %s failed: %w", runID, ErrUnconfirmedCleanup)
}

// TestStartBlockedByUnconfirmedCleanupFailure is Ruling 12 (spec §8 row 3):
// a pre-Active failure whose own cleanup could not be confirmed must keep
// blocking Start, the same as a genuinely StateActive run, because the
// cluster may still be holding what that cleanup could not verify it
// removed.
func TestStartBlockedByUnconfirmedCleanupFailure(t *testing.T) {
	e := newTestEngine(t, &fakeStep{phase: PhaseProve, err: unconfirmedCleanupErr("run-cleanup-fail")})
	startAndWait(t, e)

	_, err := e.Start(context.Background())
	if err == nil {
		t.Fatal("Start() succeeded over a run with unconfirmed cleanup, want conflict")
	}
	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) || se.Code != aicrerrors.ErrCodeConflict {
		t.Errorf("Start() error = %v, want ErrCodeConflict", err)
	}
	if !strings.Contains(err.Error(), "confirmed") {
		t.Errorf("Start() error = %q, want it to name the unconfirmed cleanup", err)
	}
}

// TestStopResolvesUnconfirmedCleanupAndUnblocksStart is fix round 1's I1:
// blocking Start (and Discard) over a run with unconfirmed cleanup, while
// offering no way to actually resolve it, recreates the operator dead end
// this whole task exists to remove -- Stop, Discard, and Start all 409ing,
// with nothing left but the unsafe Retry path. Stop retrying the exact
// delete-and-confirm-absence a failed cleanup could not complete IS the
// remedy (stoppable's third arm), so it must be the one operation this
// state does NOT block, and a successful Stop must actually clear Ruling
// 12's guard rather than leaving the run in limbo.
func TestStopResolvesUnconfirmedCleanupAndUnblocksStart(t *testing.T) {
	const runID = "run-cleanup-resolved-by-stop"
	e := newTestEngine(t, &fakeStep{phase: PhaseProve, err: unconfirmedCleanupErr(runID)})
	e.newID = func() string { return runID }
	e.SetProveClient(prove.NewClient(fake.NewSimpleClientset()))
	run := startAndWait(t, e)
	if run.State != StateFailed || !run.CleanupUnconfirmed {
		t.Fatalf("fixture run = %+v, want StateFailed with CleanupUnconfirmed = true", run)
	}
	if _, err := e.Start(context.Background()); err == nil {
		t.Fatal("Start() succeeded before Stop, want the unconfirmed-cleanup guard still blocking")
	}

	// No Job was ever created against this fake clientset -- Delete and
	// WaitAbsent are both trivially satisfied against an absent object
	// (prove.Client.Delete's own "a missing workload is success" contract),
	// which is exactly the shape a truly orphaned-then-actually-gone
	// workload has by the time an operator's Stop reaches it.
	if err := e.Stop(context.Background(), run.ID); err != nil {
		t.Fatalf("Stop() on an unconfirmed-cleanup run error = %v, want nil", err)
	}

	resolved, err := e.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if resolved.State != StateDone {
		t.Errorf("State after Stop = %q, want %q", resolved.State, StateDone)
	}
	// Fix round 3's NEW-6: a StateDone record claiming an unconfirmed
	// cleanup is a contradiction -- Stop just confirmed the workload gone,
	// which is what StateDone here means.
	if resolved.CleanupUnconfirmed {
		t.Errorf("CleanupUnconfirmed = true on a StateDone record, want false -- Stop just confirmed the workload absent")
	}

	if _, err := e.Start(context.Background()); err != nil {
		t.Errorf("Start() error = %v after Stop resolved the unconfirmed cleanup, want nil", err)
	}
}

// TestStopIsIdempotentAfterResolvingUnconfirmedCleanup is fix round 2's N4:
// spec §7's idempotency clause ("stopping an already-stopped workload
// succeeds") must hold for a run stoppable's THIRD arm resolved too, not
// just the ordinary Active-then-Done path TestStopIsIdempotent already
// covers. Before this fix, a run resolved via arm 3 reached StateDone with
// Workload still at its zero value -- Prove's own success-path write is
// never reached on a failure -- so stoppable's second arm rejected a
// repeat, idempotent Stop click with "run has no active workload to stop",
// contradicting spec §7.
func TestStopIsIdempotentAfterResolvingUnconfirmedCleanup(t *testing.T) {
	const runID = "run-cleanup-resolved-idempotent"
	e := newTestEngine(t, &fakeStep{phase: PhaseProve, err: unconfirmedCleanupErr(runID)})
	e.newID = func() string { return runID }
	e.SetProveClient(prove.NewClient(fake.NewSimpleClientset()))
	run := startAndWait(t, e)

	if err := e.Stop(context.Background(), run.ID); err != nil {
		t.Fatalf("first Stop() error = %v, want nil", err)
	}
	if err := e.Stop(context.Background(), run.ID); err != nil {
		t.Errorf("second Stop() error = %v, want nil -- spec §7 idempotency must hold for arm 3 too", err)
	}

	resolved, err := e.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if resolved.State != StateDone {
		t.Errorf("State after two Stop calls = %q, want %q", resolved.State, StateDone)
	}
}

// TestOrdinaryFailureDoesNotBlockStart is the discriminating half of Ruling
// 12: a run that failed for a reason unrelated to cleanup must not trip the
// new guard, or every ordinary failure would silently become an orphan
// investigation. Mirrors steps/prove_test.go's
// TestProveOrdinaryFailureIsNotReportedAsCleanupFailure at the engine layer.
func TestOrdinaryFailureDoesNotBlockStart(t *testing.T) {
	e := newTestEngine(t, &fakeStep{phase: PhaseApply, err: errors.New("boom")})
	startAndWait(t, e)

	if _, err := e.Start(context.Background()); err != nil {
		t.Errorf("Start() error = %v, want nil -- an ordinary failure must not block Start", err)
	}
}

// twoAttemptStep fails its first Run with firstErr, and every subsequent
// Run with laterErr -- so a test can drive Start then Retry through two
// distinct failure reasons for the SAME run. Locked, not a raw counter:
// Start's and Retry's execute calls run on different goroutines, and while
// this file's own startAndWait/waitForRunState-mediated sequencing already
// provides a real happens-before chain through e.mu, a lock here costs
// nothing and matches internal/api/runs_test.go's retryProbeStep, which
// documents the identical reasoning for the identical shape.
type twoAttemptStep struct {
	phase    Phase
	firstErr error
	laterErr error

	mu      sync.Mutex
	attempt int
}

func (s *twoAttemptStep) Phase() Phase       { return s.phase }
func (s *twoAttemptStep) Requires() []string { return nil }
func (s *twoAttemptStep) Run(_ context.Context, _ *Run, _ Emit) error {
	s.mu.Lock()
	s.attempt++
	first := s.attempt == 1
	s.mu.Unlock()
	if first {
		return s.firstErr
	}
	return s.laterErr
}

// confirmedCleanupErr wraps engine.ErrCleanupConfirmed, the same sentinel
// steps/prove.go's cleanup helper wraps around cause on ITS success path --
// a failure whose own cleanup logic ran and positively confirmed the
// workload absent, distinct from an ordinary failure that says nothing
// about cleanup at all (which twoAttemptStep's plain errors.New fixtures
// below represent).
func confirmedCleanupErr() error {
	return fmt.Errorf("ordinary failure whose own cleanup confirmed the workload absent: %w", ErrCleanupConfirmed)
}

// TestRetryWithUnrelatedFailureDoesNotClearGuard is fix round 2's N2
// regression. Fix round 1's implementation (this same test, under the name
// TestRetryClearsRuling12GuardOnUnrelatedFailure) unconditionally cleared
// Run.CleanupUnconfirmed on any retry failure that did not itself wrap
// ErrUnconfirmedCleanup -- which reads "this attempt's cleanup ran and
// confirmed the workload gone" and "this attempt never got far enough to
// even ATTEMPT cleanup" as the same signal. They are not: a retry whose
// failure carries no cleanup information at all must leave a prior
// unconfirmed-cleanup determination exactly as it was (sticky), because
// nothing about this attempt is evidence the original orphan is gone.
// TestRetryWithConfirmedCleanupClearsGuard below is the correct clearing
// case this one is deliberately NOT.
func TestRetryWithUnrelatedFailureDoesNotClearGuard(t *testing.T) {
	step := &twoAttemptStep{
		phase:    PhaseProve,
		firstErr: unconfirmedCleanupErr("run-n2"),
		laterErr: errors.New("boom -- says nothing about cleanup either way"),
	}
	e := newTestEngine(t, step)
	run := startAndWait(t, e)
	if run.State != StateFailed || !run.CleanupUnconfirmed {
		t.Fatalf("fixture run = %+v, want StateFailed with CleanupUnconfirmed = true", run)
	}

	retriedRun, err := e.Retry(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Retry() error = %v", err)
	}
	if retriedRun.ID != run.ID {
		t.Errorf("Retry() returned run ID = %q, want %q", retriedRun.ID, run.ID)
	}
	retried := waitForRunState(t, e, run.ID, StateFailed)
	if !retried.CleanupUnconfirmed {
		t.Errorf("CleanupUnconfirmed = false after a retry whose failure said nothing about cleanup, want true (sticky)")
	}

	if _, err := e.Start(context.Background()); err == nil {
		t.Fatal("Start() succeeded after a retry that never confirmed the orphan is gone, want conflict")
	}
}

// TestRetryWithConfirmedCleanupClearsGuard is the other, correct half of
// N2: when a retry's OWN cleanup logic runs and positively confirms the
// workload absent (wraps engine.ErrCleanupConfirmed), the guard DOES clear
// -- this attempt actually looked, and found nothing there.
func TestRetryWithConfirmedCleanupClearsGuard(t *testing.T) {
	step := &twoAttemptStep{
		phase:    PhaseProve,
		firstErr: unconfirmedCleanupErr("run-n2-confirmed"),
		laterErr: confirmedCleanupErr(),
	}
	e := newTestEngine(t, step)
	run := startAndWait(t, e)
	if run.State != StateFailed || !run.CleanupUnconfirmed {
		t.Fatalf("fixture run = %+v, want StateFailed with CleanupUnconfirmed = true", run)
	}

	if _, err := e.Retry(context.Background(), run.ID); err != nil {
		t.Fatalf("Retry() error = %v", err)
	}
	retried := waitForRunState(t, e, run.ID, StateFailed)
	if retried.CleanupUnconfirmed {
		t.Errorf("CleanupUnconfirmed = true after a retry whose own cleanup confirmed the workload absent, want false")
	}

	if _, err := e.Start(context.Background()); err != nil {
		t.Errorf("Start() error = %v after a confirmed-clean retry, want nil", err)
	}
}

// TestDiscardBlockedByUnconfirmedCleanupFailure closes the bypass Ruling 12
// would otherwise leave open: Discard clears e.current, and Start's new
// guard only ever consults e.current, so a Discard-then-Start sequence
// would silently free Start again if Discard itself did not also refuse.
func TestDiscardBlockedByUnconfirmedCleanupFailure(t *testing.T) {
	e := newTestEngine(t, &fakeStep{phase: PhaseProve, err: unconfirmedCleanupErr("run-cleanup-fail-2")})
	run := startAndWait(t, e)

	err := e.Discard(context.Background(), run.ID)
	if err == nil {
		t.Fatal("Discard() succeeded over a run with unconfirmed cleanup, want conflict")
	}
	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) || se.Code != aicrerrors.ErrCodeConflict {
		t.Errorf("Discard() error = %v, want ErrCodeConflict", err)
	}
	if got, ok := e.CurrentID(); !ok || got != run.ID {
		t.Errorf("CurrentID() = %q, %v after rejected Discard, want %q, true", got, ok, run.ID)
	}
}

// ownershipStep writes the evidence internal/steps.snapshotOwnership
// produces, then optionally fails -- the two paths runStep merges
// differently, and Ownership must survive both.
type ownershipStep struct {
	phase Phase
	own   Ownership
	err   error
}

func (o *ownershipStep) Phase() Phase       { return o.phase }
func (o *ownershipStep) Requires() []string { return nil }
func (o *ownershipStep) Run(_ context.Context, r *Run, _ Emit) error {
	r.Ownership = o.own
	r.Components = []ComponentState{{Name: "gpu-operator", Namespace: "gpu-operator", Index: 1, Total: 1}}
	return o.err
}

func ownershipFixture() Ownership {
	return Ownership{
		Releases:   []ReleaseRef{{Name: "somebody-elses-thing", Namespace: "gpu-operator"}},
		Namespaces: []NamespaceRef{{Name: "gpu-operator", Existed: true}},
	}
}

// A step's Ownership write must survive runStep's merge, exactly as
// Artifacts, Decisions, Components and Workload do.
//
// This is not hypothetical: it shipped. test/e2e/reset.sh's first real run
// found a record carrying fourteen installed components and no ownership at
// all, because runStep is a hand-maintained merge and this field had no
// producer in it -- the same defect class as fix round 2's N1 (envelope.go)
// and Ruling 20's parity guard, one merge site over. internal/steps' own
// tests cannot catch it: they call step.Run directly on a run they own, so
// the scratch copy the engine actually passes is not on their path.
func TestOwnershipSurvivesTheMerge(t *testing.T) {
	want := ownershipFixture()
	e := newTestEngine(t, &ownershipStep{phase: PhaseApply, own: want})

	run := startAndWait(t, e)

	if !reflect.DeepEqual(run.Ownership, want) {
		t.Errorf("Ownership = %+v after the merge, want %+v -- runStep must carry it", run.Ownership, want)
	}
}

// And on the failure path, which is the one that matters most: a failed
// Apply is the case that leaves a half-installed cluster, so it is exactly
// the run that most needs a Reset -- and a Reset with no evidence removes
// nothing.
func TestOwnershipSurvivesAFailedStep(t *testing.T) {
	want := ownershipFixture()
	e := newTestEngine(t, &ownershipStep{
		phase: PhaseApply, own: want, err: errors.New("deploy.sh failed: exit status 1"),
	})

	run := startAndWait(t, e)

	if run.State != StateFailed {
		t.Fatalf("State = %q, want %q", run.State, StateFailed)
	}
	if !reflect.DeepEqual(run.Ownership, want) {
		t.Errorf("Ownership = %+v after a failed step, want %+v -- the run that most needs a reset would have no evidence",
			run.Ownership, want)
	}
}

// runFieldsNotMergedFromSteps names every exported Run field that runStep
// deliberately does NOT carry back from a step's scratch copy, and why. The
// parity test below requires an entry for each one, so a field added to Run
// cannot silently join this list -- it either gets a merge or gets a stated
// reason.
var runFieldsNotMergedFromSteps = map[string]string{
	"ID":        "the engine's identity for the run; a step has no business changing which run it is",
	"State":     "the state machine's own, set by finish and the operation methods, never by a step",
	"Phase":     "derived from the step slice, not from a step's copy of the run",
	"StepIndex": "the execution cursor; runStep advances it itself, after the step returns",
	"Pending":   "awaitDecisions owns it -- it names the decisions a gate is blocked on",
	"Err":       "set from the step's returned error, which is the authoritative source, not a field on its scratch copy",
	"StartedAt": "stamped once by Start",
	"UpdatedAt": "stamped by whichever engine operation last touched the run",
	"Truncated": "state about the RECORD, not the run: decodeRun populates it on load and encodeRun carries it forward",
	"CleanupUnconfirmed": "decided from the step's returned error via errors.Is (Ruling 12), not read off the scratch copy -- " +
		"see runStep's failure branch for why the distinction is load-bearing",
	"Residue": "written by engine.Reset directly on e.current; no step produces it",
	"Toolchain": "the versions preflight resolved for this process, stamped by the engine when the run is created -- " +
		"a step that could rewrite them could make the evidence bundle name a helm that never ran",
	"ClusterUID": "the connected cluster's identity, stamped by the engine when the run is created -- a step that could rewrite it " +
		"could re-file the run under a cluster it never touched, which is exactly what the field exists to prevent",
}

// mergeParityStep sets every exported field of the Run it is handed to a
// distinct value, so the test below can tell a merged field from a dropped
// one by comparison rather than by inspection.
type mergeParityStep struct {
	phase Phase
	t     *testing.T
	want  *Run
}

func (m *mergeParityStep) Phase() Phase       { return m.phase }
func (m *mergeParityStep) Requires() []string { return nil }
func (m *mergeParityStep) Run(_ context.Context, r *Run, _ Emit) error {
	rv := reflect.ValueOf(r).Elem()
	rt := rv.Type()
	for i := range rt.NumField() {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		setDistinctFieldValue(m.t, rv.Field(i), f.Name)
	}
	m.want = r.Clone()
	return nil
}

// TestRunStepMergesEveryFieldAStepCanWrite is Ruling 20's guard applied to
// the OTHER hand-maintained projection in this package.
//
// envelope.go was audited for parity because a field added there without a
// producer silently persisted as its zero value. runStep is the same shape
// of hazard and had no such guard: it hands each step a scratch clone and
// copies named fields back, so a field a step writes but this merge does not
// mention is discarded at the exact moment it was produced -- no error, no
// warning, and every unit test in internal/steps still green, because those
// tests call step.Run directly on a run they own and never see the clone.
//
// That is not hypothetical. Run.Ownership shipped without a producer here,
// and test/e2e/reset.sh found it on a real cluster: fourteen components
// installed, no ownership recorded, every Reset silently a no-op. This test
// is what makes the next one a unit failure instead.
func TestRunStepMergesEveryFieldAStepCanWrite(t *testing.T) {
	step := &mergeParityStep{phase: PhaseApply, t: t}
	e := newTestEngine(t, step)

	run := startAndWait(t, e)

	if step.want == nil {
		t.Fatal("the step never ran")
	}
	wv := reflect.ValueOf(step.want).Elem()
	gv := reflect.ValueOf(run).Elem()
	rt := wv.Type()
	for i := range rt.NumField() {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		if reason, excluded := runFieldsNotMergedFromSteps[f.Name]; excluded {
			if reason == "" {
				t.Errorf("Run.%s is in runFieldsNotMergedFromSteps with no stated reason", f.Name)
			}
			continue
		}
		want := wv.Field(i).Interface()
		got := gv.Field(i).Interface()
		if !reflect.DeepEqual(want, got) {
			t.Errorf("Run.%s = %+v after the step, want %+v -- runStep must merge this field back "+
				"from the step's scratch copy, or add it to runFieldsNotMergedFromSteps with a stated reason",
				f.Name, got, want)
		}
	}
}

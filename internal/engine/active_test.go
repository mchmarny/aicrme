package engine

import (
	"context"
	"errors"
	"strings"
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
	cs.PrependReactor("get", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("get refused")
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

// unconfirmedCleanupErr mirrors the wording steps/prove.go's cleanup helper
// actually produces on a failed Delete, close enough to pin engine.go's
// hasUnconfirmedCleanup against the real message shape without importing
// internal/steps (which already imports engine; the reverse would cycle).
func unconfirmedCleanupErr(runID string) error {
	return errors.New("run " + runID + " failed (boom); cleanup failed deleting the workload")
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

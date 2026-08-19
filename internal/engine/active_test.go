package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/mchmarny/aicrme/internal/bus"
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
	phase  Phase
	active bool
	err    error
}

func (f *fakeActiveStep) Phase() Phase       { return f.phase }
func (f *fakeActiveStep) Requires() []string { return nil }
func (f *fakeActiveStep) Run(_ context.Context, r *Run, emit Emit) error {
	emit(bus.Event{Kind: bus.KindLog, Message: string(f.phase) + " ran"})
	r.Artifacts[string(f.phase)] = []byte("done")
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

package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

// ErrStepConfig reports a step slice this engine cannot recover against. It
// is a programming error, not a runtime condition: main treats it as fatal
// rather than degrading, because discovering an ambiguous rewind target
// during a real recovery is the worst possible time to find out.
var ErrStepConfig = errors.New("engine step configuration invalid")

// recoveredErr is what a recovered run carries. The wording matters: the
// cockpit shows it, and an operator needs to tell a restart apart from a
// failed helm install before deciding whether Retry is safe.
const recoveredErr = "interrupted by a console restart"

// bundleStepIndex returns the index of the single PhaseBundle step. The
// rewind below identifies Bundle by Phase() rather than a hardcoded index
// because the step slice is assembled in main, and an index would silently
// rot as steps are added or reordered.
func (e *Engine) bundleStepIndex() (int, error) {
	found := -1
	count := 0
	for i, s := range e.steps {
		if s.Phase() != PhaseBundle {
			continue
		}
		count++
		if found < 0 {
			found = i
		}
	}
	switch {
	case count > 1:
		return 0, fmt.Errorf("%w: %d steps report PhaseBundle, want exactly 1", ErrStepConfig, count)
	case count == 0:
		return 0, fmt.Errorf("%w: no step reports PhaseBundle", ErrStepConfig)
	default:
		return found, nil
	}
}

// validState reports whether s is one of the engine's declared State
// constants. A record that decodes cleanly is not automatically one this
// engine defines -- a future build's new State, or plain corruption that
// happens to survive JSON decoding, must not be trusted implicitly.
func validState(s State) bool {
	switch s {
	case StateIdle, StateRunning, StateAwaitingDecision, StateFailed, StateActive, StateDone:
		return true
	default:
		return false
	}
}

// validPhase reports whether p is one of the engine's declared Phase
// constants, mirroring validState's reasoning.
func validPhase(p Phase) bool {
	switch p {
	case PhaseDiscover, PhaseRecommend, PhaseBundle, PhaseApply, PhaseValidate, PhaseProve:
		return true
	default:
		return false
	}
}

// validateLoaded reports whether a decoded record is trustworthy enough to
// install as the current run. Decoding cleanly is not the same as being
// worth trusting: an out-of-range StepIndex or an unrecognized State can
// both survive JSON decoding as a perfectly well-formed record. A record
// failing this check takes the unreadable path -- it is not partially
// installed.
func (e *Engine) validateLoaded(r *Run) error {
	if r.ID == "" {
		return errors.New("recovered run has an empty ID")
	}
	if !validState(r.State) {
		return fmt.Errorf("recovered run has an unrecognized state %q", r.State)
	}
	if !validPhase(r.Phase) {
		return fmt.Errorf("recovered run has an unrecognized phase %q", r.Phase)
	}
	if r.StepIndex < 0 || r.StepIndex > len(e.steps) {
		return fmt.Errorf("recovered run has step index %d outside [0, %d]", r.StepIndex, len(e.steps))
	}
	if r.StartedAt.IsZero() {
		return errors.New("recovered run has a zero StartedAt")
	}
	return nil
}

// Recover loads any persisted run and installs it as the current run. It
// must be called before the HTTP server starts serving: the SPA's automatic
// POST /api/runs on load must never win a race against a run this call is
// still installing.
//
// It returns an error only for a configuration fault the process cannot run
// with (see ErrStepConfig). Store failures are handled here and reported as
// a degraded start: recovery is a convenience, and the console starting is
// not.
func (e *Engine) Recover(ctx context.Context) error {
	// Validate the step slice before touching the store, so a misconfigured
	// step slice fails fast rather than after a successful load.
	bundleIdx, err := e.bundleStepIndex()
	if err != nil {
		return err
	}

	r, err := e.store.LoadCurrent(ctx)
	if err != nil {
		// aicr@v0.19.0's errors package exposes no Code(err) helper -- New,
		// Wrap, IsTransient and friends only -- so the code is reached
		// through errors.As, matching how the rest of this repo inspects it.
		var se *aicrerrors.StructuredError
		if errors.As(err, &se) && se.Code == aicrerrors.ErrCodeNotFound {
			return nil // cold start, the common case
		}
		// Unreadable is NOT absent. Refusing to install it is half the
		// answer; the other half is the store swap below, which makes sure
		// nothing this process subsequently does can overwrite a record it
		// could not read.
		slog.Error("persisted run unreadable; starting without it and leaving it untouched", "error", err)
		e.markStoreUnreadable()
		return nil
	}

	if err := e.validateLoaded(r); err != nil {
		slog.Error("persisted run failed validation; starting without it and leaving it untouched", "error", err)
		e.markStoreUnreadable()
		return nil
	}

	if isLive(r.State) || r.State == StateIdle {
		r.State = StateFailed
		r.Err = recoveredErr
	}

	// Rewind on retryability, not on how the run reached its state. The
	// bundle directory died with the emptyDir regardless of whether the run
	// was interrupted or had already failed, so a run that failed during
	// Apply before the crash needs the same rewind as one cut off mid-step.
	if r.State == StateFailed && r.StepIndex > bundleIdx {
		r.StepIndex = bundleIdx
	}

	e.mu.Lock()
	e.current = r
	e.recoveredPending = true
	e.mu.Unlock()
	return nil
}

// markStoreUnreadable records that a persisted run could not be trusted and
// swaps store for a fresh memory store, all under one lock hold. The swap is
// what makes the guarantee real: nothing this process subsequently does --
// no Save from a new run, no checkpoint -- reaches the store that held a
// record this process could not read, whether that record was merely
// unreadable at this moment or was written by a newer image.
func (e *Engine) markStoreUnreadable() {
	e.mu.Lock()
	e.storeUnreadable = true
	e.store = NewMemoryStore()
	e.mu.Unlock()
}

// StoreUnreadable reports whether Recover found a persisted run it could not
// safely install -- a non-NotFound load failure, or a record that failed
// validation. The real consequence already happened by the time this can be
// observed (store was swapped to a memory store in the same call); this
// accessor exists purely so main can log the degradation.
func (e *Engine) StoreUnreadable() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.storeUnreadable
}

package engine

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/mchmarny/aicrme/internal/bus"
)

// maxLoadAttempts bounds LoadCurrent's retry loop. A pod restarting and the
// API server being briefly unreachable are plausibly the same event -- node
// pressure, a control-plane blip -- so the spec treats that as the load
// path's common failure and asks for a short bounded retry rather than
// degrading on the first blip. Kept small deliberately: this runs before
// :8080 binds, so startup latency is a real cost, not a free variable.
const maxLoadAttempts = 3

// loadRetryBackoff is the pause between load attempts. Short and fixed --
// this is absorbing a blip, not competing for a contended resource the way
// cmstore's Save conflict-retry loop is.
const loadRetryBackoff = 50 * time.Millisecond

// runIDLength is newID's output format: hex.EncodeToString of 8 random
// bytes is always 16 characters.
const runIDLength = 16

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

// validRunID reports whether id matches the format Start's newID always
// produces: 16 hex characters. Spec says "ID format" is part of what a
// loaded record is checked against; this is cheap, and the format checked
// is exactly what the only producer emits.
func validRunID(id string) bool {
	if len(id) != runIDLength {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

// validState reports whether s is one of the engine's declared State
// constants. A record that decodes cleanly is not automatically one this
// engine defines -- a future build's new State, or plain corruption that
// happens to survive JSON decoding, must not be trusted implicitly.
func validState(s State) bool {
	switch s {
	case StateIdle, StateRunning, StateAwaitingDecision, StateFailed, StateActive, StateDone, StateResetting:
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
	if !validRunID(r.ID) {
		return fmt.Errorf("recovered run has an invalid ID %q", r.ID)
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
	// UpdatedAt is written by every producer that ever touches a Run (Start,
	// Decide, every step transition, finish, ...), so it is exactly as safe
	// to require as StartedAt -- the spec says "timestamps," plural.
	if r.UpdatedAt.IsZero() {
		return errors.New("recovered run has a zero UpdatedAt")
	}
	return nil
}

// loadCurrentRetryable reports whether err is the plausibly-transient
// subset of LoadCurrent's failures worth retrying. cmstore wraps a failed
// Get as ErrCodeInternal; that is the only shape a control-plane blip
// produces. A decode failure, an unsupported schema version, or a missing
// payload key -- all ErrCodeInvalidRequest -- fail identically on every
// attempt, so retrying them only delays startup for no chance of a
// different answer.
//
// aicrerrors.IsTransient does not fit here: it keys on ErrCodeTimeout and
// context-based causes, not ErrCodeInternal, so it would silently treat
// this case as non-retryable. The code is gated explicitly instead, the
// same way the rest of this file inspects StructuredError.
//
// This set is startup-budget-bearing, and the budget assumes the worst: every
// member costs up to cmStoreCallTimeout per attempt, maxLoadAttempts of them
// run back to back, and all of it happens before :8080 accepts anything. That
// is not hypothetical for ErrCodeInternal -- a control plane that accepts the
// connection and then errors (a proxy 504, a reset) produces exactly this
// code after burning its full call timeout, so the realistic worst case is
// deploymentLookupTimeout + maxLoadAttempts x cmStoreCallTimeout = 40s.
// test/chart/contract.sh asserts the chart's startupProbe window covers that
// figure, so raising maxLoadAttempts or cmStoreCallTimeout fails the build
// unless the probe moves with it.
//
// ErrCodeTimeout is left out for a narrower reason, and explicitly NOT as a
// budget guarantee: an earlier version of that contract assertion inferred
// "only one attempt can run" from this exclusion and certified a 20s margin
// the shipped chart did not have, because it modeled a hung server and
// ignored an erroring one. The real reason is evidential -- a call that
// already burned its entire cmStoreCallTimeout has told us the server is not
// answering at all, so two more identical waits buy 20 more seconds of
// startup for the same answer.
func loadCurrentRetryable(err error) bool {
	var se *aicrerrors.StructuredError
	return errors.As(err, &se) && se.Code == aicrerrors.ErrCodeInternal
}

// loadCurrentWithRetry wraps Store.LoadCurrent with a short bounded retry
// for the plausibly-transient failure class (see loadCurrentRetryable).
// NotFound and deterministic failures (decode, version, missing key) return
// on the first attempt -- retrying a failure that cannot change outcome
// only costs startup latency. The backoff is bounded by ctx, same as every
// other wait in this call path.
func (e *Engine) loadCurrentWithRetry(ctx context.Context) (*Run, error) {
	var lastErr error
	for attempt := 0; attempt < maxLoadAttempts; attempt++ {
		r, err := e.store.LoadCurrent(ctx)
		if err == nil {
			return r, nil
		}
		lastErr = err
		if !loadCurrentRetryable(err) {
			return nil, err
		}
		if attempt == maxLoadAttempts-1 {
			break
		}
		select {
		case <-time.After(loadRetryBackoff):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, lastErr
}

// Recover loads any persisted run and installs it as the current run. It
// must be called before the HTTP server starts serving: the SPA's automatic
// POST /api/runs on load must never win a race against a run this call is
// still installing. That ordering also protects store itself (see the field
// comment on Engine.store): the one post-construction reassignment this
// engine ever makes to store happens here, before any goroutine exists that
// could read it concurrently.
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

	r, err := e.loadCurrentWithRetry(ctx)
	if err != nil {
		// aicr@v0.20.0's errors package exposes no Code(err) helper -- New,
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

	// Returned rather than degraded-past, unlike every other rejection here.
	// The others mean "this record cannot be trusted", and starting cold is
	// the recovery. This one means the caller pointed recovery at the wrong
	// cluster's records, which is a wiring error in the connect path, not a
	// runtime condition -- and continuing would quietly file this cluster's
	// run alongside another's. The store is deliberately NOT marked
	// unreadable: the record is perfectly readable and perfectly valid, it
	// just is not ours, and nothing here should make it harder for the
	// console that owns it to read it later.
	if err := e.checkClusterMatch(r, "recover"); err != nil {
		return err
	}

	// Pending names the decisions a now-nonexistent awaitDecisions goroutine
	// was blocked on. No such goroutine survives a restart in any state, so
	// this clear is unconditional. It used to sit inside the branch below,
	// which left the one case that most needs it uncovered: a SIGTERM at a
	// decision gate makes finish() write StateFailed while leaving Pending
	// populated, so a record already StateFailed on disk recovered as
	// state=failed pending=[intent platform] -- exactly the self-inconsistent
	// combination this clear exists to prevent, and the combination the state
	// machine never otherwise produces. Nothing is lost by dropping it:
	// Retry re-enters awaitDecisions, which rebuilds Pending from
	// step.Requires() against the decisions already recorded.
	r.Pending = nil

	// A teardown interrupted by a restart is the one live state whose
	// residue is genuinely unknown: the goroutine that would have recorded
	// what it removed died with the pod, so the record names neither what
	// went nor what stayed. Marking it incomplete is the honest answer and
	// the fail-closed one -- Start, Retry and Discard all refuse until
	// another Reset has actually established the cluster's state.
	//
	// Checked before the rewind below, because it must not depend on where
	// StepIndex happened to be pointing.
	if r.State == StateResetting {
		r.Residue.Incomplete = true
	}
	if isLive(r.State) || r.State == StateIdle {
		r.State = StateFailed
		r.Err = recoveredErr
	}

	// Rewind on retryability, not on how the run reached its state. The
	// bundle directory died with the emptyDir regardless of whether the run
	// was interrupted or had already failed, so a run that failed during
	// Apply before the crash needs the same rewind as one cut off mid-step.
	rewound := false
	if r.State == StateFailed && r.StepIndex > bundleIdx {
		r.StepIndex = bundleIdx
		rewound = true
	}

	e.mu.Lock()
	e.current = r
	e.recoveredPending = true
	e.mu.Unlock()

	// bus.Publish takes its own mutex; per the "no store I/O under e.mu"
	// rule this phase enforces everywhere else, the bootstrap publishes
	// must run after e.mu.Unlock() too, or nesting the two locks invites
	// the same lock-ordering hazard the observer's scope accessor already
	// has to avoid.
	e.publishRecoveryBootstrap(r)

	// This only ever fires after an unplanned restart, so it is the single
	// most useful startup line the console can emit -- the bus events Task 6
	// adds reach the SPA, not the pod's own logs. cleanupUnconfirmed is
	// included for the same reason decodeRun already warns about a
	// truncated record on every read (fix round 3's NEW-5): an operator
	// watching this pod's own logs after a restart -- the one channel that
	// works before the SPA has reconnected -- had no signal here that
	// Ruling 12's guard is set and Start/Discard will both 409 until it is
	// resolved.
	slog.Info("recovered a persisted run", "run", r.ID, "state", r.State, "step", r.StepIndex,
		"rewound", rewound, "cleanupUnconfirmed", r.CleanupUnconfirmed)
	return nil
}

// bootstrapComponentData is the wire shape a bootstrap KindComponent event
// carries in Data -- the same field names as applier.ComponentData
// (internal/applier/parse.go), which is what the SPA's web/src/pipeline.ts
// isComponentData actually checks. Declared locally rather than imported:
// internal/engine must not depend on internal/applier, which is a caller of
// this package (via internal/steps), not a dependency of it.
type bootstrapComponentData struct {
	Name      string `json:"name"`
	Index     int    `json:"index,omitempty"`
	Total     int    `json:"total,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Status    string `json:"status"`
}

// recoveryMarkerMsg is the KindRecovered event's message. Worded for every
// state a recovered record can carry, not just an interrupted one: a run that
// had already finished before the restart was not interrupted by anything,
// and telling the operator it was would be a lie in the most common case of
// all (any `helm upgrade` of a release that has completed one demo).
const recoveryMarkerMsg = "recovered a previous run; retry or discard it before starting a new one"

// recoveryMarkerData is the KindRecovered event's Data payload. It exists so
// the console can distinguish a recovered run that can be retried from one
// whose checkpoint lost artifacts to the size guard -- for the latter, Retry
// is guaranteed to fail at the first step that reads a dropped key
// (internal/steps/bundle.go reads snapshot.yaml, which is the first artifact
// shed), so offering it unqualified would be the console lying about a
// record that was itself honest.
type recoveryMarkerData struct {
	Truncated []string `json:"truncated,omitempty"`
	// ResidueIncomplete says a teardown was in flight or had already failed
	// when this record was written. It travels here for the same reason
	// Truncated does: the console offers actions the engine will refuse
	// unless it knows, and after a restart this marker is the only event in
	// the stream carrying the fact. Without it the console would offer
	// Start, Retry and Discard on a half-torn-down cluster and get three
	// 409s, with no explanation of what to do instead.
	ResidueIncomplete bool `json:"residueIncomplete,omitempty"`
}

// publishRecoveryBootstrap tells the SPA about a recovered run over the bus
// rather than a second fetch path: the stream is already the SPA's source
// of truth (web/src/components/Wizard.tsx's deriveRunState and
// web/src/pipeline.ts's deriveComponents both replay it), so adding a
// GET /api/runs/current would create a second source needing reconciling
// against it instead.
//
// It publishes, in order: the KindRecovered marker, which is the only event
// carrying the fact that this run is blocking Start until an operator acts;
// one KindComponent event per persisted component row, so deriveComponents
// redraws the pipeline; the interruption notice as a distinct KindError event
// when the run carries one, so the console can say "interrupted by a console
// restart" instead of a generic failure; and last, the run's identity and
// phase as a KindPhase event worded exactly "run " + state, matching the
// message engine.go's finish already uses for a live run, so a recovered run
// resolves through the identical deriveRunState branch.
//
// The marker goes first so it is set before the state-bearing event that
// follows, and because a subscriber reading the stream top-down should learn
// what it is looking at before it looks at it.
func (e *Engine) publishRecoveryBootstrap(r *Run) {
	// Data carries the shed-artifact list so the console can say a retry
	// cannot work rather than offering one that fails at the first step
	// needing what the store dropped. Omitted entirely for the ordinary case,
	// so the field's presence means something.
	var data json.RawMessage
	if len(r.Truncated) > 0 || r.Residue.Incomplete {
		data, _ = json.Marshal(recoveryMarkerData{
			Truncated: r.Truncated, ResidueIncomplete: r.Residue.Incomplete,
		})
	}
	e.bus.Publish(bus.Event{
		RunID: r.ID, Kind: bus.KindRecovered, Phase: string(r.Phase),
		Level: bus.LevelWarn, Message: recoveryMarkerMsg, Data: data,
	})
	for _, c := range r.Components {
		// ComponentState and bootstrapComponentData share the same field
		// names, order, and types (only their json tags differ), so a
		// direct conversion is what staticcheck's S1016 asks for in place
		// of a struct literal that just copies the same fields. The
		// conversion is also the guard: adding a field to ComponentState
		// and not to this type is a build error rather than a bootstrap
		// event that silently drops it (envelope_test.go's nested parity
		// test says the same thing for the persisted side).
		data, _ := json.Marshal(bootstrapComponentData(c))
		e.bus.Publish(bus.Event{
			RunID: r.ID, Kind: bus.KindComponent, Phase: string(r.Phase),
			Level: componentLevel(c.Status), Component: c.Name,
			Message: c.Name + " " + c.Status, Data: data,
		})
	}
	if r.Err != "" {
		e.bus.Publish(bus.Event{
			RunID: r.ID, Kind: bus.KindError, Phase: string(r.Phase),
			Level: bus.LevelError, Message: r.Err,
		})
	}
	e.bus.Publish(bus.Event{
		RunID: r.ID, Kind: bus.KindPhase, Phase: string(r.Phase),
		Level: levelFor(r.State), Message: "run " + string(r.State),
	})
}

// componentLevel mirrors the Level internal/applier/parse.go's parseLine
// assigns each marker kind (reFailed -> LevelError, reRetry -> LevelWarn,
// everything else -> LevelInfo), by string value rather than by importing
// applier's Status* constants -- see bootstrapComponentData's doc comment
// on why internal/engine does not depend on internal/applier. Checked
// against web/src/components/Cockpit.tsx before adding this: ComponentRow
// colors each row from ComponentState.status alone (statusClass) and never
// reads the event's level field, so this has no rendering effect today.
// It is still correct: a recovered failed component should carry the same
// severity a live one does, for any consumer that does look at Level (the
// raw event timeline, structured log export, ...), and it costs nothing.
func componentLevel(status string) bus.Level {
	switch status {
	case "failed":
		return bus.LevelError
	case "retrying":
		return bus.LevelWarn
	default:
		return bus.LevelInfo
	}
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

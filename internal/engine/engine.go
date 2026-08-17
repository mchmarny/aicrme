package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"sync"
	"time"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/mchmarny/aicrme/internal/bus"
)

// terminalSaveTimeout bounds the detached write finish performs. Terminal
// state must persist even when the run was canceled, so this write cannot
// use the (now-dead) step context -- but it also cannot block shutdown
// indefinitely against an unreachable API server.
const terminalSaveTimeout = 5 * time.Second

// decideSaveTimeout bounds the checkpoint write Decide issues before
// acknowledging an operator's decision. Decide has no request context yet
// -- threading the caller's context through Start/Retry/Get/Decide is Task
// 7's job, not this one's. Until Task 7 lands, this stays a detached
// background context bounded by an explicit timeout, the same shape
// finish's own detached terminal write already uses.
const decideSaveTimeout = 5 * time.Second

// canceledByShutdownMsg is Run.Err's message when the run is canceled by
// console shutdown, whether mid-step or parked at a decision gate. Both
// call sites share the constant so the wording (and this package's own
// tests, which match against it) cannot drift apart.
const canceledByShutdownMsg = "canceled: console shutting down"

// ErrDraining is what Start and Retry return once CancelAndWait has begun
// shutting the engine down. ErrCodeUnavailable is what makes internal/api's
// writeErr answer 503, matching the shape Server.Drain already returns for a
// mutation arriving during shutdown.
var ErrDraining = aicrerrors.New(aicrerrors.ErrCodeUnavailable,
	"engine is shutting down; not accepting new runs")

// Emit publishes a console event. The engine stamps RunID and Phase, so steps
// only supply Kind, Level, Message, Component, and Data.
type Emit func(bus.Event)

// Step is one phase of the run. Requires lists decision keys that must be
// present in Run.Decisions before Run is called; the engine parks in
// StateAwaitingDecision until they are supplied.
type Step interface {
	Phase() Phase
	Requires() []string
	Run(ctx context.Context, r *Run, emit Emit) error
}

// Engine executes steps in order for a single run. One run at a time: this is
// a single-replica demo console, not a scheduler.
type Engine struct {
	bus *bus.Bus
	// store persists run state. Set once in New for the engine's life, with
	// one exception: Recover's markStoreUnreadable reassigns it under e.mu
	// when a persisted record cannot be trusted. Every other read of this
	// field (Start, Retry, Get, runStep, finish, ...) happens without
	// holding e.mu -- safe only because that reassignment happens exactly
	// once, during Recover, before any goroutine that could read store
	// concurrently exists (Recover runs before the HTTP server starts
	// serving). A future caller reassigning store after startup -- a
	// "reload from store" action, say -- would need real synchronization
	// here, not just the lock around the write, or -race would only catch
	// it intermittently.
	store Store
	steps []Step

	mu      sync.Mutex
	current *Run
	resume  chan struct{}
	newID   func() string

	// epoch increments on every Start and Retry. Each execute goroutine
	// captures the value current when it launched and re-checks it before
	// every state write. Start's isLive check alone cannot cover this:
	// Retry deliberately relaunches execute for a run that already had a
	// goroutine, so "is a run live" and "is THIS goroutine still the one
	// driving it" are different questions.
	epoch uint64

	// draining is set by CancelAndWait and never cleared: the process is on
	// its way out. It is what makes the engine authoritative about its own
	// lifecycle rather than dependent on the HTTP layer. Without it,
	// CancelAndWait snapshots cancel/done once and then waits on that one
	// channel, so a Start landing just after the snapshot is canceled and
	// waited for -- CancelAndWait returns nil while the new run's goroutine
	// is still mid-flight and main returns out from under it.
	// Server.Drain narrows that window but cannot close it: requireNotDraining
	// gates the outer mux, so a POST /api/runs that clears the check
	// microseconds before Drain() still reaches Start.
	draining bool

	// storeUnreadable is set by Recover when a persisted run existed but
	// could not be read or failed validation. Recover itself has already
	// done the half of the work that matters -- swapping store for a fresh
	// memory store in the same call, so nothing this process subsequently
	// does can overwrite the record it could not read. This flag exists
	// purely so main can log the degradation; StoreUnreadable() is its
	// accessor.
	storeUnreadable bool

	// recoveredPending is set by Recover when it installs a persisted run as
	// the current one, and cleared only by an explicit operator action
	// (Retry, or a discard). While set, the SPA's automatic POST /api/runs
	// on load must not be allowed to silently replace the recovered run --
	// Task 4 is where Start and Retry read this flag; Recover only sets it.
	recoveredPending bool

	// cancel stops the in-flight run's step context; done closes once its
	// execute goroutine has exited AND persisted a terminal state. A cancel
	// func alone would tell a caller the run was asked to stop but not when
	// it actually had -- and the whole point of shutdown ordering is not
	// returning from main until the deploy.sh process tree is reaped.
	cancel context.CancelFunc
	done   chan struct{}
}

// New returns an Engine that will execute steps in the order given.
func New(b *bus.Bus, st Store, steps ...Step) *Engine {
	return &Engine{
		bus:   b,
		store: st,
		steps: steps,
		newID: randomID,
	}
}

func randomID() string {
	var buf [8]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

// Start creates a run and executes it in the background. It returns as soon as
// the run is registered; callers observe progress over the bus.
func (e *Engine) Start(ctx context.Context) (*Run, error) {
	e.mu.Lock()
	if e.draining {
		e.mu.Unlock()
		return nil, ErrDraining
	}
	// Checked before isLive: a recovered run lands StateFailed, which is not
	// live, so without this the SPA's automatic POST /api/runs on load would
	// silently replace it before the operator ever saw it.
	if e.recoveredPending {
		e.mu.Unlock()
		return nil, aicrerrors.New(aicrerrors.ErrCodeConflict,
			"a recovered run is waiting for retry or discard")
	}
	if e.current != nil && isLive(e.current.State) {
		e.mu.Unlock()
		return nil, aicrerrors.New(aicrerrors.ErrCodeConflict, "a run is already in progress")
	}
	previous := e.current
	now := time.Now().UTC()
	r := &Run{
		ID:        e.newID(),
		State:     StateRunning,
		Decisions: map[string]string{},
		Artifacts: map[string][]byte{},
		StartedAt: now,
		UpdatedAt: now,
	}
	// Name the phase here rather than leaving it to the first step's own
	// runStep/awaitDecisions call. Without this, the very first Save below
	// -- which happens before any step has run -- persists Phase: "", and a
	// crash in that window leaves a perfectly normal record that
	// Recover's validateLoaded cannot tell apart from a corrupt one. Since
	// that record is then never overwritten (the unreadable path swaps to a
	// memory store precisely so it never is), the mistake would be
	// permanent and silent: persistence disabled for the rest of the
	// process, repeated on every subsequent restart. An engine built with no
	// steps has nothing to derive a phase from; that construction is
	// test-only (main.go always assembles a real step slice) and already
	// completes immediately via execute's own i >= len(e.steps) branch, so
	// it is left as the zero value rather than indexing into an empty slice.
	if len(e.steps) > 0 {
		r.Phase = e.steps[0].Phase()
	}
	e.current = r
	e.resume = make(chan struct{}, 1)
	e.epoch++
	epoch := e.epoch
	// The run context derives from the detached one so an HTTP request
	// ending still cannot kill a 20-minute Apply.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	e.cancel = cancel
	e.done = make(chan struct{})
	done := e.done
	snapshot := r.Clone()
	e.mu.Unlock()

	if err := e.store.Save(ctx, snapshot); err != nil {
		// Undo the swap so the run is not left wedged: previous is
		// guaranteed non-live (the isLive check above already ensured
		// that), and no goroutine was launched for r (that only happens
		// below, after Save succeeds), so restoring the pointer is a clean
		// rollback. The epoch stays bumped -- see Retry's identical
		// rationale.
		e.mu.Lock()
		if e.current != nil && e.current.ID == r.ID && e.aliveLocked(epoch) {
			e.current = previous
		}
		e.mu.Unlock()
		// No goroutine will ever run for this epoch, so done would
		// otherwise sit open forever -- a CancelAndWait call in this
		// window must still see "no run in flight", not block for its
		// whole deadline.
		close(done)
		return nil, err
	}
	go func() {
		defer close(done)
		e.execute(runCtx, epoch)
	}()
	return snapshot, nil
}

func isLive(s State) bool {
	return s == StateRunning || s == StateAwaitingDecision
}

// aliveLocked reports whether the goroutine holding this epoch is still the
// one driving the current run. Callers must already hold e.mu.
//
// No black-box test can currently force a check here to fail: Retry only
// launches a second execute goroutine when State == StateFailed, and
// StateFailed is set exclusively by the first goroutine's own terminal
// finish() call, so that goroutine has no shared-state work left by the
// time Retry can even be called -- there is no live second writer for a
// black-box test to catch. engine_internal_test.go pins the guard anyway by
// manufacturing the condition directly (bumping epoch mid-step), since a
// guard no test can break is not a guard. docs/phase-2-handoff.md's
// "bite when Reset lands" note names the feature that will make this
// reachable through the public API.
func (e *Engine) aliveLocked(epoch uint64) bool { return e.epoch == epoch }

// CancelAndWait cancels the in-flight run and blocks until its execute
// goroutine has exited and persisted a terminal state, or ctx expires.
//
// Idempotent and safe with no run in flight: a second call sees an
// already-closed done channel and returns immediately. The deadline matters
// -- a step that ignores its context would otherwise block shutdown forever,
// and Kubernetes will SIGKILL the pod at terminationGracePeriodSeconds
// regardless of what this returns.
//
// Setting draining in the same lock hold that snapshots cancel/done is what
// makes the wait meaningful: a Start or Retry that had already taken the lock
// has by then installed its own cancel/done for this snapshot to pick up, and
// one arriving afterwards is refused with ErrDraining. Either way there is no
// run this call could return nil while leaving live. After this returns, the
// engine accepts no new runs -- it is a shutdown-only operation, not a "stop
// the current run" one.
func (e *Engine) CancelAndWait(ctx context.Context) error {
	e.mu.Lock()
	e.draining = true
	cancel, done := e.cancel, e.done
	e.mu.Unlock()

	if cancel == nil || done == nil {
		return nil
	}
	cancel()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return aicrerrors.New(aicrerrors.ErrCodeTimeout,
			"timed out waiting for the in-flight run to stop")
	}
}

// Decide supplies user decisions and unparks a run waiting on them.
//
// It persists before acknowledging: mutate and validate under e.mu, snapshot,
// unlock, Save, and only then either roll back (on failure) or signal resume
// (on success), re-acquiring the lock for that last step alone. Store I/O
// never happens while e.mu is held -- the observer's scope accessor calls
// CurrentID and Artifact on a per-watch-event path, and both take this same
// lock, so holding it across a ConfigMap round trip would stall every
// observer publish for the length of an API call. Start and Retry already
// use this exact shape.
func (e *Engine) Decide(runID string, decisions map[string]string) error {
	e.mu.Lock()

	if e.current == nil || e.current.ID != runID {
		e.mu.Unlock()
		return aicrerrors.New(aicrerrors.ErrCodeNotFound, "run not found: "+runID)
	}
	if e.current.State != StateAwaitingDecision {
		e.mu.Unlock()
		return aicrerrors.New(aicrerrors.ErrCodeConflict, "run is not awaiting a decision")
	}
	// Reject any key the client sends that this gate did not ask for.
	// Without this, a single call supplying every key a later step will ever
	// require (e.g. {"intent":..., "platform":..., "apply":"yes"}) satisfies
	// steps.Apply.Requires() before Apply's confirm gate is ever reached --
	// awaitDecisions only checks presence in e.current.Decisions, not that
	// the value arrived at the gate that asked for it. That contradicts the
	// contract steps/apply.go states (a confirm gate the console must not
	// mutate a cluster without) and, as a side effect, would let a later
	// Decide call silently overwrite intent/platform after Recommend has
	// already consumed them.
	pending := make(map[string]bool, len(e.current.Pending))
	for _, key := range e.current.Pending {
		pending[key] = true
	}
	for key := range decisions {
		if !pending[key] {
			e.mu.Unlock()
			return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "decision not currently requested: "+key)
		}
	}
	for _, key := range e.current.Pending {
		if _, ok := decisions[key]; !ok {
			e.mu.Unlock()
			return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "missing required decision: "+key)
		}
	}

	// prev captures every field this call is about to mutate -- Decisions,
	// Pending, State, and UpdatedAt -- so a failed Save can restore all four.
	// Task 4's rollback bug was restoring two of three mutated fields and
	// silently reopening the wedge that rollback existed to close; cloning
	// the whole run rather than hand-picking fields is what keeps a future
	// mutation added here from repeating it.
	prev := e.current.Clone()
	epoch := e.epoch

	for k, v := range decisions {
		e.current.Decisions[k] = v
	}
	e.current.Pending = nil
	e.current.State = StateRunning
	e.current.UpdatedAt = time.Now().UTC()
	snapshot := e.current.Clone()
	e.mu.Unlock()

	saveCtx, cancel := context.WithTimeout(context.Background(), decideSaveTimeout)
	defer cancel()
	if err := e.store.Save(saveCtx, snapshot); err != nil {
		// Guarded the same way Start's and Retry's rollbacks are: identity
		// plus epoch-aliveness, so this cannot stomp a run that has since
		// legitimately superseded this one.
		e.mu.Lock()
		if e.current != nil && e.current.ID == runID && e.aliveLocked(epoch) {
			e.current.Decisions = prev.Decisions
			e.current.Pending = prev.Pending
			e.current.State = prev.State
			e.current.UpdatedAt = prev.UpdatedAt
		}
		e.mu.Unlock()
		return aicrerrors.Wrap(aicrerrors.ErrCodeUnavailable, "persisting the decision failed", err)
	}

	// The resume signal must not be sent until the save above has actually
	// succeeded, or the parked step proceeds on a decision that was never
	// durably recorded.
	e.mu.Lock()
	if e.current != nil && e.current.ID == runID && e.aliveLocked(epoch) {
		select {
		case e.resume <- struct{}{}:
		default:
		}
	}
	e.mu.Unlock()
	return nil
}

// Current returns a copy of the most recently started run, or nil if none
// has been started yet. Unlike Get, it needs no run ID -- callers that care
// about "whatever this single-replica console is doing right now" (e.g. the
// options endpoint deriving cluster coordinates from the latest snapshot)
// would otherwise have no way to find it.
func (e *Engine) Current() *Run {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current == nil {
		return nil
	}
	return e.current.Clone()
}

// CurrentID returns the current run's ID without cloning the run. Current()
// deep-copies every artifact -- including the raw snapshot, which is tens of
// kilobytes -- so a caller that only needs the ID on a hot path (the
// observer, on every watch event) must not go through it.
func (e *Engine) CurrentID() (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current == nil {
		return "", false
	}
	return e.current.ID, true
}

// Artifact returns a copy of one artifact of the current run. It reports
// false when runID is not the current run or the key is absent -- taking the
// ID rather than reading e.current.ID is what keeps a caller that paired this
// with CurrentID from silently attributing a new run's artifact to the old
// run's scope.
//
// It exists so per-event callers stay off Current(), which deep-copies every
// artifact: snapshot.yaml alone is 67-74 KB in the KWOK fixtures and larger
// on real hardware, and the observer's run-scope accessor consults an
// artifact on every watch event for the whole window before recipe.json is
// written.
//
// The returned slice is a copy, not the stored one: handing out the live
// backing array would give a per-event caller a reference into engine-owned
// state.
func (e *Engine) Artifact(runID, key string) ([]byte, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current == nil || e.current.ID != runID {
		return nil, false
	}
	v, ok := e.current.Artifacts[key]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), v...), true
}

// Get returns a copy of the run's current state.
func (e *Engine) Get(runID string) (*Run, error) {
	e.mu.Lock()
	if e.current != nil && e.current.ID == runID {
		out := e.current.Clone()
		e.mu.Unlock()
		return out, nil
	}
	e.mu.Unlock()
	return e.store.Load(context.Background(), runID)
}

func (e *Engine) execute(ctx context.Context, epoch uint64) {
	for {
		e.mu.Lock()
		if !e.aliveLocked(epoch) {
			e.mu.Unlock()
			return
		}
		i := e.current.StepIndex
		e.mu.Unlock()

		if i >= len(e.steps) {
			break
		}
		step := e.steps[i]

		if !e.awaitDecisions(ctx, epoch, step) {
			return
		}
		if err := e.runStep(ctx, epoch, i, step); err != nil {
			return
		}
	}
	e.finish(ctx, epoch, StateDone, "")
}

// awaitDecisions parks the run until every key in step.Requires() is present.
// Returns false if the context ended while parked or this goroutine was
// superseded.
func (e *Engine) awaitDecisions(ctx context.Context, epoch uint64, step Step) bool {
	for {
		e.mu.Lock()
		if !e.aliveLocked(epoch) {
			e.mu.Unlock()
			return false
		}
		var missing []string
		for _, key := range step.Requires() {
			if _, ok := e.current.Decisions[key]; !ok {
				missing = append(missing, key)
			}
		}
		if len(missing) == 0 {
			e.mu.Unlock()
			return true
		}
		e.current.Pending = missing
		e.current.State = StateAwaitingDecision
		e.current.Phase = step.Phase()
		e.current.UpdatedAt = time.Now().UTC()
		runID := e.current.ID
		resume := e.resume
		snapshot := e.current.Clone()
		e.mu.Unlock()

		// Best-effort: losing this checkpoint degrades recovery granularity,
		// which is preferable to failing a live run over it, but it must
		// never be silent -- this warning is the only signal an operator has
		// that recovery has quietly stopped working.
		if err := e.store.Save(ctx, snapshot); err != nil {
			slog.Warn("run checkpoint failed", "run", runID, "error", err)
		}
		e.bus.Publish(bus.Event{
			RunID: runID, Kind: bus.KindDecision, Phase: string(step.Phase()),
			Message: "awaiting decision",
		})

		select {
		case <-resume:
		case <-ctx.Done():
			// A run frozen mid-gate with no goroutine is the same wedge
			// class Ruling 13 fixed for Save failures. Harmless with the
			// memory store (the process is exiting) but 2b-ii persists this.
			e.finish(ctx, epoch, StateFailed, canceledByShutdownMsg)
			return false
		}
	}
}

func (e *Engine) runStep(ctx context.Context, epoch uint64, i int, step Step) error {
	e.mu.Lock()
	if !e.aliveLocked(epoch) {
		e.mu.Unlock()
		return nil
	}
	e.current.Phase = step.Phase()
	e.current.State = StateRunning
	e.current.UpdatedAt = time.Now().UTC()
	runID := e.current.ID
	// The step gets a private copy, not e.current. A step writing
	// r.Artifacts while a concurrent Get() clones e.current is a data race
	// that -race reports; the copy's writes are merged back under the lock
	// after Run returns.
	scratch := e.current.Clone()
	snapshot := e.current.Clone()
	e.mu.Unlock()

	// Best-effort, same as awaitDecisions' checkpoint: never fails the run,
	// never silent.
	if err := e.store.Save(ctx, snapshot); err != nil {
		slog.Warn("run checkpoint failed", "run", runID, "error", err)
	}
	e.bus.Publish(bus.Event{
		RunID: runID, Kind: bus.KindPhase, Phase: string(step.Phase()),
		Message: "phase started",
	})

	emit := func(ev bus.Event) {
		ev.RunID = runID
		if ev.Phase == "" {
			ev.Phase = string(step.Phase())
		}
		e.bus.Publish(ev)
	}

	if err := step.Run(ctx, scratch, emit); err != nil {
		// context.Canceled.Error() is "context canceled" -- accurate but
		// useless to an operator watching the console for why their install
		// stopped. Use the same wording awaitDecisions produces when
		// canceled at a gate.
		msg := err.Error()
		if errors.Is(err, context.Canceled) {
			msg = canceledByShutdownMsg
		}
		e.bus.Publish(bus.Event{
			RunID: runID, Kind: bus.KindError, Phase: string(step.Phase()),
			Level: bus.LevelError, Message: msg,
		})
		e.finish(ctx, epoch, StateFailed, msg)
		return err
	}

	// Merge the step's writes back under the lock. Artifacts and Decisions are
	// the only fields a step may add to; the engine owns everything else. A
	// step can run for up to twenty minutes (Apply), which is precisely the
	// window in which this run can be superseded by a retry -- re-check here,
	// not just at the top of the function.
	e.mu.Lock()
	if !e.aliveLocked(epoch) {
		e.mu.Unlock()
		return nil
	}
	for k, v := range scratch.Artifacts {
		e.current.Artifacts[k] = v
	}
	for k, v := range scratch.Decisions {
		e.current.Decisions[k] = v
	}
	// Advance the cursor before this checkpoint is taken, not after: the
	// save below must carry the advanced StepIndex, and it must complete
	// before the next step begins (it does, trivially -- this call is
	// synchronous and execute's loop does not start the next step until
	// runStep returns). Otherwise a crash between step success and this
	// checkpoint replays a completed step on Retry.
	e.current.StepIndex = i + 1
	e.current.UpdatedAt = time.Now().UTC()
	merged := e.current.Clone()
	e.mu.Unlock()

	if err := e.store.Save(ctx, merged); err != nil {
		slog.Warn("run checkpoint failed", "run", runID, "error", err)
	}

	e.bus.Publish(bus.Event{
		RunID: runID, Kind: bus.KindPhase, Phase: string(step.Phase()),
		Message: "phase complete",
	})
	return nil
}

func (e *Engine) finish(ctx context.Context, epoch uint64, state State, errMsg string) {
	e.mu.Lock()
	if !e.aliveLocked(epoch) {
		e.mu.Unlock()
		return
	}
	e.current.State = state
	e.current.Err = errMsg
	e.current.UpdatedAt = time.Now().UTC()
	snapshot := e.current.Clone()
	e.mu.Unlock()

	saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(ctx), terminalSaveTimeout)
	defer saveCancel()
	if err := e.store.Save(saveCtx, snapshot); err != nil {
		// Unrecoverable, not silent: the run is already terminal, so there
		// is nothing left to roll back to. The real consequence is precise
		// and worth stating plainly -- the persisted record is not absent,
		// it is a stale earlier checkpoint. The next startup's recovery will
		// find that older record, treat it as an interrupted run, and mark
		// it failed, which is a more confusing outcome for an operator than
		// finding nothing at all.
		slog.Error("terminal run checkpoint failed; the next startup will recover a stale earlier record instead of this one",
			"run", snapshot.ID, "state", state, "error", err)
		e.bus.Publish(bus.Event{
			RunID: snapshot.ID, Kind: bus.KindError, Level: bus.LevelError,
			Message: "run finished but its checkpoint could not be saved; a restart will recover a stale earlier state",
		})
	}
	e.bus.Publish(bus.Event{
		RunID: snapshot.ID, Kind: bus.KindPhase,
		Level: levelFor(state), Message: "run " + string(state),
	})
}

// Retry re-executes a failed run from the step that failed. Valid only from
// StateFailed.
//
// Safe to re-run the whole Apply step: every component's install.sh is
// `helm upgrade --install`, which is idempotent, and deploy.sh's own
// preflight and stale-hook-Job cleanup run again on the retry. Components
// that already installed are no-ops on the second pass.
func (e *Engine) Retry(runID string) (*Run, error) {
	e.mu.Lock()
	if e.draining {
		e.mu.Unlock()
		return nil, ErrDraining
	}
	if e.current == nil || e.current.ID != runID {
		e.mu.Unlock()
		return nil, aicrerrors.New(aicrerrors.ErrCodeNotFound, "run not found: "+runID)
	}
	if e.current.State != StateFailed {
		e.mu.Unlock()
		return nil, aicrerrors.New(aicrerrors.ErrCodeConflict, "run is not in a failed state")
	}
	prevErr := e.current.Err
	prevRecoveredPending := e.recoveredPending
	// Retry is the intended resume path for a recovered run: accepting it
	// here is the operator action that clears the bootstrap gate in Start.
	e.recoveredPending = false
	e.current.State = StateRunning
	e.current.Err = ""
	e.current.UpdatedAt = time.Now().UTC()
	e.resume = make(chan struct{}, 1)
	e.epoch++
	epoch := e.epoch
	runCtx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	e.done = make(chan struct{})
	done := e.done
	snapshot := e.current.Clone()
	e.mu.Unlock()

	if err := e.store.Save(context.Background(), snapshot); err != nil {
		// Undo the flip to StateRunning so the run is not left wedged: with
		// no goroutine launched (that only happens below, after Save
		// succeeds) and State no longer StateFailed, neither Start
		// (isLive) nor Retry (requires StateFailed) could ever revive it.
		// The epoch stays bumped regardless -- epochs are monotonic, and a
		// bumped-but-unused epoch only invalidates goroutines that are
		// already gone.
		e.mu.Lock()
		if e.current != nil && e.current.ID == runID && e.aliveLocked(epoch) {
			e.current.State = StateFailed
			e.current.Err = prevErr
			e.current.UpdatedAt = time.Now().UTC()
			// recoveredPending is part of the state this rollback restores,
			// same as State and Err: without it, a Save failure here silently
			// re-opens the bootstrap gate this task closed -- Start would stop
			// 409ing and the SPA's automatic POST /api/runs would destroy the
			// run on its next load. Guarded by the same aliveLocked check as
			// the rest of the block so this cannot stomp a run that has since
			// legitimately superseded this one.
			e.recoveredPending = prevRecoveredPending
		}
		e.mu.Unlock()
		// See Start's identical rationale: no goroutine will ever run for
		// this epoch, so done must not sit open forever.
		close(done)
		return nil, err
	}
	e.bus.Publish(bus.Event{
		RunID: runID, Kind: bus.KindPhase, Message: "run retrying",
	})
	go func() {
		defer close(done)
		e.execute(runCtx, epoch)
	}()
	return snapshot, nil
}

// Discard drops a recovered run and its persisted record, freeing the
// console to start fresh. Without it, a recovered run would block Start
// forever -- a worse wedge than the one the block exists to prevent.
func (e *Engine) Discard(ctx context.Context, runID string) error {
	e.mu.Lock()
	if e.draining {
		e.mu.Unlock()
		return ErrDraining
	}
	if e.current == nil || e.current.ID != runID {
		e.mu.Unlock()
		return aicrerrors.New(aicrerrors.ErrCodeNotFound, "run not found: "+runID)
	}
	// A live run has an execute goroutine driving it, and every one of that
	// goroutine's e.current dereferences (execute, awaitDecisions, runStep,
	// finish) is guarded only by an aliveLocked(epoch) check, never by a
	// nil check on e.current itself -- nilling it here while that goroutine
	// still owns the epoch crashes the whole process on its next
	// checkpoint, not just this caller. This is deliberately not "bump
	// epoch instead": every guarded dereference above sits in the same
	// lock hold as its check today, so a bump would also close the gap
	// right now, but that safety is an incidental property of the current
	// code, not a structural guarantee -- a future dereference added
	// between a checkpoint and its use would silently reopen it. Never
	// nilling a live run holds regardless of that pairing, so it is the
	// only guard this method relies on.
	if isLive(e.current.State) {
		e.mu.Unlock()
		return aicrerrors.New(aicrerrors.ErrCodeConflict, "run is live; retry, wait for it to finish, or cancel before discarding")
	}
	e.current = nil
	e.recoveredPending = false
	e.mu.Unlock()

	// Store I/O deliberately outside the lock: the observer's scope accessor
	// calls CurrentID and Artifact on a per-watch-event path, and both take
	// e.mu.
	if err := e.store.Delete(ctx); err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeUnavailable, "deleting the persisted run failed", err)
	}
	return nil
}

func levelFor(s State) bus.Level {
	if s == StateFailed {
		return bus.LevelError
	}
	return bus.LevelInfo
}

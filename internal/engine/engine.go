package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"time"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/prove"
)

// terminalSaveTimeout bounds the detached write finish performs. Terminal
// state must persist even when the run was canceled, so this write cannot
// use the (now-dead) step context -- but it also cannot block shutdown
// indefinitely against an unreachable API server.
const terminalSaveTimeout = 5 * time.Second

// decideSaveTimeout bounds the checkpoint write Decide issues before
// acknowledging an operator's decision. Decide takes the caller's context
// (Task 7) but must not let that context's cancellation govern this write:
// an operator's decision that already returned 200 must never be silently
// undone because a browser tab closed mid-save. So Decide detaches the
// cancellation (context.WithoutCancel) while keeping the context's values,
// and bounds the write with this timeout instead -- the same shape finish's
// own detached terminal write already uses.
const decideSaveTimeout = 5 * time.Second

// canceledByShutdownMsg is Run.Err's message when the run is canceled by
// console shutdown, whether mid-step or parked at a decision gate. Both
// call sites share the constant so the wording (and this package's own
// tests, which match against it) cannot drift apart.
const canceledByShutdownMsg = "canceled: console shutting down"

// stopWaitAbsentTimeout bounds how long Stop waits for the workload's
// foreground deletion to finish cascading before reporting failure. Chosen
// to match steps.defaultGangTimeout's order of magnitude for the same
// single Job (plus its pods) Prove creates -- engine has no dependency on
// internal/steps (the reverse would be a cycle: steps already imports
// engine), so this is a second, independently chosen constant for the same
// real-world bound rather than a shared one.
const stopWaitAbsentTimeout = 3 * time.Minute

// ErrUnconfirmedCleanup is the sentinel a Step should wrap into the error it
// returns from Run when its own cleanup after a failure could not itself be
// confirmed -- steps/prove.go's cleanup helper is today's one producer
// (spec §8 row 3: "keep Start blocked" when cleanup cannot complete).
// internal/steps already imports engine (the reverse would cycle), so this
// is the direction a shared marker can travel; wrapping it (errors.Is, not
// string matching) is what fix round 1's C3 replaced a substring match
// with, after a one-character reword of steps/prove.go's message text was
// shown to leave go test ./... fully green with the guard silently dead.
//
// Deliberately checked at runStep's failure branch, on the actual error
// value a Step returned, and recorded on Run.CleanupUnconfirmed there --
// not re-derived later from Run.Err. Err is human-readable text that Retry
// legitimately overwrites on every attempt, so the determination has to be
// made once, from the real error, at the one moment it is still a typed
// value instead of a string.
var ErrUnconfirmedCleanup = errors.New("cleanup could not be confirmed")

// ErrCleanupConfirmed is the sentinel a Step should wrap into an otherwise
// ordinary failure's error when ITS OWN cleanup call ran and positively
// confirmed the workload absent -- steps/prove.go's cleanup helper wraps it
// around cause on the success path. Fix round 2's N2: runStep's failure
// branch used to CLEAR Run.CleanupUnconfirmed on any failure that did not
// wrap ErrUnconfirmedCleanup, which conflated two very different cases --
// "this attempt's cleanup ran and confirmed the workload gone" (safe to
// clear) and "this attempt never got far enough to attempt cleanup at all"
// (client.Ready() false, an EnsureNamespace error -- NOT safe to clear,
// since a PRIOR attempt's orphan may still be exactly as unconfirmed as it
// was). Demonstrated live: a retry failing at EnsureNamespace cleared the
// guard over an orphan Job the first attempt's Delete never removed, and
// Start then succeeded over it with nothing logged. Only ErrUnconfirmedCleanup
// and ErrCleanupConfirmed move the flag now; every other failure leaves it
// exactly as it was -- "only cleared by evidence the orphan is gone."
var ErrCleanupConfirmed = errors.New("cleanup confirmed the workload absent")

// hasUnconfirmedCleanup reports whether r is a failed run whose own cleanup
// attempt could not be confirmed -- see Run.CleanupUnconfirmed and
// ErrUnconfirmedCleanup. Ruling 12 (spec §8 row 3): such a run must keep
// blocking Start (and Discard -- see Discard's own guard), the same as a
// genuinely StateActive one, because the cluster may still be holding what
// that cleanup could not verify it removed. Stop is deliberately NOT
// blocked by this -- see stoppable's third arm -- because Stop retrying the
// same delete-and-confirm-absence Ruling 12 is worried about is the actual
// remedy, not a second exposure to the same risk.
func hasUnconfirmedCleanup(r *Run) bool {
	return r.State == StateFailed && r.CleanupUnconfirmed
}

// ErrDraining is what Start and Retry return once CancelAndWait has begun
// shutting the engine down. ErrCodeUnavailable is what makes internal/api's
// writeErr answer 503, matching the shape Server.Drain already returns for a
// mutation arriving during shutdown.
var ErrDraining = aicrerrors.New(aicrerrors.ErrCodeUnavailable,
	"engine is shutting down; not accepting new runs")

// ErrClusterMismatch means a persisted record describes a different cluster
// than the one this console is connected to. Not recoverable and not
// reconcilable: every release name in that record is now a name in somebody
// else's cluster.
//
// In-cluster this could not happen -- the store lived inside the cluster it
// described. A file on the operator's laptop has no such property, which is
// why the record carries its cluster's identity and both Recover and Reset
// revalidate it.
var ErrClusterMismatch = errors.New("run record belongs to a different cluster")

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

	// proveClient deletes and confirms absence of the workload a run left
	// running, for Stop -- the only exit from StateActive. Nil by default
	// (New sets none), matching prove.Client's own nil-outside-a-pod
	// contract; Stop checks Ready() before using it, the same guard
	// steps.proveStep.Run already applies, rather than dereferencing a
	// possibly-nil client. Set once via SetProveClient, before the engine
	// starts serving Stop requests -- the same "assigned once, before
	// concurrent readers exist" shape store's own doc comment describes for
	// Recover's markStoreUnreadable exception.
	proveClient *prove.Client

	// teardown removes the releases and namespaces a run installed, for
	// Reset. Nil by default and nil outside a cluster, the same shape as
	// proveClient; Reset refuses with 503 rather than dereferencing it.
	// Declared as an interface here because internal/teardown imports THIS
	// package -- see the Teardown interface in reset.go.
	teardown Teardown

	// clusterUID is the kube-system UID of the connected cluster. Empty
	// until SetClusterUID, and empty forever for a caller that never
	// connects one -- which is what every test in this package is. It is
	// read by Recover and Reset to refuse a record that describes somewhere
	// else; a comparison against empty is not a mismatch, on either side.
	clusterUID string

	// toolchain is what preflight resolved for this process: the version of
	// every executable a run shells out to. Set once, before serving, the
	// same shape as clusterUID.
	toolchain map[string]string

	mu      sync.Mutex
	current *Run
	resume  chan struct{}
	newID   func() string

	// attribution is the small, cheap snapshot Engine.Attribution() serves --
	// see attribution.go. It holds only ActiveAction/ActiveIndex/ActiveTotal/
	// Generation; RunID and Phase are composed from e.current at read time
	// instead of mirrored here, because e.current.ID never changes after
	// Start and e.current.Phase is set eagerly at the top of runStep --
	// unlike e.current.Components, which is exactly why this snapshot exists
	// at all. Keeping RunID/Phase out of this field means there is nothing
	// about them to keep in sync across Start/Retry/finish; only
	// ActiveAction's own transitions need a mutator.
	attribution Attribution

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

// SetProveClient wires the cluster client Stop uses to delete and confirm
// absence of the workload a run left running. Call once, before the engine
// starts serving Stop requests (main.go, right after New) -- mirrors
// markStoreUnreadable's "one exception, one lock hold" shape rather than
// adding a required constructor parameter that every existing caller of New
// (main.go and every test in this package and internal/api) would otherwise
// have to thread through for a client only Stop needs. A nil client, or one
// whose Ready() reports false, is valid and expected outside a cluster --
// Stop then fails cleanly (ErrCodeUnavailable) rather than panicking, the
// same posture steps.proveStep.Run already takes for the identical case.
func (e *Engine) SetProveClient(c *prove.Client) {
	e.mu.Lock()
	e.proveClient = c
	e.mu.Unlock()
}

// SetStore installs the run store the connect path selected, replacing the
// placeholder New was given. Same "assigned once, under the lock, before
// concurrent readers exist" shape as SetProveClient.
//
// It exists because the store's directory is named for a cluster this process
// has not chosen at construction time, and the engine has to exist before then
// -- internal/api takes it at New, and the server that serves the Connect
// screen is the same one that serves every run route. Every route that could
// reach the store is behind api's connect gate, and this lands while the
// connection is still stateConnecting, so no request can observe the
// placeholder.
//
// Per Ruling 4 this and Recover's unreadable-record swap are the only two
// places Engine.store is ever reassigned.
func (e *Engine) SetStore(st Store) {
	e.mu.Lock()
	e.store = st
	e.mu.Unlock()
}

// SetSteps installs the pipeline the connect path built. Same shape and same
// timing as SetStore, and for the same reason: three of the six steps hold a
// clientset that does not exist until a cluster is chosen (Discover and
// Apply directly, Prove through the prove.Client it is handed; Validate
// takes only the session kubeconfig path and the aicr.Client, not a
// kubernetes.Interface).
func (e *Engine) SetSteps(steps ...Step) {
	e.mu.Lock()
	e.steps = steps
	e.mu.Unlock()
}

// SetClusterUID records which cluster this engine is connected to, so Recover
// and Reset can refuse a record that describes a different one. Same
// "assigned once, under the lock, before concurrent readers exist" shape as
// SetProveClient -- the console calls it in the connect path, before recovery
// runs against the store that connect selected.
func (e *Engine) SetClusterUID(uid string) {
	e.mu.Lock()
	e.clusterUID = uid
	e.mu.Unlock()
}

// SetToolchain records the executables this process resolved at startup, so
// every run it starts carries them into its evidence. Same "assigned once,
// before concurrent readers exist" shape as SetClusterUID.
func (e *Engine) SetToolchain(tc map[string]string) {
	e.mu.Lock()
	e.toolchain = maps.Clone(tc)
	e.mu.Unlock()
}

// ClusterUID returns the connected cluster's identity, or empty if none was
// set.
func (e *Engine) ClusterUID() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.clusterUID
}

// checkClusterMatch refuses a record that describes a different cluster than
// the one this console is connected to. Both sides empty-tolerant: a record
// written by the ConfigMap store carries no UID and could not have been about
// anywhere but the cluster it lived in, and an engine with no UID has no
// grounds to reject anything.
func (e *Engine) checkClusterMatch(r *Run, action string) error {
	uid := e.ClusterUID()
	if uid == "" || r == nil || r.ClusterUID == "" || r.ClusterUID == uid {
		return nil
	}
	return fmt.Errorf("%w: %s refused -- the persisted run describes cluster %s but this console is connected to %s",
		ErrClusterMismatch, action, r.ClusterUID, uid)
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
	// StateActive is not isLive -- it has no execute goroutine -- but it does
	// hold a workload in the cluster, and starting over it would abandon that
	// workload with nothing tracking it. Teardown is never a side effect of
	// starting something (approach.md, Reset).
	if e.current != nil && e.current.State == StateActive {
		e.mu.Unlock()
		return nil, aicrerrors.New(aicrerrors.ErrCodeConflict,
			"a workload from the previous run is still running; stop it before starting a new run")
	}
	// Ruling 12 (spec §8 row 3): a failure is not enough to free Start when
	// that failure's own cleanup could not itself be confirmed -- the
	// cluster may still be holding what it tried and failed to verify it
	// removed. Same remedy as StateActive's guard above (stop it), because
	// from the operator's side it is the same problem: something might
	// still be running, and Start starting over it is the outcome the whole
	// discipline exists to prevent.
	if e.current != nil && hasUnconfirmedCleanup(e.current) {
		e.mu.Unlock()
		return nil, aicrerrors.New(aicrerrors.ErrCodeConflict,
			"a previous run's cleanup could not be confirmed; the workload may still be running -- resolve it before starting a new run")
	}
	// A Reset that did not finish leaves the cluster half-torn-down, and a
	// new run would install into it: helm would adopt whatever survived the
	// teardown, and the ownership snapshot this run takes would record
	// those leftovers as pre-existing, making them permanently unremovable.
	// Reset again is the remedy and is deliberately still allowed.
	if e.current != nil && hasIncompleteTeardown(e.current) {
		e.mu.Unlock()
		return nil, aicrerrors.New(aicrerrors.ErrCodeConflict,
			"a previous run's reset did not finish; parts of it are still installed -- reset it again before starting a new run")
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
		// Stamped at creation, under the same lock that reads it, so the
		// record says which cluster it describes from its very first Save.
		// A record that only acquired the UID later would be indistinguishable
		// -- while it lacked one -- from a record written before the field
		// existed, and those are deliberately accepted by any cluster.
		ClusterUID: e.clusterUID,
		// Copied, not shared: a run record is cloned and persisted
		// independently, and a map shared across every run would let one
		// record's mutation reach another's.
		Toolchain: maps.Clone(e.toolchain),
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

// isLive means "a goroutine owns this run". StateResetting qualifies for
// exactly that reason -- Reset launches one, the same way Start and Retry
// do -- which is also what makes Start, Discard and a second Reset all
// refuse it for free, and what makes Recover treat an interrupted teardown
// as an interrupted run.
func isLive(s State) bool {
	return s == StateRunning || s == StateAwaitingDecision || s == StateResetting
}

// isTerminal reports whether s is a state finish() actually reaches. finish
// is called from exactly three sites today (:511 StateDone, :561 and :652
// StateFailed) -- two distinct states -- so this matches finish's real
// coverage exactly.
//
// Deliberately not "!isLive(s)": that would also be true for StateIdle
// (never observed on e.current after Start, which always sets StateRunning)
// and StateActive (run.go's own comment reserves it for the Prove workload;
// no path sets it today). A consumer keyed off "not live" instead of
// "actually terminal" -- internal/observer's scoped informer teardown is
// exactly this shape -- would tear down the day StateActive is wired up,
// mid-Prove, which is the opposite of what that state exists to mean.
func isTerminal(s State) bool {
	return s == StateDone || s == StateFailed
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
//
// ctx is the caller's request context, but only its values travel into the
// save -- see decideSaveTimeout for why its cancellation is deliberately not
// allowed to reach the save that persists the operator's decision.
func (e *Engine) Decide(ctx context.Context, runID string, decisions map[string]string) error {
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

	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), decideSaveTimeout)
	defer cancel()
	if err := e.store.Save(saveCtx, snapshot); err != nil {
		// Guarded the same way Start's and Retry's rollbacks are: identity
		// plus epoch-aliveness, so this cannot stomp a run that has since
		// legitimately superseded this one. isLive additionally guards a
		// window unique to Decide: awaitDecisions is still blocked on
		// <-resume while this Save is in flight (resume is not sent until
		// Save succeeds), so a shutdown landing in that window cancels the
		// run's context, awaitDecisions takes the ctx.Done() branch, and
		// finish sets StateFailed -- via this SAME epoch, since only Start
		// and Retry bump it. Without the isLive check, this rollback would
		// overwrite that terminal state with StateAwaitingDecision, leaving
		// a live state with no goroutine behind it: exactly the wedge this
		// whole discipline exists to prevent.
		e.mu.Lock()
		if e.current != nil && e.current.ID == runID && e.aliveLocked(epoch) && isLive(e.current.State) {
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

// Get returns a copy of the run's current state. ctx governs only the
// store.Load fallback below (a run this process didn't start, e.g. after a
// restart before the SPA's next poll lands): the in-memory branch above
// never touches the store, so ctx is unused on that path.
func (e *Engine) Get(ctx context.Context, runID string) (*Run, error) {
	e.mu.Lock()
	if e.current != nil && e.current.ID == runID {
		out := e.current.Clone()
		e.mu.Unlock()
		return out, nil
	}
	e.mu.Unlock()
	return e.store.Load(ctx, runID)
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
	// The final step decides the terminal state. StateActive means something
	// this run created is still running in the cluster and only an operator
	// action ends it -- see Stop. A failure earlier in the loop returns
	// before reaching here, so a failed run can never land Active.
	terminal := StateDone
	if len(e.steps) > 0 && isActive(e.steps[len(e.steps)-1]) {
		terminal = StateActive
	}
	e.finish(ctx, epoch, terminal, "")
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
		// Attribution updates AFTER the marker reaches the bus, never
		// before: that ordering is the contract (design doc §2, "Marker
		// ordering is part of the contract"), not an incidental consequence
		// of statement order. Update it any earlier and a concurrent reader
		// of Attribution() (the observer, on its own goroutine) could label
		// a cluster event with an action whose header this line has not yet
		// handed to the bus -- the SPA would then receive an event citing a
		// row it has never heard of.
		if ev.Kind == bus.KindComponent {
			applyComponentMarker(e, epoch, ev.Data)
		}
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

		// Ruling 14: Components merges here too, unlike Artifacts and
		// Decisions. Those two are step OUTPUTS -- a step that errors
		// partway through may leave them inconsistent, which is exactly why
		// merging them only on success is correct. Components is progress
		// NARRATION, not an output: its partial value on a failing Apply
		// (a few components installed, one failed) is precisely what
		// recovery needs to redraw the pipeline instead of a bare failure.
		// It is the one field whose half-written state is meaningful, so it
		// is the one field that survives a failed step. Same epoch/identity
		// guard as the success-path merge below, and it must land before
		// finish's terminal save, or that save persists the pre-Apply rows.
		//
		// CleanupUnconfirmed is set from err itself (still a typed value
		// here, before msg above threw away everything but its text) --
		// see ErrUnconfirmedCleanup's doc comment for why Ruling 12's guard
		// has to be decided at exactly this point rather than re-derived
		// later from Run.Err.
		//
		// Fix round 2's N2: NOT an unconditional overwrite. Only
		// ErrUnconfirmedCleanup (sets true) and ErrCleanupConfirmed (clears
		// to false) move the flag; every other failure -- including one
		// whose own cleanup logic was never reached at all -- leaves
		// whatever was there STICKY. An unconditional "false unless
		// ErrUnconfirmedCleanup" (fix round 1's shape) cleared the guard on
		// a retry that failed at, say, EnsureNamespace: that attempt never
		// got far enough to look at the workload a PRIOR attempt's Delete
		// never removed, so it is not evidence of anything, and treating it
		// as confirmation let Start proceed over a still-live orphan with
		// nothing logged. slog calls make both real transitions visible --
		// a silent flip on a safety guard is how that stayed invisible.
		//
		// The decision of WHETHER to log is made under the lock (it reads
		// the prior value of CleanupUnconfirmed), but the slog calls
		// themselves run after e.mu.Unlock() -- fix round 3's NEW-4. slog's
		// default handler does I/O (a Write to stderr), and this file's own
		// rule, stated at Discard's store.Delete call, is that I/O never
		// runs under e.mu: the observer's scope accessor calls CurrentID and
		// Artifact on a per-watch-event path, and both take this same lock.
		var logUnconfirmed, logConfirmed bool
		e.mu.Lock()
		if e.aliveLocked(epoch) {
			e.current.Components = scratch.Components
			// Ownership merges on the failure path for the same reason
			// Components does, only more so. It is evidence, not an output:
			// Apply records it BEFORE it installs anything, so by the time
			// a step fails the snapshot is already complete and describes
			// the cluster as it was. And a failed Apply is exactly when
			// Reset matters most -- it is the case that leaves a
			// half-installed cluster -- so losing the evidence here would
			// leave the one run that most needs a teardown with nothing it
			// can prove it owns.
			e.current.Ownership = scratch.Ownership
			// AgentNamespace merges on the failure path for the strongest
			// version of that same reason: Discover creates the namespace
			// BEFORE it deploys the agent, so the most likely failure -- the
			// snapshot timing out -- is one that has already left a namespace
			// on the cluster. Dropping the record here would leave the operator
			// with a namespace nothing told them about, created by a run that
			// never got far enough to install anything.
			e.current.AgentNamespace = scratch.AgentNamespace
			switch {
			case errors.Is(err, ErrUnconfirmedCleanup):
				logUnconfirmed = !e.current.CleanupUnconfirmed
				e.current.CleanupUnconfirmed = true
			case errors.Is(err, ErrCleanupConfirmed):
				logConfirmed = e.current.CleanupUnconfirmed
				e.current.CleanupUnconfirmed = false
			}
		}
		e.mu.Unlock()
		if logUnconfirmed {
			slog.Warn("run's cleanup could not be confirmed; the cluster may still be holding the workload",
				"run", runID, "phase", step.Phase())
		}
		if logConfirmed {
			slog.Info("run's previously unconfirmed cleanup is now confirmed -- the workload is absent",
				"run", runID, "phase", step.Phase())
		}

		// The run is leaving Apply on a failure -- clear the cursor so a
		// retry (or the terminal state finish is about to record) does not
		// keep pointing at an action that stopped installing.
		if step.Phase() == PhaseApply {
			e.clearActiveAction(epoch)
		}

		e.finish(ctx, epoch, StateFailed, msg)
		return err
	}

	// Merge the step's writes back under the lock -- see mergeStepSuccess's
	// doc comment for which fields move and why. A step can run for up to
	// twenty minutes (Apply), which is precisely the window in which this
	// run can be superseded by a retry; mergeStepSuccess re-checks
	// aliveLocked itself rather than trusting the check at the top of this
	// function.
	merged, ok := e.mergeStepSuccess(epoch, scratch, i)
	if !ok {
		return nil
	}

	// The run is leaving Apply on success -- the action cursor is meaningless
	// once nothing in this step is installing, and the next step (if any)
	// will set its own Phase before any new marker can arrive.
	if step.Phase() == PhaseApply {
		e.clearActiveAction(epoch)
	}

	if err := e.store.Save(ctx, merged); err != nil {
		slog.Warn("run checkpoint failed", "run", runID, "error", err)
	}

	e.bus.Publish(bus.Event{
		RunID: runID, Kind: bus.KindPhase, Phase: string(step.Phase()),
		Message: "phase complete",
	})
	return nil
}

// mergeStepSuccess folds a step's scratch copy back onto e.current once Run
// has returned without error, and advances the cursor past the step that
// just completed. Artifacts, Decisions, and Components (Task 6: Apply's
// per-component projection) are the only fields a step may add to; the
// engine owns everything else. Returns the merged clone runStep should
// persist, and false if the run was superseded (a retry landed) while the
// step was executing -- in which case the caller has nothing to save and
// must not advance a cursor that no longer belongs to it.
func (e *Engine) mergeStepSuccess(epoch uint64, scratch *Run, i int) (*Run, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.aliveLocked(epoch) {
		return nil, false
	}
	for k, v := range scratch.Artifacts {
		e.current.Artifacts[k] = v
	}
	for k, v := range scratch.Decisions {
		e.current.Decisions[k] = v
	}
	// scratch.Components started as a full clone of e.current.Components
	// (see the Clone call in runStep) and the step only ever upserts rows in
	// place, so it is already the complete, current projection -- unlike
	// Artifacts and Decisions, there is nothing to merge key-by-key.
	e.current.Components = scratch.Components
	// Workload is an output, same as Artifacts and Decisions (Ruling 14's
	// distinction from Components' narration), and merged only here on
	// success for the same reason: Prove sets it only once its gang has
	// actually placed, and a step that errors and cleans up after itself has
	// left nothing to name. Without this line, an ActiveStep's identity
	// write lands on its own scratch copy and never reaches e.current -- the
	// run still ends at StateActive (isActive reads the step, not this
	// field), but Workload would be silently stuck at the zero value on
	// every real run, defeating the console-relabel-after-restart purpose
	// the field exists for.
	e.current.Workload = scratch.Workload
	// Ownership, same shape as Workload and for the same class of reason:
	// without this line internal/steps.snapshotOwnership's write lands on
	// the scratch copy and is discarded, and every Reset skips everything
	// for want of evidence that was in fact collected correctly. That is
	// what test/e2e/reset.sh caught on its first real run -- the run record
	// carried fourteen installed components and no ownership at all -- and
	// nothing in the unit suite could: internal/steps' tests call step.Run
	// directly on a run they own, so the merge is not on their path.
	//
	// Merged on BOTH paths, unlike Workload. See runStep's failure-path
	// merge for why.
	e.current.Ownership = scratch.Ownership
	// AgentNamespace, merged on both paths for the reason stated on
	// runStep's failure-path merge.
	e.current.AgentNamespace = scratch.AgentNamespace
	// Validation is an output, same shape as Workload and for the same class
	// of reason: without this line internal/steps.Validate's write lands on
	// the scratch copy and is discarded, and a recovered run would read as
	// "never validated" when in fact it was.
	e.current.Validation = scratch.Validation
	// Advance the cursor before this checkpoint is taken, not after: the
	// save below must carry the advanced StepIndex, and it must complete
	// before the next step begins (it does, trivially -- this call is
	// synchronous and execute's loop does not start the next step until
	// runStep returns). Otherwise a crash between step success and this
	// checkpoint replays a completed step on Retry.
	e.current.StepIndex = i + 1
	e.current.UpdatedAt = time.Now().UTC()
	return e.current.Clone(), true
}

func (e *Engine) finish(ctx context.Context, epoch uint64, state State, errMsg string) {
	e.mu.Lock()
	// e.current == nil is checked separately from aliveLocked(epoch): fix
	// round 1's C1. Discard nils e.current WITHOUT bumping epoch
	// (deliberately -- see Discard's own doc comment), which was safe only
	// because Discard rejected every state a live goroutine's own finish
	// call could still be in flight for. Stop's idempotency arm broke that
	// invariant from a direction Discard's comment did not anticipate: Stop
	// calls finish from outside the execute goroutine machinery, for a run
	// in StateDone -- a state Discard does NOT reject -- so a Discard
	// racing a second, idempotent Stop call (still blocked in
	// Delete/WaitAbsent, at the SAME epoch, since Stop never bumps it) can
	// nil e.current out from under this call. aliveLocked(epoch) alone
	// cannot see that: epoch is unchanged, so it still reports true, and
	// the dereference two lines down was a nil-pointer panic that killed
	// the process (reproduced empirically in fix round 1's review). Once
	// Discard has cleared the run, there is nothing left for this call to
	// finish -- returning here is correct, not merely defensive.
	if !e.aliveLocked(epoch) || e.current == nil {
		e.mu.Unlock()
		return
	}
	e.current.State = state
	e.current.Err = errMsg
	e.current.UpdatedAt = time.Now().UTC()
	snapshot := e.current.Clone()
	e.mu.Unlock()

	// Defensive backstop: runStep's success and failure branches already
	// clear the active action on every path that leaves Apply, but a
	// terminal state is where that guarantee must hold regardless of how it
	// was reached -- nothing this run does from here on should ever again be
	// read as "an action is installing". epoch-guarded like the two runStep
	// call sites, which also closes the window between the e.mu.Unlock()
	// above and this call, where a new Start could in principle have already
	// landed.
	e.clearActiveAction(epoch)

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
//
// ctx is the caller's request context, already detached by the handler
// (context.WithoutCancel) before it reaches here -- Retry can relaunch a
// step that runs for up to 20 minutes (Apply), same as Start, so the run
// must survive the request that kicked it off. Both the relaunched
// execution's context and the checkpoint Save below derive from it, mirroring
// Start's identical shape.
func (e *Engine) Retry(ctx context.Context, runID string) (*Run, error) {
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
	// New relative to Ruling 12, which only had to consider Start and
	// Discard. A failed Reset lands in StateFailed, which is precisely the
	// state Retry accepts -- so without this, the console would offer to
	// re-run Apply over a cluster it has just half-removed, reinstalling
	// components on top of releases whose uninstall had failed midway.
	if hasIncompleteTeardown(e.current) {
		e.mu.Unlock()
		return nil, aicrerrors.New(aicrerrors.ErrCodeConflict,
			"this run's reset did not finish; parts of it are still installed -- reset it again rather than retrying the install")
	}
	prevErr := e.current.Err
	prevRecoveredPending := e.recoveredPending
	// Retry is the intended resume path for a recovered run: accepting it
	// here is the operator action that clears the bootstrap gate in Start.
	e.recoveredPending = false
	e.current.State = StateRunning
	e.current.Err = ""
	// CleanupUnconfirmed is deliberately NOT reset here, unlike Err --
	// fix round 2's N2. This field is sticky by design (see runStep's own
	// comment): only ErrUnconfirmedCleanup and ErrCleanupConfirmed move it,
	// and neither is reachable on a retry that parks at a decision gate or
	// is canceled by shutdown before runStep's failure branch ever runs.
	// Resetting it here regardless (fix round 1's shape) would clear Ruling
	// 12's guard on exactly the paths that produced no new evidence either
	// way -- the same defect class N2 fixed at runStep, reopened one call
	// site over. If the run resumes normally, runStep's own failure branch
	// still recomputes the field fresh from whatever THIS attempt's actual
	// error says, on every real step failure.
	e.current.UpdatedAt = time.Now().UTC()
	e.resume = make(chan struct{}, 1)
	e.epoch++
	epoch := e.epoch
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	e.cancel = cancel
	e.done = make(chan struct{})
	done := e.done
	snapshot := e.current.Clone()
	e.mu.Unlock()

	if err := e.store.Save(ctx, snapshot); err != nil {
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
			// CleanupUnconfirmed needs no restoration here: this call never
			// touched it (see the reset above's own comment), so it is
			// already exactly what it was before Retry was called.
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
	// goroutine's e.current dereferences (execute, awaitDecisions, runStep)
	// is guarded only by an aliveLocked(epoch) check, never by a nil check
	// on e.current itself -- nilling it here while that goroutine still
	// owns the epoch would corrupt its next checkpoint. This is
	// deliberately not "bump epoch instead": every guarded dereference
	// above sits in the same lock hold as its check today, so a bump would
	// also close the gap right now, but that safety is an incidental
	// property of the current code, not a structural guarantee -- a future
	// dereference added between a checkpoint and its use would silently
	// reopen it. Never nilling a live run holds regardless of that pairing,
	// so it is the only guard this method relies on for isLive callers.
	//
	// finish is the one exception, and it is Stop's, not execute's: Stop
	// (fix round 1's idempotency arm) calls finish for a run in StateDone
	// -- not live, so this guard does not cover it -- from outside the
	// execute machinery entirely, at the SAME epoch Discard is free to nil
	// e.current under. finish itself now nil-checks e.current for exactly
	// this reason (see finish's own comment, fix round 1's C1); this
	// guard's job stays scoped to isLive, not extended to cover it, so the
	// two continue to reason about disjoint states.
	if isLive(e.current.State) {
		e.mu.Unlock()
		return aicrerrors.New(aicrerrors.ErrCodeConflict, "run is live; retry, wait for it to finish, or cancel before discarding")
	}
	// Not folded into isLive: isLive means "a goroutine owns this run", which
	// StateActive does not. This is a different claim -- "the cluster holds
	// something this run created" -- and discarding would delete the record
	// that is the only pointer to it.
	if e.current.State == StateActive {
		e.mu.Unlock()
		return aicrerrors.New(aicrerrors.ErrCodeConflict,
			"run holds a running workload; stop the workload before discarding")
	}
	// Ruling 12's Start guard (above, in Start) is only real if Discard
	// cannot clear e.current out from under it: discarding a run whose own
	// cleanup could not be confirmed would nil e.current, and Start's guard
	// checks e.current -- so a Discard-then-Start would silently reopen
	// exactly the hole Ruling 12 closes. Same remedy as StateActive's reject
	// two lines up, for the same underlying reason.
	if hasUnconfirmedCleanup(e.current) {
		e.mu.Unlock()
		return aicrerrors.New(aicrerrors.ErrCodeConflict,
			"run's cleanup could not be confirmed; the workload may still be running -- resolve it before discarding")
	}
	// The sharpest of the three. Discard deletes the record, and after a
	// failed Reset that record is the ONLY inventory of what is still
	// installed -- Run.Residue names every release and namespace the
	// teardown could not remove. Discarding would leave the cluster exactly
	// as it is and destroy the only description of it.
	if hasIncompleteTeardown(e.current) {
		e.mu.Unlock()
		return aicrerrors.New(aicrerrors.ErrCodeConflict,
			"run's reset did not finish; this record is the only list of what is still installed -- reset it again before discarding")
	}
	previous := e.current
	previousRecoveredPending := e.recoveredPending
	epoch := e.epoch
	e.current = nil
	e.recoveredPending = false
	e.mu.Unlock()

	// Store I/O deliberately outside the lock: the observer's scope accessor
	// calls CurrentID and Artifact on a per-watch-event path, and both take
	// e.mu.
	if err := e.store.Delete(ctx); err != nil {
		// Put the run back. The record still exists, so the state this call
		// cleared no longer describes the store -- and the console now offers
		// the operator a Discard button whose second press would answer 404
		// ("run not found") against a checkpoint that is still there, and
		// which the next restart will recover all over again. Restoring makes
		// the retry the SPA already invites actually retry the delete.
		//
		// This deliberately reverses the earlier "clear regardless so a store
		// outage can never block Start" semantics: that reasoning predates
		// Discard having a caller, and it traded a wedge nobody could reach
		// for a lie the operator now can. recoveredPending comes back with
		// it, because the record it gates on is still on the cluster.
		//
		// Guarded like every other rollback in this file: only restore if
		// nothing has since claimed the slot. A Start landing in this window
		// installs its own run and bumps the epoch, and that run must win --
		// it is live and this one is not.
		e.mu.Lock()
		if e.current == nil && e.aliveLocked(epoch) {
			e.current = previous
			e.recoveredPending = previousRecoveredPending
		}
		e.mu.Unlock()
		return aicrerrors.Wrap(aicrerrors.ErrCodeUnavailable, "deleting the persisted run failed", err)
	}
	return nil
}

// stoppable reports whether r is a valid target for Stop:
//
//  1. Genuinely StateActive.
//  2. StateDone with a workload identity still recorded -- the shape a run
//     has immediately after Stop itself finishes it (see Stop's own
//     e.finish call below). This is what makes Stop idempotent against a
//     real double-click or a racing reconciliation (spec §7: "stopping an
//     already-stopped workload succeeds") without also accepting a run
//     that reached StateDone by ordinary completion and never held a
//     workload at all: only an ActiveStep's own write ever sets Workload
//     (runStep's success-path merge; run.go's doc comment on the field),
//     so a run whose last step was not one carries the zero value here
//     and stays rejected.
//  3. StateFailed with an unconfirmed cleanup (hasUnconfirmedCleanup).
//     Fix round 1's I1: Ruling 12 blocks Start and Discard on such a run,
//     but blocking every operation that could resolve it -- including the
//     one that actually performs the delete-and-confirm-absence the
//     cleanup itself could not complete -- recreates the operator dead
//     end this task exists to remove, one state over. Stop retrying that
//     exact operation is the remedy, not a second exposure to the risk.
//
// Workload's CONTENTS play no part in arm 2, only its presence -- Delete
// and WaitAbsent below still address the workload by runID alone, exactly
// as every other prove.Client caller does; leaning on Workload for more
// than that would contradict its own "hint, not identity" doc comment.
func stoppable(r *Run) bool {
	return r.State == StateActive ||
		(r.State == StateDone && r.Workload != (Workload{})) ||
		hasUnconfirmedCleanup(r)
}

// Stop is the only way a run leaves StateActive -- operator-initiated,
// always. Nothing in this file calls it on the operator's behalf: not on
// restart (Recover installs a persisted StateActive record as StateActive,
// unresumed -- see recover.go), not on a timeout, not as a side effect of
// Start. Same rule as Reset, for the same reason: it destroys something the
// operator is watching (spec §7). It is also the remedy for Ruling 12's
// third case (stoppable's arm 3): retrying the delete-and-confirm-absence a
// failed cleanup could not complete.
//
// Foreground deletion, then WaitAbsent, and StateDone is set ONLY once both
// have succeeded -- active_test.go's bite-proof pins this exact ordering:
// reversing it (finishing at StateDone before confirming absence) is what
// makes TestFailedStopLeavesRunActive fail alone. On any failure the run is
// left exactly where it was, and the returned error names what failed -- it
// must never claim to have stopped something it did not (spec §7's one
// outcome this design most wants to avoid).
//
// Idempotent per stoppable's second arm above and per prove.Client.Delete's
// own contract (a missing workload is success -- its doc comment): an
// operator double-click, or a reconciliation racing one, sees nil either
// way.
func (e *Engine) Stop(ctx context.Context, runID string) error {
	e.mu.Lock()
	// Same guard as Start/Retry/Discard, same reason: requireNotDraining
	// gates the outer mux, but a request that cleared that check
	// microseconds before Drain() still reaches here.
	if e.draining {
		e.mu.Unlock()
		return ErrDraining
	}
	if e.current == nil || e.current.ID != runID {
		e.mu.Unlock()
		return aicrerrors.New(aicrerrors.ErrCodeNotFound, "run not found: "+runID)
	}
	if !stoppable(e.current) {
		e.mu.Unlock()
		return aicrerrors.New(aicrerrors.ErrCodeConflict, "run has no active workload to stop")
	}
	if e.proveClient == nil || !e.proveClient.Ready() {
		e.mu.Unlock()
		return aicrerrors.New(aicrerrors.ErrCodeUnavailable,
			"stop: a live cluster client is required to stop the reference workload")
	}
	epoch := e.epoch
	// Copied to a local under the same lock hold as epoch, for the same
	// reason epoch is: fix round 1's I2. SetProveClient writes this field
	// under e.mu; reading it again after e.mu.Unlock() below (as the two
	// calls further down used to) is a benign-today, structurally-wrong
	// read of mutex-protected state -- benign only because main.go sets it
	// once before api.New ever runs and nothing else in this binary writes
	// it concurrently, which is exactly the shape of bug -race can never
	// catch and a future second writer could turn real.
	client := e.proveClient
	e.mu.Unlock()

	// Delete-then-confirm, as one call. Reset needs the identical guarantee
	// and cannot reach it through Stop -- stoppable() rejects an ordinary
	// StateFailed run and rejects StateResetting outright -- so the sequence
	// lives in prove.Client and both callers require it there. The wrapping
	// is unchanged, so the operator-facing message for either half is the
	// same text it has always been.
	if err := client.EnsureAbsent(ctx, runID, stopWaitAbsentTimeout); err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeUnavailable, "stopping the workload failed", err)
	}

	// Recorded so a SECOND Stop call also recognizes the resulting
	// StateDone run via stoppable's arm 2 -- fix round 2's N4. Without
	// this, a run resolved via arm 3 (StateFailed with unconfirmed
	// cleanup) reaches StateDone with Workload still at its zero value --
	// Prove's own success-path write (steps/prove.go) is never reached on
	// a failure -- so a repeat, idempotent Stop click (spec §7) would be
	// rejected with "run has no active workload to stop" instead of
	// succeeding. WorkloadName is deterministic from runID alone, so this
	// is accurate regardless of which arm resolved it: it names the exact
	// object Delete/WaitAbsent above just addressed. A harmless no-op for
	// arm 1 (Prove's own write already set the same value).
	//
	// CleanupUnconfirmed is cleared here too -- fix round 3's NEW-6.
	// hasUnconfirmedCleanup already requires StateFailed, so a stale true
	// on a StateDone record never re-blocks anything today, but Stop just
	// confirmed the workload IS gone (that is what Delete+WaitAbsent
	// succeeding above means), and a record claiming both "done" and
	// "cleanup unconfirmed" is a contradiction a future reader -- a JSON
	// API consumer, a console badge, a later reimplementation that keys
	// off this field alone -- has no reason to expect. A harmless no-op
	// for arms 1 and 2, where the field was already false.
	e.mu.Lock()
	if e.current != nil && e.current.ID == runID && e.aliveLocked(epoch) {
		e.current.Workload = Workload{Namespace: prove.Namespace, Kind: "Job", Name: prove.WorkloadName(runID)}
		e.current.CleanupUnconfirmed = false
	}
	e.mu.Unlock()

	// finish is what actually persists StateDone and publishes the terminal
	// bus event -- the same call every other terminal transition in this
	// file uses (execute's own StateActive/StateFailed sites). epoch was
	// captured before the two cluster calls above: nothing can bump e.epoch
	// while e.current.State is StateActive, the Stop-produced StateDone
	// stoppable's second arm accepts, or the StateFailed its third arm
	// accepts -- Start's own guard rejects all three (StateActive
	// directly, StateFailed-with-unconfirmed-cleanup via Ruling 12, and
	// StateDone is simply not StateFailed so Retry cannot touch it either)
	// -- so aliveLocked(epoch) inside finish is a defensive re-check here,
	// not a load-bearing one, matching the rationale every other call site
	// in this file already gives for checking anyway. finish itself now
	// also nil-checks e.current, closing the one way this WAS reachable: a
	// concurrent Discard of an already-Stopped (StateDone) run -- fix round
	// 1's C1.
	e.finish(ctx, epoch, StateDone, "")

	// Mirrors Retry's and Discard's own clearing of the bootstrap gate:
	// fix round 1's M2. Without this, a Stop that succeeds against a
	// recovered StateActive run leaves recoveredPending set, and Start
	// keeps 409ing on "a recovered run is waiting for retry or discard"
	// even though the run it names is now StateDone and gone -- forcing a
	// second, differently-named click for no reason. Only cleared on
	// success, matching Stop's own "on failure, nothing about the run
	// changes" contract: a failed Delete or WaitAbsent above already
	// returned before reaching here, so this line is never reached on that
	// path. Unconditional, not identity-guarded like Start's/Retry's own
	// rollback branches: recoveredPending blocks EVERY Start while true, so
	// e.current can only be the recovered run itself for as long as it
	// stays true, and Stop already required e.current.ID == runID above.
	e.mu.Lock()
	e.recoveredPending = false
	e.mu.Unlock()

	return nil
}

func levelFor(s State) bus.Level {
	if s == StateFailed {
		return bus.LevelError
	}
	return bus.LevelInfo
}

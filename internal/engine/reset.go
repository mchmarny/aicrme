package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/prove"
)

// resetWorkloadTimeout bounds the confirmed stop of the run's workload
// before any uninstall begins. Same budget as Stop's, because it is the
// same operation against the same object.
const resetWorkloadTimeout = stopWaitAbsentTimeout

// KindRelease and KindNamespace are ResidueItem.Kind's two values. They are
// constants because the SPA switches on them and Residue.Removed counts by
// them, so a typo would silently produce a zero count rather than a
// compile error.
const (
	KindRelease   = "release"
	KindNamespace = "namespace"
	// KindObject is a single named cluster object a component's chart
	// created and then instructed helm to keep, so that `helm uninstall`
	// leaves it standing. Reported separately from the release it came with
	// because it is a different claim: the release is gone, and this is what
	// the release left behind and what the teardown did about it.
	KindObject = "object"
)

// Teardown removes from the cluster what one run installed.
//
// Declared here rather than imported from internal/teardown because that
// package imports THIS one -- for ComponentState and Ownership, the
// evidence every decision it makes is founded on. The dependency runs one
// way only, so the interface lives on this side and main.go injects the
// implementation, exactly as SetProveClient already does for prove.Client.
//
// Releases reports each outcome through emit as it happens and also returns
// the full list: a thirteen-component teardown with --wait takes minutes, and
// an operator watching it needs the rows to land as they resolve rather than
// all at once at the end.
//
// One method, not two. Namespaces used to be the other half; it is now
// namespaceResidue, a pure function over the ownership snapshot, because the
// console reports namespaces rather than deleting them.
type Teardown interface {
	// Releases uninstalls each component's release, in reverse install
	// order, skipping every release the run cannot prove it created.
	//
	// ctx runs each command and is DETACHED -- Reset never cancels it. cancel
	// is the one an operator's cancellation reaches, and implementations must
	// check it only BETWEEN commands, so an uninstall in flight is never
	// interrupted. See Reset's goroutine and internal/teardown for why that
	// distinction is load-bearing on both sides.
	Releases(ctx, cancel context.Context, comps []ComponentState, own Ownership, emit func(ResidueItem)) []ResidueItem
}

// SetTeardown installs the cluster-removal half of Reset. Set once, before
// the engine starts serving Reset requests -- the same "assigned once,
// before concurrent readers exist" shape SetProveClient uses.
func (e *Engine) SetTeardown(t Teardown) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.teardown = t
}

// hasIncompleteTeardown reports whether a Reset left this run's cluster in a
// state only another Reset should act on.
//
// The three operations it blocks each break something different: Start
// would install over a half-removed cluster, Retry would re-run Apply over
// one, and Discard would delete the only inventory of what is still
// installed. Reset itself is deliberately not blocked -- re-running the
// teardown is the remedy, the same reasoning stoppable's third arm applies
// to Ruling 12.
//
// A structural field, not text re-derived from Run.Err, for the same reason
// CleanupUnconfirmed is: Err is human text that a later operation
// legitimately overwrites.
func hasIncompleteTeardown(r *Run) bool { return r.Residue.Incomplete }

// Reset tears down exactly what this run installed: its workload, the helm
// releases it created, and the namespaces it created and left empty.
//
// Operator-initiated and operator-confirmed, always. Nothing in this
// package calls it: not a failed run, not a restart, not a timeout, not a
// discard. Same rule as Stop, for a stronger version of the same reason --
// it removes things an operator may be relying on, and anything it cannot
// prove this run created it leaves alone and names.
//
// Backgrounded like Start: the teardown runs for minutes (thirteen helm
// uninstalls with --wait), so this installs StateResetting, persists it,
// launches the goroutine and returns. ctx is the caller's request context,
// already detached by the handler, and everything below derives from it.
//
// StateActive is a legitimate starting point and the most common one: a run
// that finished Prove has a workload deliberately still running, and step 1
// below is what stops it.
func (e *Engine) Reset(ctx context.Context, runID string) error {
	e.mu.Lock()
	if e.draining {
		e.mu.Unlock()
		return ErrDraining
	}
	if e.current == nil || e.current.ID != runID {
		e.mu.Unlock()
		return aicrerrors.New(aicrerrors.ErrCodeNotFound, "run not found: "+runID)
	}
	// The guard that matters most. Reset acts on release names, and a release
	// name is only meaningful in the cluster it was installed into --
	// uninstalling "gpu-operator" against the wrong one removes a stranger's
	// working install and reports success. Checked inline rather than through
	// checkClusterMatch's own ClusterUID() call because e.mu is already held
	// here and that method takes it.
	if e.clusterUID != "" && e.current.ClusterUID != "" && e.current.ClusterUID != e.clusterUID {
		recordUID, connectedUID := e.current.ClusterUID, e.clusterUID
		e.mu.Unlock()
		return fmt.Errorf("%w: reset refused -- the persisted run describes cluster %s but this console is connected to %s",
			ErrClusterMismatch, recordUID, connectedUID)
	}
	// isLive now includes StateResetting, so this also refuses a second
	// Reset while the first is still running.
	if isLive(e.current.State) {
		e.mu.Unlock()
		return aicrerrors.New(aicrerrors.ErrCodeConflict,
			"run is live; wait for it to finish or cancel it before resetting")
	}
	// Nothing was installed, so there is nothing this run can prove it
	// owns. Refusing is not pedantry: a Reset that reported success here
	// would tell an operator their cluster had been cleaned when this run
	// had never touched it.
	if len(e.current.Components) == 0 {
		e.mu.Unlock()
		return aicrerrors.New(aicrerrors.ErrCodeConflict,
			"this run installed nothing, so there is nothing to reset")
	}
	if e.proveClient == nil || !e.proveClient.Ready() {
		e.mu.Unlock()
		return aicrerrors.New(aicrerrors.ErrCodeUnavailable,
			"reset: a live cluster client is required to stop the reference workload")
	}
	if e.teardown == nil {
		e.mu.Unlock()
		return aicrerrors.New(aicrerrors.ErrCodeUnavailable,
			"reset: no teardown is configured in this process")
	}
	client := e.teardown
	prover := e.proveClient

	e.current.State = StateResetting
	e.current.Err = ""
	// Cleared, not accumulated: this Reset's inventory describes what is on
	// the cluster now, and merging a previous attempt's items would report
	// releases that a re-Reset has since removed.
	e.current.Residue = Residue{}
	e.current.UpdatedAt = time.Now().UTC()
	e.epoch++
	epoch := e.epoch
	// Same shape as Start: the operation's context is detached from the
	// request, and e.cancel is what an operator's cancellation reaches.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	e.cancel = cancel
	e.done = make(chan struct{})
	done := e.done
	snapshot := e.current.Clone()
	e.mu.Unlock()

	if err := e.store.Save(ctx, snapshot); err != nil {
		// Roll the state back so the run is not left wedged in
		// StateResetting with no goroutine driving it: nothing has been
		// torn down (that only starts below, after Save succeeds), so the
		// run is exactly as it was. Same shape and same rationale as
		// Start's and Retry's rollbacks, including the epoch staying
		// bumped.
		e.mu.Lock()
		if e.current != nil && e.current.ID == runID && e.aliveLocked(epoch) {
			// StateFailed rather than the state it came from: the
			// checkpoint is the only durable record that a teardown was
			// attempted, and a run silently returned to StateActive or
			// StateDone would look untouched. The run itself is fine --
			// nothing was removed -- so no residue guard is set, and Retry
			// and Discard both stay available.
			e.current.State = StateFailed
			e.current.UpdatedAt = time.Now().UTC()
		}
		e.mu.Unlock()
		close(done)
		return err
	}

	// "run " + state, exactly the shape finish publishes for every other
	// state. web/src/components/Wizard.tsx's deriveRunState switches on that
	// literal, and its default arm is 'running' -- so a differently-worded
	// message here would render a teardown as an ordinary install in
	// progress, complete with the actions that go with one.
	e.bus.Publish(bus.Event{
		RunID: runID, Kind: bus.KindPhase, Level: bus.LevelWarn,
		Message: "run " + string(StateResetting),
	})
	e.publishReset(runID, bus.LevelWarn,
		"resetting: stopping the workload, then removing what this run installed")
	go func() {
		defer close(done)
		defer cancel()
		// TWO DIFFERENT CONTEXTS, and the difference is the whole point.
		//
		// cmdCtx is detached and never canceled. internal/applier's
		// BashExec SIGTERMs the entire process group the instant its
		// context is done, so running helm under runCtx would interrupt an
		// uninstall mid-flight and strand the release half-removed -- the
		// exact residue Reset exists to eliminate, produced by the
		// operation meant to prevent it. Each command is bounded instead by
		// helm's own --timeout, which internal/teardown passes explicitly.
		//
		// runCtx is what an operator's cancellation reaches, and
		// internal/teardown checks it only BETWEEN commands: the in-flight
		// uninstall finishes, the next one never starts.
		cmdCtx := context.WithoutCancel(runCtx)
		e.runReset(cmdCtx, runCtx, epoch, runID, prover, client)
	}()
	return nil
}

// runReset is the teardown itself, in the three steps the design requires
// (section 4), in this order and not in parallel.
func (e *Engine) runReset(ctx, cancelCtx context.Context, epoch uint64, runID string,
	prover *prove.Client, client Teardown) {

	run := e.snapshotRun(epoch, runID)
	if run == nil {
		return
	}

	// Step 1 is a hard precondition, not a parallel step. Uninstalling
	// kai-scheduler and the GPU operator out from under a gang that still
	// holds devices is the failure mode this whole ordering exists to
	// prevent, so a workload that cannot be confirmed gone stops the
	// teardown before a single release is touched.
	if err := prover.EnsureAbsent(ctx, runID, resetWorkloadTimeout); err != nil {
		e.recordResidue(epoch, runID, nil, true)
		e.finish(ctx, epoch, StateFailed,
			"reset stopped before removing anything: the reference workload could not be confirmed stopped: "+err.Error())
		return
	}
	e.publishReset(runID, bus.LevelInfo, "workload confirmed stopped; removing "+
		fmt.Sprintf("%d releases", len(run.Components)))

	emit := func(item ResidueItem) { e.publishResidueItem(runID, item) }
	items := client.Releases(ctx, cancelCtx, run.Components, run.Ownership, emit)
	// Reported, not removed -- and reported even when the release half was
	// cut short, because a half-finished teardown is exactly when the
	// operator most needs the inventory.
	for _, it := range namespaceResidue(run.Ownership, run.AgentNamespace, run.Validation.Namespace) {
		emit(it)
		items = append(items, it)
	}

	// Interrupted counts as incomplete even when every command that ran
	// succeeded: what was never attempted is still on the cluster, and the
	// operator needs the same guard and the same inventory either way.
	incomplete := cancelCtx.Err() != nil
	for _, it := range items {
		if it.Err != "" {
			incomplete = true
		}
	}
	e.recordResidue(epoch, runID, items, incomplete)

	final := e.snapshotRun(epoch, runID)
	if final == nil {
		return
	}
	summary := resetSummary(final.Residue)
	if incomplete {
		// The record stays. It is the only inventory of what is still
		// installed, and hasIncompleteTeardown is what stops Discard
		// deleting it.
		e.publishResetSummary(runID, bus.LevelError, summary, final.Residue)
		e.finish(ctx, epoch, StateFailed, summary)
		return
	}

	e.publishResetSummary(runID, bus.LevelInfo, summary, final.Residue)
	e.finish(ctx, epoch, StateDone, "")

	// Only now, and only when clean: the console is free to start a new run
	// and a restart must not recover this one. Deliberately after finish --
	// finish's own Save is what would otherwise write the record straight
	// back after this delete.
	if err := e.store.Delete(ctx); err != nil {
		// Not fatal to the reset, which really did remove everything. The
		// consequence is confined to a restart, which would recover a run
		// whose cluster state is already gone -- recoverable by one
		// Discard, which this run's now-clean residue leaves available.
		slog.Error("reset completed but its record could not be deleted; a restart will recover an already-torn-down run",
			"run", runID, "error", err)
	}

	// Mirrors Stop's own M2: a recovered run that has now been fully torn
	// down must not keep 409ing Start on "a recovered run is waiting for
	// retry or discard".
	e.mu.Lock()
	if e.aliveLocked(epoch) {
		e.recoveredPending = false
	}
	e.mu.Unlock()
}

// snapshotRun returns a copy of the current run if this epoch still owns it,
// and nil otherwise -- the same aliveLocked discipline every other
// out-of-band operation in this package follows.
func (e *Engine) snapshotRun(epoch uint64, runID string) *Run {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.aliveLocked(epoch) || e.current == nil || e.current.ID != runID {
		return nil
	}
	return e.current.Clone()
}

// recordResidue writes the teardown's inventory onto the run before finish
// persists it. Written under the lock and epoch-guarded, like every other
// mutation of e.current outside the execute goroutine.
func (e *Engine) recordResidue(epoch uint64, runID string, items []ResidueItem, incomplete bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.aliveLocked(epoch) || e.current == nil || e.current.ID != runID {
		return
	}
	e.current.Residue = Residue{Incomplete: incomplete, Items: items}
	e.current.UpdatedAt = time.Now().UTC()
}

// namespaceResidue reports every namespace the ownership snapshot recorded,
// and removes none of them.
//
// A pure function over evidence Apply already persisted -- no API call, no
// discovery document, no dynamic client. That is the point: deciding whether
// a namespace was safe to delete required walking every namespaced kind the
// API server serves, and the answer was almost always "not empty, keeping
// it". The walk is gone and the honest half of its output is kept, because
// whoever applied the bundle owns the cleanup of what it applied and this
// console is only the bash deployer.
//
// Best effort about completeness, never about destructiveness: a namespace
// left standing is one command for the operator, and one deleted out from
// under something is unrecoverable. So each is reported with what the
// operator needs to decide -- did this run create it, or did it predate the
// install -- and the console acts on neither.
//
// agent is the namespace AICR's snapshot agent ran in, and it has to be passed
// separately because own.Namespaces cannot carry it: steps.recipeNamespaces
// builds that set from recipe.json's components, and the agent namespace is
// not one of them -- it exists before a recipe does. Reported only when this
// run created it. One that predates the install is not residue; it is somebody
// else's namespace a Job briefly ran in, and listing it would send an operator
// to delete something this console never touched.
//
// Reported, never removed, and not because removing it would be hard. AICR's
// deployer already cleans up the Job, ServiceAccount and RoleBinding it
// created (DiscoverConfig.Cleanup is always true), so the namespace is all
// that remains; adding teardown code to chase it would put this console in the
// business of undoing a deployer's work, which is the line this repo has
// drawn. The deployer owns its cleanup, and aicrme prints what is left.
func namespaceResidue(own Ownership, agent AgentNamespace, validation string) []ResidueItem {
	out := make([]ResidueItem, 0, len(own.Namespaces)+1)
	for _, ns := range own.Namespaces {
		note := "this run created it; remove it when you no longer need it"
		if ns.Existed {
			note = "it existed before the install, so it was used rather than created"
		}
		out = append(out, ResidueItem{
			Kind:    KindNamespace,
			Name:    ns.Name,
			Skip:    note,
			Created: !ns.Existed,
		})
	}
	// Appended last and only if the ownership snapshot did not already name
	// it. Nothing stops an operator pointing AICRME_NAMESPACE at a namespace
	// the recipe also installs into, and two residue rows for one namespace
	// would double it in the summary counts the operator reads to tell a clean
	// teardown from a partial one.
	if validation != "" && !namesNamespace(out, validation) {
		out = append(out, ResidueItem{
			Kind:    KindNamespace,
			Name:    validation,
			Skip:    "AICR's validator created it; its Jobs and RBAC are already cleaned up, but the namespace remains",
			Created: true,
		})
	}
	if agent.Created && agent.Name != "" && !namesNamespace(out, agent.Name) {
		out = append(out, ResidueItem{
			Kind:    KindNamespace,
			Name:    agent.Name,
			Skip:    "this run created it for the snapshot agent; remove it when you no longer need it",
			Created: true,
		})
	}
	return out
}

func namesNamespace(items []ResidueItem, name string) bool {
	for _, it := range items {
		if it.Kind == KindNamespace && it.Name == name {
			return true
		}
	}
	return false
}

// resetSummary is the counts-not-verdict line the design requires (section
// 6): an operator must be able to tell a clean teardown from a partial one
// without reading the timeline.
func resetSummary(res Residue) string {
	var failed, skipped int
	for _, it := range res.Items {
		switch {
		case it.Err != "":
			failed++
		case it.Skip != "":
			skipped++
		}
	}
	s := fmt.Sprintf("reset: %d of %d releases removed, %d of %d namespaces removed",
		res.Removed(KindRelease), res.Considered(KindRelease),
		res.Removed(KindNamespace), res.Considered(KindNamespace))
	if failed > 0 {
		s += fmt.Sprintf("; %d failed", failed)
	}
	if skipped > 0 {
		// No reason is given here on purpose. This line used to say "because
		// this run did not create them", which contradicted the items printed
		// directly beneath it -- most of those namespaces DID come from this
		// run, and they survive because Reset never deletes a namespace, not
		// because it disclaimed them. Observed on real H100s 2026-08-30. Each
		// item carries its own accurate reason; the summary counts, it does not
		// explain.
		s += fmt.Sprintf("; %d left in place, each with its reason below", skipped)
	}
	return s
}

func (e *Engine) publishReset(runID string, level bus.Level, msg string) {
	e.bus.Publish(bus.Event{RunID: runID, Kind: bus.KindLog, Level: level, Message: msg})
}

// ResetSummaryData is the terminal teardown event's Data payload. It carries
// the whole inventory, not just the counts, because the console has no other
// way to learn it: the SPA derives everything from the event stream, and a
// failed Reset's residue is precisely what the operator has to act on.
//
// Incomplete is duplicated here rather than inferred from the item list: an
// interrupted teardown can have no failed items at all (what was never
// attempted has no item saying so), so counting errors would report a clean
// teardown for exactly the case that most needs the guard shown.
type ResetSummaryData struct {
	Incomplete bool          `json:"incomplete"`
	Summary    string        `json:"summary"`
	Items      []ResidueItem `json:"items,omitempty"`
}

func (e *Engine) publishResetSummary(runID string, level bus.Level, msg string, res Residue) {
	// ResetSummaryData holds only strings, bools and ints, so Marshal
	// cannot fail.
	data, _ := json.Marshal(ResetSummaryData{
		Incomplete: res.Incomplete, Summary: msg, Items: res.Items,
	})
	e.bus.Publish(bus.Event{
		RunID: runID, Kind: bus.KindLog, Level: level, Message: msg, Data: data,
	})
}

// residueData is the wire shape a teardown KindComponent event carries.
// Operation is what tells web/src/pipeline.ts it is looking at a removal
// rather than an install running backwards -- without it the SPA would
// render a teardown as a pipeline whose rows go from installed to
// something-else in descending order.
type residueData struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Kind      string `json:"kind"`
	Status    string `json:"status"`
	Operation string `json:"operation"`
	Reason    string `json:"reason,omitempty"`
}

// StatusRemoved and friends are the teardown statuses the SPA renders.
const (
	StatusRemoved = "removed"
	StatusSkipped = "skipped"
	StatusFailed  = "failed"
	// OperationTeardown is residueData.Operation's only value today. It
	// exists so the SPA can discriminate on a field rather than infer the
	// operation from the run's state, which it does not receive per event.
	OperationTeardown = "teardown"
)

func (e *Engine) publishResidueItem(runID string, item ResidueItem) {
	status, level, reason := StatusRemoved, bus.LevelInfo, ""
	switch {
	case item.Err != "":
		status, level, reason = StatusFailed, bus.LevelError, item.Err
	case item.Skip != "":
		// Warn, not info: a skipped item is something the operator now has
		// to deal with by hand, and it must not scroll past looking routine.
		status, level, reason = StatusSkipped, bus.LevelWarn, item.Skip
	}
	// residueData holds only strings and is always marshalable.
	data, _ := json.Marshal(residueData{
		Name: item.Name, Namespace: item.Namespace, Kind: item.Kind,
		Status: status, Operation: OperationTeardown, Reason: reason,
	})
	msg := item.Kind + " " + item.Name + " " + status
	if reason != "" {
		msg += ": " + reason
	}
	e.bus.Publish(bus.Event{
		RunID: runID, Kind: bus.KindComponent, Level: level,
		Component: item.Name, Message: msg, Data: data,
	})
}

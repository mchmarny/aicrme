package engine

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/prove"
)

// The three things reconciliation can find, worded for the operator reading
// the console rather than the log. Each states what is true of the CLUSTER,
// because that is the fact the persisted record turned out not to carry.
const (
	workloadStillRunningMsg = "the reference workload from the previous session is still running"
	workloadGoneMsg         = "the reference workload this run left is no longer in the cluster; the run has ended"
	workloadAdoptedMsg      = "found a reference workload from an earlier session and adopted it; Stop is the only way to end it"
)

// matchesRun reports whether w is the workload run runID left behind.
//
// Either identity is enough, deliberately. The run-id label is the primary
// key, but prove reads it back through a literal key of its own
// (prove's runIDLabelKey) that is independent of the one prove.Labels writes:
// if those two ever drift, every discovered object comes back with an empty
// RunID rather than an error. The name is derived from the run ID by
// prove.WorkloadName and is just as authoritative. Matching on either means
// such a drift makes reconciliation see a workload that is still there --
// which is the safe direction, because the alternative is concluding a run's
// workload is gone while it keeps holding GPUs.
func matchesRun(w prove.OwnedWorkload, runID string) bool {
	return runID != "" && (w.RunID == runID || w.Name == prove.WorkloadName(runID))
}

// workloadPresent reports whether any discovered workload belongs to runID.
func workloadPresent(owned []prove.OwnedWorkload, runID string) bool {
	for _, w := range owned {
		if matchesRun(w, runID) {
			return true
		}
	}
	return false
}

// adoptable reports whether w can be turned into a run record this console
// can actually act on.
//
// Both checks exist to keep a Stop button honest. Stop addresses the workload
// as prove.WorkloadName(runID) and prove.Client.Delete treats a missing object
// as success, so adopting a workload whose name does not derive from its own
// run-id label would give the operator a Stop that reports success while
// deleting nothing -- the one outcome spec §7 most wants to avoid. The ID
// format check is narrower: an ID this engine could not have produced would
// persist a record the NEXT startup's validateLoaded rejects, which swaps the
// store for a memory one and disables persistence for that whole process.
func adoptable(w prove.OwnedWorkload) bool {
	return validRunID(w.RunID) && prove.WorkloadName(w.RunID) == w.Name
}

// ReconcileWorkloads settles the persisted run record against what is
// actually running in the cluster, and must be called once at startup right
// after Recover -- before the HTTP server starts serving, for the same reason
// Recover carries (the SPA's automatic POST /api/runs on load must not race a
// record this call is still installing).
//
// It exists because the record and the workload can be lost independently:
// terminal saves are best-effort and the store can degrade to memory
// (cmstore.go), while the workload outlives the process entirely. Spec §3
// enumerates the three combinations, and this reconciles all three:
//
//   - record active, workload present -- normal. Nothing changes; Stop is the
//     operator's exit, exactly as it was before the restart.
//   - record absent, workload present -- the store lost it. The workload is
//     adopted into a synthetic StateActive run so the operator gets a Stop
//     button back. It is NEVER deleted: tearing down a workload nobody asked
//     to stop is Reset's job, and Reset is never automatic (approach.md).
//   - record active, workload absent -- the run already ended. It finishes at
//     StateDone.
//
// Two rules govern everything it does NOT do. It never deletes anything: a
// workload it cannot adopt is reported and left running for an operator with
// kubectl. And it never clears the recovered-run gate (recoveredPending) --
// only an operator action (Retry, Discard, a successful Stop) does that, so a
// run finished here still waits to be acknowledged rather than vanishing
// under the next page load.
//
// It returns an error only when the cluster could not be asked. A failed list
// is not evidence of absence, so nothing is decided on that path -- concluding
// "gone" from a failed list would finish a run at StateDone while its workload
// keeps holding GPUs. Callers log the error and start anyway: reconciliation
// is a convenience, and the console starting is not.
func (e *Engine) ReconcileWorkloads(ctx context.Context, c *prove.Client) error {
	// A nil client, or one whose Ready reports false, is the ordinary
	// developer-laptop shape (rest.InClusterConfig fails outside a pod, and
	// main.go constructs the client anyway). Every other prove.Client method
	// dereferences kube immediately and panics on a nil one, and this runs on
	// every startup -- so the check is what keeps a laptop run from crashing
	// in the one place a crash is least recoverable.
	if c == nil || !c.Ready() {
		return nil
	}
	owned, err := c.ListOwned(ctx)
	if err != nil {
		return fmt.Errorf("reconciling reference workloads: %w", err)
	}
	// Sorted so the adoption below picks the same workload on every run given
	// the same cluster. ListOwned's order is the API server's, and adopting
	// "whichever came first" would make the console's choice unreproducible
	// in the one situation where an operator is already confused.
	sort.Slice(owned, func(i, j int) bool { return owned[i].Name < owned[j].Name })

	e.mu.Lock()
	epoch := e.epoch
	var curID string
	var curState State
	var curPhase Phase
	if e.current != nil {
		curID, curState, curPhase = e.current.ID, e.current.State, e.current.Phase
	}
	e.mu.Unlock()

	present := workloadPresent(owned, curID)
	switch {
	case curState == StateActive && present:
		slog.Info("reconciled: the recovered run's reference workload is still running", "run", curID)
		e.publishReconciled(curID, curPhase, bus.LevelInfo, workloadStillRunningMsg)
	case curState == StateActive:
		slog.Info("reconciled: the recovered active run has no workload left in the cluster; finishing it",
			"run", curID)
		// Published before finish, so a subscriber reading the stream
		// top-down learns why the run ended before it sees it end.
		e.publishReconciled(curID, curPhase, bus.LevelInfo, workloadGoneMsg)
		// finish is what every other terminal transition in this package
		// uses: it persists the state, clears any lingering active action,
		// and publishes the same "run done" event a live run would. epoch was
		// captured moments ago, at startup, before any goroutine that could
		// bump it exists.
		e.finish(ctx, epoch, StateDone, "")
	case present:
		// A run that is not active but whose workload is still there. The
		// realistic shape is StateFailed with an unconfirmed cleanup, where
		// Stop is already the remedy (stoppable's third arm), so there is
		// nothing to change -- but saying nothing would leave the operator
		// reading a failed run with no hint that the cluster is still holding
		// something.
		slog.Warn("reconciled: a workload from a run that is not active is still running",
			"run", curID, "state", curState)
		e.publishReconciled(curID, curPhase, bus.LevelWarn, workloadStillRunningMsg)
	}

	for _, w := range owned {
		if matchesRun(w, curID) {
			continue
		}
		// Adoption installs a run record, and this console tracks exactly one
		// run at a time. When a record already exists it is the operator's --
		// possibly a recovered one they have not seen yet -- so replacing it
		// would erase the only account of what happened. Everything not
		// adopted is reported by identity, which is what an operator needs to
		// find it with kubectl, and left running.
		if curID == "" && adoptable(w) {
			e.adopt(ctx, w)
			// So a second discovered workload is reported rather than
			// adopted over the top of the one just installed.
			curID = w.RunID
			continue
		}
		slog.Warn("a reference workload is running that this console does not manage; left untouched",
			"namespace", w.Namespace, "name", w.Name, "runID", w.RunID)
	}
	return nil
}

// adopt installs w as a synthetic StateActive run so the operator can Stop
// it. Called only for a workload adoptable() accepted, and only when this
// console holds no run record at all.
//
// recoveredPending is deliberately NOT set, unlike Recover's own install.
// That flag makes Start answer "a recovered run is waiting for retry or
// discard", and for a StateActive run both of those named remedies are
// rejected outright (Retry needs StateFailed; Discard refuses StateActive) --
// so it would name two dead ends. Start is already blocked here by its own
// StateActive guard, which says the true thing: stop it before starting a new
// run.
func (e *Engine) adopt(ctx context.Context, w prove.OwnedWorkload) {
	now := time.Now().UTC()
	r := &Run{
		ID:        w.RunID,
		State:     StateActive,
		Phase:     PhaseProve,
		Decisions: map[string]string{},
		Artifacts: map[string][]byte{},
		// The run completed every step -- that is what leaving a workload
		// running means -- so nothing remains to resume from. Nothing reads
		// this on a StateActive run today (Retry is the only consumer and it
		// requires StateFailed); it is set to the honest value rather than a
		// zero one that would claim the run had not started.
		StepIndex: len(e.steps),
		Workload:  Workload{Namespace: w.Namespace, Kind: "Job", Name: w.Name},
		// When this console adopted it, not when the workload started: the
		// object's own creation timestamp is not part of what ListOwned
		// returns. Both timestamps are required non-zero by validateLoaded,
		// and this is the only honest value available here.
		StartedAt: now,
		UpdatedAt: now,
	}

	e.mu.Lock()
	// Re-checked under the lock: the caller decided to adopt from a snapshot
	// taken outside it. Nothing at startup can install a run in that window
	// today (this runs before the server serves), but adopting OVER a record
	// is the one thing this function must never do, so it is checked where
	// the guarantee actually holds rather than where it was inferred.
	if e.current != nil {
		e.mu.Unlock()
		slog.Warn("not adopting a discovered workload: a run record already exists",
			"namespace", w.Namespace, "name", w.Name)
		return
	}
	e.current = r
	snapshot := r.Clone()
	e.mu.Unlock()

	// Best-effort, same as every other checkpoint in this package: the
	// adoption is already real in memory, and a store that cannot take it
	// only costs the NEXT restart a re-adoption from the same labels -- which
	// is exactly the path that produced this one. Bounded by the store's own
	// per-call timeout (cmstore.go), so it cannot stall startup.
	if err := e.store.Save(ctx, snapshot); err != nil {
		slog.Warn("adopted workload could not be checkpointed; it is tracked for this process only",
			"run", r.ID, "error", err)
	}

	slog.Warn("adopted a reference workload this console did not start", "run", r.ID,
		"namespace", w.Namespace, "name", w.Name)
	e.publishReconciled(r.ID, PhaseProve, bus.LevelWarn, workloadAdoptedMsg)
	// The state-bearing event, published last so a subscriber reading
	// top-down learns what it is looking at before it looks at it -- the same
	// ordering, and the same "run <state>" wording, publishRecoveryBootstrap
	// uses. An adopted run has no other bootstrap: Recover found no record to
	// publish one from, so without this the SPA renders an idle wizard while
	// a workload holds GPUs.
	e.bus.Publish(bus.Event{
		RunID: r.ID, Kind: bus.KindPhase, Phase: string(PhaseProve),
		Level: levelFor(StateActive), Message: "run " + string(StateActive),
	})
}

// publishReconciled reports one reconciliation finding on the stream. Phase
// is the run's own, not PhaseProve: the SPA sets its current phase from any
// event carrying one (web/src/components/Wizard.tsx), so stamping "prove" on
// a finding about a run that failed during Apply would move the console's
// phase to one that run never reached.
func (e *Engine) publishReconciled(runID string, phase Phase, level bus.Level, msg string) {
	e.bus.Publish(bus.Event{
		RunID: runID, Kind: bus.KindLog, Phase: string(phase),
		Level: level, Message: msg,
	})
}

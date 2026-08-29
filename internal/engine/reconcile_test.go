package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/prove"
)

// Both IDs match validRunID's format (16 hex characters, what randomID
// actually produces), so a fixture exercising adoption is never rejected for
// an incidental reason the real producer could not create.
const (
	reconcileRunID = "00112233445566ff"
	orphanRunID    = "aabbccddeeff0011"
)

// ownedJob is the object prove.Client.Apply leaves in the cluster: the
// ownership labels ListOwned selects on, and the name Stop derives from the
// run ID. Built from prove's own helpers rather than literals, so a change to
// either side cannot leave these fixtures asserting against a shape the
// producer no longer creates.
func ownedJob(runID string) *batchv1.Job {
	return namedOwnedJob(runID, prove.WorkloadName(runID))
}

// namedOwnedJob is ownedJob with the name and the run-id label decoupled, for
// the one test that needs them to disagree.
func namedOwnedJob(runID, name string) *batchv1.Job {
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name:      name,
		Namespace: prove.Namespace,
		Labels:    prove.Labels(runID),
	}}
}

// activeRecord is what a console that reached StateActive persisted before it
// was restarted: every field validateLoaded requires, plus the Workload hint
// an ActiveStep's own success path writes.
func activeRecord() *Run {
	const id = reconcileRunID
	now := time.Now().UTC()
	return &Run{
		ID:        id,
		State:     StateActive,
		Phase:     PhaseProve,
		Decisions: map[string]string{},
		Artifacts: map[string][]byte{},
		StepIndex: 2, // len(steps) below -- the run completed every step
		Workload:  Workload{Namespace: prove.Namespace, Kind: "Job", Name: prove.WorkloadName(id)},
		StartedAt: now,
		UpdatedAt: now,
	}
}

// restartedEngine reproduces main.go's startup ordering: a store that may
// already hold a record, a fresh Engine over it, SetProveClient, then
// Recover -- so every test below calls ReconcileWorkloads against exactly the
// state a real restart hands it, not a hand-assembled one.
//
// The step slice must contain a PhaseBundle step or Recover fails outright
// with ErrStepConfig (bundleStepIndex), and its final step is an ActiveStep
// so len(e.steps) matches activeRecord's StepIndex.
func restartedEngine(t *testing.T, cs *fake.Clientset, seeded *Run) (*Engine, *bus.Bus, *prove.Client) {
	t.Helper()
	b := bus.New(64)
	store := NewMemoryStore()
	if seeded != nil {
		if err := store.Save(context.Background(), seeded); err != nil {
			t.Fatalf("seeding the store: %v", err)
		}
	}
	e := New(b, store,
		&fakeStep{phase: PhaseBundle},
		&fakeActiveStep{phase: PhaseProve, active: true},
	)
	client := prove.NewClient(cs)
	e.SetProveClient(client)
	if err := e.Recover(context.Background()); err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	return e, b, client
}

// Case 1 (spec §3): the record survived and so did the workload. Nothing to
// change -- but "nothing to change" has to mean the run is still StateActive
// AND the workload is still there AND Stop still works, because a
// reconciliation that quietly tore down what it found would satisfy the first
// two on its own.
func TestReconcileKeepsActiveRunWithLiveWorkload(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset(ownedJob(reconcileRunID))
	e, _, client := restartedEngine(t, cs, activeRecord())

	if err := e.ReconcileWorkloads(ctx, client); err != nil {
		t.Fatalf("ReconcileWorkloads() error = %v", err)
	}

	got := e.Current()
	if got == nil || got.ID != reconcileRunID || got.State != StateActive {
		t.Fatalf("Current() = %+v, want run %s still at %q", got, reconcileRunID, StateActive)
	}
	if _, err := cs.BatchV1().Jobs(prove.Namespace).
		Get(ctx, prove.WorkloadName(reconcileRunID), metav1.GetOptions{}); err != nil {
		t.Fatalf("workload Get() after reconcile = %v, want it left running", err)
	}

	// "Offer Stop" is only a real claim if Stop actually resolves the run.
	if err := e.Stop(ctx, reconcileRunID); err != nil {
		t.Fatalf("Stop() after reconcile error = %v, want the recovered run stoppable", err)
	}
}

// Case 2 (spec §3): the workload is there and the store lost the record --
// the case labels exist for. Adoption is what gives the operator a Stop
// button back; deleting it instead would be the console tearing down GPUs
// nobody asked it to touch. That is Reset's job, and Reset is never automatic.
func TestReconcileAdoptsOrphanedWorkload(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset(ownedJob(orphanRunID))
	e, _, client := restartedEngine(t, cs, nil)
	if e.Current() != nil {
		t.Fatalf("fixture check: Current() = %+v before reconcile, want no record at all", e.Current())
	}

	if err := e.ReconcileWorkloads(ctx, client); err != nil {
		t.Fatalf("ReconcileWorkloads() error = %v", err)
	}

	id, ok := e.CurrentID()
	if !ok || id != orphanRunID {
		t.Fatalf("CurrentID() = %q, %v; want the orphan's own run ID %q", id, ok, orphanRunID)
	}
	got := e.Current()
	if got.State != StateActive {
		t.Errorf("State = %q, want %q -- only StateActive offers Stop", got.State, StateActive)
	}
	if got.Phase != PhaseProve {
		t.Errorf("Phase = %q, want %q", got.Phase, PhaseProve)
	}
	want := Workload{Namespace: prove.Namespace, Kind: "Job", Name: prove.WorkloadName(orphanRunID)}
	if got.Workload != want {
		t.Errorf("Workload = %+v, want %+v", got.Workload, want)
	}

	// Adoption must not delete: this is the assertion the bite-proof inverts.
	if _, err := cs.BatchV1().Jobs(prove.Namespace).
		Get(ctx, prove.WorkloadName(orphanRunID), metav1.GetOptions{}); err != nil {
		t.Fatalf("workload Get() after adoption = %v, want it still running", err)
	}
	// Adoption that is not persisted has to be redone on every restart, and
	// would be lost entirely the moment anything else saves over it.
	if _, err := e.store.Load(ctx, orphanRunID); err != nil {
		t.Errorf("store.Load() for the adopted run = %v, want it checkpointed", err)
	}
	// The whole point: the operator can now end it.
	if err := e.Stop(ctx, orphanRunID); err != nil {
		t.Fatalf("Stop() on the adopted run error = %v", err)
	}
	if _, err := cs.BatchV1().Jobs(prove.Namespace).
		Get(ctx, prove.WorkloadName(orphanRunID), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("workload Get() after Stop = %v, want NotFound", err)
	}
}

// Case 3 (spec §3): the record says active, the cluster says otherwise. The
// run already ended, so it finishes at StateDone -- and the recovered-run
// gate stays closed, because only an operator action (Discard here) may clear
// it. Reconciliation observed a fact; it did not act on the operator's behalf.
func TestReconcileFinishesActiveRunWhoseWorkloadIsGone(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	e, _, client := restartedEngine(t, cs, activeRecord())

	if err := e.ReconcileWorkloads(ctx, client); err != nil {
		t.Fatalf("ReconcileWorkloads() error = %v", err)
	}

	got := e.Current()
	if got == nil || got.State != StateDone {
		t.Fatalf("Current() = %+v, want %q", got, StateDone)
	}
	persisted, err := e.store.Load(ctx, reconcileRunID)
	if err != nil {
		t.Fatalf("store.Load() = %v", err)
	}
	if persisted.State != StateDone {
		t.Errorf("persisted State = %q, want %q -- the next restart must not recover it as active again",
			persisted.State, StateDone)
	}

	if _, err := e.Start(ctx); err == nil {
		t.Error("Start() = nil error, want the recovered-run gate still closed until the operator acts")
	}
	if err := e.Discard(ctx, reconcileRunID); err != nil {
		t.Fatalf("Discard() error = %v, want a run whose workload is gone discardable", err)
	}
	if _, err := e.Start(ctx); err != nil {
		t.Errorf("Start() after Discard error = %v, want the console usable again", err)
	}
}

// A workload whose name does not derive from its own run-id label cannot be
// stopped by this console: Stop addresses the object as
// prove.WorkloadName(runID) and prove.Client.Delete treats a missing object as
// success, so adopting it would hand the operator a Stop button that reports
// success while deleting nothing. That is not hypothetical -- prove's
// runIDLabelKey is a literal independent of prove.Labels' own key, and drift
// between them empties RunID on every discovered object rather than erroring.
//
// Reporting it and leaving it alone is the honest outcome. Deleting it is the
// one thing reconciliation may never do.
func TestReconcileNeverDeletesAWorkloadItCannotAdopt(t *testing.T) {
	ctx := context.Background()
	const name = "prove-somebody-elses-workload"
	cs := fake.NewSimpleClientset(namedOwnedJob("", name))
	e, _, client := restartedEngine(t, cs, nil)

	if err := e.ReconcileWorkloads(ctx, client); err != nil {
		t.Fatalf("ReconcileWorkloads() error = %v", err)
	}

	if id, ok := e.CurrentID(); ok {
		t.Errorf("CurrentID() = %q, want nothing adopted from a workload Stop could not address", id)
	}
	if _, err := cs.BatchV1().Jobs(prove.Namespace).Get(ctx, name, metav1.GetOptions{}); err != nil {
		t.Fatalf("workload Get() = %v, want it left exactly where it was", err)
	}
}

// A failed list is not evidence of absence. Treating it as one would finish a
// run at StateDone while its workload keeps holding GPUs -- the single worst
// outcome available to this function, and the reason every decision below the
// list is skipped entirely when the list itself did not answer.
func TestReconcileLeavesTheRunAloneWhenListingFails(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset(ownedJob(reconcileRunID))
	cs.PrependReactor("list", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(batchv1.Resource("jobs"), "", errors.New("no access"))
	})
	e, _, client := restartedEngine(t, cs, activeRecord())

	if err := e.ReconcileWorkloads(ctx, client); err == nil {
		t.Fatal("ReconcileWorkloads() error = nil, want the listing failure surfaced to main")
	}

	got := e.Current()
	if got == nil || got.State != StateActive {
		t.Fatalf("Current() = %+v, want the run left at %q", got, StateActive)
	}
}

// Reconciliation runs on every startup, including on a laptop where
// rest.InClusterConfig fails and main still constructs a prove.Client around
// a nil kube. Every other method on that client dereferences it immediately
// and panics (prove.Client.Ready's own contract), so this must check Ready
// first -- a startup path that panics is worse than one that cannot reconcile.
func TestReconcileWithoutALiveClusterIsANoOp(t *testing.T) {
	ctx := context.Background()
	e, _, _ := restartedEngine(t, fake.NewSimpleClientset(), activeRecord())

	if err := e.ReconcileWorkloads(ctx, prove.NewClient(nil)); err != nil {
		t.Fatalf("ReconcileWorkloads(not ready) error = %v, want a clean no-op", err)
	}
	if err := e.ReconcileWorkloads(ctx, nil); err != nil {
		t.Fatalf("ReconcileWorkloads(nil) error = %v, want a clean no-op", err)
	}

	got := e.Current()
	if got == nil || got.State != StateActive {
		t.Fatalf("Current() = %+v, want the run untouched at %q", got, StateActive)
	}
}

// An adopted run reached e.current without Recover's own bootstrap publish --
// there was no record for Recover to find, so it published nothing. The SPA's
// only source of truth is the stream (web/src/components/Wizard.tsx replays
// it), so without an event here the console renders an idle wizard while a
// workload holds GPUs: exactly the state adoption exists to prevent.
//
// The wording is not free either: "run active" is what finish() publishes for
// a live run reaching StateActive, and publishRecoveryBootstrap deliberately
// reuses it so a recovered run resolves through the identical branch. A third
// spelling here would be a third branch for the SPA to grow.
func isAdoptionAnnouncement(ev bus.Event) bool {
	return ev.Kind == bus.KindPhase && ev.RunID == orphanRunID &&
		ev.Phase == string(PhaseProve) && ev.Message == "run "+string(StateActive)
}

func TestReconcileAdoptionAnnouncesTheRunOnTheStream(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset(ownedJob(orphanRunID))
	e, b, client := restartedEngine(t, cs, nil)

	if err := e.ReconcileWorkloads(ctx, client); err != nil {
		t.Fatalf("ReconcileWorkloads() error = %v", err)
	}

	events := b.Replay(0)
	var announced bool
	for _, ev := range events {
		if isAdoptionAnnouncement(ev) {
			announced = true
		}
	}
	if !announced {
		t.Fatalf("no `run active` phase event for the adopted run on the stream; events = %+v", events)
	}
}

// Adoption installs a run record. When one already exists, that record is the
// operator's -- it may be a recovered run they have not seen yet, and
// replacing it would erase the only thing telling them what happened. So a
// second workload is reported and left alone, never adopted over the top of
// the current run and never deleted.
func TestReconcileNeverReplacesAnExistingRunRecord(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset(ownedJob(reconcileRunID), ownedJob(orphanRunID))
	e, _, client := restartedEngine(t, cs, activeRecord())

	if err := e.ReconcileWorkloads(ctx, client); err != nil {
		t.Fatalf("ReconcileWorkloads() error = %v", err)
	}

	got := e.Current()
	if got == nil || got.ID != reconcileRunID || got.State != StateActive {
		t.Fatalf("Current() = %+v, want the recovered run %s still installed", got, reconcileRunID)
	}
	if _, err := cs.BatchV1().Jobs(prove.Namespace).
		Get(ctx, prove.WorkloadName(orphanRunID), metav1.GetOptions{}); err != nil {
		t.Errorf("orphan workload Get() = %v, want it reported and left in place", err)
	}
}

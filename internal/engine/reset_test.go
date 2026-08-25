package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/prove"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// fakeTeardown is the cluster-removal half of Reset, recorded rather than
// performed. It returns the outcomes it is told to and remembers what it was
// asked to remove, which is what the ordering and precondition tests below
// assert on.
type fakeTeardown struct {
	releases []ResidueItem
	// gotComponents is what Releases was handed, in the order it was handed
	// them -- the real ordering decision lives in internal/teardown and is
	// tested there; what matters here is that Reset passes the run's own
	// component rows through untouched.
	gotComponents []ComponentState
	releasesRan   bool
	// onReleases runs inside Releases, standing in for an operator
	// canceling while an uninstall is in flight.
	onReleases func()
	// cmdCtxErr and cancelCtxErr are each context's state immediately after
	// onReleases -- i.e. what the teardown would see mid-command.
	cmdCtxErr    error
	cancelCtxErr error
}

func (f *fakeTeardown) Releases(ctx, cancelCtx context.Context, comps []ComponentState, _ Ownership,
	emit func(ResidueItem)) []ResidueItem {

	f.releasesRan = true
	if f.onReleases != nil {
		f.onReleases()
		f.cmdCtxErr, f.cancelCtxErr = ctx.Err(), cancelCtx.Err()
	}
	f.gotComponents = comps
	for _, it := range f.releases {
		emit(it)
	}
	return f.releases
}

// unstoppableWorkload is a real prove.Client over a clientset that refuses
// the delete. A reactor rather than a hand-rolled double: EnsureAbsent's
// contract is the thing Reset leans on, so the test exercises the real
// implementation of it and only fakes the API server underneath.
func unstoppableWorkload() *prove.Client {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("etcdserver: request timed out")
	})
	return prove.NewClient(cs)
}

// resettableRun installs a run in the shape Reset accepts: terminal, with
// components recorded and ownership evidence for them. Built directly
// rather than driven through Start, because what is under test is Reset's
// own guards and goroutine, not how the run reached its state.
func resettableRun(t *testing.T, e *Engine, state State) *Run {
	t.Helper()
	now := time.Now().UTC()
	r := &Run{
		// A real-shaped ID: Recover's validateLoaded requires 16 hex
		// characters, and TestRecoverTreatsAnInterruptedTeardownAsIncomplete
		// reads this run back through it.
		ID:        "0123456789abcdef",
		State:     state,
		Phase:     PhaseApply,
		Decisions: map[string]string{},
		Artifacts: map[string][]byte{},
		StartedAt: now,
		UpdatedAt: now,
		Components: []ComponentState{
			{Name: "cert-manager", Namespace: "cert-manager", Index: 1, Total: 2, Status: "installed"},
			{Name: "gpu-operator", Namespace: "gpu-operator", Index: 2, Total: 2, Status: "installed"},
		},
		// One of each, because the two are reported differently: a namespace
		// that predates the install is never this console's to touch, and one
		// the run created is what the operator may now want gone.
		Ownership: Ownership{Namespaces: []NamespaceRef{
			{Name: "cert-manager", Existed: true},
			{Name: "gpu-operator"},
		}},
	}
	e.mu.Lock()
	e.current = r
	e.mu.Unlock()
	if err := e.store.Save(context.Background(), r.Clone()); err != nil {
		t.Fatalf("seeding the store: %v", err)
	}
	return r
}

// newResetEngine wires the two dependencies Reset requires, both of which
// are nil by default and both of which Reset refuses to run without.
func newResetEngine(t *testing.T, td Teardown) *Engine {
	t.Helper()
	e := newTestEngine(t)
	e.SetProveClient(prove.NewClient(fake.NewSimpleClientset()))
	e.SetTeardown(td)
	return e
}

// resetAndWait calls Reset and blocks until its goroutine has finished.
// Reset is backgrounded like Start, so every assertion about what it did
// has to wait for the operation, not for the call.
func resetAndWait(t *testing.T, e *Engine, runID string) *Run {
	t.Helper()
	if err := e.Reset(context.Background(), runID); err != nil {
		t.Fatalf("Reset() error = %v", err)
	}
	e.mu.Lock()
	done := e.done
	e.mu.Unlock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the teardown goroutine")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current == nil {
		return nil
	}
	return e.current.Clone()
}

func removedRelease(name, namespace string) ResidueItem {
	return ResidueItem{Kind: KindRelease, Name: name, Namespace: namespace, Removed: true}
}

// A run with a goroutine driving it is not a teardown candidate: Reset
// would race the very step that is still installing things.
func TestResetRejectsALiveRun(t *testing.T) {
	e := newResetEngine(t, &fakeTeardown{})
	for _, state := range []State{StateRunning, StateAwaitingDecision, StateResetting} {
		t.Run(string(state), func(t *testing.T) {
			run := resettableRun(t, e, state)
			err := e.Reset(context.Background(), run.ID)
			if err == nil {
				t.Fatal("Reset() error = nil, want a conflict")
			}
			if !strings.Contains(err.Error(), "live") {
				t.Errorf("Reset() error = %q, want it to say the run is live", err)
			}
		})
	}
}

// A Reset that reported success against a run which installed nothing would
// tell an operator their cluster had been cleaned when it was never touched.
func TestResetRejectsARunThatInstalledNothing(t *testing.T) {
	e := newResetEngine(t, &fakeTeardown{})
	run := resettableRun(t, e, StateDone)
	e.mu.Lock()
	e.current.Components = nil
	e.mu.Unlock()

	err := e.Reset(context.Background(), run.ID)
	if err == nil {
		t.Fatal("Reset() error = nil, want a conflict")
	}
	if !strings.Contains(err.Error(), "installed nothing") {
		t.Errorf("Reset() error = %q, want it to say the run installed nothing", err)
	}
}

// Step 1 is a hard precondition. Uninstalling kai-scheduler and the GPU
// operator out from under a gang that still holds devices is the failure
// this ordering exists to prevent, so a workload that cannot be confirmed
// gone must stop the teardown before a single release is touched.
func TestResetRequiresTheConfirmedWorkloadStop(t *testing.T) {
	td := &fakeTeardown{releases: []ResidueItem{removedRelease("gpu-operator", "gpu-operator")}}
	e := newResetEngine(t, td)
	e.SetProveClient(unstoppableWorkload())
	run := resettableRun(t, e, StateActive)

	got := resetAndWait(t, e, run.ID)

	if td.releasesRan {
		t.Fatal("uninstalled releases after failing to confirm the workload was stopped")
	}
	if got.State != StateFailed {
		t.Errorf("State = %q, want %q", got.State, StateFailed)
	}
	if !hasIncompleteTeardown(got) {
		t.Error("the incomplete-teardown guard is not set -- the workload may still be holding GPUs")
	}
	if !strings.Contains(got.Err, "workload") {
		t.Errorf("Err = %q, want it to name the workload as the reason", got.Err)
	}
}

// Namespaces are the operator's to remove, not this console's. Reset
// uninstalls the releases it can prove it created and then REPORTS every
// namespace the ownership snapshot recorded -- it deletes none of them.
//
// Whoever applied the bundle owns the cleanup of what it applied, and this
// console is the bash deployer. Uninstall is best effort about COMPLETENESS
// but not about DESTRUCTIVENESS: a namespace left behind is an annoyance the
// operator clears with one command, while deleting one that turned out to
// hold something is unrecoverable. So the console names what it left and lets
// the operator act, which is also why the discovery-driven emptiness walk
// that used to decide this is gone.
func TestResetReportsEveryNamespaceAndDeletesNone(t *testing.T) {
	td := &fakeTeardown{releases: []ResidueItem{removedRelease("gpu-operator", "gpu-operator")}}
	e := newResetEngine(t, td)
	run := resettableRun(t, e, StateActive)

	got := resetAndWait(t, e, run.ID)

	if n := got.Residue.Removed(KindNamespace); n != 0 {
		t.Errorf("Reset removed %d namespaces, want 0 -- namespaces are the operator's to delete", n)
	}
	byName := map[string]ResidueItem{}
	for _, it := range got.Residue.Items {
		if it.Kind == KindNamespace {
			byName[it.Name] = it
		}
	}
	if len(byName) != 2 {
		t.Fatalf("reported %d namespaces, want the 2 the snapshot recorded: %+v", len(byName), got.Residue.Items)
	}
	for name, it := range byName {
		if it.Skip == "" {
			t.Errorf("namespace %s carries no note -- an orphan the operator is never told about is one they cannot act on", name)
		}
	}
	// The two are not interchangeable: one the run created and the operator
	// may now want gone, one that predates the install and is none of our
	// business. The note has to tell them apart or it cannot be acted on.
	if byName["gpu-operator"].Skip == byName["cert-manager"].Skip {
		t.Errorf("a namespace this run created reads identically to one that predates it: %q",
			byName["gpu-operator"].Skip)
	}
}

// The namespace AICR's snapshot agent ran in is residue too, and it is the one
// namespace the ownership snapshot cannot describe: recipeNamespaces builds
// that set from recipe.json's components and the agent namespace is not one of
// them. A run that reached Discover and stopped there installed nothing, so
// this is the ONLY thing it left behind -- and before this it was reported
// nowhere.
func TestNamespaceResidueReportsTheAgentNamespaceThisRunCreated(t *testing.T) {
	items := namespaceResidue(Ownership{}, AgentNamespace{
		Name: "aicrme", UID: "u-1", Created: true,
	})

	if len(items) != 1 {
		t.Fatalf("reported %d namespaces, want the agent namespace: %+v", len(items), items)
	}
	if items[0].Name != "aicrme" || items[0].Kind != KindNamespace {
		t.Errorf("item = %+v, want the aicrme namespace", items[0])
	}
	if !items[0].Created {
		t.Error("Created is false -- the console shows the cleanup command only for namespaces it made")
	}
	if items[0].Removed {
		t.Error("the agent namespace was removed; namespaces are reported, never reclaimed")
	}
}

// A namespace that predates the install is not residue. It is somebody else's
// namespace that a Job briefly ran in, and reporting it would send an operator
// to delete something this console never created.
func TestNamespaceResidueOmitsAnAgentNamespaceThisRunDidNotCreate(t *testing.T) {
	items := namespaceResidue(Ownership{}, AgentNamespace{Name: "kube-system", UID: "u-1"})

	if len(items) != 0 {
		t.Errorf("reported %+v for a namespace this run did not create", items)
	}
}

// Nothing stops an operator pointing AICRME_NAMESPACE at a namespace the
// recipe also installs into. Two rows for one namespace would double it in the
// counts resetSummary reports, which is the number an operator reads to tell a
// clean teardown from a partial one.
func TestNamespaceResidueDoesNotReportOneNamespaceTwice(t *testing.T) {
	own := Ownership{Namespaces: []NamespaceRef{{Name: "aicrme"}}}
	items := namespaceResidue(own, AgentNamespace{Name: "aicrme", UID: "u-1", Created: true})

	if len(items) != 1 {
		t.Fatalf("reported %d rows for one namespace: %+v", len(items), items)
	}
}

// The clean path end to end: the workload is stopped, the releases are
// uninstalled, the run ends at StateDone, and the persisted record is gone
// so the console is free and a restart recovers nothing.
func TestResetUninstallsInReverseAndDeletesTheRecordWhenClean(t *testing.T) {
	td := &fakeTeardown{
		releases: []ResidueItem{
			removedRelease("gpu-operator", "gpu-operator"),
			removedRelease("cert-manager", "cert-manager"),
		},
	}
	e := newResetEngine(t, td)
	run := resettableRun(t, e, StateActive)

	got := resetAndWait(t, e, run.ID)

	if !td.releasesRan {
		t.Fatal("the release teardown never ran")
	}
	if got.State != StateDone {
		t.Errorf("State = %q, want %q", got.State, StateDone)
	}
	if hasIncompleteTeardown(got) {
		t.Error("the incomplete-teardown guard is set on a clean reset")
	}
	// Releases removed, namespaces only reported -- see
	// TestResetReportsEveryNamespaceAndDeletesNone for why that asymmetry is
	// the design rather than an oversight.
	if got.Residue.Removed(KindRelease) != 2 || got.Residue.Removed(KindNamespace) != 0 {
		t.Errorf("Residue = %+v, want 2 releases removed and 0 namespaces removed", got.Residue)
	}
	// The run's own component rows reach the teardown untouched -- the
	// reverse ordering itself is internal/teardown's decision and is tested
	// there.
	if len(td.gotComponents) != 2 {
		t.Errorf("teardown got %d components, want the run's 2", len(td.gotComponents))
	}
	// Only the namespaces the ownership snapshot recorded are ever reported:
	// the inventory is bounded by what Apply was about to install into, so a
	// namespace the console never touched is never named at all.
	if n := len(got.Residue.Items) - got.Residue.Removed(KindRelease); n != 2 {
		t.Errorf("reported %d namespaces, want the 2 from the ownership snapshot: %+v", n, got.Residue.Items)
	}
	if _, err := e.store.LoadCurrent(context.Background()); err == nil {
		t.Error("the persisted record survived a clean reset -- a restart would recover an already-torn-down run")
	}
}

// The record is the only inventory of what is still installed, so a failed
// teardown must keep it and must set the guard that stops anything else
// touching the cluster.
func TestResetKeepsTheRecordAndSetsTheGuardOnFailure(t *testing.T) {
	td := &fakeTeardown{releases: []ResidueItem{
		removedRelease("gpu-operator", "gpu-operator"),
		{Kind: KindRelease, Name: "cert-manager", Namespace: "cert-manager", Err: "release is in a failed state"},
	}}
	e := newResetEngine(t, td)
	run := resettableRun(t, e, StateDone)

	got := resetAndWait(t, e, run.ID)

	if got.State != StateFailed {
		t.Errorf("State = %q, want %q", got.State, StateFailed)
	}
	if !hasIncompleteTeardown(got) {
		t.Fatal("the incomplete-teardown guard is not set after a failed uninstall")
	}
	if _, err := e.store.LoadCurrent(context.Background()); err != nil {
		t.Errorf("the persisted record was deleted after a failed reset: %v -- it is the only list of what is still installed", err)
	}
	// The inventory names the failure and the success alike, so an operator
	// can see what is left without reading the timeline.
	if got.Residue.Considered(KindRelease) != 2 || got.Residue.Removed(KindRelease) != 1 {
		t.Errorf("Residue = %+v, want 2 considered and 1 removed", got.Residue)
	}
	if !strings.Contains(got.Err, "1 of 2") {
		t.Errorf("Err = %q, want counts rather than a bare verdict", got.Err)
	}
}

// A skip is not a failure. Leaving alone what this run did not create is
// the designed outcome, and treating it as incomplete would wedge the
// console after every entirely successful reset of a cluster that happened
// to have a pre-existing release in it.
func TestResetTreatsAnOwnershipSkipAsCleanRatherThanIncomplete(t *testing.T) {
	td := &fakeTeardown{releases: []ResidueItem{
		removedRelease("gpu-operator", "gpu-operator"),
		{Kind: KindRelease, Name: "cert-manager", Namespace: "cert-manager",
			Skip: "this release already existed before the install"},
	}}
	e := newResetEngine(t, td)
	run := resettableRun(t, e, StateDone)

	got := resetAndWait(t, e, run.ID)

	if got.State != StateDone {
		t.Errorf("State = %q, want %q -- a skip is an ownership answer, not a failure", got.State, StateDone)
	}
	if hasIncompleteTeardown(got) {
		t.Error("the incomplete-teardown guard is set for a run that only skipped what it did not create")
	}
	if !strings.Contains(got.Residue.Items[1].Skip, "already existed") {
		t.Errorf("Residue items = %+v, want the skipped release named", got.Residue.Items)
	}
}

// Stop's M2, one state over: a recovered run that has now been fully torn
// down must not keep 409ing Start on "a recovered run is waiting for retry
// or discard".
func TestResetClearsRecoveredPendingWhenClean(t *testing.T) {
	td := &fakeTeardown{releases: []ResidueItem{removedRelease("gpu-operator", "gpu-operator")}}
	e := newResetEngine(t, td)
	run := resettableRun(t, e, StateFailed)
	e.mu.Lock()
	e.recoveredPending = true
	e.mu.Unlock()

	resetAndWait(t, e, run.ID)

	e.mu.Lock()
	pending := e.recoveredPending
	e.mu.Unlock()
	if pending {
		t.Error("recoveredPending is still set after a clean reset -- Start would keep refusing")
	}
}

// The three hazards an ordinary StateFailed leaves open. Each is a separate
// test because each is a separate call site that must learn to refuse.
func TestIncompleteTeardownBlocksStart(t *testing.T) {
	e := newResetEngine(t, &fakeTeardown{})
	seedIncompleteTeardown(t, e)

	_, err := e.Start(context.Background())
	if err == nil {
		t.Fatal("Start() error = nil, want a conflict -- a new run would install over a half-removed cluster")
	}
	if !strings.Contains(err.Error(), "reset") {
		t.Errorf("Start() error = %q, want it to name reset as the remedy", err)
	}
}

func TestIncompleteTeardownBlocksRetry(t *testing.T) {
	e := newResetEngine(t, &fakeTeardown{})
	run := seedIncompleteTeardown(t, e)

	_, err := e.Retry(context.Background(), run.ID)
	if err == nil {
		t.Fatal("Retry() error = nil, want a conflict -- Retry would re-run Apply over a half-removed cluster")
	}
	if !strings.Contains(err.Error(), "reset") {
		t.Errorf("Retry() error = %q, want it to name reset as the remedy", err)
	}
}

func TestIncompleteTeardownBlocksDiscard(t *testing.T) {
	e := newResetEngine(t, &fakeTeardown{})
	run := seedIncompleteTeardown(t, e)

	err := e.Discard(context.Background(), run.ID)
	if err == nil {
		t.Fatal("Discard() error = nil, want a conflict -- discarding destroys the only list of what is still installed")
	}
	if !strings.Contains(err.Error(), "reset") {
		t.Errorf("Discard() error = %q, want it to name reset as the remedy", err)
	}
}

// The remedy must stay reachable. Blocking every operation that could
// resolve the residue -- including the one that performs the removal --
// would recreate the operator dead end the guard exists to prevent, exactly
// as stoppable's third arm reasons about Ruling 12.
func TestIncompleteTeardownStillAllowsReset(t *testing.T) {
	td := &fakeTeardown{releases: []ResidueItem{removedRelease("gpu-operator", "gpu-operator")}}
	e := newResetEngine(t, td)
	run := seedIncompleteTeardown(t, e)

	got := resetAndWait(t, e, run.ID)

	if got.State != StateDone {
		t.Errorf("State = %q, want %q -- a second reset must be able to resolve the first one's residue", got.State, StateDone)
	}
	if hasIncompleteTeardown(got) {
		t.Error("the guard survived a second, successful reset")
	}
}

// A restart during a teardown leaves residue nobody recorded: the goroutine
// that would have written it died with the pod. Marking it incomplete is
// both the honest answer and the fail-closed one.
func TestRecoverTreatsAnInterruptedTeardownAsIncomplete(t *testing.T) {
	store := NewMemoryStore()
	// A step reporting PhaseBundle is Recover's own configuration
	// requirement (it is the rewind target), not anything this test cares
	// about -- recover_test.go's engines all carry one for the same reason.
	bundleStep := func() Step { return &fakeStep{phase: PhaseBundle} }
	seed := New(bus.New(64), store, bundleStep())
	run := resettableRun(t, seed, StateResetting)

	e := New(bus.New(64), store, bundleStep())
	if err := e.Recover(context.Background()); err != nil {
		t.Fatalf("Recover() error = %v", err)
	}

	got, err := e.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.State != StateFailed {
		t.Errorf("State = %q, want %q", got.State, StateFailed)
	}
	if !hasIncompleteTeardown(got) {
		t.Error("a teardown interrupted by a restart is not marked incomplete -- Start, Retry and Discard would all be offered over an unknown cluster")
	}
}

// seedIncompleteTeardown installs a run in the state a failed Reset leaves:
// StateFailed, with the guard set and an inventory naming what is left.
func seedIncompleteTeardown(t *testing.T, e *Engine) *Run {
	t.Helper()
	run := resettableRun(t, e, StateFailed)
	e.mu.Lock()
	e.current.Residue = Residue{
		Incomplete: true,
		Items: []ResidueItem{
			{Kind: KindRelease, Name: "cert-manager", Namespace: "cert-manager", Err: "release is in a failed state"},
		},
	}
	e.mu.Unlock()
	return run
}

// The two contexts Reset hands the teardown must be DIFFERENT contexts, and
// only the second may be cancellable. internal/applier's BashExec SIGTERMs
// the whole process group the instant its context is done, so running helm
// under the cancellable one interrupts an uninstall mid-flight and strands
// the release half-removed -- which is the exact residue Reset exists to
// eliminate, produced by the operation meant to prevent it.
//
// internal/teardown is written to that contract and tested against it, but
// nothing there can see what Reset actually passes: this is the only place
// the two ends meet.
func TestResetDoesNotCancelTheCommandContext(t *testing.T) {
	td := &fakeTeardown{releases: []ResidueItem{removedRelease("gpu-operator", "gpu-operator")}}
	e := newResetEngine(t, td)
	run := resettableRun(t, e, StateDone)
	// Cancel from inside the teardown, standing in for an operator
	// canceling while an uninstall is in flight.
	td.onReleases = func() {
		e.mu.Lock()
		cancel := e.cancel
		e.mu.Unlock()
		cancel()
	}

	resetAndWait(t, e, run.ID)

	if td.cancelCtxErr == nil {
		t.Fatal("the cancellation context was not canceled -- this test proves nothing")
	}
	if td.cmdCtxErr != nil {
		t.Errorf("the command context was canceled too (%v) -- helm would be SIGTERMed mid-uninstall", td.cmdCtxErr)
	}
}

// The guard that matters most. Reset acts on release names, and a release
// name only means something in the cluster it was installed into --
// uninstalling "gpu-operator" against the wrong one removes a stranger's
// working install and reports success.
func TestResetRefusesARecordFromADifferentCluster(t *testing.T) {
	const (
		recordUID    = "11111111-2222-3333-4444-555555555555"
		connectedUID = "99999999-8888-7777-6666-555555555555"
	)
	td := &fakeTeardown{}
	e := newResetEngine(t, td)
	run := resettableRun(t, e, StateDone)
	e.mu.Lock()
	e.current.ClusterUID = recordUID
	e.mu.Unlock()
	e.SetClusterUID(connectedUID)

	err := e.Reset(context.Background(), run.ID)
	if !errors.Is(err, ErrClusterMismatch) {
		t.Fatalf("Reset() error = %v, want ErrClusterMismatch", err)
	}
	if !strings.Contains(err.Error(), recordUID) || !strings.Contains(err.Error(), connectedUID) {
		t.Errorf("the error names neither UID: %v", err)
	}
	if td.releasesRan {
		t.Error("the teardown ran against the wrong cluster")
	}
}

func TestResetProceedsForTheConnectedCluster(t *testing.T) {
	const uid = "11111111-2222-3333-4444-555555555555"
	e := newResetEngine(t, &fakeTeardown{})
	run := resettableRun(t, e, StateDone)
	e.mu.Lock()
	e.current.ClusterUID = uid
	e.mu.Unlock()
	e.SetClusterUID(uid)

	if got := resetAndWait(t, e, run.ID); got != nil && got.State == StateFailed {
		t.Errorf("Reset() left the run failed for its own cluster: %s", got.Err)
	}
}

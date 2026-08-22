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
	releases   []ResidueItem
	namespaces []ResidueItem
	// gotComponents is what Releases was handed, in the order it was handed
	// them -- the real ordering decision lives in internal/teardown and is
	// tested there; what matters here is that Reset passes the run's own
	// component rows through untouched.
	gotComponents []ComponentState
	gotNamespaces []string
	releasesRan   bool
	namespacesRan bool
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

func (f *fakeTeardown) Namespaces(_ context.Context, names []string, _ Ownership,
	emit func(ResidueItem)) []ResidueItem {

	f.namespacesRan = true
	f.gotNamespaces = names
	for _, it := range f.namespaces {
		emit(it)
	}
	return f.namespaces
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
		Ownership: Ownership{Namespaces: []NamespaceRef{
			{Name: "cert-manager", CreatedUID: "uid-1"},
			{Name: "gpu-operator", CreatedUID: "uid-2"},
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

// The clean path end to end: the workload is stopped, both halves of the
// teardown run, the run ends at StateDone, and the persisted record is gone
// so the console is free and a restart recovers nothing.
func TestResetUninstallsInReverseAndDeletesTheRecordWhenClean(t *testing.T) {
	td := &fakeTeardown{
		releases: []ResidueItem{
			removedRelease("gpu-operator", "gpu-operator"),
			removedRelease("cert-manager", "cert-manager"),
		},
		namespaces: []ResidueItem{
			{Kind: KindNamespace, Name: "gpu-operator", Removed: true},
			{Kind: KindNamespace, Name: "cert-manager", Removed: true},
		},
	}
	e := newResetEngine(t, td)
	run := resettableRun(t, e, StateActive)

	got := resetAndWait(t, e, run.ID)

	if !td.releasesRan || !td.namespacesRan {
		t.Fatalf("releases ran = %v, namespaces ran = %v, want both", td.releasesRan, td.namespacesRan)
	}
	if got.State != StateDone {
		t.Errorf("State = %q, want %q", got.State, StateDone)
	}
	if hasIncompleteTeardown(got) {
		t.Error("the incomplete-teardown guard is set on a clean reset")
	}
	if got.Residue.Removed(KindRelease) != 2 || got.Residue.Removed(KindNamespace) != 2 {
		t.Errorf("Residue = %+v, want 2 releases and 2 namespaces removed", got.Residue)
	}
	// The run's own component rows reach the teardown untouched -- the
	// reverse ordering itself is internal/teardown's decision and is tested
	// there.
	if len(td.gotComponents) != 2 {
		t.Errorf("teardown got %d components, want the run's 2", len(td.gotComponents))
	}
	// Only the namespaces the ownership snapshot recorded are ever
	// candidates.
	if len(td.gotNamespaces) != 2 {
		t.Errorf("teardown got namespaces %v, want the 2 from the ownership snapshot", td.gotNamespaces)
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

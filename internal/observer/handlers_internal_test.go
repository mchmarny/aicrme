package observer

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"

	"github.com/mchmarny/aicrme/internal/bus"
)

// These tests drive the handlers directly rather than through an informer.
// Two properties are unreachable from observer_test.go's black box: onNode's
// first-sighting guard (an informer always delivers an Add first, which seeds
// the baseline and makes the guard unreachable) and onDelete's effect (the
// only thing it changes is map occupancy, which publishes nothing).

// newTestObserver returns an Observer whose handlers can be called directly,
// plus a subscriber holding everything it publishes. bus.Publish fans out
// synchronously under its own lock, so by the time a handler returns any
// event it produced is already queued -- no test here needs to wait.
//
// o.scoped.entries["gpu-operator"] is pre-seeded with a bare *factoryEntry
// (Important B, Task 5 fix round 2): onPodChange/seedPodBaseline now gate
// every write on o.scoped.withNamespaceLive, which checks namespace
// presence in o.scoped.entries -- a real precondition in production (that
// map is only ever populated by a namespace's own running informer), so a
// direct handler call here needs the same fact recorded, without paying for
// a real informer this file's whole point is to avoid. stop is a real
// channel, not the zero value (Minor finding, Task 5 fix round 3): a nil
// channel is fine for every test today (nothing here calls
// scoped.stop()/reconcile(), the only callers that ever close(e.stop)), but
// a bare &factoryEntry{} is a panic waiting for whichever future test does
// -- close(nil channel) panics -- and make(chan struct{}) costs nothing to
// avoid it now.
func newTestObserver(t *testing.T) (*Observer, <-chan bus.Event) {
	t.Helper()
	b := bus.New(64)
	sub, unsub := b.Subscribe(0)
	t.Cleanup(unsub)
	// A nil client is fine: Start is never called, only the handlers are.
	o := New(nil, b, func() RunScope {
		return RunScope{RunID: "run-1", Namespaces: map[string]struct{}{"gpu-operator": {}}}
	})
	o.scoped.entries["gpu-operator"] = &factoryEntry{stop: make(chan struct{})}
	// No debounce window here. Every test using this helper asserts that an
	// event WAS published, and the production window would make each of them
	// wait two seconds; a zero delay publishes inline. The window itself is
	// covered directly in debounce_test.go.
	o.debounce = newDebouncer(0)
	return o, sub
}

func assertNoEvent(t *testing.T, sub <-chan bus.Event) {
	t.Helper()
	select {
	case e := <-sub:
		t.Fatalf("published %+v, want nothing", e)
	default:
	}
}

// decodeClusterData reads the next published event and decodes its Data as
// ClusterData, failing the test if there is no event or Data does not decode.
// This is the check no pre-existing test performs: every observer_test.go
// and handlers_internal_test.go assertion up to this point reads only
// e.Message, so a handler could attach the wrong UID, an unconditional
// Severity, or an inverted Resolved and every one of those tests would still
// pass -- the message text those tests pin never depended on ClusterData.
func decodeClusterData(t *testing.T, sub <-chan bus.Event) bus.ClusterData {
	t.Helper()
	select {
	case e := <-sub:
		var cd bus.ClusterData
		if err := json.Unmarshal(e.Data, &cd); err != nil {
			t.Fatalf("Unmarshal(ClusterData) error = %v, raw = %s", err, e.Data)
		}
		return cd
	default:
		t.Fatal("no event published")
		return bus.ClusterData{}
	}
}

func testDaemonSet(uid types.UID, ready, desired int32) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "gpu-operator", Name: "nvidia-driver-daemonset", UID: uid},
		Status: appsv1.DaemonSetStatus{
			NumberReady:            ready,
			DesiredNumberScheduled: desired,
		},
	}
}

func testDeployment(uid types.UID) *appsv1.Deployment {
	replicas := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "gpu-operator", Name: "nim-service", UID: uid},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 1},
	}
}

func testNode(name string, uid types.UID, gpus string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: uid},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{gpuResource: resource.MustParse(gpus)},
		},
	}
}

// TestOnNodeFirstSightingRecordsBaselineWithoutEmitting pins onNode's
// `if !had { return }` guard. The black-box test that names this property
// cannot reach the guard: an informer's initial list delivers an Add, onAdd
// seeds gpuQty from it, so had is already true and the prev.Cmp(cur) == 0
// early return fires first -- that test exercises Cmp, not the guard.
// Calling onNode with no prior onAdd is the only way in.
//
// The second half is what keeps the guard from being satisfiable by an
// unconditional return: a genuine transition off the recorded baseline must
// still narrate.
func TestOnNodeFirstSightingRecordsBaselineWithoutEmitting(t *testing.T) {
	o, sub := newTestObserver(t)

	o.onNode(testNode("gpu-node-2", "node-uid", "8"))

	assertNoEvent(t, sub)
	if len(o.gpuQty) != 1 {
		t.Fatalf("gpuQty = %v, want the first sighting recorded as a baseline", o.gpuQty)
	}

	o.onNode(testNode("gpu-node-2", "node-uid", "0"))

	select {
	case e := <-sub:
		if !strings.Contains(e.Message, "allocatable 8 → 0") {
			t.Errorf("Message = %q, want the 8 → 0 transition off the recorded baseline", e.Message)
		}
	default:
		t.Fatal("no event for a genuine transition off the recorded baseline")
	}
}

// TestOnDeleteReleasesTheCacheEntry pins what onDelete is actually for.
// Nothing observable over the bus changes when it is removed -- the reviewer
// gutted it to a no-op and the whole package stayed green -- because
// stateKey carries the object's UID, so a recreate always gets a fresh key
// and can never inherit a deleted object's state regardless. What onDelete
// buys is that o.workload and o.gpuQty release the entry instead of retaining
// one dead key per deleted object for the life of the process.
func TestOnDeleteReleasesTheCacheEntry(t *testing.T) {
	tests := []struct {
		name string
		obj  any
		size func(*Observer) int
	}{
		{
			name: "DaemonSet",
			obj:  testDaemonSet("ds-uid", 8, 8),
			size: func(o *Observer) int { return len(o.workload) },
		},
		{
			name: "Deployment",
			obj:  testDeployment("deploy-uid"),
			size: func(o *Observer) int { return len(o.workload) },
		},
		{
			name: "Node",
			obj:  testNode("gpu-node-1", "node-uid", "8"),
			size: func(o *Observer) int { return len(o.gpuQty) },
		},
		{
			// The tombstone client-go delivers when a watch gap meant the
			// final object was missed. Unwrapping it is the difference
			// between reclaiming the entry and leaking it exactly when the
			// watch was already unhealthy.
			name: "DeletedFinalStateUnknown tombstone",
			obj: cache.DeletedFinalStateUnknown{
				Key: "gpu-operator/nvidia-driver-daemonset",
				Obj: testDaemonSet("ds-uid", 8, 8),
			},
			size: func(o *Observer) int { return len(o.workload) },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o, _ := newTestObserver(t)

			seed := tc.obj
			if tomb, ok := seed.(cache.DeletedFinalStateUnknown); ok {
				seed = tomb.Obj
			}
			o.onAdd(seed)
			if got := tc.size(o); got != 1 {
				t.Fatalf("cache size after onAdd = %d, want 1", got)
			}

			o.onDelete(tc.obj)
			if got := tc.size(o); got != 0 {
				t.Errorf("cache size after onDelete = %d, want 0 -- the entry was retained", got)
			}
		})
	}
}

// TestDeleteRecreateCyclesDoNotAccumulate is the failure mode the entry
// release exists to prevent, stated as growth rather than as a single
// delete. Every recreate carries a new UID and therefore a new stateKey, so
// without onDelete the maps gain one permanently unreachable entry per cycle
// -- an operator retrying a stuck DaemonSet during a long Apply drives this.
func TestDeleteRecreateCyclesDoNotAccumulate(t *testing.T) {
	o, _ := newTestObserver(t)

	for i := range 20 {
		ds := testDaemonSet(types.UID(fmt.Sprintf("ds-uid-%d", i)), 8, 8)
		o.onAdd(ds)
		if got := len(o.workload); got != 1 {
			t.Fatalf("cycle %d: workload = %d entries, want 1", i, got)
		}
		o.onDelete(ds)
	}

	if got := len(o.workload); got != 0 {
		t.Errorf("workload retained %d entries after 20 delete/recreate cycles, want 0", got)
	}
}

// TestOnDaemonSetClusterDataFromTheObject pins the two rules the whole task
// exists to protect, at the one layer that had never checked them: the
// handler that actually builds ClusterData from a live *appsv1.DaemonSet.
// UID must come from ds.UID, not ds.Name -- testDaemonSet fixes Name to
// "nvidia-driver-daemonset" and gives every case a distinct UID precisely so
// a UID-from-Name mistake shows up as a mismatch here. Severity is checked
// against rolloutSeverityInfo (Ruling 4: RolloutProgress is never Warn) and
// Resolved against the ready>=desired threshold.
func TestOnDaemonSetClusterDataFromTheObject(t *testing.T) {
	tests := []struct {
		name           string
		ready, desired int32
		wantResolved   bool
	}{
		{name: "short of desired", ready: 3, desired: 8, wantResolved: false},
		{name: "at desired", ready: 8, desired: 8, wantResolved: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o, sub := newTestObserver(t)
			ds := testDaemonSet("ds-real-uid", tc.ready, tc.desired)

			o.onDaemonSet(ds)

			cd := decodeClusterData(t, sub)
			if cd.UID != "ds-real-uid" {
				t.Errorf("UID = %q, want the object's UID %q, not its Name %q", cd.UID, "ds-real-uid", ds.Name)
			}
			if cd.Ready != tc.ready || cd.Desired != tc.desired {
				t.Errorf("Ready/Desired = %d/%d, want %d/%d", cd.Ready, cd.Desired, tc.ready, tc.desired)
			}
			if cd.Severity != bus.SeverityInfo {
				t.Errorf("Severity = %v, want SeverityInfo: a readiness shortfall mid-rollout is not itself actionable", cd.Severity)
			}
			if cd.Resolved != tc.wantResolved {
				t.Errorf("Resolved = %v, want %v", cd.Resolved, tc.wantResolved)
			}
		})
	}
}

// TestOnDeploymentClusterDataFromTheObject is TestOnDaemonSetClusterDataFromTheObject's
// counterpart for onDeployment, constructed inline rather than through
// testDeployment (which fixes ready=desired=1) so both the short-of-desired
// and at-desired cases are reachable.
func TestOnDeploymentClusterDataFromTheObject(t *testing.T) {
	tests := []struct {
		name           string
		ready, desired int32
		wantResolved   bool
	}{
		{name: "short of desired", ready: 1, desired: 8, wantResolved: false},
		{name: "at desired", ready: 8, desired: 8, wantResolved: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o, sub := newTestObserver(t)
			desired := tc.desired
			d := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Namespace: "gpu-operator", Name: "nim-service", UID: "deploy-real-uid"},
				Spec:       appsv1.DeploymentSpec{Replicas: &desired},
				Status:     appsv1.DeploymentStatus{ReadyReplicas: tc.ready},
			}

			o.onDeployment(d)

			cd := decodeClusterData(t, sub)
			if cd.UID != "deploy-real-uid" {
				t.Errorf("UID = %q, want the object's UID %q, not its Name %q", cd.UID, "deploy-real-uid", d.Name)
			}
			if cd.Ready != tc.ready || cd.Desired != tc.desired {
				t.Errorf("Ready/Desired = %d/%d, want %d/%d", cd.Ready, cd.Desired, tc.ready, tc.desired)
			}
			if cd.Severity != bus.SeverityInfo {
				t.Errorf("Severity = %v, want SeverityInfo: a readiness shortfall mid-rollout is not itself actionable", cd.Severity)
			}
			if cd.Resolved != tc.wantResolved {
				t.Errorf("Resolved = %v, want %v", cd.Resolved, tc.wantResolved)
			}
		})
	}
}

// TestOnNodeClusterDataFromTheObject covers onNode's ClusterData, where --
// unlike the workload handlers -- Severity/Resolved genuinely do vary with
// direction: a capacity drop is the state worth watching (Warn, unresolved),
// a rise back to or above the prior value is the condition clearing (Info,
// resolved). testNode's name ("gpu-node-real") differs from its UID
// ("node-real-uid") for the same reason as the DaemonSet/Deployment cases.
func TestOnNodeClusterDataFromTheObject(t *testing.T) {
	tests := []struct {
		name              string
		prevGPUs, curGPUs string
		wantSeverity      bus.Severity
		wantResolved      bool
	}{
		{name: "capacity drop", prevGPUs: "8", curGPUs: "0", wantSeverity: bus.SeverityWarn, wantResolved: false},
		{name: "capacity rise", prevGPUs: "0", curGPUs: "8", wantSeverity: bus.SeverityInfo, wantResolved: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o, sub := newTestObserver(t)

			o.onNode(testNode("gpu-node-real", "node-real-uid", tc.prevGPUs))
			assertNoEvent(t, sub) // first sighting seeds the baseline, does not publish

			o.onNode(testNode("gpu-node-real", "node-real-uid", tc.curGPUs))

			cd := decodeClusterData(t, sub)
			if cd.UID != "node-real-uid" {
				t.Errorf("UID = %q, want the object's UID %q, not its Name", cd.UID, "node-real-uid")
			}
			if cd.Severity != tc.wantSeverity {
				t.Errorf("Severity = %v, want %v", cd.Severity, tc.wantSeverity)
			}
			if cd.Resolved != tc.wantResolved {
				t.Errorf("Resolved = %v, want %v", cd.Resolved, tc.wantResolved)
			}
		})
	}
}

// TestOnDeleteClearsTheConditionForTrackedResources is spec Section 4's "or
// is deleted" half of clearing: a resource deleted while its condition is
// still unresolved (an operator killing a stuck DaemonSet mid-rollout) must
// still clear the row, not leave the last unresolved condition pinned
// forever. Each case seeds the observer via onAdd, exactly as an informer's
// initial list or a prior Add would, then deletes it and checks the
// published ClusterData carries the object's own UID (not a zero value) and
// Reason, so a row can find the right (UID, Reason) entry to clear via
// Supersedes.
func TestOnDeleteClearsTheConditionForTrackedResources(t *testing.T) {
	t.Run("DaemonSet", func(t *testing.T) {
		o, sub := newTestObserver(t)
		ds := testDaemonSet("ds-real-uid", 3, 8)
		o.onAdd(ds)

		o.onDelete(ds)

		cd := decodeClusterData(t, sub)
		if cd.UID != "ds-real-uid" {
			t.Errorf("UID = %q, want %q", cd.UID, "ds-real-uid")
		}
		if cd.Reason != reasonRollout {
			t.Errorf("Reason = %q, want %q", cd.Reason, reasonRollout)
		}
		if !cd.Resolved {
			t.Error("Resolved = false, want true: deletion must clear the row")
		}
	})
	t.Run("Deployment", func(t *testing.T) {
		o, sub := newTestObserver(t)
		d := testDeployment("deploy-real-uid")
		o.onAdd(d)

		o.onDelete(d)

		cd := decodeClusterData(t, sub)
		if cd.UID != "deploy-real-uid" {
			t.Errorf("UID = %q, want %q", cd.UID, "deploy-real-uid")
		}
		if cd.Reason != reasonRollout {
			t.Errorf("Reason = %q, want %q", cd.Reason, reasonRollout)
		}
		if !cd.Resolved {
			t.Error("Resolved = false, want true: deletion must clear the row")
		}
	})
	t.Run("Node", func(t *testing.T) {
		o, sub := newTestObserver(t)
		n := testNode("gpu-node-real", "node-real-uid", "8")
		o.onAdd(n)

		o.onDelete(n)

		cd := decodeClusterData(t, sub)
		if cd.UID != "node-real-uid" {
			t.Errorf("UID = %q, want %q", cd.UID, "node-real-uid")
		}
		if cd.Reason != reasonGPUAllocatable {
			t.Errorf("Reason = %q, want %q", cd.Reason, reasonGPUAllocatable)
		}
		if !cd.Resolved {
			t.Error("Resolved = false, want true: deletion must clear the row")
		}
	})
}

// TestOnDeleteTombstoneMatchesDirectDelete pins that DeletedFinalStateUnknown
// is unwrapped before ClusterData is built, not just before the cache
// lookup. A tombstone is exactly the case where the cache would otherwise be
// the only record of what existed, so the published UID must come from the
// wrapped object, not be left zero.
func TestOnDeleteTombstoneMatchesDirectDelete(t *testing.T) {
	o, sub := newTestObserver(t)
	ds := testDaemonSet("ds-real-uid", 3, 8)
	o.onAdd(ds)

	o.onDelete(cache.DeletedFinalStateUnknown{
		Key: "gpu-operator/nvidia-driver-daemonset",
		Obj: ds,
	})

	cd := decodeClusterData(t, sub)
	if cd.UID != "ds-real-uid" {
		t.Errorf("UID = %q, want %q", cd.UID, "ds-real-uid")
	}
	if !cd.Resolved {
		t.Error("Resolved = false, want true")
	}
}

// TestOnDeleteOfAnUntrackedResourcePublishesNothing: no cache entry means no
// condition to clear. Publishing anyway would put a phantom entry on a row
// that never showed a condition in the first place -- ds here is never
// passed to onAdd or onDaemonSet, so the observer has no record of it.
func TestOnDeleteOfAnUntrackedResourcePublishesNothing(t *testing.T) {
	o, sub := newTestObserver(t)
	ds := testDaemonSet("ds-real-uid", 3, 8)

	o.onDelete(ds)

	assertNoEvent(t, sub)
}

// TestOnDeletePublishesWithoutHoldingTheLock is the ordering bite-proof: a
// publish from onDelete must happen after o.mu is released, matching the
// shape onDaemonSet/onDeployment/onNode already use, because
// o.bus.Publish takes bus's own lock and calling it while holding o.mu would
// nest the two locks in the one order 2b-i was deliberate about avoiding.
//
// bus.Bus.Publish never blocks on a slow subscriber (its fan-out send is
// select-with-default in bus.go), so there is no way to build a "blocking
// bus" in this package to observe the ordering that way, and Observer.bus is
// a concrete *bus.Bus field, not an interface, so it cannot be swapped for a
// fake. No existing test in this package has this shape either. Instead this
// asserts the ordering directly through the one hook publish() calls first:
// the RunScope provider func. That func calls o.mu.TryLock() on itself --
// TryLock is non-blocking and reports false if the mutex is already held by
// anyone, including the calling goroutine (sync.Mutex tracks lock state, not
// ownership), so it directly answers "is o.mu held right now" at the exact
// moment publish runs, without needing a second goroutine or a timeout to
// detect a hang.
func TestOnDeletePublishesWithoutHoldingTheLock(t *testing.T) {
	b := bus.New(64)
	sub, unsub := b.Subscribe(0)
	t.Cleanup(unsub)

	var scopeRan, lockWasFree bool
	var o *Observer
	o = New(nil, b, func() RunScope {
		scopeRan = true
		if lockWasFree = o.mu.TryLock(); lockWasFree {
			o.mu.Unlock()
		}
		return RunScope{RunID: "run-1", Namespaces: map[string]struct{}{"gpu-operator": {}}}
	})

	ds := testDaemonSet("ds-real-uid", 3, 8)
	o.onAdd(ds)

	o.onDelete(ds)

	if !scopeRan {
		t.Fatal("scope callback never ran -- onDelete did not publish at all")
	}
	if !lockWasFree {
		t.Error("o.mu.TryLock() failed from inside publish's scope callback: o.mu was still held when publish ran")
	}

	cd := decodeClusterData(t, sub)
	if !cd.Resolved {
		t.Error("Resolved = false, want true")
	}
}

// newAttributionTestObserver mirrors newTestObserver but takes the scope func
// directly, so these tests can supply a fake that varies Component,
// Generation, or counts calls -- properties newTestObserver's fixed RunScope
// cannot exercise.
func newAttributionTestObserver(t *testing.T, scope func() RunScope) (*Observer, <-chan bus.Event) {
	t.Helper()
	b := bus.New(64)
	sub, unsub := b.Subscribe(0)
	t.Cleanup(unsub)
	o := New(nil, b, scope)
	return o, sub
}

// TestPublishStampsTheActiveAction pins the whole point of this task: a
// cluster event observed while action 7 ("gpu-operator") is active carries
// that action's name in Component, so the cockpit can show cluster activity
// against the row currently installing. It would fail if publish stopped
// setting Event.Component from the scope's Component field.
func TestPublishStampsTheActiveAction(t *testing.T) {
	scope := func() RunScope {
		return RunScope{
			RunID:      "run-1",
			Namespaces: map[string]struct{}{"gpu-operator": {}},
			Component:  "gpu-operator",
			Generation: 7,
		}
	}
	o, sub := newAttributionTestObserver(t, scope)

	o.onDaemonSet(testDaemonSet("ds-real-uid", 3, 8))

	select {
	case e := <-sub:
		if e.Component != "gpu-operator" {
			t.Errorf("Component = %q, want %q", e.Component, "gpu-operator")
		}
	default:
		t.Fatal("no event published")
	}
}

// TestPublishLeavesComponentEmptyWhenNoActiveAction pins the outside-Apply
// case: no action is installing, so a cluster event must still publish --
// unattributed is a first-class outcome (spec Section 1), not an error --
// but Component must stay empty rather than carry a stale or hardcoded
// action name. It would fail if publish stamped a non-empty Component
// regardless of what the scope actually reported.
func TestPublishLeavesComponentEmptyWhenNoActiveAction(t *testing.T) {
	scope := func() RunScope {
		return RunScope{
			RunID:      "run-1",
			Namespaces: map[string]struct{}{"gpu-operator": {}},
			// Component and Generation left at their zero values,
			// matching engine.Attribution outside Apply.
		}
	}
	o, sub := newAttributionTestObserver(t, scope)

	o.onDaemonSet(testDaemonSet("ds-real-uid", 3, 8))

	select {
	case e := <-sub:
		if e.Component != "" {
			t.Errorf("Component = %q, want empty outside Apply", e.Component)
		}
	default:
		t.Fatal("no event published")
	}
}

// TestPublishReadsAttributionOncePerEvent is the required bite-proof's
// counterpart: a counting fake proves publish reads the scope exactly once
// per event, not once for the namespace filter and again for the RunID/
// Component stamp. Reading twice is the natural-looking way to write publish
// and the wrong one -- see TestPublishDoesNotStampAcrossARunTransition for
// why it matters, not just that it costs an extra call.
func TestPublishReadsAttributionOncePerEvent(t *testing.T) {
	var calls int
	scope := func() RunScope {
		calls++
		return RunScope{
			RunID:      "run-1",
			Namespaces: map[string]struct{}{"gpu-operator": {}},
			Component:  "gpu-operator",
			Generation: 7,
		}
	}
	o, sub := newAttributionTestObserver(t, scope)

	o.onDaemonSet(testDaemonSet("ds-real-uid", 3, 8))

	select {
	case <-sub:
	default:
		t.Fatal("no event published")
	}
	if calls != 1 {
		t.Errorf("scope() called %d times for one published event, want exactly 1", calls)
	}
}

// TestPublishDoesNotStampAcrossARunTransition is the bite-proof from the
// task brief, made concrete: the fake scope answers a DIFFERENT snapshot on
// its second call than its first, simulating an active-action transition
// landing between two reads. If publish read the scope once (correct), only
// the first call is ever made, and the event is entirely the pre-transition
// snapshot -- namespace and Component agree with each other. If publish read
// the scope twice -- once to filter, once to stamp -- the filter would pass
// using the FIRST call's Namespaces (still gpu-operator) while the stamp
// would carry the SECOND call's Component ("kai-scheduler"), producing an
// event whose Component names an action that was never active for the
// namespace that triggered it: a mix of the old and new snapshot, not
// either one cleanly.
func TestPublishDoesNotStampAcrossARunTransition(t *testing.T) {
	var calls int
	scope := func() RunScope {
		calls++
		if calls == 1 {
			return RunScope{
				RunID:      "run-1",
				Namespaces: map[string]struct{}{"gpu-operator": {}},
				Component:  "gpu-operator",
				Generation: 7,
			}
		}
		return RunScope{
			RunID:      "run-1",
			Namespaces: map[string]struct{}{"kai-scheduler-ns": {}},
			Component:  "kai-scheduler",
			Generation: 8,
		}
	}
	o, sub := newAttributionTestObserver(t, scope)

	o.onDaemonSet(testDaemonSet("ds-real-uid", 3, 8))

	select {
	case e := <-sub:
		if e.Component != "gpu-operator" {
			t.Errorf("Component = %q, want %q (the pre-transition snapshot read once, not a mix with the post-transition one)",
				e.Component, "gpu-operator")
		}
	default:
		t.Fatal("no event published; the pre-transition snapshot's namespace should have passed the filter")
	}
}

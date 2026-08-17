package observer

import (
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
func newTestObserver(t *testing.T) (*Observer, <-chan bus.Event) {
	t.Helper()
	b := bus.New(64)
	sub, unsub := b.Subscribe(0)
	t.Cleanup(unsub)
	// A nil client is fine: Start is never called, only the handlers are.
	o := New(nil, b, func() RunScope {
		return RunScope{RunID: "run-1", Namespaces: map[string]struct{}{"gpu-operator": {}}}
	})
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

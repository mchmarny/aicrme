package observer_test

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/observer"
)

// collect drains the bus into a slice until deadline, so a test can assert
// both what was published and that nothing was. A single-goroutine select
// loop is the only writer to out, so no mutex is needed to guard it.
func collect(t *testing.T, b *bus.Bus, within time.Duration) []bus.Event {
	t.Helper()
	sub, unsub := b.Subscribe(0)
	t.Cleanup(unsub)
	var (
		out  []bus.Event
		done = time.After(within)
	)
	for {
		select {
		case e := <-sub:
			out = append(out, e)
		case <-done:
			return out
		}
	}
}

func scopeFor(ns ...string) func() observer.RunScope {
	set := make(map[string]struct{}, len(ns))
	for _, n := range ns {
		set[n] = struct{}{}
	}
	return func() observer.RunScope { return observer.RunScope{RunID: "run-1", Namespaces: set} }
}

func daemonSet(ns, name string, ready, desired int32) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: "ds-uid"},
		Status: appsv1.DaemonSetStatus{
			NumberReady:            ready,
			DesiredNumberScheduled: desired,
		},
	}
}

func deployment(ns, name string, ready, desired int32, gen, observedGen int64) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: "deploy-uid", Generation: gen},
		Spec: appsv1.DeploymentSpec{
			Replicas: &desired,
		},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas:      ready,
			Replicas:           ready,
			ObservedGeneration: observedGen,
		},
	}
}

func node(name, gpus string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: "node-uid"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				"nvidia.com/gpu": resource.MustParse(gpus),
			},
		},
	}
}

func TestDaemonSetRolloutProgressIsNarrated(t *testing.T) {
	client := fake.NewSimpleClientset(daemonSet("gpu-operator", "nvidia-driver-daemonset", 0, 8))
	b := bus.New(256)
	o := observer.New(client, b, scopeFor("gpu-operator"))

	stop := make(chan struct{})
	defer close(stop)
	if err := o.Start(stop); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	ds := daemonSet("gpu-operator", "nvidia-driver-daemonset", 2, 8)
	if _, err := client.AppsV1().DaemonSets("gpu-operator").Update(
		context.Background(), ds, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	events := collect(t, b, 2*time.Second)
	var found bool
	for _, e := range events {
		if strings.Contains(e.Message, "nvidia-driver-daemonset 2/8 nodes ready") {
			found = true
			if e.Kind != bus.KindCluster {
				t.Errorf("Kind = %q, want %q", e.Kind, bus.KindCluster)
			}
			if e.RunID != "run-1" {
				t.Errorf("RunID = %q, want run-1", e.RunID)
			}
			if !strings.Contains(e.Message, "gpu-operator/") {
				t.Errorf("Message = %q, want it namespace-qualified", e.Message)
			}
		}
	}
	if !found {
		t.Fatalf("no rollout event published; got %d events", len(events))
	}
}

// The property the whole design rests on. An update that changes nothing the
// observer reports must publish nothing -- informer UpdateFunc fires on
// managedFields and annotation churn, and the bus drops subscribers 256
// events behind.
func TestUnchangedStateEmitsNothing(t *testing.T) {
	initial := daemonSet("gpu-operator", "nvidia-driver-daemonset", 2, 8)
	client := fake.NewSimpleClientset(initial)
	b := bus.New(256)
	o := observer.New(client, b, scopeFor("gpu-operator"))

	stop := make(chan struct{})
	defer close(stop)
	if err := o.Start(stop); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	noisy := daemonSet("gpu-operator", "nvidia-driver-daemonset", 2, 8)
	noisy.Annotations = map[string]string{"kubectl.kubernetes.io/restartedAt": "now"}
	if _, err := client.AppsV1().DaemonSets("gpu-operator").Update(
		context.Background(), noisy, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	if events := collect(t, b, time.Second); len(events) != 0 {
		t.Fatalf("published %d events for an unchanged rollout, want 0: %+v", len(events), events)
	}
}

// The informer's initial list delivers every existing object as an Add.
// Emitting there would narrate the cluster's entire pre-existing state at
// pod start.
func TestInitialListEmitsNothing(t *testing.T) {
	client := fake.NewSimpleClientset(daemonSet("gpu-operator", "nvidia-driver-daemonset", 8, 8))
	b := bus.New(256)
	o := observer.New(client, b, scopeFor("gpu-operator"))

	stop := make(chan struct{})
	defer close(stop)
	if err := o.Start(stop); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	if events := collect(t, b, time.Second); len(events) != 0 {
		t.Fatalf("published %d events for the initial list, want 0: %+v", len(events), events)
	}
}

func TestWorkloadsOutsideTheRunScopeAreIgnored(t *testing.T) {
	client := fake.NewSimpleClientset(daemonSet("kube-system", "some-other-ds", 0, 3))
	b := bus.New(256)
	o := observer.New(client, b, scopeFor("gpu-operator"))

	stop := make(chan struct{})
	defer close(stop)
	if err := o.Start(stop); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	ds := daemonSet("kube-system", "some-other-ds", 1, 3)
	if _, err := client.AppsV1().DaemonSets("kube-system").Update(
		context.Background(), ds, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	if events := collect(t, b, time.Second); len(events) != 0 {
		t.Fatalf("published %d events for an out-of-scope namespace, want 0", len(events))
	}
}

func TestNilClientYieldsANoOpObserver(t *testing.T) {
	o := observer.New(nil, bus.New(8), scopeFor())
	stop := make(chan struct{})
	defer close(stop)
	if err := o.Start(stop); err != nil {
		t.Fatalf("Start() with nil client error = %v, want nil (degrade, not fail)", err)
	}
}

// The trap: status.replicas is the count of pods that currently exist, not
// the desired count. During a scale-up it reads "1/1 ready" while eight are
// desired -- a "finished" message during precisely the stall this observer
// exists to narrate. The denominator must come from spec.replicas.
func TestDeploymentUsesSpecReplicasAsDenominator(t *testing.T) {
	initial := deployment("gpu-operator", "nim-service", 0, 8, 3, 3)
	client := fake.NewSimpleClientset(initial)
	b := bus.New(256)
	o := observer.New(client, b, scopeFor("gpu-operator"))

	stop := make(chan struct{})
	defer close(stop)
	if err := o.Start(stop); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	d := deployment("gpu-operator", "nim-service", 1, 8, 3, 3)
	if _, err := client.AppsV1().Deployments("gpu-operator").Update(
		context.Background(), d, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	events := collect(t, b, 2*time.Second)
	var found bool
	for _, e := range events {
		if strings.Contains(e.Message, "1/1 ready") {
			t.Fatalf("published %q, want spec.replicas as the denominator, not status.replicas", e.Message)
		}
		if strings.Contains(e.Message, "1/8 ready") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no deployment readiness event published; got %d events", len(events))
	}
}

// Before observedGeneration catches up with generation, status describes the
// PREVIOUS spec and is actively misleading -- emission must be suppressed
// entirely, not just miscounted.
func TestDeploymentStaleStatusIsSuppressed(t *testing.T) {
	initial := deployment("ai-runtime", "triton-server", 1, 1, 1, 1)
	client := fake.NewSimpleClientset(initial)
	b := bus.New(256)
	o := observer.New(client, b, scopeFor("ai-runtime"))

	stop := make(chan struct{})
	defer close(stop)
	if err := o.Start(stop); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	d := deployment("ai-runtime", "triton-server", 1, 8, 2, 1)
	if _, err := client.AppsV1().Deployments("ai-runtime").Update(
		context.Background(), d, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	if events := collect(t, b, time.Second); len(events) != 0 {
		t.Fatalf("published %d events for stale status (observedGeneration < generation), want 0: %+v", len(events), events)
	}
}

func TestNodeGPUAllocatableTransitionIsNarrated(t *testing.T) {
	initial := node("gpu-node-1", "0")
	client := fake.NewSimpleClientset(initial)
	b := bus.New(256)
	o := observer.New(client, b, scopeFor())

	stop := make(chan struct{})
	defer close(stop)
	if err := o.Start(stop); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	n := node("gpu-node-1", "8")
	if _, err := client.CoreV1().Nodes().Update(
		context.Background(), n, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	events := collect(t, b, 2*time.Second)
	var found bool
	for _, e := range events {
		if strings.Contains(e.Message, "gpu-node-1") && strings.Contains(e.Message, "nvidia.com/gpu allocatable 0 → 8") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no node GPU-allocatable transition event published; got %d events: %+v", len(events), events)
	}
}

// The bug caching the message would introduce: if the cache held "0 -> 8"
// instead of the Quantity 8, a repeated identical update would compute
// "8 -> 8", compare unequal to the cached "0 -> 8", and emit again.
func TestNodeRepeatedIdenticalAllocatableEmitsOnce(t *testing.T) {
	initial := node("gpu-node-1", "0")
	client := fake.NewSimpleClientset(initial)
	b := bus.New(256)
	o := observer.New(client, b, scopeFor())

	stop := make(chan struct{})
	defer close(stop)
	if err := o.Start(stop); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	for i := 0; i < 2; i++ {
		n := node("gpu-node-1", "8")
		if _, err := client.CoreV1().Nodes().Update(
			context.Background(), n, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("Update() [%d] error = %v", i, err)
		}
	}

	events := collect(t, b, 2*time.Second)
	var count int
	for _, e := range events {
		if strings.Contains(e.Message, "nvidia.com/gpu allocatable 0 → 8") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("got %d transition events for two identical updates, want exactly 1: %+v", count, events)
	}
}

// "8" and "8e0" are the same quantity (Cmp == 0) but Quantity.String()
// preserves the format it was parsed with (DecimalSI vs DecimalExponent),
// so their String() output genuinely differs ("8" vs "8e0"). This is
// deliberately not the "8" vs "8000m" pair the design note uses as an
// example: Quantity.String() canonicalizes milli-suffixed whole numbers
// back down to the bare form, so those two already produce the identical
// string "8" and would pass this test under plain string equality too --
// proving nothing about why Cmp is required. "8e0" does not canonicalize
// away, so this pair actually exercises the Cmp-vs-string-equality
// distinction the test's name claims.
func TestNodeEquivalentQuantitySerializationsAreOneState(t *testing.T) {
	initial := node("gpu-node-1", "0")
	client := fake.NewSimpleClientset(initial)
	b := bus.New(256)
	o := observer.New(client, b, scopeFor())

	stop := make(chan struct{})
	defer close(stop)
	if err := o.Start(stop); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	n1 := node("gpu-node-1", "8")
	if _, err := client.CoreV1().Nodes().Update(
		context.Background(), n1, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	n2 := node("gpu-node-1", "8e0")
	if _, err := client.CoreV1().Nodes().Update(
		context.Background(), n2, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	events := collect(t, b, 2*time.Second)
	var count int
	for _, e := range events {
		if strings.Contains(e.Message, "allocatable") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("got %d allocatable events for 8 then 8e0, want exactly 1 (the 0 -> 8 transition): %+v", count, events)
	}
}

// A node already at capacity when the console started must never be
// narrated as a transition from zero.
//
// Note which mechanism actually carries this end to end: onAdd seeds gpuQty
// from the informer's initial list, so by the time the Update below arrives
// `had` is already true and onNode's prev.Cmp(cur) == 0 early return fires
// first. The `if !had` first-sighting guard is unreachable from here --
// replacing it with `_ = had` leaves this test green. It is pinned directly
// in handlers_internal_test.go instead.
func TestNodeAlreadyAtCapacityIsNotNarratedAsZeroToEight(t *testing.T) {
	initial := node("gpu-node-2", "8")
	client := fake.NewSimpleClientset(initial)
	b := bus.New(256)
	o := observer.New(client, b, scopeFor())

	stop := make(chan struct{})
	defer close(stop)
	if err := o.Start(stop); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	n := node("gpu-node-2", "8")
	if _, err := client.CoreV1().Nodes().Update(
		context.Background(), n, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	if events := collect(t, b, time.Second); len(events) != 0 {
		t.Fatalf("published %d events for a node already at capacity, want 0: %+v", len(events), events)
	}
}

// What this actually pins: the recreate's own Add re-seeds the baseline
// silently, and the update that follows is diffed against that baseline
// rather than against the pre-delete state.
//
// It does NOT pin onDelete, and it cannot: stateKey carries the object's UID
// (handlers.go), and daemonSet() hardcodes UID "ds-uid", so the recreate here
// reuses the deleted object's key and its own onAdd overwrites the stale
// entry whether or not onDelete ran. With a realistic distinct UID the
// recreate gets a fresh key and cannot inherit either -- state inheritance is
// precluded by the key itself, in both directions. onDelete is memory
// hygiene, and handlers_internal_test.go is where that is pinned.
func TestRecreatedWorkloadIsDiffedAgainstItsOwnAddBaseline(t *testing.T) {
	initial := daemonSet("gpu-operator", "nvidia-driver-daemonset", 8, 8)
	client := fake.NewSimpleClientset(initial)
	b := bus.New(256)
	o := observer.New(client, b, scopeFor("gpu-operator"))

	stop := make(chan struct{})
	defer close(stop)
	if err := o.Start(stop); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	if err := client.AppsV1().DaemonSets("gpu-operator").Delete(
		context.Background(), "nvidia-driver-daemonset", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	recreated := daemonSet("gpu-operator", "nvidia-driver-daemonset", 0, 8)
	if _, err := client.AppsV1().DaemonSets("gpu-operator").Create(
		context.Background(), recreated, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Give the recreate's Add time to land before the next Update, so the
	// baseline it establishes is what the Update below is diffed against.
	time.Sleep(200 * time.Millisecond)

	updated := daemonSet("gpu-operator", "nvidia-driver-daemonset", 3, 8)
	if _, err := client.AppsV1().DaemonSets("gpu-operator").Update(
		context.Background(), updated, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	events := collect(t, b, 2*time.Second)
	for _, e := range events {
		if strings.Contains(e.Message, "8/8 nodes ready") {
			t.Fatalf("recreate's own Add emitted the pre-delete state: %+v", e)
		}
	}
	var found bool
	for _, e := range events {
		if strings.Contains(e.Message, "3/8 nodes ready") {
			found = true
		}
	}
	if !found {
		t.Fatalf("update after recreate did not publish against the new baseline; got %d events: %+v", len(events), events)
	}
}

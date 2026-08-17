package observer

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/tools/cache"
)

// onAdd records state without emitting. An informer's initial list delivers
// every existing object as an Add, so emitting here would narrate the
// cluster's entire pre-existing state at pod start -- and would report a
// node that already has 8 GPUs as "0 -> 8", which is false.
func (o *Observer) onAdd(obj any) {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch t := obj.(type) {
	case *appsv1.DaemonSet:
		o.workload[dsKey(t)] = dsSummary(t)
	case *appsv1.Deployment:
		// Deliberately without onDeployment's observedGeneration gate: that
		// gate suppresses *emission* of a status describing the previous
		// spec, and this path emits nothing. Seeding a mid-rollout pairing is
		// harmless -- the next update compares against it and publishes the
		// difference.
		o.workload[deployKey(t)] = deploySummary(t)
	case *corev1.Node:
		o.gpuQty[nodeKey(t)] = nodeGPUs(t)
	}
}

// onDelete drops the cache entry so a delete-then-recreate of the same name
// does not inherit the old object's state. DeletedFinalStateUnknown is the
// tombstone client-go delivers when a watch gap meant the final object was
// missed.
func (o *Observer) onDelete(obj any) {
	if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tomb.Obj
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	switch t := obj.(type) {
	case *appsv1.DaemonSet:
		delete(o.workload, dsKey(t))
	case *appsv1.Deployment:
		delete(o.workload, deployKey(t))
	case *corev1.Node:
		delete(o.gpuQty, nodeKey(t))
	}
}

func dsKey(ds *appsv1.DaemonSet) stateKey {
	return stateKey{kind: "DaemonSet", namespace: ds.Namespace, name: ds.Name, uid: ds.UID}
}

func dsSummary(ds *appsv1.DaemonSet) string {
	return fmt.Sprintf("%d/%d nodes ready", ds.Status.NumberReady, ds.Status.DesiredNumberScheduled)
}

func (o *Observer) onDaemonSet(obj any) {
	ds, ok := obj.(*appsv1.DaemonSet)
	if !ok {
		return
	}
	summary := dsSummary(ds)
	key := dsKey(ds)

	o.mu.Lock()
	prev, had := o.workload[key]
	if had && prev == summary {
		o.mu.Unlock()
		return
	}
	o.workload[key] = summary
	o.mu.Unlock()

	// Namespace-qualified: two DaemonSets in different namespaces can share
	// a name, and an unqualified message would be ambiguous.
	o.publish(ds.Namespace, fmt.Sprintf("%s/%s %s", ds.Namespace, ds.Name, summary))
}

func deployKey(d *appsv1.Deployment) stateKey {
	return stateKey{kind: "Deployment", namespace: d.Namespace, name: d.Name, uid: d.UID}
}

// deploySummary reports readiness against spec.replicas, not status.replicas.
// status.replicas is the number of pods that currently exist, so a scale-up
// in progress would read "1/1 ready" while eight are desired -- a "finished"
// message during precisely the stall this observer exists to narrate.
func deploySummary(d *appsv1.Deployment) string {
	desired := int32(1)
	if d.Spec.Replicas != nil {
		desired = *d.Spec.Replicas
	}
	return fmt.Sprintf("%d/%d ready", d.Status.ReadyReplicas, desired)
}

func (o *Observer) onDeployment(obj any) {
	d, ok := obj.(*appsv1.Deployment)
	if !ok {
		return
	}
	// Before the controller observes the current generation, status
	// describes the PREVIOUS spec and is actively misleading.
	if d.Status.ObservedGeneration < d.Generation {
		return
	}
	summary := deploySummary(d)
	key := deployKey(d)

	o.mu.Lock()
	prev, had := o.workload[key]
	if had && prev == summary {
		o.mu.Unlock()
		return
	}
	o.workload[key] = summary
	o.mu.Unlock()

	o.publish(d.Namespace, fmt.Sprintf("%s/%s %s", d.Namespace, d.Name, summary))
}

// gpuResource is the allocatable resource this product cares about. A bare
// allocatable diff would also fire on cpu/memory churn on every node.
const gpuResource = "nvidia.com/gpu"

func nodeKey(n *corev1.Node) stateKey {
	return stateKey{kind: "Node", name: n.Name, uid: n.UID}
}

func nodeGPUs(n *corev1.Node) resource.Quantity {
	if q, ok := n.Status.Allocatable[gpuResource]; ok {
		return q
	}
	return *resource.NewQuantity(0, resource.DecimalSI)
}

func (o *Observer) onNode(obj any) {
	n, ok := obj.(*corev1.Node)
	if !ok {
		return
	}
	cur := nodeGPUs(n)
	key := nodeKey(n)

	o.mu.Lock()
	prev, had := o.gpuQty[key]
	// Cmp, not string equality: 8, 8000m and "8" are the same quantity with
	// different serializations, and an informer round trip can change which
	// one you get.
	if had && prev.Cmp(cur) == 0 {
		o.mu.Unlock()
		return
	}
	o.gpuQty[key] = cur
	o.mu.Unlock()

	if !had {
		// No prior value: this is the first sighting, not a transition.
		// Narrating it as "0 -> 8" would be false for a node that already
		// had capacity when the console started.
		return
	}
	// The message is a TRANSITION, formatted here from the cached previous
	// value -- it is deliberately not what gets cached, because a repeated
	// identical update would then compute "8 -> 8", compare unequal to
	// "0 -> 8", and emit again.
	o.publish("", fmt.Sprintf("%s: %s allocatable %s → %s",
		n.Name, gpuResource, prev.String(), cur.String()))
}

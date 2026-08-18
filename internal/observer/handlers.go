package observer

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/tools/cache"

	"github.com/mchmarny/aicrme/internal/bus"
)

// reasonRollout and reasonGPUAllocatable name the condition a ClusterData
// event reports. They stay constant across a resource's transitions
// (0/8 ready -> 8/8 ready is still "RolloutProgress") so ClusterData.Supersedes
// can compare successive events on the same (UID, Reason) pair.
const (
	reasonRollout        = "RolloutProgress"
	reasonGPUAllocatable = "GPUAllocatable"
)

// kindDaemonSet, kindDeployment and kindNode are ClusterData.Kind values.
// Each handler's live-update path and onDelete's clearing path both set
// Kind for the same resource type, so these are shared rather than
// re-literaled.
const (
	kindDaemonSet  = "DaemonSet"
	kindDeployment = "Deployment"
	kindNode       = "Node"
)

// rolloutSeverityInfo is RolloutProgress's Severity, always: Supersedes only
// orders conditions sharing a Reason, and RolloutProgress at Warn would rank
// equal to an unrelated Reason -- say ImagePullBackOff -- also at Warn, so a
// row picking the worse of the conditions it holds could no longer tell
// "still installing" from "actually stuck". Warn/Error are reserved for
// Reasons that describe a fault requiring operator action; a readiness
// shortfall mid-rollout is expected, not that.
const rolloutSeverityInfo = bus.SeverityInfo

// allocatableSeverity flags a GPU capacity drop as the state worth watching;
// a rise back to or above the prior value is the condition clearing.
func allocatableSeverity(prev, cur resource.Quantity) bus.Severity {
	if cur.Cmp(prev) < 0 {
		return bus.SeverityWarn
	}
	return bus.SeverityInfo
}

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

// onDelete releases the cache entry and, if the observer had a tracked
// condition for the resource, publishes it Resolved: true so the row it was
// pinned to clears. stateKey carries the object's UID, so a recreate already
// gets a fresh key and cannot inherit the deleted object's state either way
// -- eviction here is memory hygiene, not what makes that safe. Publishing
// is what spec Section 4's "or is deleted" half of clearing needs: the
// healthy-state half is covered by onDaemonSet/onDeployment/onNode's own
// Resolved computation, but a resource deleted while still unresolved (an
// operator killing a stuck DaemonSet) never reaches that path any other way.
//
// A resource this observer never tracked (no cache entry) publishes nothing
// -- there is no condition to clear, and inventing one would put a phantom
// entry on a row that never showed anything.
//
// DeletedFinalStateUnknown is the tombstone client-go delivers when a watch
// gap meant the final object was missed; it is unwrapped first because the
// cache is the only record of what existed once that happens.
//
// Each case locks, reads and evicts, then UNLOCKS before publish -- same
// shape onDaemonSet/onDeployment/onNode already use, and required here for
// the same reason: o.bus.Publish takes bus's own lock, and calling it while
// holding o.mu would nest the two locks in the one order 2b-i was deliberate
// about avoiding.
func (o *Observer) onDelete(obj any) {
	if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tomb.Obj
	}

	switch t := obj.(type) {
	case *appsv1.DaemonSet:
		key := dsKey(t)
		o.mu.Lock()
		_, had := o.workload[key]
		delete(o.workload, key)
		o.mu.Unlock()
		if !had {
			return
		}
		cd := bus.ClusterData{
			Kind:      kindDaemonSet,
			Namespace: t.Namespace,
			Name:      t.Name,
			UID:       string(t.UID),
			Reason:    reasonRollout,
			Ready:     t.Status.NumberReady,
			Desired:   t.Status.DesiredNumberScheduled,
			Severity:  rolloutSeverityInfo,
			Resolved:  true,
		}
		o.publish(t.Namespace, fmt.Sprintf("%s/%s removed", t.Namespace, t.Name), cd)
	case *appsv1.Deployment:
		key := deployKey(t)
		o.mu.Lock()
		_, had := o.workload[key]
		delete(o.workload, key)
		o.mu.Unlock()
		if !had {
			return
		}
		cd := bus.ClusterData{
			Kind:      kindDeployment,
			Namespace: t.Namespace,
			Name:      t.Name,
			UID:       string(t.UID),
			Reason:    reasonRollout,
			Ready:     t.Status.ReadyReplicas,
			Desired:   deployDesired(t),
			Severity:  rolloutSeverityInfo,
			Resolved:  true,
		}
		o.publish(t.Namespace, fmt.Sprintf("%s/%s removed", t.Namespace, t.Name), cd)
	case *corev1.Node:
		key := nodeKey(t)
		o.mu.Lock()
		_, had := o.gpuQty[key]
		delete(o.gpuQty, key)
		o.mu.Unlock()
		if !had {
			return
		}
		cd := bus.ClusterData{
			Kind:     kindNode,
			Name:     t.Name,
			UID:      string(t.UID),
			Reason:   reasonGPUAllocatable,
			Severity: bus.SeverityInfo,
			Resolved: true,
		}
		o.publish("", fmt.Sprintf("%s removed", t.Name), cd)
	case *corev1.Pod:
		key := podKey(t)
		o.mu.Lock()
		prev, had := o.pods[key]
		delete(o.pods, key)
		o.mu.Unlock()
		if !had {
			// No tracked trouble -- either the pod was healthy, or this
			// observer never saw it. Either way there is no condition on any
			// row to clear.
			return
		}
		o.publish(t.Namespace, podMessage(t, prev, true), podClusterData(t, prev, true))
	}
}

func dsKey(ds *appsv1.DaemonSet) stateKey {
	return stateKey{kind: kindDaemonSet, namespace: ds.Namespace, name: ds.Name, uid: ds.UID}
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

	cd := bus.ClusterData{
		Kind:      kindDaemonSet,
		Namespace: ds.Namespace,
		Name:      ds.Name,
		UID:       string(ds.UID),
		Reason:    reasonRollout,
		Ready:     ds.Status.NumberReady,
		Desired:   ds.Status.DesiredNumberScheduled,
		Severity:  rolloutSeverityInfo,
		Resolved:  ds.Status.NumberReady >= ds.Status.DesiredNumberScheduled,
	}

	// Namespace-qualified: two DaemonSets in different namespaces can share
	// a name, and an unqualified message would be ambiguous.
	o.publish(ds.Namespace, fmt.Sprintf("%s/%s %s", ds.Namespace, ds.Name, summary), cd)
}

func deployKey(d *appsv1.Deployment) stateKey {
	return stateKey{kind: kindDeployment, namespace: d.Namespace, name: d.Name, uid: d.UID}
}

// deployDesired defaults to 1, matching the API server's default for an
// unset Spec.Replicas.
func deployDesired(d *appsv1.Deployment) int32 {
	if d.Spec.Replicas != nil {
		return *d.Spec.Replicas
	}
	return 1
}

// deploySummary reports readiness against spec.replicas, not status.replicas.
// status.replicas is the number of pods that currently exist, so a scale-up
// in progress would read "1/1 ready" while eight are desired -- a "finished"
// message during precisely the stall this observer exists to narrate.
func deploySummary(d *appsv1.Deployment) string {
	return fmt.Sprintf("%d/%d ready", d.Status.ReadyReplicas, deployDesired(d))
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

	desired := deployDesired(d)
	cd := bus.ClusterData{
		Kind:      kindDeployment,
		Namespace: d.Namespace,
		Name:      d.Name,
		UID:       string(d.UID),
		Reason:    reasonRollout,
		Ready:     d.Status.ReadyReplicas,
		Desired:   desired,
		Severity:  rolloutSeverityInfo,
		Resolved:  d.Status.ReadyReplicas >= desired,
	}

	o.publish(d.Namespace, fmt.Sprintf("%s/%s %s", d.Namespace, d.Name, summary), cd)
}

// gpuResource is the allocatable resource this product cares about. A bare
// allocatable diff would also fire on cpu/memory churn on every node.
const gpuResource = "nvidia.com/gpu"

func nodeKey(n *corev1.Node) stateKey {
	return stateKey{kind: kindNode, name: n.Name, uid: n.UID}
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
	cd := bus.ClusterData{
		Kind:     kindNode,
		Name:     n.Name,
		UID:      string(n.UID),
		Reason:   reasonGPUAllocatable,
		Severity: allocatableSeverity(prev, cur),
		Resolved: cur.Cmp(prev) >= 0,
	}

	// The message is a TRANSITION, formatted here from the cached previous
	// value -- it is deliberately not what gets cached, because a repeated
	// identical update would then compute "8 -> 8", compare unequal to
	// "0 -> 8", and emit again.
	o.publish("", fmt.Sprintf("%s: %s allocatable %s → %s",
		n.Name, gpuResource, prev.String(), cur.String()), cd)
}

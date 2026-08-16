package observer

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/client-go/tools/cache"
)

// onAdd records state without emitting. An informer's initial list delivers
// every existing object as an Add, so emitting here would narrate the
// cluster's entire pre-existing state at pod start -- and would report a
// node that already has 8 GPUs as "0 -> 8", which is false.
func (o *Observer) onAdd(obj any) {
	if t, ok := obj.(*appsv1.DaemonSet); ok {
		o.mu.Lock()
		o.workload[dsKey(t)] = dsSummary(t)
		o.mu.Unlock()
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
	if t, ok := obj.(*appsv1.DaemonSet); ok {
		delete(o.workload, dsKey(t))
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

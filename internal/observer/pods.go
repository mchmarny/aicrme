package observer

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	"github.com/mchmarny/aicrme/internal/bus"
)

// kindPod is a ClusterData.Kind value, alongside kindDaemonSet/kindDeployment/
// kindNode in handlers.go. Defined here rather than folded into that block
// because everything else about Pod narration lives in this file.
const kindPod = "Pod"

// reasonImagePullBackOff, reasonErrImagePull and reasonCrashLoopBackOff name
// the container ContainerStateWaiting.Reason values this observer treats as
// operator-actionable. reasonUnschedulable mirrors corev1.PodReasonUnschedulable
// as an untyped string constant so it compares directly against
// PodCondition.Reason (a plain string field, not PodConditionType) --
// spelled out locally rather than referencing the k8s constant at every call
// site, matching reasonRollout/reasonGPUAllocatable's own local-constant
// style in handlers.go.
//
// Deliberately excluded: a Pod going Pending -> Running. The Deployment and
// DaemonSet ready-count narration already summarizes that transition (spec
// Section 3, Volume control) -- adding a second, pod-level signal for the
// same fact is exactly the noise this observer exists to avoid.
const (
	reasonImagePullBackOff = "ImagePullBackOff"
	reasonErrImagePull     = "ErrImagePull"
	reasonCrashLoopBackOff = "CrashLoopBackOff"
	reasonUnschedulable    = string(corev1.PodReasonUnschedulable)
)

// podCondition is the trouble this observer currently attributes to a Pod:
// the Reason/Container pair its last publish carried. The zero value means
// healthy. No entry is ever stored for a healthy pod (onPodChange and
// seedPodBaseline both skip it), so a map lookup miss and "recorded healthy"
// are the same state and never need to be told apart -- unlike gpuQty, which
// must distinguish "never seen" from "seen at zero".
type podCondition struct {
	reason    string
	container string
	// narrated is true once this observer has actually published cond as
	// unresolved. seedPodBaseline (an informer's initial-list Add) never
	// sets it -- a snapshot of pre-existing state is not a transition this
	// observer ever told anyone about. onPodChange always sets it when it
	// publishes cur. onDelete (handlers.go) checks it before publishing a
	// resolution: a condition only ever seeded silently must not manufacture
	// a "resolved"/"removed" event for something no consumer was ever shown
	// (Important 3, Task 5 fix round 1).
	narrated bool
}

// podReasonSeverity ranks CrashLoopBackOff and an unschedulable pod as more
// urgent than an image-pull stall: a stuck pull is often transient (registry
// throttling, a slow mirror) and frequently self-resolves, while a
// crash-looping container is failing after it started, and a pod the
// scheduler cannot place at all is blocked on capacity or constraints no
// retry fixes on its own. Both class of condition warrant an operator's
// attention sooner.
func podReasonSeverity(reason string) bus.Severity {
	if reason == reasonCrashLoopBackOff || reason == reasonUnschedulable {
		return bus.SeverityError
	}
	return bus.SeverityWarn
}

// podKey identifies a Pod the same way dsKey/deployKey/nodeKey do for their
// own kinds: stateKey's kind field keeps a Pod named identically to some
// DaemonSet from ever colliding in the shared o.pods map.
func podKey(p *corev1.Pod) stateKey {
	return stateKey{kind: kindPod, namespace: p.Namespace, name: p.Name, uid: p.UID}
}

// podTrouble reports the single condition this observer narrates for pod, if
// any. PodScheduled=False/Unschedulable wins outright (no container has even
// started, so nothing else can be more relevant). Otherwise it scans every
// init and regular container's waiting reason and reports the
// HIGHEST-SEVERITY one found, not the first in slice order: a pod with one
// container ImagePullBackOff (Warn) and another CrashLoopBackOff (Error)
// must not have the Error hidden behind whichever container happened to
// list first (Minor finding, Task 5 fix round 1 -- probed and confirmed:
// first-in-slice order left the CrashLoopBackOff completely unnarrated).
// Ties (equal severity) keep whichever was found first, which incidentally
// still favors an init container's trouble over a regular one's when both
// carry the same severity, since init containers are scanned first.
//
// A Pod can, in principle, have more than one container in trouble at once.
// This observer tracks and reports at most one condition per Pod, matching
// the granularity every other handler in this package uses (one summary per
// DaemonSet/Deployment/Node, not one per container) -- a judgment call, not
// a constraint from the type system: ClusterData already carries a Container
// field capable of a finer key. Selecting by severity, rather than position,
// is what keeps that simplification from silently costing the worse of two
// simultaneous conditions.
func podTrouble(pod *corev1.Pod) (reason, container string, ok bool) {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == reasonUnschedulable {
			return reasonUnschedulable, "", true
		}
	}

	var bestReason, bestContainer string
	bestSeverity := bus.Severity(-1) // below SeverityInfo (0): any real match replaces it
	consider := func(statuses []corev1.ContainerStatus) {
		for _, cs := range statuses {
			r, ok := waitingTroubleReason(cs)
			if !ok {
				continue
			}
			if sev := podReasonSeverity(r); sev > bestSeverity {
				bestSeverity = sev
				bestReason = r
				bestContainer = cs.Name
			}
		}
	}
	consider(pod.Status.InitContainerStatuses)
	consider(pod.Status.ContainerStatuses)
	if bestReason == "" {
		return "", "", false
	}
	return bestReason, bestContainer, true
}

func waitingTroubleReason(cs corev1.ContainerStatus) (string, bool) {
	if cs.State.Waiting == nil {
		return "", false
	}
	switch cs.State.Waiting.Reason {
	case reasonImagePullBackOff, reasonErrImagePull, reasonCrashLoopBackOff:
		return cs.State.Waiting.Reason, true
	default:
		return "", false
	}
}

// podClusterData builds the typed payload for cond, which is either pod's
// newly-arrived trouble (resolved == false) or the trouble it just left
// (resolved == true, in which case cond is the PREVIOUS condition, not
// whatever podTrouble reports now -- see onPodChange).
func podClusterData(pod *corev1.Pod, cond podCondition, resolved bool) bus.ClusterData {
	return bus.ClusterData{
		Kind:      kindPod,
		Namespace: pod.Namespace,
		Name:      pod.Name,
		UID:       string(pod.UID),
		Container: cond.container,
		Reason:    cond.reason,
		Severity:  podReasonSeverity(cond.reason),
		Resolved:  resolved,
	}
}

func podMessage(pod *corev1.Pod, cond podCondition, resolved bool) string {
	subject := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	if cond.container != "" {
		subject = fmt.Sprintf("%s (%s)", subject, cond.container)
	}
	if resolved {
		return fmt.Sprintf("%s: %s resolved", subject, cond.reason)
	}
	return fmt.Sprintf("%s: %s", subject, cond.reason)
}

// seedPodBaseline records pod's condition without narrating, mirroring
// onAdd's rule for the three cluster-scoped kinds in handlers.go: an
// informer's initial list delivers every pre-existing pod as an Add, and
// narrating here would report a pod that has sat broken for hours as a fresh
// transition. A pod with no current trouble is not recorded at all --
// onPodChange's own rule for what belongs in o.pods -- so this only ever
// seeds a trouble state a later change can resolve. narrated stays at its
// zero value (false): see podCondition's own doc comment for why that
// distinction matters to onDelete.
func (o *Observer) seedPodBaseline(pod *corev1.Pod) {
	reason, container, ok := podTrouble(pod)
	if !ok {
		return
	}
	o.mu.Lock()
	o.pods[podKey(pod)] = podCondition{reason: reason, container: container}
	o.mu.Unlock()
}

// onPodChange computes pod's current trouble state and publishes what
// changed relative to what was last recorded.
//
// Ruling 14(b) (Task 5 fix round 1): the PREVIOUS condition is only
// published resolved when the pod is now FULLY healthy (cur.reason == "").
// A Reason CHANGE while the pod remains broken -- kubelet oscillating
// ErrImagePull and ImagePullBackOff every backoff cycle on one stuck pull,
// or a different container now being the one in trouble -- is not a
// resolution: publishing "resolved" for it would be false narration on the
// operator's timeline, since the pod never stopped being broken. The
// resolved event, when it does fire, carries the PREVIOUS reason/container
// -- not whatever podTrouble reports now -- so ClusterData.Supersedes (same
// UID, same Reason) matches the unresolved row entry it is meant to clear.
func (o *Observer) onPodChange(pod *corev1.Pod) {
	reason, container, ok := podTrouble(pod)
	var cur podCondition
	if ok {
		cur = podCondition{reason: reason, container: container}
	}

	key := podKey(pod)
	o.mu.Lock()
	prev, had := o.pods[key]
	if had && prev.reason == cur.reason && prev.container == cur.container {
		o.mu.Unlock()
		return // no change (narrated is bookkeeping, not part of this comparison)
	}
	if cur.reason == "" {
		delete(o.pods, key)
	} else {
		// About to publish cur as unresolved below -- record it narrated so
		// a later delete (handlers.go's onDelete) knows this condition was
		// actually shown, not just seeded silently from an initial list.
		o.pods[key] = podCondition{reason: cur.reason, container: cur.container, narrated: true}
	}
	o.mu.Unlock()

	if had && prev.reason != "" && cur.reason == "" {
		o.publish(pod.Namespace, podMessage(pod, prev, true), podClusterData(pod, prev, true))
	}
	if cur.reason != "" {
		o.publish(pod.Namespace, podMessage(pod, cur, false), podClusterData(pod, cur, false))
	}
}

// onPodAdd is the Pod informer's AddFunc, registered as part of
// ResourceEventHandlerDetailedFuncs (not the plain ResourceEventHandlerFuncs
// Observer.register uses for the three cluster-scoped kinds) precisely for
// isInInitialList: an initial-list Add is a snapshot of state that predates
// this process, seeded silently via seedPodBaseline. A LATER Add -- a pod
// genuinely created after this namespace's informer was already watching --
// is not a snapshot of anything; it is routed through the same onPodChange
// an Update would use, so a pod that is already broken the moment it is
// first observed (say, a bad image reference from the start) is narrated
// without waiting for a subsequent Update that might arrive much later, or
// -- for a pod that never changes state again -- might not arrive at all.
func (o *Observer) onPodAdd(obj any, isInInitialList bool) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	if isInInitialList {
		o.seedPodBaseline(pod)
		return
	}
	o.onPodChange(pod)
}

// onPodUpdate is the Pod informer's UpdateFunc. Its signature is fixed by
// cache.ResourceEventHandlerDetailedFuncs (oldObj, newObj any); oldObj is
// unused because onPodChange diffs against o.pods' own recorded baseline,
// not against whatever the informer happened to pass as "old".
func (o *Observer) onPodUpdate(_, newObj any) {
	pod, ok := newObj.(*corev1.Pod)
	if !ok {
		return
	}
	o.onPodChange(pod)
}

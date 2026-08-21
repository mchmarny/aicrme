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

// normalizeReason collapses kubelet's two names for one stuck image pull --
// ErrImagePull (a failed pull attempt) and ImagePullBackOff (waiting out the
// backoff before the next attempt) -- into the single Reason
// ClusterData.Reason/podCondition.reason (the Supersedes key) uses.
//
// Ruling 17 (Task 5 fix round 2), replacing Ruling 14(b)'s attempted fix:
// they are ONE condition with two names, not two conditions, and keying per
// RAW reason meant a recovered pull's resolution carried only whichever raw
// reason kubelet happened to be using LAST -- every earlier raw reason the
// pod oscillated through kept a live, unresolved row entry nothing would
// ever clear (Important A). Normalizing removes the second row identity
// entirely: there is only ever one (UID, ImagePullBackOff) entry to arise
// and resolve, so oscillation collapses to a single narration instead of a
// new transition on every backoff cycle. The raw reason is preserved for
// the narration MESSAGE (podMessage uses podCondition.detail), which is
// still useful operator detail -- Reason is the supersede key, not the
// display string. CrashLoopBackOff and Unschedulable already name one
// condition each and pass through unchanged.
func normalizeReason(raw string) string {
	if raw == reasonErrImagePull {
		return reasonImagePullBackOff
	}
	return raw
}

// podCondition is ONE narrated trouble Reason this observer currently
// attributes to a Pod. o.pods holds a SET of these per Pod, keyed by
// reason (Ruling 20, Task 5 fix round 3) -- a Pod can accumulate more than
// one over its lifetime (Unschedulable while pending, then ImagePullBackOff
// once scheduled), and each needs its own resolve when the pod recovers, not
// just whichever was narrated last. reason is normalized
// (podTrouble/normalizeReason) and is what ClusterData.Reason carries -- the
// Supersedes key; it is also, redundantly, the key o.pods[key] stores this
// value under, kept on the struct so a podCondition pulled out of the map
// during a resolve sweep is self-describing without threading the key
// alongside it. detail is the raw kubelet reason (equal to reason except
// for the ErrImagePull/ImagePullBackOff pair Ruling 17 normalizes), used
// only in the narration message. No entry, and no SET entry for a given
// reason, is ever stored for a healthy pod (onPodChange and seedPodBaseline
// both skip it) -- see podCondition's own doc comment.
type podCondition struct {
	reason    string
	detail    string
	container string
	// narrated is true once this observer has actually published cond as
	// unresolved. seedPodBaseline (an informer's initial-list Add) never
	// sets it -- a snapshot of pre-existing state is not a transition this
	// observer ever told anyone about. onDelete (handlers.go) checks it
	// before publishing a resolution/removal: a condition only ever seeded
	// silently must not manufacture an event for something no consumer was
	// ever shown (Important 3, Task 5 fix round 1).
	//
	// onPodChange's OWN recovery path deliberately does NOT consult
	// narrated (Minor C, Task 5 fix round 2, re-verified independently and
	// upheld in fix round 3's re-review): a pod that was already broken
	// before this process started, and later genuinely recovers, is telling
	// this observer something true and useful -- "the thing I never
	// mentioned is fixed now" -- which TestPodConditionResolves (a
	// brief-named test) correctly pins as the wanted behavior. A DELETE
	// cannot make that same claim: the pod did not get better, it is gone,
	// and reporting "resolved" for a condition no one was ever shown would
	// be inventing a claim this observer cannot back either way. The two
	// call sites differ because the underlying claims differ, not by
	// oversight. This asymmetry now applies PER REASON in the set, not just
	// to a single tracked value: onPodChange's full-recovery sweep resolves
	// every entry regardless of narrated, onDelete's sweep resolves only the
	// narrated ones.
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
// any: reason is normalized (normalizeReason, Ruling 17) and is the
// Supersedes key; detail is the raw kubelet reason for the narration
// message. PodScheduled=False/Unschedulable wins outright (no container has
// even started, so nothing else can be more relevant). Otherwise it scans
// every init and regular container's waiting reason and reports the
// HIGHEST-SEVERITY one found (by its NORMALIZED severity), not the first in
// slice order: a pod with one container ImagePullBackOff (Warn) and another
// CrashLoopBackOff (Error) must not have the Error hidden behind whichever
// container happened to list first (Minor finding, Task 5 fix round 1 --
// probed and confirmed: first-in-slice order left the CrashLoopBackOff
// completely unnarrated). Ties (equal severity) keep whichever was found
// first, which incidentally still favors an init container's trouble over a
// regular one's when both carry the same severity, since init containers
// are scanned first.
//
// A Pod can, in principle, have more than one container in trouble at once.
// This observer tracks and reports at most one condition per Pod, matching
// the granularity every other handler in this package uses (one summary per
// DaemonSet/Deployment/Node, not one per container) -- a judgment call, not
// a constraint from the type system: ClusterData already carries a Container
// field capable of a finer key. Selecting by severity, rather than position,
// is what keeps that simplification from silently costing the worse of two
// simultaneous conditions.
func podTrouble(pod *corev1.Pod) (reason, detail, container string, ok bool) {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == reasonUnschedulable {
			return reasonUnschedulable, reasonUnschedulable, "", true
		}
	}

	var bestReason, bestContainer string
	bestSeverity := bus.Severity(-1) // below SeverityInfo (0): any real match replaces it
	consider := func(statuses []corev1.ContainerStatus) {
		for _, cs := range statuses {
			raw, ok := waitingRawReason(cs)
			if !ok {
				continue
			}
			if sev := podReasonSeverity(normalizeReason(raw)); sev > bestSeverity {
				bestSeverity = sev
				bestReason = raw
				bestContainer = cs.Name
			}
		}
	}
	consider(pod.Status.InitContainerStatuses)
	consider(pod.Status.ContainerStatuses)
	if bestReason == "" {
		return "", "", "", false
	}
	return normalizeReason(bestReason), bestReason, bestContainer, true
}

// waitingRawReason returns the RAW kubelet reason a container is currently
// waiting on -- before Ruling 17's normalization -- or "" if it is not one
// this observer tracks.
func waitingRawReason(cs corev1.ContainerStatus) (string, bool) {
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
// whatever podTrouble reports now -- see onPodChange). Reason is cond's
// NORMALIZED reason (Ruling 17) -- the Supersedes key -- not the raw
// kubelet detail, which only ever reaches the narration message.
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

// podMessage narrates using cond.detail, the RAW kubelet reason -- not
// cond.reason, which Ruling 17 normalizes for the Supersedes key. An
// operator reading the timeline still sees "ErrImagePull" or
// "ImagePullBackOff" specifically; only the row's identity is normalized.
func podMessage(pod *corev1.Pod, cond podCondition, resolved bool) string {
	subject := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	if cond.container != "" {
		subject = fmt.Sprintf("%s (%s)", subject, cond.container)
	}
	if resolved {
		return fmt.Sprintf("%s: %s resolved", subject, cond.detail)
	}
	return fmt.Sprintf("%s: %s", subject, cond.detail)
}

// seedPodBaseline records pod's condition without narrating, mirroring
// onAdd's rule for the three cluster-scoped kinds in handlers.go: an
// informer's initial list delivers every pre-existing pod as an Add, and
// narrating here would report a pod that has sat broken for hours as a fresh
// transition. A pod with no current trouble is not recorded at all --
// onPodChange's own rule for what belongs in o.pods -- so this only ever
// seeds one entry in the pod's set (Ruling 20) that a later change can
// resolve. narrated stays at its zero value (false): see podCondition's own
// doc comment for why that distinction matters to onDelete.
//
// Gated by o.scoped.withNamespaceLive (Important B, Task 5 fix round 2): an
// informer's initial list can still be draining when its namespace tears
// down (close(stop) does not stop delivery synchronously), and a stale
// write here would reintroduce the exact stranding this gate exists to
// prevent -- see withNamespaceLive's own doc comment for the race and why
// only a check made under the SAME lock as the teardown sweep closes it.
func (o *Observer) seedPodBaseline(pod *corev1.Pod) {
	reason, detail, container, ok := podTrouble(pod)
	if !ok {
		return
	}
	o.scoped.withNamespaceLive(pod.Namespace, func() {
		o.mu.Lock()
		defer o.mu.Unlock()
		key := podKey(pod)
		if o.pods[key] == nil {
			o.pods[key] = make(map[string]podCondition)
		}
		o.pods[key][reason] = podCondition{reason: reason, detail: detail, container: container}
	})
}

// onPodChange computes pod's current trouble state and publishes what
// changed relative to what was last recorded for THAT SPECIFIC reason, or --
// on full recovery -- resolves every reason ever narrated for this pod.
//
// Ruling 20 (Task 5 fix round 3), replacing Ruling 14(b)'s per-pod single
// value: o.pods holds a SET of reasons per pod (podCondition's own doc
// comment), because 14(b)/17 tracking only the CURRENT condition meant a
// pod that narrated Unschedulable and then ImagePullBackOff -- pends on GPU
// capacity, gets scheduled, then its image pull stalls; an ORDINARY Apply
// sequence, not an edge case -- lost track of Unschedulable the instant
// ImagePullBackOff overwrote the single tracked value. Nothing ever
// resolved it: the row kept an Error-severity Unschedulable pinned
// unresolved on a pod that went on to become fully healthy, outranking the
// resolved ImagePullBackOff entry and showing permanent red. On full
// recovery (cur.reason == ""), every reason in the pod's set is now
// resolved and the whole set is dropped -- not just whichever reason was
// tracked most recently.
//
// A single onPodChange call still narrates at most ONE arising reason
// (podTrouble reports only the current highest-severity condition), added
// to or updated within the set; reasons already in the set that are not
// podTrouble's current pick are left untouched until the pod either fully
// recovers or is deleted -- this observer has no signal telling it a
// SPECIFIC earlier reason cleared mid-lifecycle, only that the pod overall
// has or has not. Ruling 17's normalization still matters here: without it,
// kubelet's ErrImagePull/ImagePullBackOff oscillation would populate the
// set with what looks like two reasons for one stuck pull, each needing its
// own arise/resolve pair, exactly the noise Ruling 17 exists to collapse.
//
// Ruling 23 (Task 6 fix round 1, Important 1): full recovery ALSO resolves
// any o.events entries keyed identically to this pod (events.go's
// resolveEventsLocked) -- eventInvolvedKey(ev) is byte-identical to
// podKey(pod) for a pod-involved Event, so a Warning this observer narrated
// from the Event informer (e.g. FailedScheduling) is exactly as resolvable
// on this pod's recovery as a podCondition is. Without this, transient
// FailedScheduling -- the norm while a GPU cluster scales, not an edge case
// -- pins a permanent Warn-severity row on a pod that went on to become
// fully healthy: ClusterData.Supersedes never compares across Reasons, so
// nothing else could ever clear it. This is Ruling 20's stranding class
// (a condition tracked by one identity, resolved only through a DIFFERENT
// identity's signal) surfacing a fourth time, now across the Pod/Event
// informer boundary rather than within a single Pod's reason set.
// TestEventSourcedWarningResolvesWhenThePodRecovers walks the full
// arise/arise/recover SEQUENCE, not a single condition in isolation --
// every earlier occurrence of this class was missed by a test that only
// exercised one.
//
// The map read, compare, and write all happen inside the closure passed to
// o.scoped.withNamespaceLive (Important B, Task 5 fix round 2), which holds
// s.mu for the closure's entire duration -- the same lock
// stopNamespaceLocked uses to remove a torn-down namespace from
// s.entries -- so a notification queued before teardown but delivered after
// it (close(stop) does not stop delivery synchronously) is told apart from
// a live one and declines to write, rather than racing the sweep that
// already ran. Publishing happens OUTSIDE that closure, after both s.mu and
// o.mu have been released, matching this package's existing rule that a
// publish must never run under either lock. resolveEventsLocked is called
// INSIDE the closure, under the SAME already-held o.mu (not a second
// acquisition -- see its own doc comment for why that would deadlock).
func (o *Observer) onPodChange(pod *corev1.Pod) {
	reason, detail, container, ok := podTrouble(pod)

	key := podKey(pod)
	var toResolve []podCondition // every reason in the set, only populated on full recovery
	var resolvedEvents []bus.ClusterData
	var arose podCondition
	var arising bool

	o.scoped.withNamespaceLive(pod.Namespace, func() {
		o.mu.Lock()
		defer o.mu.Unlock()
		set := o.pods[key]

		if !ok {
			// Fully healthy: resolve EVERY reason ever narrated for this
			// pod, not just the current one -- Ruling 20's whole point.
			// Deliberately does NOT filter on narrated (Minor C): matches
			// TestPodConditionResolves' pinned behavior for a
			// seeded-but-never-shown condition that later genuinely clears.
			for _, cond := range set {
				toResolve = append(toResolve, cond)
			}
			delete(o.pods, key)
			// narratedOnly: false -- matches this branch's own "does NOT
			// filter on narrated" rule three lines up (Minor C/Ruling 19):
			// the pod genuinely got better, a claim this observer can back
			// for a seeded-only Event entry exactly as it can for a
			// seeded-only podCondition (Task 6 fix round 2, Important 1(new)).
			resolvedEvents = o.resolveEventsLocked(key, false) // Ruling 23
			// ...and the same for a Warning about the CONTROLLER that owns
			// this pod (docs/ux-feedback.md item 2). A ReplicaSet's
			// FailedCreate is about its inability to create a pod; this pod
			// existing and being healthy is that inability ending, observed
			// through an informer this package already runs rather than a new
			// one. narratedOnly: false for the same reason the line above
			// passes it -- this is a genuine improvement, not a deletion.
			if owner, hasOwner := controllerEventKey(pod); hasOwner {
				resolvedEvents = append(resolvedEvents, o.resolveEventsLocked(owner, false)...)
			}
			return
		}

		existing, had := set[reason]
		if had && existing.container == container {
			return // no change for this specific reason
		}

		if set == nil {
			set = make(map[string]podCondition)
			o.pods[key] = set
		}
		// About to publish this reason as unresolved below -- record it
		// narrated so a later delete (handlers.go's onDelete) knows this
		// specific reason was actually shown, not just seeded silently
		// from an initial list.
		arose = podCondition{reason: reason, detail: detail, container: container, narrated: true}
		set[reason] = arose
		arising = true
	})

	for _, cond := range toResolve {
		o.publish(pod.Namespace, podMessage(pod, cond, true), podClusterData(pod, cond, true))
	}
	for _, cd := range resolvedEvents {
		o.publish(pod.Namespace, eventResolutionMessage(cd, "resolved"), cd)
	}
	if arising {
		o.publish(pod.Namespace, podMessage(pod, arose, false), podClusterData(pod, arose, false))
	}
}

// onPodAdd is the Pod informer's AddFunc, registered as part of
// ResourceEventHandlerDetailedFuncs (not the plain ResourceEventHandlerFuncs
// Observer.register uses for the three cluster-scoped kinds) precisely for
// isInInitialList: an initial-list Add is a snapshot of state that predates
// this process's FIRST-EVER sighting of the namespace, seeded silently via
// seedPodBaseline. A LATER Add -- a pod genuinely created after this
// namespace's informer was already watching -- is not a snapshot of
// anything; it is routed through the same onPodChange an Update would use,
// so a pod that is already broken the moment it is first observed (say, a
// bad image reference from the start) is narrated without waiting for a
// subsequent Update that might arrive much later, or -- for a pod that
// never changes state again -- might not arrive at all.
//
// Ruling 32 (Task 6 fix round 3): "predates this process's first-ever
// sighting" is the operative phrase above, and it stopped being universally
// true once Task 6 gave namespaces a restart lifecycle. o.scoped.isRestart
// tells the two initial-list cases apart: a genuine first sighting (this
// process has never torn ns down) still seeds silently, but an initial list
// delivered on a RESTARTED informer -- after this SAME process already
// discarded whatever it knew about ns (clearNamespacePods, wired as
// onNamespaceStop) -- is routed through onPodChange instead, the same as a
// later Add. engine.Retry reusing the SAME RunID is the motivating case: the
// SPA clears its condition state on "run retrying" and expects to be
// re-told the truth, but without this check a pod still wedged on the
// identical reason across the retry published nothing for the entire
// retried attempt (bus/cluster.go's own comment: a clean row hiding a live
// failure is the worse direction). Routing through onPodChange also means
// the re-narrated condition is correctly marked narrated (podCondition's own
// bookkeeping), so the delete path's resolved-vs-removed asymmetry still
// holds for it -- no separate plumbing needed, onPodChange already does this
// for every condition it records.
func (o *Observer) onPodAdd(obj any, isInInitialList bool) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	if isInInitialList && !o.scoped.isRestart(pod.Namespace) {
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

package observer

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/mchmarny/aicrme/internal/bus"
)

// eventWarningFieldSelector restricts the Event ListWatch to type=Warning
// server-side, where the API server supports it (spec Section 3): filtering
// happens before the bytes are sent, not after. corev1's event registry
// (pkg/registry/core/event/strategy.go's GetAttrs) accepts "type" as a
// selectable field, so this is a supported selector against a real cluster --
// the fake clientset used by this package's tests does not enforce it at all
// (k8s.io/client-go/testing's ObjectTracker.List ignores FieldSelector
// entirely), which is exactly why onEventChange below ALSO checks
// ev.Type == Warning itself: the client-side check is what makes
// TestEventIgnoresNormalType meaningful against a fake clientset, and what
// keeps a real cluster's behavior correct if a future API server version, or
// a CRD-backed Event store, silently stops honoring the selector.
const eventWarningFieldSelector = "type=Warning"

// eventWarningListOptions is scoped.go's informers.WithTweakListOptions
// argument for the Event factory only -- see startNamespace's own comment for
// why this must never reach podFactory.
func eventWarningListOptions(opts *metav1.ListOptions) {
	opts.FieldSelector = eventWarningFieldSelector
}

// eventInvolvedKey identifies the RESOURCE an Event is ABOUT for dedupe
// purposes: Kind/Name/UID read from ev.InvolvedObject (the same Pod a
// podCondition entry in o.pods might also be tracking), but namespace reads
// from ev.Namespace -- the Event API OBJECT's own namespace -- not
// io.Namespace.
//
// M1 (Task 6 fix round 1): for a namespaced InvolvedObject the two agree
// (Kubernetes creates an Event in the SAME namespace as what it's about), so
// this changes nothing for the common Pod case. They diverge for a
// CLUSTER-SCOPED InvolvedObject -- a Node or ClusterPolicy Warning has
// io.Namespace == "" but still lives in a real namespace as an Event object
// (Kubernetes substitutes NamespaceDefault for an empty ref.Namespace,
// client-go's own event.go:makeEvent) -- and that real namespace is what
// withNamespaceLive gates writes on (onEventChange/seedEventBaseline, both
// key off ev.Namespace) and what clearNamespaceEvents' sweep receives as its
// ns argument (observer.go, driven by the informer's actual per-namespace
// factory, which is likewise keyed on the real namespace, not
// InvolvedObject's). Keying this dedupe map's namespace field off
// io.Namespace instead left the write gate and the eviction sweep walking
// two different namespace universes: a Node Warning's entry could be
// written (gate correctly matched "default") but never swept (key.namespace
// was "", which "default" never matches) -- a permanent strand.
// TestEventAboutAClusterScopedResourceDoesNotStrand pins this.
func eventInvolvedKey(ev *corev1.Event) stateKey {
	io := ev.InvolvedObject
	return stateKey{kind: io.Kind, namespace: ev.Namespace, name: io.Name, uid: io.UID}
}

// eventContainer extracts a container name from InvolvedObject.FieldPath,
// which the API server populates as e.g. "spec.containers{trainer}" when an
// Event is scoped to one container rather than the whole Pod. "" (most
// events -- FailedScheduling has no container at all) leaves
// ClusterData.Container unset, matching podClusterData's own omitempty
// field.
func eventContainer(fieldPath string) string {
	start := strings.IndexByte(fieldPath, '{')
	end := strings.IndexByte(fieldPath, '}')
	if start < 0 || end < 0 || end <= start+1 {
		return ""
	}
	return fieldPath[start+1 : end]
}

// eventMessageMaxLen bounds how much of ev.Message (M6, Task 6 fix round 1)
// reaches a bus payload. ev.Message is free-form text the REPORTING
// COMPONENT wrote -- kubelet, the scheduler, a third-party controller -- not
// something this observer controls the shape of, unlike every other string
// this package interpolates into a message. internal/bus drops live events
// for a subscriber more than 256 events behind (subscriberBuffer); an
// oversized message consumes more of that budget than the ones around it
// without being any more informative past this length.
const eventMessageMaxLen = 256

// truncateEventMessage bounds s to max runes, appending an ellipsis when it
// cuts something off so the truncation itself is visible rather than
// silently swallowing the tail. Rune-based, not byte-based: ev.Message can
// contain multi-byte UTF-8 (a component name, a quoted user value), and
// slicing by byte index risks cutting a multi-byte rune in half.
func truncateEventMessage(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// eventMessage narrates ev against the resource it is about, matching
// podMessage's "namespace/name: detail" shape. ev.Message -- the human
// description the reporting component wrote, e.g. "0/3 nodes are available:
// 3 Insufficient nvidia.com/gpu" -- is appended when present, since
// ev.Reason alone ("FailedScheduling") is a category, not the specific
// blocker an operator needs. Truncated per eventMessageMaxLen (M6).
func eventMessage(ev *corev1.Event) string {
	io := ev.InvolvedObject
	subject := io.Name
	if io.Namespace != "" {
		subject = fmt.Sprintf("%s/%s", io.Namespace, io.Name)
	}
	if ev.Message == "" {
		return fmt.Sprintf("%s: %s", subject, ev.Reason)
	}
	return fmt.Sprintf("%s: %s -- %s", subject, ev.Reason, truncateEventMessage(ev.Message, eventMessageMaxLen))
}

// eventClusterData builds the typed payload for ev's ARISING (Resolved:
// false, always). A Kubernetes Event reports an occurrence, not an ongoing
// condition with a healthy state to return to the way a
// DaemonSet/Deployment/Node/Pod has one -- this file's own Add/Update/Delete
// handling of the Event API object never has a signal that a Warning's
// underlying cause cleared, only that this specific Event object no longer
// exists (handlers.go's onDelete Event case, clearNamespaceEvents).
//
// Ruling 23 (Task 6 fix round 1, overriding I1's spec-consistent-but-adverse
// default): that does NOT mean an Event-sourced Warning never resolves at
// all. eventInvolvedKey(ev) is byte-identical to podKey(pod) for a
// pod-involved Event, and pods.go's onPodChange already has a genuine
// recovery signal for that SAME key -- resolveEventsLocked below is what
// lets onPodChange's full-recovery sweep, and handlers.go's Pod onDelete
// case, resolve/remove an Event-sourced entry through the Pod side of this
// shared identity. Severity is always Warn on arising: the field selector
// (and onEventChange's own defensive check) admit only type=Warning events,
// so there is exactly one severity to assign, unlike podReasonSeverity's
// per-reason ranking.
func eventClusterData(ev *corev1.Event) bus.ClusterData {
	io := ev.InvolvedObject
	return bus.ClusterData{
		Kind:      io.Kind,
		Namespace: io.Namespace,
		Name:      io.Name,
		UID:       string(io.UID),
		Container: eventContainer(io.FieldPath),
		Reason:    ev.Reason,
		Severity:  bus.SeverityWarn,
		Resolved:  false,
	}
}

// onEventChange is the shared path for a genuinely new watch delivery: either
// a later Add (onEventAdd's isInInitialList == false case -- Kubernetes
// creates an Event once and never updates it for its FIRST occurrence, so
// this is how essentially every Warning this observer narrates actually
// arrives, TestEventArrivingOnlyAsAddIsNarrated's whole point) or an Update
// (a recurrence Kubernetes coalesced into the SAME Event object by bumping
// Count/LastTimestamp rather than creating a new one -- TestEventDoesNotReEmitOnCountIncrease).
// Both need the identical dedupe decision, so both route through here rather
// than duplicating it, mirroring onPodChange's role for onPodAdd/onPodUpdate.
//
// Deduped on (eventInvolvedKey, Reason) -- Ruling for Task 6, matching
// podCondition's own (resource, reason) granularity: a resource can have more
// than one distinct Warning reason live at once (FailedScheduling, then
// separately FailedMount), and narrating each once is the design's whole
// point ("Kubernetes coalesces repeat events into count precisely so
// consumers need not re-narrate", spec Section 3) -- narrating every
// recurrence of the SAME reason would defeat that coalescing at this layer
// instead of respecting it.
//
// The map read, compare and write happen inside o.scoped.withNamespaceLive's
// closure -- mandatory here for the identical reason pods.go's onPodChange
// needs it (Important B, Task 5 fix round 2): close(stop) does not stop
// client-go's sharedProcessor synchronously, so a notification queued before
// a namespace's teardown can still be delivered after clearNamespaceEvents
// already swept o.events clean for that namespace, and a write from that
// stale delivery would silently strand a dedupe entry no future teardown
// would ever clear (TestEventDedupeDoesNotStrandUnderConcurrentTeardown is
// this file's version of Task 5's own probe, which stranded entries on 23 of
// 25 attempts before the gate existed). o.publish runs OUTSIDE the closure,
// after s.mu and o.mu are both released -- withNamespaceLive's own doc
// comment: that mutex also gates every OTHER namespace's handler delivery, so
// a publish call under it would serialize far more than the one map write it
// exists to protect.
func (o *Observer) onEventChange(ev *corev1.Event) {
	if ev.Type != corev1.EventTypeWarning {
		return
	}

	key := eventInvolvedKey(ev)
	var arising bool

	o.scoped.withNamespaceLive(ev.Namespace, func() {
		o.mu.Lock()
		defer o.mu.Unlock()
		if _, seen := o.events[key][ev.Reason]; seen {
			return
		}
		set := o.events[key]
		if set == nil {
			set = make(map[string]struct{})
			o.events[key] = set
		}
		set[ev.Reason] = struct{}{}
		arising = true
	})

	if arising {
		o.publish(ev.Namespace, eventMessage(ev), eventClusterData(ev))
	}
}

// seedEventBaseline records ev's (resource, reason) as already-dedup'd
// without narrating -- the Event counterpart of seedPodBaseline, called from
// an informer's INITIAL list (isInInitialList == true), which is a snapshot
// of Warnings that existed before this process started watching, not
// something newly happening.
//
// Seeding, rather than ignoring the initial list entirely, matters for
// TestEventDoesNotReEmitOnCountIncrease's sibling case: an already-existing
// Event object delivered in the initial list can later be updated (its Count
// bumped by a recurrence within Kubernetes' own correlation window), and that
// Update must not narrate either -- it is exactly as much "not a new event"
// as a recurrence on an Event this process watched arise itself. Without
// seeding, that Update would find no dedupe entry and narrate a Warning that
// (from an operator's perspective) has been sitting there the whole time.
//
// Gated by withNamespaceLive for the same race seedPodBaseline documents: an
// informer's initial list can still be draining when its namespace tears
// down.
func (o *Observer) seedEventBaseline(ev *corev1.Event) {
	if ev.Type != corev1.EventTypeWarning {
		return
	}
	key := eventInvolvedKey(ev)
	o.scoped.withNamespaceLive(ev.Namespace, func() {
		o.mu.Lock()
		defer o.mu.Unlock()
		set := o.events[key]
		if set == nil {
			set = make(map[string]struct{})
			o.events[key] = set
		}
		set[ev.Reason] = struct{}{}
	})
}

// onEventAdd is the Event informer's AddFunc, registered as part of
// ResourceEventHandlerDetailedFuncs precisely so isInInitialList can tell an
// initial-list Add from a later one -- the trap this task exists to avoid.
// handlers.go's onAdd (used for DaemonSet/Deployment/Node) records on EVERY
// Add and narrates only on Update; copying that posture here would emit
// nothing at all, because Kubernetes creates an Event once and never updates
// it for a first occurrence (spec Section 3) -- a later Add IS the narration
// opportunity, not a baseline-only snapshot the way a Pod's later Add
// sometimes still is.
func (o *Observer) onEventAdd(obj any, isInInitialList bool) {
	ev, ok := obj.(*corev1.Event)
	if !ok {
		return
	}
	if isInInitialList {
		o.seedEventBaseline(ev)
		return
	}
	o.onEventChange(ev)
}

// onEventUpdate is the Event informer's UpdateFunc, reached when Kubernetes
// coalesces a recurrence into the SAME Event object (Count/LastTimestamp
// bumped) rather than creating a new one. Its signature is fixed by
// cache.ResourceEventHandlerDetailedFuncs; oldObj is unused for the same
// reason onPodUpdate's is -- onEventChange dedupes against o.events' own
// recorded baseline, not against whatever the informer passed as "old".
func (o *Observer) onEventUpdate(_, newObj any) {
	ev, ok := newObj.(*corev1.Event)
	if !ok {
		return
	}
	o.onEventChange(ev)
}

// resolveEventsLocked is Ruling 23's (Task 6 fix round 1, Important 1)
// cross-boundary resolution: it returns Resolved: true ClusterData for every
// Warning Reason this observer has narrated about key, then evicts them from
// o.events -- key is expected to be podKey(pod)'s value, since the only
// caller-known genuine "this resource is better/gone now" signal an
// Event-only informer never gets on its own is the SAME pod's own recovery
// (pods.go's onPodChange, full recovery) or deletion (handlers.go's onDelete
// Pod case) -- eventInvolvedKey(ev) is byte-identical to podKey(pod) for a
// pod-involved Event, so a Pod-side signal can speak for both.
//
// Built directly from key/reason, NOT by re-deriving a *corev1.Event: by the
// time either caller reaches here, the original Event object (Message,
// FieldPath, etc.) is long gone from the code path that triggered this --
// onPodChange has a *corev1.Pod, onDelete's Pod case has a *corev1.Pod being
// deleted, neither has the Event that narrated the arising condition. Only
// what eventClusterData's own arising payload already carried on Kind/
// Namespace/Name/UID/Reason survives to be echoed back on the resolution,
// which is exactly what ClusterData.Supersedes needs to match against (same
// UID, same Reason) -- Severity is fixed at Warn on arising (eventClusterData's
// own doc comment), so pinning it here again is consistent, not a guess.
//
// "Locked": callers must already hold o.mu, matching this codebase's
// existing convention (stopNamespaceLocked, aliveLocked) for a function whose
// contract requires a lock the caller -- not this function -- acquires. Both
// call sites already hold o.mu for their OWN reasons (onPodChange's
// withNamespaceLive+o.mu closure, onDelete's Pod case's lock/unlock around
// o.pods) before this is reached, so taking a second lock here would either
// deadlock (sync.Mutex is not reentrant) or, if it were a different mutex,
// still be pointless serialization for state (o.events) this same lock
// already protects everywhere else in the package.
//
// Not gated by withNamespaceLive: like the Event onDelete case (handlers.go),
// this only ever DELETES entries, never writes a new one, so a stray call
// racing a torn-down namespace's own sweep costs at most a redundant delete
// against an already-absent key.
func (o *Observer) resolveEventsLocked(key stateKey) []bus.ClusterData {
	set := o.events[key]
	if len(set) == 0 {
		return nil
	}
	out := make([]bus.ClusterData, 0, len(set))
	for reason := range set {
		out = append(out, bus.ClusterData{
			Kind:      key.kind,
			Namespace: key.namespace,
			Name:      key.name,
			UID:       string(key.uid),
			Reason:    reason,
			Severity:  bus.SeverityWarn,
			Resolved:  true,
		})
	}
	delete(o.events, key)
	return out
}

// eventResolutionMessage narrates cd's resolution or removal for a caller
// that only has the retained ClusterData, not the original *corev1.Event
// (resolveEventsLocked's own doc comment explains why neither of its two
// callers still has one). verb is "resolved" (onPodChange's recovery path --
// the pod got better) or "removed" (onDelete's Pod case -- the pod is gone,
// it did not recover), matching podMessage/podClusterData's own
// resolved-vs-removed distinction for the identical reason.
func eventResolutionMessage(cd bus.ClusterData, verb string) string {
	subject := cd.Name
	if cd.Namespace != "" {
		subject = fmt.Sprintf("%s/%s", cd.Namespace, cd.Name)
	}
	return fmt.Sprintf("%s: %s %s", subject, cd.Reason, verb)
}

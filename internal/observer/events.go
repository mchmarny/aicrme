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

// eventInvolvedKey identifies the RESOURCE an Event is ABOUT, not the Event
// API object itself: ev.InvolvedObject is what a row on the cockpit
// correlates against (the same Pod a podCondition entry in o.pods might also
// be tracking), so ClusterData.Kind/Namespace/Name/UID (eventClusterData
// below) and this dedupe key both read from InvolvedObject, never from
// ev.ObjectMeta. Reused as o.events' map key alongside the involved
// resource's Reason (o.events[key][ev.Reason]) -- mirrors podKey/stateKey's
// existing shape rather than inventing a second key type.
func eventInvolvedKey(ev *corev1.Event) stateKey {
	io := ev.InvolvedObject
	return stateKey{kind: io.Kind, namespace: io.Namespace, name: io.Name, uid: io.UID}
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

// eventMessage narrates ev against the resource it is about, matching
// podMessage's "namespace/name: detail" shape. ev.Message -- the human
// description the reporting component wrote, e.g. "0/3 nodes are available:
// 3 Insufficient nvidia.com/gpu" -- is appended when present, since
// ev.Reason alone ("FailedScheduling") is a category, not the specific
// blocker an operator needs.
func eventMessage(ev *corev1.Event) string {
	io := ev.InvolvedObject
	subject := io.Name
	if io.Namespace != "" {
		subject = fmt.Sprintf("%s/%s", io.Namespace, io.Name)
	}
	if ev.Message == "" {
		return fmt.Sprintf("%s: %s", subject, ev.Reason)
	}
	return fmt.Sprintf("%s: %s -- %s", subject, ev.Reason, ev.Message)
}

// eventClusterData builds the typed payload for ev, always arising
// (Resolved: false) -- see clearNamespaceEvents and handlers.go's onDelete
// Event case for why nothing in this file ever publishes Resolved: true. A
// Kubernetes Event reports an occurrence, not an ongoing condition with a
// healthy state to return to the way a DaemonSet/Deployment/Node/Pod has one;
// this observer has no signal telling it a Warning's underlying cause has
// cleared, only that this specific Event object no longer exists. Severity is
// always Warn: the field selector (and onEventChange's own defensive check)
// admit only type=Warning events, so there is exactly one severity to assign,
// unlike podReasonSeverity's per-reason ranking.
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

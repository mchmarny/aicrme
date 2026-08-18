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
		Kind: io.Kind,
		// Namespace: io.Namespace (InvolvedObject's), not ev.Namespace (the
		// Event API object's own) -- the resource this payload describes is
		// InvolvedObject, and for a cluster-scoped one (a Node) io.Namespace
		// is correctly "" even though the Event object itself lives in a
		// real namespace. resolveEventsLocked's resolution payload builds
		// Namespace from a DIFFERENT source (key.namespace, i.e.
		// ev.Namespace) for its own reason (M-4, Phase 2b-iii whole-branch
		// review) -- see that function's own comment; the two agree for
		// every case either is ever actually reached with today.
		Namespace: io.Namespace,
		Name:      io.Name,
		UID:       string(io.UID),
		Container: eventContainer(io.FieldPath),
		Reason:    ev.Reason,
		Severity:  bus.SeverityWarn,
		Resolved:  false,
	}
}

// eventDedupe is ONE Warning Reason this observer has already recorded for a
// resource, tracked in Observer.events (observer.go) -- the Event
// counterpart of podCondition, at the smaller granularity resolveEventsLocked
// needs: narrated distinguishes a seeded-only entry (an informer's initial
// list, seedEventBaseline) from one onEventChange actually published as
// arising (Important 1(new), Task 6 fix round 2) -- the exact bit
// podCondition.narrated carries for Pods, restoring the SAME
// resolved-vs-removed asymmetry on the Event side that Ruling 19 already
// established and this file had stopped honoring. container is
// eventContainer's result, captured once at arising/seed time and echoed
// back on resolution (Minor 3, same round) so a row does not lose which
// container failed the moment the condition clears -- resolveEventsLocked
// has no *corev1.Event left to recompute it from by the time either of its
// callers reaches it (see that function's own doc comment).
type eventDedupe struct {
	narrated  bool
	container string
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
			set = make(map[string]eventDedupe)
			o.events[key] = set
		}
		// About to publish below -- narrated: true (Important 1(new)).
		set[ev.Reason] = eventDedupe{narrated: true, container: eventContainer(ev.InvolvedObject.FieldPath)}
		arising = true
	})

	if !arising {
		return
	}
	o.publish(ev.Namespace, eventMessage(ev), eventClusterData(ev))

	// Ruling 26 (Task 6 fix round 2): LEVEL-triggered, not just EDGE-triggered.
	// Everything above resolves an Event-sourced Warning only when a FUTURE
	// Pod transition delivers the recovery (onPodChange's own sweep,
	// Ruling 23) -- but a Warning can narrate for a pod that is ALREADY
	// healthy right now, with no future transition ever coming to clear it
	// (the re-review's own probe: a pod whose only Pod-informer delivery was
	// an initial-list Add seedPodBaseline skips entirely for an
	// already-healthy pod, so o.pods never even gets an entry to prove the
	// pod was ever observed). scopedInformers.currentPod reads the Pod
	// informer's cache directly -- a PULL that answers "is this pod healthy
	// RIGHT NOW", not a memory of the last PUSH this process happened to
	// receive -- so this closes the gap without waiting on a transition that
	// may never arrive. Gated on Kind == Pod: this is the one signal an
	// Event-only informer can independently corroborate against, matching
	// Ruling 23's own Pod-only scope (a DaemonSet/Deployment/Node-involved
	// Warning has no informer cache this package can consult the same way --
	// out of scope, carried to the whole-branch review as Mn1).
	if key.kind != kindPod {
		return
	}
	pod, ok := o.scoped.currentPod(key.namespace, key.name)
	if !ok {
		return
	}
	if _, _, _, troubled := podTrouble(pod); troubled {
		return
	}
	o.mu.Lock()
	resolvedEvents := o.resolveEventsLocked(key, false)
	o.mu.Unlock()
	for _, cd := range resolvedEvents {
		o.publish(ev.Namespace, eventResolutionMessage(cd, "resolved"), cd)
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
// Recorded as narrated: false (Important 1(new), Task 6 fix round 2): a
// silently-seeded entry has not been shown to any consumer, so
// resolveEventsLocked's narratedOnly filter must be able to tell it apart
// from one onEventChange actually published -- see the Observer.events field
// doc comment for the full asymmetry this restores (podCondition.narrated's
// own rule, now honored on the Event side too instead of diverging from it).
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
			set = make(map[string]eventDedupe)
			o.events[key] = set
		}
		set[ev.Reason] = eventDedupe{narrated: false, container: eventContainer(ev.InvolvedObject.FieldPath)}
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
//
// Ruling 32 (Task 6 fix round 3): the SAME reasoning pods.go's onPodAdd doc
// comment states applies here identically -- an initial-list Add is a
// snapshot that predates this process's FIRST-EVER sighting of ns, not
// necessarily this process's only sighting of it. o.scoped.isRestart tells a
// genuine first sighting (still seed silently) from an initial list
// delivered on a namespace this SAME process already tore down before
// (route through onEventChange instead, narrating a Warning that is still
// present after a Retry the exact way a later Add would). See
// pods.go:onPodAdd for the full reasoning shared between both call sites.
func (o *Observer) onEventAdd(obj any, isInInitialList bool) {
	ev, ok := obj.(*corev1.Event)
	if !ok {
		return
	}
	if isInInitialList && !o.scoped.isRestart(ev.Namespace) {
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
// cross-boundary resolution: it returns Resolved: true ClusterData for
// Warning Reasons this observer has recorded about key, then unconditionally
// evicts the WHOLE key from o.events -- key is expected to be podKey(pod)'s
// value, since the only caller-known genuine "this resource is better/gone
// now" signal an Event-only informer never gets on its own is the SAME pod's
// own recovery (pods.go's onPodChange, full recovery) or deletion
// (handlers.go's onDelete Pod case) -- eventInvolvedKey(ev) is byte-identical
// to podKey(pod) for a pod-involved Event, so a Pod-side signal can speak for
// both.
//
// narratedOnly reproduces, on the Event side, the SAME asymmetry
// podCondition.narrated's own doc comment establishes for Pods (Task 6 fix
// round 2, Important 1(new) -- the phantom-resolution defect this restores
// against): onPodChange's full recovery path passes false, because "the pod
// genuinely got better" is a claim this observer CAN back even for an entry
// seedEventBaseline only ever recorded silently (Ruling 19/Minor C) -- every
// entry resolves, narrated or not. handlers.go's onDelete Pod case passes
// true, because a delete cannot make that same claim: the pod did not get
// better, it is gone, and publishing "removed" for a Warning no consumer was
// ever shown would be inventing a claim this observer cannot back either way
// (the exact failure Important 3, Task 5 fix round 1, already fixed for
// o.pods -- this is that fix's Event-side counterpart, not a new rule).
// narratedOnly only filters what is RETURNED for publishing; eviction from
// o.events is unconditional either way, matching onDelete's existing Pod
// case (`delete(o.pods, key)` runs before its own narrated filter too) --
// once the resource is resolved or gone, a seeded-only entry has nothing
// left to serve either.
//
// Built directly from key/reason/eventDedupe, NOT by re-deriving a
// *corev1.Event: by the time either caller reaches here, the original Event
// object (Message, etc.) is long gone from the code path that triggered
// this -- onPodChange has a *corev1.Pod, onDelete's Pod case has a
// *corev1.Pod being deleted, neither has the Event that narrated the arising
// condition. Kind/Namespace/Name/UID come from key, Container from the
// retained eventDedupe (Minor 3, same round) -- together with Reason,
// exactly what ClusterData.Supersedes needs to match against (same UID, same
// Reason) plus what a row would otherwise lose on resolution. Severity is
// fixed at Warn on arising (eventClusterData's own doc comment), so pinning
// it here again is consistent, not a guess.
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
func (o *Observer) resolveEventsLocked(key stateKey, narratedOnly bool) []bus.ClusterData {
	set := o.events[key]
	if len(set) == 0 {
		return nil
	}
	out := make([]bus.ClusterData, 0, len(set))
	for reason, cond := range set {
		if narratedOnly && !cond.narrated {
			continue
		}
		out = append(out, bus.ClusterData{
			Kind: key.kind,
			// Namespace: key.namespace, not io.InvolvedObject.Namespace the
			// way eventClusterData's own arising payload builds it (M-4,
			// Phase 2b-iii whole-branch review) -- key.namespace is
			// eventInvolvedKey's field, which M1 (Task 6) deliberately keys
			// off ev.Namespace (the Event API object's own namespace), not
			// io.Namespace, so the sweep and the write gate agree (M1's own
			// doc comment). The two sources AGREE for every key this
			// function is ever actually called with today: both call sites
			// pass podKey(pod), and for a namespaced Pod ev.Namespace ==
			// io.Namespace always. They would diverge only if this were
			// ever called with a cluster-scoped resource's key (io.Namespace
			// == "" but ev.Namespace is a real namespace, M1's whole
			// point) -- unreachable today because onEventChange's own
			// key.kind != kindPod guard (Ruling 26) means only Pod keys
			// ever reach here.
			Namespace: key.namespace,
			Name:      key.name,
			UID:       string(key.uid),
			Container: cond.container,
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

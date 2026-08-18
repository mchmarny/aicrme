package observer

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/mchmarny/aicrme/internal/bus"
)

// reasonFailedScheduling is the brief's own example Warning reason
// ("FailedScheduling reaches the bus"); used throughout this file rather than
// an arbitrary string so a reader can tell these tests exercise a real
// FailedScheduling-shaped Event, not a synthetic reason invented for the
// test.
const reasonFailedScheduling = "FailedScheduling"

// testEvent builds a Warning- or Normal-typed corev1.Event about
// podTestNamespace/worker-1 (the same fixture pods_test.go's testPod uses),
// so a test exercising both Pod and Event informers -- or comparing this
// file's fixtures against podKey/podCondition's shape -- reads as the SAME
// resource, not two coincidentally similar ones. Reason is always
// reasonFailedScheduling: no test in this file varies it, since dedup keys on
// (resource, Reason) together and every scenario here needs only one Reason
// to exercise. name must be unique per Event object created in a test that
// creates more than one (real Event objects are named uniquely by the
// reporting component, e.g. "worker-1.17d3f9a2b1c4e5f6" -- these fixtures use
// a plain distinguishing suffix instead, since the exact scheme is not
// something this observer depends on).
func testEvent(involvedUID types.UID, name, evType string) *corev1.Event {
	return &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Namespace: podTestNamespace, Name: name},
		InvolvedObject: corev1.ObjectReference{
			Kind:      kindPod,
			Namespace: podTestNamespace,
			Name:      podTestName,
			UID:       involvedUID,
		},
		Reason:  reasonFailedScheduling,
		Type:    evType,
		Count:   1,
		Message: "0/3 nodes are available: 3 Insufficient nvidia.com/gpu",
	}
}

// newScopedEventTestObserver mirrors pods_test.go's newScopedPodTestObserver
// exactly, waiting on the Event informer's own HasSynced rather than the
// Pod's -- see that function's doc comment for why driving scoped.reconcile
// directly, instead of production's bus/ticker-driven run(), is what lets
// these tests avoid reconcileInterval's 2s floor.
func newScopedEventTestObserver(t *testing.T, client *fake.Clientset) (*Observer, <-chan bus.Event) {
	t.Helper()
	b := bus.New(64)
	sub, unsub := b.Subscribe(0)
	t.Cleanup(unsub)

	o := New(client, b, func() RunScope {
		return RunScope{RunID: "run-1", Namespaces: map[string]struct{}{podTestNamespace: {}}}
	})
	o.scoped.reconcile(scopeWith("run-1", podTestNamespace))
	t.Cleanup(o.scoped.stop)

	entry, ok := o.scoped.entries[podTestNamespace]
	if !ok {
		t.Fatalf("no scoped entry for namespace %q after reconcile", podTestNamespace)
	}
	waitFor(t, entry.event.HasSynced)

	return o, sub
}

// TestEventNarratesWarnings is the brief's first required test: a Warning
// event (FailedScheduling, the brief's own example) reaches the bus, with the
// typed payload -- not just a message string -- carrying the resource it is
// about. FieldPath is set here (unlike this file's other fixtures) so this
// test also exercises eventContainer's "spec.containers{name}" parsing --
// otherwise nothing in this package would ever drive that branch away from
// its empty-container default.
func TestEventNarratesWarnings(t *testing.T) {
	o, sub := newTestObserver(t)
	ev := testEvent("pod-uid", "worker-1.a", corev1.EventTypeWarning)
	ev.InvolvedObject.FieldPath = "spec.containers{trainer}"

	o.onEventAdd(ev, false) // a later Add -- see TestEventArrivingOnlyAsAddIsNarrated for why this must narrate

	cd := decodeClusterData(t, sub)
	if cd.Kind != kindPod {
		t.Errorf("Kind = %q, want %q", cd.Kind, kindPod)
	}
	if cd.UID != "pod-uid" {
		t.Errorf("UID = %q, want %q", cd.UID, "pod-uid")
	}
	if cd.Reason != reasonFailedScheduling {
		t.Errorf("Reason = %q, want %q", cd.Reason, reasonFailedScheduling)
	}
	if cd.Container != "trainer" {
		t.Errorf("Container = %q, want %q", cd.Container, "trainer")
	}
	if cd.Severity != bus.SeverityWarn {
		t.Errorf("Severity = %v, want %v", cd.Severity, bus.SeverityWarn)
	}
	if cd.Resolved {
		t.Error("Resolved = true, want false: this is the condition arising, not clearing")
	}
}

// TestEventIgnoresNormalType pins the client-side defensive check
// (onEventChange's ev.Type != Warning guard): the fake clientset used
// throughout this file does not enforce eventWarningFieldSelector at all
// (k8s.io/client-go/testing's ObjectTracker.List ignores FieldSelector
// entirely -- verified against the pinned client-go source), so this is the
// only thing standing between a Normal event and a false narration in these
// tests, matching what would also protect a real cluster if a future API
// server ever stopped honoring the selector server-side.
func TestEventIgnoresNormalType(t *testing.T) {
	o, sub := newTestObserver(t)
	ev := testEvent("pod-uid", "worker-1.a", corev1.EventTypeNormal)

	o.onEventAdd(ev, false)

	assertNoEvent(t, sub)
}

// TestEventDedupesOnUIDAndReason: two DISTINCT Event API objects (different
// names/identities, as Kubernetes would create if the first's correlation
// window had already lapsed) reporting the SAME reason for the SAME
// InvolvedObject narrate once, not twice -- dedup keys on the resource the
// Event is ABOUT and its Reason, not on the Event object's own identity.
func TestEventDedupesOnUIDAndReason(t *testing.T) {
	o, sub := newTestObserver(t)
	first := testEvent("pod-uid", "worker-1.a", corev1.EventTypeWarning)
	second := testEvent("pod-uid", "worker-1.b", corev1.EventTypeWarning)

	o.onEventAdd(first, false)
	decodeClusterData(t, sub) // the one narration

	o.onEventAdd(second, false)
	assertNoEvent(t, sub)
}

// TestEventDoesNotReEmitOnCountIncrease: Kubernetes coalesces a recurrence
// into the SAME Event object by bumping Count/LastTimestamp (an Update, not a
// new Add) precisely so consumers need not re-narrate it (spec Section 3) --
// a rising count is not a new event.
func TestEventDoesNotReEmitOnCountIncrease(t *testing.T) {
	o, sub := newTestObserver(t)
	ev := testEvent("pod-uid", "worker-1.a", corev1.EventTypeWarning)

	o.onEventAdd(ev, false)
	decodeClusterData(t, sub)

	recurred := ev.DeepCopy()
	recurred.Count = 5
	recurred.LastTimestamp = metav1.Now()
	o.onEventUpdate(ev, recurred)

	assertNoEvent(t, sub)
}

// TestEventDedupeIsClearedOnResourceDeletion is the brief's test 5: the
// dedupe map must not grow for the process lifetime. handlers.go's onDelete
// (Event case) is reached when the Event API OBJECT itself is deleted --
// Kubernetes TTL-garbage-collects Events independently of the resource they
// describe (--event-ttl, 1h by default; Events carry no owner reference back
// to InvolvedObject) -- so this is the only deletion signal an Event-only
// informer ever receives, and hooking o.events' eviction to it is what bounds
// the map: a resource that keeps generating the SAME reason indefinitely,
// each occurrence's Event object eventually TTL-evicted and replaced by a
// fresh one, gets a fresh dedupe entry each time rather than piling up
// entries under keys nothing ever revisits.
func TestEventDedupeIsClearedOnResourceDeletion(t *testing.T) {
	o, sub := newTestObserver(t)
	ev := testEvent("pod-uid", "worker-1.a", corev1.EventTypeWarning)

	o.onEventAdd(ev, false)
	decodeClusterData(t, sub)

	o.onDelete(ev) // the Event object ages out of etcd (TTL), not a resolution
	assertNoEvent(t, sub)

	o.mu.Lock()
	after := len(o.events)
	o.mu.Unlock()
	if after != 0 {
		t.Fatalf("o.events after the Event object's deletion = %d entries, want 0 -- the map must not grow for the process lifetime", after)
	}

	// A fresh Event object (new identity), same InvolvedObject and Reason --
	// exactly what Kubernetes creates once the old object's correlation
	// window has lapsed. If the dedupe entry survived the delete above, this
	// would be silently swallowed and the assertion below would time out.
	recurrence := testEvent("pod-uid", "worker-1.b", corev1.EventTypeWarning)
	o.onEventAdd(recurrence, false)

	cd := decodeClusterData(t, sub)
	if cd.Reason != reasonFailedScheduling {
		t.Errorf("Reason = %q, want %q", cd.Reason, reasonFailedScheduling)
	}
	if cd.Resolved {
		t.Error("Resolved = true, want false: this is a fresh arising, not the earlier one resolving")
	}
}

// TestEventDedupeDoesNotSurviveARunBoundary is the brief's test 6: a new run
// does not inherit the previous run's dedupe state. Renamed from
// TestEventDedupeIsBoundedByGeneration (M3, Task 6 fix round 1, review's
// finding): the original name promised a mechanism this test does not
// exercise -- it reads no Generation anywhere. Keeping a name that traces
// back to a requirement the review (and the coordinator, upheld above)
// confirmed was itself wrong would mislead a CI failure list or a
// Generation grep even though the 28-line comment below it (unchanged from
// before the rename) already explains the divergence honestly to anyone who
// opens the file.
//
// This drives the run boundary through scoped.reconcile with a CHANGED
// RunID, not by constructing a RunScope with a different Generation value
// directly: engine.Attribution.Generation (internal/engine/attribution.go)
// advances on every ActiveAction transition WITHIN a run, not at run
// boundaries -- confirmed by reading setActiveAction/clearActiveAction's own
// call sites (internal/engine/engine.go), which fire roughly twice per
// deployment action. A test (or an implementation) that reset o.events on
// every Generation change would defeat dedup within a single ordinary run,
// not just across runs. RunID is the field that actually marks a run
// boundary (scoped.go's own package doc, Ruling 6/8), and scoped.reconcile
// already tears down and rebuilds every namespace's factories on exactly
// that signal (stopAllLocked, invoking clearNamespaceEvents via
// onNamespaceStop) -- this test drives that real mechanism rather than
// asserting against a value this package never reads for this purpose. See
// clearNamespaceEvents' own doc comment (observer.go) for the full
// reasoning, including why this diverges from a literal reading of the
// brief's "RunScope carries Generation for this".
func TestEventDedupeDoesNotSurviveARunBoundary(t *testing.T) {
	client := fake.NewSimpleClientset()
	b := bus.New(64)
	o := New(client, b, func() RunScope {
		return RunScope{RunID: "run-1", Namespaces: map[string]struct{}{podTestNamespace: {}}}
	})
	sub, unsub := b.Subscribe(0)
	t.Cleanup(unsub)

	o.scoped.reconcile(scopeWith("run-1", podTestNamespace))
	t.Cleanup(o.scoped.stop)

	ev := testEvent("pod-uid", "worker-1.a", corev1.EventTypeWarning)
	o.onEventAdd(ev, false)
	decodeClusterData(t, sub)

	// A new run, same namespace: reconcile tears down run-1's factory
	// (clearNamespaceEvents fires via onNamespaceStop) before starting
	// run-2's fresh one, within this single reconcile call.
	o.scoped.reconcile(scopeWith("run-2", podTestNamespace))

	recurrence := testEvent("pod-uid", "worker-1.b", corev1.EventTypeWarning)
	o.onEventAdd(recurrence, false)

	cd := decodeClusterData(t, sub)
	if cd.Reason != reasonFailedScheduling {
		t.Errorf("Reason = %q, want %q -- run-2 inherited run-1's dedupe state", cd.Reason, reasonFailedScheduling)
	}
}

// TestEventArrivingOnlyAsAddIsNarrated is the brief's test 7 and "the case
// initial-list suppression would swallow" -- the whole point of this task.
// Kubernetes creates an Event once and never updates it for its first
// occurrence, so a Warning genuinely arrives as an AddFunc call with
// isInInitialList == false, never as an Update. This drives a REAL informer
// (unlike most of this file's tests, which call the handlers directly) so
// the isInInitialList distinction is exercised by client-go itself, not
// simulated: Create() happens AFTER the informer has already synced, so
// client-go delivers it as a genuine watch Add.
//
// The required bite-proof: remove DetailedFuncs (register plain
// ResourceEventHandlerFuncs instead, the way Observer.register does for the
// three cluster-scoped kinds in handlers.go) and this test fails, because
// nothing distinguishes a later Add needing narration from an initial-list
// Add that must stay silent -- see events.go's onEventAdd doc comment for
// why copying handlers.go's onAdd posture (record on every Add, narrate only
// on Update) would emit nothing at all for Events.
func TestEventArrivingOnlyAsAddIsNarrated(t *testing.T) {
	client := fake.NewSimpleClientset()
	_, sub := newScopedEventTestObserver(t, client)

	ev := testEvent("pod-uid", "worker-1.a", corev1.EventTypeWarning)
	if _, err := client.CoreV1().Events(podTestNamespace).Create(context.Background(), ev, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	cd := waitForClusterData(t, sub)
	if cd.Reason != reasonFailedScheduling {
		t.Errorf("Reason = %q, want %q", cd.Reason, reasonFailedScheduling)
	}
	if cd.UID != "pod-uid" {
		t.Errorf("UID = %q, want %q", cd.UID, "pod-uid")
	}
	if cd.Resolved {
		t.Error("Resolved = true, want false")
	}
}

// TestEventInitialListDoesNotNarrate is TestEventArrivingOnlyAsAddIsNarrated's
// required sibling, matching TestPodInitialListDoesNotNarrate's role for
// Pods: an informer's initial list delivers every pre-existing Warning as an
// Add too, and narrating those would flood the timeline with the cluster's
// entire pre-existing Warning history at process start. Without this test,
// an implementation that dropped the isInInitialList branch entirely (always
// treating an Add as a later Add) would still pass every OTHER test in this
// file.
func TestEventInitialListDoesNotNarrate(t *testing.T) {
	preexisting := testEvent("pod-uid", "worker-1.a", corev1.EventTypeWarning)
	client := fake.NewSimpleClientset(preexisting)

	_, sub := newScopedEventTestObserver(t, client)

	if events := collectPodEvents(sub); len(events) != 0 {
		t.Fatalf("published %d events for the initial list, want 0: %+v", len(events), events)
	}
}

// TestEventStillPresentAcrossRetryIsReNarrated is Ruling 32's (Task 6 fix
// round 3) Event-side counterpart to pods_test.go's
// TestPodStillBrokenAcrossRetryIsReNarrated -- the coordinator's explicit
// requirement that seedEventBaseline, which has the identical
// isInInitialList shape as seedPodBaseline, gets the identical fix and the
// identical test rather than being assumed to work by analogy.
//
// Walks the same full sequence: a Warning predates this process's
// first-ever sighting of the namespace (silent seed, matching
// TestEventInitialListDoesNotNarrate exactly) -> the run fails and tears the
// namespace down (o.events cleared, namespace marked as torn down) -> retry
// with the SAME RunID restarts the informers fresh -> the Event API object
// is UNCHANGED in the fake clientset, so the restarted informer's initial
// list is this process's only remaining opportunity to learn about it -> it
// narrates instead of being silently re-seeded. Also confirms the round-2
// narrated bookkeeping: deleting the involved pod afterward publishes
// "removed" for it, which onDelete's Pod case only does (via
// resolveEventsLocked's narratedOnly filter) for an entry recorded with
// narrated: true -- so onEventChange, not seedEventBaseline, must be what
// recorded this one.
func TestEventStillPresentAcrossRetryIsReNarrated(t *testing.T) {
	ev := testEvent("pod-uid", "worker-1.a", corev1.EventTypeWarning)
	client := fake.NewSimpleClientset(ev)
	o, sub := newScopedEventTestObserver(t, client)

	// First-ever start: matches TestEventInitialListDoesNotNarrate exactly
	// -- this must stay silent.
	if events := collectPodEvents(sub); len(events) != 0 {
		t.Fatalf("published %d events on the first-ever start, want 0 (silent seed): %+v", len(events), events)
	}

	// The run fails: teardown clears o.events and marks the namespace as
	// having been torn down.
	o.scoped.reconcile(terminalScope(podTestNamespace))

	// Retry: same RunID, Terminal now false -- restarts the namespace's
	// informers fresh. The Event object in the fake clientset is untouched,
	// so the new informer's initial list delivers the SAME still-present
	// Warning again.
	o.scoped.reconcile(scopeWith("run-1", podTestNamespace))
	entry, ok := o.scoped.entries[podTestNamespace]
	if !ok {
		t.Fatalf("no scoped entry for namespace %q after retry reconcile", podTestNamespace)
	}
	waitFor(t, entry.event.HasSynced)

	cd := waitForClusterData(t, sub)
	if cd.Reason != reasonFailedScheduling {
		t.Errorf("Reason = %q, want %q", cd.Reason, reasonFailedScheduling)
	}
	if cd.UID != "pod-uid" {
		t.Errorf("UID = %q, want %q", cd.UID, "pod-uid")
	}
	if cd.Resolved {
		t.Error("Resolved = true, want false: this is a re-arising, not a resolution")
	}

	// narrated bookkeeping (Task 6 fix round 2): the re-narrated entry must
	// be marked narrated, or this delete would publish nothing for it.
	o.onDelete(testPod("pod-uid"))
	removed := waitForClusterData(t, sub)
	if removed.Reason != reasonFailedScheduling {
		t.Errorf("removed Reason = %q, want %q", removed.Reason, reasonFailedScheduling)
	}
	if !removed.Resolved {
		t.Error("removed Resolved = false, want true")
	}
}

// TestEventFieldSelectorAppliesOnlyToTheEventFactory verifies Step 2's
// server-side filter (informers.WithTweakListOptions setting
// FieldSelector: "type=Warning") reaches the Event factory's List/Watch
// calls and ONLY those -- not the Pod factory's, which is Important 2's
// exact failure mode (Task 5 fix round 1): a shared factory's
// WithTweakListOptions applies to every informer built from it, and `type`
// is not a valid Pod field selector, so leaking this tweak onto the Pod
// ListWatch would make the API server reject it and the Pod informer would
// never sync. Unlike TestScopedInformersStartOncePerNamespace's pointer
// comparison (which only proves the two factories are DISTINCT objects),
// this reads client.Actions() directly -- the actual FieldSelector each
// call carried -- so it fails under a mutation that shares one factory
// between Pod and Event just as surely as it fails under one that forgets
// the tweak entirely.
func TestEventFieldSelectorAppliesOnlyToTheEventFactory(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := newScopedInformers(client, scopedHandlers{}, nil)

	s.reconcile(scopeWith("run-1", podTestNamespace))
	waitFor(t, func() bool { return len(client.Actions()) > 0 })

	var sawEventAction, sawPodAction bool
	for _, a := range client.Actions() {
		var fieldSelector string
		switch act := a.(type) {
		case k8stesting.WatchActionImpl:
			fieldSelector = act.GetWatchRestrictions().Fields.String()
		case k8stesting.ListActionImpl:
			fieldSelector = act.GetListRestrictions().Fields.String()
		default:
			continue
		}

		switch a.GetResource().Resource {
		case "events":
			sawEventAction = true
			if fieldSelector != eventWarningFieldSelector {
				t.Errorf("events %s action FieldSelector = %q, want %q", a.GetVerb(), fieldSelector, eventWarningFieldSelector)
			}
		case "pods":
			sawPodAction = true
			if fieldSelector != "" {
				t.Errorf("pods %s action FieldSelector = %q, want empty -- the Event-only tweak leaked onto the Pod ListWatch", a.GetVerb(), fieldSelector)
			}
		}
	}
	if !sawEventAction {
		t.Fatal("no events List/Watch action recorded against the client -- test did not exercise the Event factory")
	}
	if !sawPodAction {
		t.Fatal("no pods List/Watch action recorded against the client -- test did not exercise the Pod factory")
	}
}

// TestEventDedupeDoesNotStrandUnderConcurrentTeardown is this file's version
// of Task 5's Important B probe (TestPodTroubleDoesNotStrandUnderConcurrentTeardown):
// many Warning events created back to back with NO synchronization point
// before an immediate terminal reconcile, repeated enough times that a racy
// onEventChange/seedEventBaseline (missing the withNamespaceLive gate the
// brief calls out as mandatory) would show stranding with overwhelming
// probability, the way Task 5's own equivalent did on 23 of 25 attempts
// before its fix.
func TestEventDedupeDoesNotStrandUnderConcurrentTeardown(t *testing.T) {
	const eventCount = 40
	const attempts = 25

	for attempt := 0; attempt < attempts; attempt++ {
		client := fake.NewSimpleClientset()
		o, _ := newScopedEventTestObserver(t, client)

		for i := 0; i < eventCount; i++ {
			ev := testEvent(
				types.UID(fmt.Sprintf("pod-uid-%d-%d", attempt, i)),
				fmt.Sprintf("worker-%d.a", i),
				corev1.EventTypeWarning)
			ev.InvolvedObject.Name = fmt.Sprintf("worker-%d", i)
			if _, err := client.CoreV1().Events(podTestNamespace).Create(context.Background(), ev, metav1.CreateOptions{}); err != nil {
				t.Fatalf("attempt %d: Create() error = %v", attempt, err)
			}
		}

		// M5 (Task 6 fix round 1), fixed under C1 (Task 6 fix round 2): before
		// the immediate teardown below, prove this attempt actually wrote
		// SOMETHING into o.events -- an implementation that never wrote at
		// all (e.g. onEventChange gutted entirely) would otherwise pass
		// len(o.events) == 0 after teardown just as easily as a
		// correctly-gated one does, and nothing else in this test would
		// catch that. Not gated on ALL eventCount having landed -- only that
		// the write path fired at least once -- so this still leaves the
		// race the rest of the test exists to probe (whether EVERY
		// delivery, including ones still in flight, survives the immediate
		// reconcile below) genuinely open.
		//
		// spinUntil, not a bare read: the original M5 fix read len(o.events)
		// exactly once, with no wait, immediately after the 40 Create()
		// calls above return. Those calls only enqueue work for the
		// informer's own watch-delivery goroutine -- they do not wait for
		// it -- so "zero deliveries have landed yet" was a routine, frequent
		// outcome on entirely correct code, not just a rare edge: measured
		// at 26/30 failures without -race and 7/30 with -race on shipped,
		// unmutated code (Task 6 fix round 2 re-review). spinUntil returns
		// the INSTANT the first entry lands rather than sampling once, which
		// is exactly the fix already applied to this file's two seed-strand
		// tests -- this was the one write-proof check in the package that
		// hadn't been converted.
		spinUntil(t, "TestEventDedupeDoesNotStrandUnderConcurrentTeardown: waiting for the first delivered entry", func() bool {
			o.mu.Lock()
			defer o.mu.Unlock()
			return len(o.events) > 0
		})

		// No synchronization point here -- this IS the shape that bites: an
		// immediate terminal reconcile with event creation still (possibly)
		// in flight through the informer.
		o.scoped.reconcile(terminalScope(podTestNamespace))

		// Give any straggling delivery every opportunity to land before
		// checking -- a flaky pass here would UNDERSTATE the race, never
		// overstate it.
		time.Sleep(20 * time.Millisecond)

		o.mu.Lock()
		after := len(o.events)
		stranded := make(map[stateKey]map[string]eventDedupe, len(o.events))
		for k, v := range o.events {
			stranded[k] = v
		}
		o.mu.Unlock()
		if after != 0 {
			t.Fatalf("attempt %d: o.events after concurrent teardown = %d entries, want 0 -- stranded: %+v", attempt, after, stranded)
		}
	}
}

// TestEventSourcedWarningResolvesWhenThePodRecovers is Important 1 / Ruling
// 23 (Task 6 fix round 1): a Warning this observer narrated from the Event
// informer must resolve when the pod it is about fully recovers, the same
// way a podCondition does -- otherwise transient FailedScheduling, the norm
// while a GPU cluster scales rather than an edge case, pins a permanent
// Warn-severity row on a pod that went on to become fully healthy.
//
// Walks the full SEQUENCE the coordinator asked for, not a single condition
// in isolation -- this stranding class (a condition tracked under one
// identity, resolved only through a signal that arrives under a DIFFERENT
// identity) has now surfaced four times, and every prior time a test that
// exercised only one condition in isolation missed it:
//  1. Unschedulable arises via the Pod informer (pods.go).
//  2. FailedScheduling arises via the Event informer (events.go) for the
//     SAME resource identity (eventInvolvedKey(ev) == podKey(pod)).
//  3. The pod fully recovers -- a single onPodUpdate call.
//
// The mutation this fails under: removing the
// `resolvedEvents = o.resolveEventsLocked(key, false)` call from
// onPodChange's full-recovery branch (pods.go). Without it, step 3 publishes
// only the Unschedulable resolution; the second decodeClusterData call below
// would hit "no event published" and fail, and o.events would still hold
// the FailedScheduling entry afterward.
func TestEventSourcedWarningResolvesWhenThePodRecovers(t *testing.T) {
	o, sub := newTestObserver(t)

	o.onPodUpdate(nil, withUnschedulable(testPod("pod-uid")))
	podArose := decodeClusterData(t, sub)
	if podArose.Reason != reasonUnschedulable || podArose.Resolved {
		t.Fatalf("setup: Unschedulable arising = %+v, want Reason=%q Resolved=false", podArose, reasonUnschedulable)
	}

	ev := testEvent("pod-uid", "worker-1.a", corev1.EventTypeWarning)
	o.onEventAdd(ev, false)
	eventArose := decodeClusterData(t, sub)
	if eventArose.Reason != reasonFailedScheduling || eventArose.Resolved {
		t.Fatalf("setup: FailedScheduling arising = %+v, want Reason=%q Resolved=false", eventArose, reasonFailedScheduling)
	}

	o.onPodUpdate(nil, withRunning(testPod("pod-uid"))) // full recovery

	resolved := map[string]bus.ClusterData{}
	for range 2 {
		cd := decodeClusterData(t, sub)
		resolved[cd.Reason] = cd
	}
	assertNoEvent(t, sub) // exactly two resolves, nothing more

	for _, reason := range []string{reasonUnschedulable, reasonFailedScheduling} {
		cd, ok := resolved[reason]
		if !ok {
			t.Errorf("no resolve event published for %q -- stranded across the Pod/Event boundary", reason)
			continue
		}
		if !cd.Resolved {
			t.Errorf("%q Resolved = false, want true", reason)
		}
		if cd.UID != "pod-uid" {
			t.Errorf("%q UID = %q, want %q", reason, cd.UID, "pod-uid")
		}
	}

	o.mu.Lock()
	remaining := len(o.events)
	o.mu.Unlock()
	if remaining != 0 {
		t.Errorf("o.events after full recovery = %d entries, want 0 -- the FailedScheduling entry was left stranded", remaining)
	}
}

// TestEventSourcedWarningResolvesWhenThePodIsDeleted is M2 (Task 6 fix round
// 1): the deleted-Pod signal is exactly as valid a "clear this Event-sourced
// Warning" trigger as the Pod's own recovery is (both route through
// resolveEventsLocked keyed identically), but the VERB differs -- "removed",
// not "resolved", matching podClusterData/podMessage's own removed-vs-
// resolved distinction for the identical reason (the pod did not get
// better, it is gone).
func TestEventSourcedWarningResolvesWhenThePodIsDeleted(t *testing.T) {
	o, sub := newTestObserver(t)

	pod := withContainerWaiting(testPod("pod-uid"), "app", reasonImagePullBackOff)
	o.onPodAdd(pod, false) // later Add -- narrates
	decodeClusterData(t, sub)

	ev := testEvent("pod-uid", "worker-1.a", corev1.EventTypeWarning)
	o.onEventAdd(ev, false)
	decodeClusterData(t, sub)

	o.onDelete(pod)

	removed := map[string]bus.ClusterData{}
	for range 2 {
		cd := decodeClusterData(t, sub)
		removed[cd.Reason] = cd
	}
	assertNoEvent(t, sub)

	for _, reason := range []string{reasonImagePullBackOff, reasonFailedScheduling} {
		cd, ok := removed[reason]
		if !ok {
			t.Errorf("delete did not publish a removal for %q -- stranded", reason)
			continue
		}
		if !cd.Resolved {
			t.Errorf("%q Resolved = false, want true", reason)
		}
	}

	o.mu.Lock()
	remaining := len(o.events)
	o.mu.Unlock()
	if remaining != 0 {
		t.Errorf("o.events after pod deletion = %d entries, want 0, spec Section 3's \"cleaned on resource deletion\"", remaining)
	}
}

// TestEventDeleteOfASeededButNeverNarratedTroublePublishesNothing is
// Important 1(new)'s required sibling (Task 6 fix round 2, re-review): the
// Event-side counterpart of pods_test.go's
// TestPodDeleteOfASeededButNeverNarratedTroublePublishesNothing (Ruling
// 15/Important 3, Task 5 fix round 1) -- the SAME rule, now needed on the
// Event side too since Ruling 23's resolveEventsLocked reintroduced the
// phantom-resolution defect that rule exists to close. A Warning seeded
// silently from an informer's initial list (isInInitialList == true) was
// never shown to any consumer; the pod it is about being deleted afterward
// must not manufacture a "removed" event for it -- reachable on every
// process start against a cluster with pre-existing Warnings, and on every
// engine.Retry (whose restarted informer re-seeds a still-present Event
// object's initial list, per the round-1 review's own real-informer probe).
//
// o.events IS still evicted (map hygiene, unconditional in
// resolveEventsLocked) even though nothing publishes -- asserted directly,
// not just inferred from the absence of a published event.
func TestEventDeleteOfASeededButNeverNarratedTroublePublishesNothing(t *testing.T) {
	o, sub := newTestObserver(t)
	pod := testPod("pod-uid")
	ev := testEvent("pod-uid", "worker-1.a", corev1.EventTypeWarning)

	o.onEventAdd(ev, true) // initial list: seeds the baseline, does not narrate
	assertNoEvent(t, sub)

	o.onDelete(pod)

	assertNoEvent(t, sub)

	o.mu.Lock()
	remaining := len(o.events)
	o.mu.Unlock()
	if remaining != 0 {
		t.Errorf("o.events after pod deletion = %d entries, want 0 -- a seeded-only entry must still be evicted, just never published", remaining)
	}
}

// TestEventSourcedWarningSeededOnlyStillResolvesOnPodRecovery pins the OTHER
// half of the asymmetry TestEventDeleteOfASeededButNeverNarratedTroublePublishesNothing
// just pinned (Task 6 fix round 2, re-review's own distinction, upholding
// Ruling 19/Minor C): unlike delete, a pod's own recovery MAY resolve a
// seeded-only (never narrated) Event-sourced Warning -- "the thing I never
// mentioned is fixed now" is a claim this observer can back for an Event
// entry exactly as it already can for a seeded-only podCondition
// (onPodChange's own recovery sweep deliberately does not filter on
// narrated either). A test that only exercised delete's side of this
// asymmetry could not tell a correct narratedOnly=false at the recovery call
// site apart from an accidentally-also-true one -- this is that check.
//
// Also walks the case the re-review named as untested: a pod with NO
// tracked o.pods trouble at all (no Unschedulable, nothing) whose ONLY
// signal is a seeded FailedScheduling -- the common real shape, and the
// `!ok` branch in onPodChange already runs unconditionally for it.
func TestEventSourcedWarningSeededOnlyStillResolvesOnPodRecovery(t *testing.T) {
	o, sub := newTestObserver(t)

	ev := testEvent("pod-uid", "worker-1.a", corev1.EventTypeWarning)
	o.onEventAdd(ev, true) // initial list: seeds the baseline, does not narrate
	assertNoEvent(t, sub)

	o.onPodUpdate(nil, withRunning(testPod("pod-uid"))) // full recovery, no prior o.pods entry

	cd := decodeClusterData(t, sub)
	if cd.Reason != reasonFailedScheduling {
		t.Errorf("Reason = %q, want %q", cd.Reason, reasonFailedScheduling)
	}
	if !cd.Resolved {
		t.Error("Resolved = false, want true -- a seeded-only entry must still resolve on genuine recovery")
	}
	assertNoEvent(t, sub)

	o.mu.Lock()
	remaining := len(o.events)
	o.mu.Unlock()
	if remaining != 0 {
		t.Errorf("o.events after full recovery = %d entries, want 0", remaining)
	}
}

// TestEventResolutionCarriesContainer pins Minor 3 (Task 6 fix round 2,
// re-review): resolveEventsLocked used to drop Container entirely on
// resolution (arose with "trainer", resolved with ""), which Supersedes
// tolerates (it matches on UID/Reason only) but a row rendering Container
// would silently lose which container failed the instant the condition
// cleared. eventDedupe now carries container alongside narrated, captured
// once at arising/seed time and echoed back here.
func TestEventResolutionCarriesContainer(t *testing.T) {
	o, sub := newTestObserver(t)

	ev := testEvent("pod-uid", "worker-1.a", corev1.EventTypeWarning)
	ev.InvolvedObject.FieldPath = "spec.containers{trainer}"
	o.onEventAdd(ev, false) // later Add -- narrates
	arose := decodeClusterData(t, sub)
	if arose.Container != "trainer" {
		t.Fatalf("setup: arising Container = %q, want %q", arose.Container, "trainer")
	}

	pod := testPod("pod-uid")
	o.onDelete(pod)

	resolved := decodeClusterData(t, sub)
	if resolved.Container != "trainer" {
		t.Errorf("resolved Container = %q, want %q -- dropped on resolution", resolved.Container, "trainer")
	}
	if !resolved.Resolved {
		t.Error("Resolved = false, want true")
	}
}

// TestEventNarratedForAnAlreadyHealthyPodResolvesImmediately pins Ruling 26
// (Task 6 fix round 2, re-review's Minor 2): resolution used to be purely
// EDGE-triggered -- it only fired off a FUTURE Pod transition (onPodChange's
// recovery sweep, Ruling 23) -- so a Warning that narrates for a pod that is
// ALREADY healthy, with no future transition ever coming to deliver the
// edge, stranded forever. The re-review's own probe: a pod whose only
// Pod-informer delivery was an initial-list Add for an already-healthy pod
// (seedPodBaseline's own `if !ok { return }` records nothing for it, so
// o.pods gets no entry proving the pod was ever observed) followed by a
// later Event Add -- narrated Resolved:false, nothing left to ever clear it.
//
// Drives REAL Pod and Event informers (unlike most of this file's tests,
// which call handlers directly): the fix reads the Pod informer's cache
// directly (scopedInformers.currentPod), so it needs a real
// cache.SharedIndexInformer behind entry.pod, not the bare *factoryEntry
// newTestObserver seeds. The healthy pod is pre-loaded into the fake
// clientset BEFORE reconcile starts the namespace, so it arrives via the
// Pod informer's initial list and is fully synced (waited for explicitly,
// unlike newScopedEventTestObserver's helper, which only waits on the Event
// informer) before the Warning Event is created.
func TestEventNarratedForAnAlreadyHealthyPodResolvesImmediately(t *testing.T) {
	client := fake.NewSimpleClientset(withRunning(testPod("pod-uid")))
	o, sub := newScopedEventTestObserver(t, client)
	waitFor(t, o.scoped.entries[podTestNamespace].pod.HasSynced)

	ev := testEvent("pod-uid", "worker-1.a", corev1.EventTypeWarning)
	if _, err := client.CoreV1().Events(podTestNamespace).Create(context.Background(), ev, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	arose := waitForClusterData(t, sub)
	if arose.Reason != reasonFailedScheduling {
		t.Fatalf("setup: arising Reason = %q, want %q", arose.Reason, reasonFailedScheduling)
	}
	if arose.Resolved {
		t.Fatal("setup: arising Resolved = true, want false")
	}

	resolved := waitForClusterData(t, sub)
	if resolved.Reason != reasonFailedScheduling {
		t.Errorf("resolved Reason = %q, want %q", resolved.Reason, reasonFailedScheduling)
	}
	if resolved.UID != "pod-uid" {
		t.Errorf("resolved UID = %q, want %q", resolved.UID, "pod-uid")
	}
	if !resolved.Resolved {
		t.Error("resolved Resolved = false, want true -- the pod is already healthy, nothing should be left to strand")
	}

	o.mu.Lock()
	remaining := len(o.events)
	o.mu.Unlock()
	if remaining != 0 {
		t.Errorf("o.events after immediate resolution = %d entries, want 0", remaining)
	}
}

// TestEventAboutAClusterScopedResourceDoesNotStrand pins M1 (Task 6 fix
// round 1): a Warning about a cluster-scoped InvolvedObject (a Node --
// io.Namespace == "") is still delivered as an Event API OBJECT that lives
// in a real namespace ("default", per client-go's own event.go substituting
// NamespaceDefault for an empty ref.Namespace) -- ev.Namespace, not
// io.Namespace. eventInvolvedKey's namespace field must track ev.Namespace
// so the SAME namespace value the write gate admitted the entry under is
// also what clearNamespaceEvents' sweep later matches it against; keying on
// io.Namespace ("") instead left an entry the sweep could never reach, since
// no namespace name is ever the empty string.
//
// Not reachable in production today (no shipped recipe names "default" as a
// component namespace), but exercised directly here at the unit level
// rather than left latent.
func TestEventAboutAClusterScopedResourceDoesNotStrand(t *testing.T) {
	const clusterEventNamespace = "default"

	b := bus.New(64)
	o := New(nil, b, func() RunScope {
		return RunScope{RunID: "run-1", Namespaces: map[string]struct{}{clusterEventNamespace: {}}}
	})
	sub, unsub := b.Subscribe(0)
	t.Cleanup(unsub)
	o.scoped.entries[clusterEventNamespace] = &factoryEntry{stop: make(chan struct{})}

	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Namespace: clusterEventNamespace, Name: "gpu-node-1.a"},
		InvolvedObject: corev1.ObjectReference{
			Kind: kindNode,
			Name: "gpu-node-1",
			UID:  "node-uid",
		},
		Reason: "NodeNotReady",
		Type:   corev1.EventTypeWarning,
	}

	o.onEventAdd(ev, false)
	decodeClusterData(t, sub)

	o.mu.Lock()
	before := len(o.events)
	o.mu.Unlock()
	if before != 1 {
		t.Fatalf("o.events before teardown = %d, want 1; test setup failure", before)
	}

	o.scoped.reconcile(terminalScope(clusterEventNamespace))

	o.mu.Lock()
	after := len(o.events)
	o.mu.Unlock()
	if after != 0 {
		t.Errorf("o.events after the namespace's informer tore down = %d, want 0 -- a cluster-scoped resource's Event strands permanently", after)
	}
}

// TestEventSeedDoesNotStrandUnderConcurrentTeardown is Important 2's Event
// half (Task 6 fix round 1, review's control probe): seedEventBaseline's
// withNamespaceLive gate was entirely unpinned -- removing it (numstat
// `2 2`) left the whole package green, because
// TestEventDedupeDoesNotStrandUnderConcurrentTeardown creates every Event
// AFTER HasSynced, so only onEventChange (a later Add) is ever exercised,
// never the initial-list seed path.
//
// This pre-loads the fake clientset BEFORE reconcile ever starts the
// namespace, so every Event arrives via the informer's INITIAL list
// (seedEventBaseline, isInInitialList == true), then tears down immediately
// with no wait for HasSynced at all -- the initial list's own delivery
// goroutine can still be draining when the sweep runs.
func TestEventSeedDoesNotStrandUnderConcurrentTeardown(t *testing.T) {
	// 500, not 40 -- see TestPodSeedDoesNotStrandUnderConcurrentTeardown's
	// doc comment (pods_test.go) for why the seed path needs a much larger
	// count than the live-Add stress tests: every object here is pre-loaded
	// before the informer starts, so the whole initial list is one delivery
	// burst with no caller-side pacing, and a short list can finish that
	// burst before this goroutine is even scheduled again.
	const eventCount = 500
	const attempts = 25

	for attempt := 0; attempt < attempts; attempt++ {
		preexisting := make([]runtime.Object, 0, eventCount)
		for i := 0; i < eventCount; i++ {
			ev := testEvent(
				types.UID(fmt.Sprintf("seed-pod-uid-%d-%d", attempt, i)),
				fmt.Sprintf("seed-worker-%d.a", i),
				corev1.EventTypeWarning)
			ev.InvolvedObject.Name = fmt.Sprintf("seed-worker-%d", i)
			preexisting = append(preexisting, ev)
		}
		client := fake.NewSimpleClientset(preexisting...)
		b := bus.New(64)
		o := New(client, b, func() RunScope {
			return RunScope{RunID: "run-1", Namespaces: map[string]struct{}{podTestNamespace: {}}}
		})

		o.scoped.reconcile(scopeWith("run-1", podTestNamespace))
		// Waits for the FIRST entry to land, then races the terminal
		// reconcile immediately -- via spinUntil (pods_test.go), not
		// waitFor. See TestPodSeedDoesNotStrandUnderConcurrentTeardown's
		// identical pattern for why waitFor's own 10ms-ticker poll
		// (fine for every other caller in this package) is too coarse
		// here: an entire several-hundred-object initial-list delivery
		// burst can finish in well under 10ms, so a 10ms-spaced check
		// reliably observes the whole batch already landed rather than
		// mid-flight, closing the race this test exists to probe. This
		// doubles as this test's M5 proof-of-write check (Task 6 fix
		// round 1).
		spinUntil(t, "TestEventSeedDoesNotStrandUnderConcurrentTeardown: waiting for the first seeded entry", func() bool {
			o.mu.Lock()
			defer o.mu.Unlock()
			return len(o.events) > 0
		})
		o.scoped.reconcile(terminalScope(podTestNamespace))

		time.Sleep(20 * time.Millisecond)

		o.mu.Lock()
		after := len(o.events)
		stranded := make(map[stateKey]map[string]eventDedupe, len(o.events))
		for k, v := range o.events {
			stranded[k] = v
		}
		o.mu.Unlock()
		if after != 0 {
			t.Fatalf("attempt %d: o.events after concurrent initial-list teardown = %d entries, want 0 -- stranded: %+v", attempt, after, stranded)
		}
	}
}

// TestEventMessageIsTruncated pins M6 (Task 6 fix round 1):
// ev.Message -- free-form text from the reporting component, not something
// this observer controls the shape of -- is bounded before it reaches a bus
// payload the way no other handler in this package needs to bound its own
// interpolated strings.
func TestEventMessageIsTruncated(t *testing.T) {
	o, sub := newTestObserver(t)
	ev := testEvent("pod-uid", "worker-1.a", corev1.EventTypeWarning)
	ev.Message = strings.Repeat("x", eventMessageMaxLen*2)

	o.onEventAdd(ev, false)

	e := waitForEvent(t, sub)
	// Well under the untruncated length (2x eventMessageMaxLen, ev.Message
	// alone) rather than a tight arithmetic bound on top of
	// eventMessageMaxLen: the subject/reason/separator prefix around the
	// truncated ev.Message is this observer's own formatting, not part of
	// what M6 bounds, and pinning its exact byte count here would make this
	// test fragile to an unrelated eventMessage format change.
	if len(e.Message) >= eventMessageMaxLen*2 {
		t.Errorf("Message length = %d, want it truncated well below the untruncated length (%d); got %q", len(e.Message), eventMessageMaxLen*2, e.Message)
	}
	if !strings.Contains(e.Message, "…") {
		t.Errorf("Message = %q, want a truncation marker for an oversized ev.Message", e.Message)
	}
}

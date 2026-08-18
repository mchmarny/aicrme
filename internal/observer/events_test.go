package observer

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// TestEventDedupeIsBoundedByGeneration is the brief's test 6: a new run does
// not inherit the previous run's dedupe state.
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
func TestEventDedupeIsBoundedByGeneration(t *testing.T) {
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

	if events := collectPodEvents(sub, time.Second); len(events) != 0 {
		t.Fatalf("published %d events for the initial list, want 0: %+v", len(events), events)
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
		stranded := make(map[stateKey]map[string]struct{}, len(o.events))
		for k, v := range o.events {
			stranded[k] = v
		}
		o.mu.Unlock()
		if after != 0 {
			t.Fatalf("attempt %d: o.events after concurrent teardown = %d entries, want 0 -- stranded: %+v", attempt, after, stranded)
		}
	}
}

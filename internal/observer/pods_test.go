package observer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/mchmarny/aicrme/internal/bus"
)

// podTestNamespace is the one namespace every test in this file uses. Not
// varied per test case: what changes across cases is a pod's status, never
// which namespace it lives in.
const podTestNamespace = "gpu-operator"

// newScopedPodTestObserver wires an Observer exactly as production's New
// does, then drives its Pod informer for podTestNamespace through the same
// scoped.reconcile path production's run() goroutine takes -- calling it
// directly and synchronously, rather than through run()'s bus/ticker
// machinery, is what lets these tests avoid waiting out reconcileInterval's
// 2s floor for no reason. Start (the cluster-wide DaemonSet/Deployment/Node
// factory) is never called: no Pod test needs it, and the whole point of
// scoped.go's lazy lifecycle is that Pods/Events do not ride that factory.
func newScopedPodTestObserver(t *testing.T, client *fake.Clientset) (*Observer, <-chan bus.Event) {
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
	waitFor(t, entry.pod.HasSynced)

	return o, sub
}

// podEventWait bounds how long these tests wait for a real informer to
// deliver an event it is expected to. Every waitForEvent/waitForClusterData
// call site in this file wants the same bound, so it is a constant rather
// than a threaded-through parameter (an unused degree of freedom golangci-
// lint's unparam correctly flags when nothing varies it).
const podEventWait = 2 * time.Second

// waitForEvent blocks for up to podEventWait for the next published event,
// failing the test on timeout instead of checking immediately: unlike
// handlers_internal_test.go's decodeClusterData (which calls handlers
// directly, so bus.Publish has already run by the time the call returns),
// these tests drive a real informer whose delivery is asynchronous relative
// to the fake clientset call that triggered it.
func waitForEvent(t *testing.T, sub <-chan bus.Event) bus.Event {
	t.Helper()
	select {
	case e := <-sub:
		return e
	case <-time.After(podEventWait):
		t.Fatal("no event published within deadline")
		return bus.Event{}
	}
}

func waitForClusterData(t *testing.T, sub <-chan bus.Event) bus.ClusterData {
	t.Helper()
	_, cd := waitForEventAndClusterData(t, sub)
	return cd
}

// waitForEventAndClusterData is waitForClusterData's counterpart when a test
// also needs the raw Event.Message -- Ruling 17 (Task 5 fix round 2) keeps
// the raw kubelet reason in the narration message even though
// ClusterData.Reason is normalized, and TestPodNarratesImagePullBackOff's
// ErrImagePull case needs to check both.
func waitForEventAndClusterData(t *testing.T, sub <-chan bus.Event) (bus.Event, bus.ClusterData) {
	t.Helper()
	e := waitForEvent(t, sub)
	var cd bus.ClusterData
	if err := json.Unmarshal(e.Data, &cd); err != nil {
		t.Fatalf("Unmarshal(ClusterData) error = %v, raw = %s", err, e.Data)
	}
	return e, cd
}

// collectPodEvents drains sub for the full duration, so a "nothing published"
// assertion actually waited long enough to mean something rather than
// sampling the channel once before an async informer had a chance to
// deliver.
func collectPodEvents(sub <-chan bus.Event, within time.Duration) []bus.Event {
	var out []bus.Event
	deadline := time.After(within)
	for {
		select {
		case e := <-sub:
			out = append(out, e)
		case <-deadline:
			return out
		}
	}
}

// podTestName is the one pod name every test in this file uses; only Status
// and UID vary across cases.
const podTestName = "worker-1"

func testPod(uid types.UID) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: podTestNamespace, Name: podTestName, UID: uid},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
}

func withContainerWaiting(pod *corev1.Pod, container, reason string) *corev1.Pod {
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{Name: container, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}},
	}
	return pod
}

// withRunning's container is always "app" -- no test in this package (as of
// Task 6 fix round 1, when a new call site made golangci-lint's unparam
// notice) needs a healthy pod's container to be named anything else, so the
// name is a constant rather than a parameter every call site would
// otherwise repeat identically.
func withRunning(pod *corev1.Pod) *corev1.Pod {
	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{Name: "app", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, Ready: true},
	}
	return pod
}

func withUnschedulable(pod *corev1.Pod) *corev1.Pod {
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: reasonUnschedulable},
	}
	return pod
}

// TestPodNarratesImagePullBackOff is also, deliberately, the "later Add"
// half of the bite-proof the brief requires: each case creates its pod via
// Create AFTER the informer has already synced, so client-go delivers it as
// a genuine watch Add (isInInitialList == false), never as part of the
// initial list. A mutation that suppresses every Add regardless of
// isInInitialList breaks every case here while leaving
// TestPodInitialListDoesNotNarrate (which never expects narration) green --
// that asymmetry is what proves the suppression is keyed on the right
// signal, not just "no Adds ever narrate".
func TestPodNarratesImagePullBackOff(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*corev1.Pod) *corev1.Pod
		wantReason string // normalized -- the ClusterData.Reason / Supersedes key (Ruling 17)
		wantDetail string // raw kubelet reason, expected inside the narration message
		wantSev    bus.Severity
		wantCont   string
	}{
		{
			name:       "ImagePullBackOff",
			mutate:     func(p *corev1.Pod) *corev1.Pod { return withContainerWaiting(p, "app", reasonImagePullBackOff) },
			wantReason: reasonImagePullBackOff,
			wantDetail: reasonImagePullBackOff,
			wantSev:    bus.SeverityWarn,
			wantCont:   "app",
		},
		{
			// Ruling 17 (Task 5 fix round 2): ErrImagePull normalizes to the
			// SAME Reason as ImagePullBackOff -- kubelet's two names for one
			// stuck pull -- so this case's wantReason is deliberately
			// reasonImagePullBackOff, not reasonErrImagePull. The raw
			// "ErrImagePull" detail must still reach the narration message.
			name:       "ErrImagePull",
			mutate:     func(p *corev1.Pod) *corev1.Pod { return withContainerWaiting(p, "app", reasonErrImagePull) },
			wantReason: reasonImagePullBackOff,
			wantDetail: reasonErrImagePull,
			wantSev:    bus.SeverityWarn,
			wantCont:   "app",
		},
		{
			name:       "CrashLoopBackOff",
			mutate:     func(p *corev1.Pod) *corev1.Pod { return withContainerWaiting(p, "app", reasonCrashLoopBackOff) },
			wantReason: reasonCrashLoopBackOff,
			wantDetail: reasonCrashLoopBackOff,
			wantSev:    bus.SeverityError,
			wantCont:   "app",
		},
		{
			name:       "unschedulable",
			mutate:     withUnschedulable,
			wantReason: reasonUnschedulable,
			wantDetail: reasonUnschedulable,
			wantSev:    bus.SeverityError,
			wantCont:   "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			_, sub := newScopedPodTestObserver(t, client)

			pod := tc.mutate(testPod("pod-uid"))
			if _, err := client.CoreV1().Pods(podTestNamespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
				t.Fatalf("Create() error = %v", err)
			}

			e, cd := waitForEventAndClusterData(t, sub)
			if cd.Kind != kindPod {
				t.Errorf("Kind = %q, want %q", cd.Kind, kindPod)
			}
			if cd.UID != "pod-uid" {
				t.Errorf("UID = %q, want %q", cd.UID, "pod-uid")
			}
			if cd.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", cd.Reason, tc.wantReason)
			}
			if !strings.Contains(e.Message, tc.wantDetail) {
				t.Errorf("Message = %q, want it to contain the raw reason %q", e.Message, tc.wantDetail)
			}
			if cd.Container != tc.wantCont {
				t.Errorf("Container = %q, want %q", cd.Container, tc.wantCont)
			}
			if cd.Severity != tc.wantSev {
				t.Errorf("Severity = %v, want %v", cd.Severity, tc.wantSev)
			}
			if cd.Resolved {
				t.Error("Resolved = true, want false: this is the condition arising, not clearing")
			}
		})
	}
}

// TestPodDoesNotNarrateHealthyTransitions: Pending -> Running carries no
// trouble reason at either end, and the workload's Deployment/DaemonSet
// ready counts already summarize that transition (spec Section 3, Volume
// control) -- narrating it here would be a second, redundant signal for the
// same fact.
func TestPodDoesNotNarrateHealthyTransitions(t *testing.T) {
	initial := testPod("pod-uid")
	client := fake.NewSimpleClientset(initial)
	_, sub := newScopedPodTestObserver(t, client)

	running := withRunning(testPod("pod-uid"))
	if _, err := client.CoreV1().Pods(podTestNamespace).Update(context.Background(), running, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	if events := collectPodEvents(sub, time.Second); len(events) != 0 {
		t.Fatalf("published %d events for a healthy Pending -> Running transition, want 0: %+v", len(events), events)
	}
}

// TestPodConditionResolves: a pod already broken when the informer's initial
// list delivers it (seeded silently, not narrated -- see
// TestPodInitialListDoesNotNarrate) that then recovers must publish a
// Resolved condition carrying the SAME Reason it arose under, so
// ClusterData.Supersedes can match it to the row entry it clears.
func TestPodConditionResolves(t *testing.T) {
	broken := withContainerWaiting(testPod("pod-uid"), "app", reasonImagePullBackOff)
	client := fake.NewSimpleClientset(broken)
	_, sub := newScopedPodTestObserver(t, client)

	healthy := withRunning(testPod("pod-uid"))
	if _, err := client.CoreV1().Pods(podTestNamespace).Update(context.Background(), healthy, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	cd := waitForClusterData(t, sub)
	if cd.Reason != reasonImagePullBackOff {
		t.Errorf("Reason = %q, want %q (the reason it resolved FROM)", cd.Reason, reasonImagePullBackOff)
	}
	if cd.UID != "pod-uid" {
		t.Errorf("UID = %q, want %q", cd.UID, "pod-uid")
	}
	if !cd.Resolved {
		t.Error("Resolved = false, want true: the pod recovered")
	}
}

// TestPodInitialListDoesNotNarrate: an informer's initial list delivers every
// pre-existing pod as an Add. A pod that has sat in CrashLoopBackOff for
// hours before this process started is not "newly" anything -- narrating it
// here would misreport age as a fresh transition.
func TestPodInitialListDoesNotNarrate(t *testing.T) {
	broken := withContainerWaiting(testPod("pod-uid"), "app", reasonCrashLoopBackOff)
	client := fake.NewSimpleClientset(broken)
	_, sub := newScopedPodTestObserver(t, client)

	if events := collectPodEvents(sub, time.Second); len(events) != 0 {
		t.Fatalf("published %d events for the initial list, want 0: %+v", len(events), events)
	}
}

// TestPodEmitsTypedClusterData pins that every field the cockpit needs to
// render and correlate a Pod row is populated from the live object, not left
// at a zero value: Kind identifies the resource type, UID is what
// Supersedes keys on, Container names which container is in trouble,
// Reason/Severity are what the row displays and ranks by.
func TestPodEmitsTypedClusterData(t *testing.T) {
	client := fake.NewSimpleClientset()
	_, sub := newScopedPodTestObserver(t, client)

	pod := withContainerWaiting(testPod("pod-typed-uid"), "trainer", reasonImagePullBackOff)
	if _, err := client.CoreV1().Pods(podTestNamespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	cd := waitForClusterData(t, sub)
	if cd.Kind != kindPod {
		t.Errorf("Kind = %q, want %q", cd.Kind, kindPod)
	}
	if cd.UID != "pod-typed-uid" {
		t.Errorf("UID = %q, want %q", cd.UID, "pod-typed-uid")
	}
	if cd.Container != "trainer" {
		t.Errorf("Container = %q, want %q", cd.Container, "trainer")
	}
	if cd.Reason != reasonImagePullBackOff {
		t.Errorf("Reason = %q, want %q", cd.Reason, reasonImagePullBackOff)
	}
	if cd.Severity != bus.SeverityWarn {
		t.Errorf("Severity = %v, want %v", cd.Severity, bus.SeverityWarn)
	}
}

// TestPodDeleteClearsTrackedTrouble is Ruling 5's binding requirement for the
// kind this task introduces: a pod that goes away while its trouble
// condition is still unresolved must not leave that condition pinned to its
// row forever.
//
// Rewritten under Ruling 15 (Task 5 fix round 1): the original version
// seeded via onPodAdd's INITIAL-LIST path (isInInitialList=true), which
// never narrates -- so it was actually pinning a defect (Important 3): a
// delete manufacturing a "resolved" event for a condition no consumer was
// ever shown. This version seeds via a LATER Add (isInInitialList=false),
// which does narrate, so the condition being cleared here is one the
// operator's timeline genuinely reported as arising.
// TestPodDeleteOfASeededButNeverNarratedTroublePublishesNothing is the
// required sibling pinning the corrected initial-list case.
func TestPodDeleteClearsTrackedTrouble(t *testing.T) {
	o, sub := newTestObserver(t)
	pod := withContainerWaiting(testPod("pod-uid"), "app", reasonImagePullBackOff)

	o.onPodAdd(pod, false) // a later Add: this DOES narrate
	select {
	case e := <-sub:
		if e.Message == "" {
			t.Fatal("onPodAdd(isInInitialList=false) published an empty message; test setup failure")
		}
	default:
		t.Fatal("onPodAdd(isInInitialList=false) did not narrate; test setup failure")
	}

	o.onDelete(pod)

	select {
	case e := <-sub:
		if !strings.Contains(e.Message, "removed") {
			t.Errorf("Message = %q, want it to say removed -- the pod was deleted, it did not recover", e.Message)
		}
		if strings.Contains(e.Message, "resolved") {
			t.Errorf("Message = %q, must not say resolved for a deleted pod", e.Message)
		}
		var cd bus.ClusterData
		if err := json.Unmarshal(e.Data, &cd); err != nil {
			t.Fatalf("Unmarshal(ClusterData) error = %v, raw = %s", err, e.Data)
		}
		if cd.UID != "pod-uid" {
			t.Errorf("UID = %q, want %q", cd.UID, "pod-uid")
		}
		if cd.Reason != reasonImagePullBackOff {
			t.Errorf("Reason = %q, want %q", cd.Reason, reasonImagePullBackOff)
		}
		if cd.Container != "app" {
			t.Errorf("Container = %q, want %q", cd.Container, "app")
		}
		if !cd.Resolved {
			t.Error("Resolved = false, want true: pod deletion must clear the row")
		}
	default:
		t.Fatal("no event published for deleting a narrated trouble")
	}
}

// TestPodDeleteOfASeededButNeverNarratedTroublePublishesNothing is Ruling
// 15's required sibling: a pod already broken in an informer's initial list
// is seeded silently (onPodAdd's isInInitialList=true path -- see
// TestPodInitialListDoesNotNarrate), and its deletion must not manufacture a
// "removed" event for a condition no consumer was ever shown (Important 3,
// Task 5 fix round 1; podCondition.narrated is what makes onDelete able to
// tell the two cases apart).
func TestPodDeleteOfASeededButNeverNarratedTroublePublishesNothing(t *testing.T) {
	o, sub := newTestObserver(t)
	pod := withContainerWaiting(testPod("pod-uid"), "app", reasonImagePullBackOff)

	o.onPodAdd(pod, true) // initial list: seeds the baseline, does not narrate
	assertNoEvent(t, sub)

	o.onDelete(pod)

	assertNoEvent(t, sub)
}

// TestPodDeleteOfAHealthyPodPublishesNothing matches
// TestOnDeleteOfAnUntrackedResourcePublishesNothing's rule, specialized to
// Pods: a healthy pod is never recorded in o.pods (onPodChange/
// seedPodBaseline only ever store trouble states), so its deletion has no
// condition to clear.
func TestPodDeleteOfAHealthyPodPublishesNothing(t *testing.T) {
	o, sub := newTestObserver(t)
	pod := testPod("pod-uid")

	o.onPodAdd(pod, true)
	o.onDelete(pod)

	assertNoEvent(t, sub)
}

// TestPodImagePullOscillationNarratesOnceAndNeverFalselyResolves pins Ruling
// 17 (Task 5 fix round 2) against the exact scenario the original review
// demonstrated: kubelet alternates ErrImagePull and ImagePullBackOff every
// backoff cycle on one continuously-stuck pull.
//
// Round 1's fix (Ruling 14(b): only resolve on full recovery) stopped the
// false "resolved" events, but the FOLLOW-UP review (Important A) found the
// mirror defect: keyed per RAW reason, a genuine recovery resolved only
// whichever raw reason kubelet happened to be using LAST, leaving the other
// permanently unresolved on the row. Ruling 17 fixes this at the source by
// normalizing both raw reasons into ONE Reason (normalizeReason) before
// podTrouble ever returns -- so this 4-cycle oscillation is not four
// transitions at all, just one arising event followed by three no-op
// updates against the SAME normalized condition. That is the property this
// test now pins: EXACTLY ONE event total, never a false resolve, and the
// raw detail from the FIRST cycle survives in the message (podMessage uses
// podCondition.detail, captured once and not live-updated by later
// wobbles -- see onPodChange's no-change guard).
func TestPodImagePullOscillationNarratesOnceAndNeverFalselyResolves(t *testing.T) {
	o, sub := newTestObserver(t)
	pod := testPod("pod-uid")

	reasons := []string{reasonErrImagePull, reasonImagePullBackOff, reasonErrImagePull, reasonImagePullBackOff}
	for _, r := range reasons {
		o.onPodUpdate(nil, withContainerWaiting(pod, "app", r))
	}

	e, cd := waitForEventAndClusterData(t, sub)
	if cd.Reason != reasonImagePullBackOff {
		t.Errorf("Reason = %q, want the normalized %q", cd.Reason, reasonImagePullBackOff)
	}
	if !strings.Contains(e.Message, reasonErrImagePull) {
		t.Errorf("Message = %q, want it to carry the raw detail from the first cycle (%q)", e.Message, reasonErrImagePull)
	}
	if cd.Resolved {
		t.Error("Resolved = true, want false: the pod never stopped being broken")
	}
	if cd.Container != "app" {
		t.Errorf("Container = %q, want %q", cd.Container, "app")
	}
	assertNoEvent(t, sub) // every later wobble is the SAME normalized condition -- nothing more to publish
}

// TestPodImagePullOscillationThenDeleteLeavesNothingStranded is Important
// A's delete-path sibling (Task 5 fix round 2). Before Ruling 17, deleting a
// pod after it had oscillated between ErrImagePull and ImagePullBackOff
// cleared only the last raw reason kubelet had used, stranding the other
// permanently unresolved (the review's PROBE1c). With both normalized to
// one Reason, there is only ever one row entry for a stuck pull to strand --
// deleting the pod clears that one entry, full stop.
func TestPodImagePullOscillationThenDeleteLeavesNothingStranded(t *testing.T) {
	o, sub := newTestObserver(t)
	pod := testPod("pod-uid")

	for _, r := range []string{reasonErrImagePull, reasonImagePullBackOff, reasonErrImagePull} {
		o.onPodUpdate(nil, withContainerWaiting(pod, "app", r))
	}
	decodeClusterData(t, sub) // the single arising event; later wobbles publish nothing (see the sibling above)

	o.onDelete(pod)

	cd := decodeClusterData(t, sub)
	if cd.Reason != reasonImagePullBackOff {
		t.Errorf("Reason = %q, want %q", cd.Reason, reasonImagePullBackOff)
	}
	if !cd.Resolved {
		t.Error("Resolved = false, want true: delete must clear the row")
	}
	assertNoEvent(t, sub) // nothing stranded -- there was only ever one row entry to clear
}

// TestPodUnchangedTroubleEmitsExactlyOnce pins Minor D (Task 5 fix round 2):
// this package's own headline constraint, "it aggregates, it never relays"
// (observer.go), applied to Pods. An informer's UpdateFunc fires on any
// field change -- managedFields, resourceVersion, annotations -- not just
// the ones this observer tracks. Ten repeated, IDENTICAL updates for one
// still-broken pod must produce exactly one event, not one per delivery:
// reverting onPodChange's dedupe guard from the explicit
// "p.reason == cur.reason && p.container == cur.container" back to a bare
// struct comparison (`prev == cur`) leaves every OTHER Pod test green,
// because podCondition.narrated (added for Important 3) makes a
// freshly-recorded entry compare unequal to its own stored value forever --
// nothing before this test asserted on REPEATED identical updates
// specifically.
func TestPodUnchangedTroubleEmitsExactlyOnce(t *testing.T) {
	o, sub := newTestObserver(t)
	pod := withContainerWaiting(testPod("pod-uid"), "app", reasonImagePullBackOff)

	for i := 0; i < 10; i++ {
		o.onPodUpdate(nil, pod)
	}

	decodeClusterData(t, sub) // the one genuine transition
	assertNoEvent(t, sub)     // the other 9 identical updates must not re-publish
}

// TestPodHighestSeverityTroubleWinsAcrossContainers pins the Minor finding
// (Task 5 fix round 1): podTrouble must not report whichever container's
// waiting reason happens to sort first in ContainerStatuses. "puller" (Warn,
// ImagePullBackOff) is placed before "worker" (Error, CrashLoopBackOff) in
// the slice specifically so a first-in-slice implementation would report
// the wrong one -- the review's probe showed exactly that (0 further events
// for the CrashLoopBackOff container, ever).
func TestPodHighestSeverityTroubleWinsAcrossContainers(t *testing.T) {
	o, sub := newTestObserver(t)
	pod := testPod("pod-uid")
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{Name: "puller", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reasonImagePullBackOff}}},
		{Name: "worker", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reasonCrashLoopBackOff}}},
	}

	o.onPodUpdate(nil, pod)

	cd := decodeClusterData(t, sub)
	if cd.Reason != reasonCrashLoopBackOff {
		t.Errorf("Reason = %q, want %q: the higher-severity condition must not be hidden by container order", cd.Reason, reasonCrashLoopBackOff)
	}
	if cd.Container != "worker" {
		t.Errorf("Container = %q, want %q", cd.Container, "worker")
	}
	if cd.Severity != bus.SeverityError {
		t.Errorf("Severity = %v, want %v", cd.Severity, bus.SeverityError)
	}
}

// TestPodMultiReasonRecoveryResolvesEveryNarratedReason pins Ruling 20 (Task
// 5 fix round 3) against the re-review's exact probe: an ORDINARY Apply
// sequence, not an edge case -- a pod pends on GPU capacity (Unschedulable),
// then gets scheduled and its image pull stalls (ImagePullBackOff), then
// fully recovers. Ruling 17 already collapsed the ErrImagePull/
// ImagePullBackOff ALIAS pair into one Reason, but that left the STRANDING
// CLASS open: Unschedulable and ImagePullBackOff are genuinely different
// Reasons, and round 2's single-tracked-value o.pods lost track of
// Unschedulable the instant ImagePullBackOff overwrote it -- nothing ever
// resolved it. The review found this strictly worse than the case Ruling 17
// closed: the stranded Unschedulable entry is Error severity, which
// OUTRANKS a resolved ImagePullBackOff entry, so a row picking
// highest-severity-unresolved would show permanent red on a fully healthy
// pod. A test that only exercises one reason arising and clearing cannot
// see this -- three rounds of tests didn't -- so this one walks the full
// three-state sequence and asserts BOTH reasons resolve, not just the last.
func TestPodMultiReasonRecoveryResolvesEveryNarratedReason(t *testing.T) {
	o, sub := newTestObserver(t)

	o.onPodUpdate(nil, withUnschedulable(testPod("pod-uid")))
	arose1 := decodeClusterData(t, sub)
	if arose1.Reason != reasonUnschedulable || arose1.Resolved {
		t.Fatalf("setup: Unschedulable arising = %+v, want Reason=%q Resolved=false", arose1, reasonUnschedulable)
	}

	// Scheduled now: podTrouble no longer reports Unschedulable at all (the
	// fresh pod object below carries no PodScheduled=False condition), but
	// its image pull is stuck.
	o.onPodUpdate(nil, withContainerWaiting(testPod("pod-uid"), "app", reasonImagePullBackOff))
	arose2 := decodeClusterData(t, sub)
	if arose2.Reason != reasonImagePullBackOff || arose2.Resolved {
		t.Fatalf("setup: ImagePullBackOff arising = %+v, want Reason=%q Resolved=false", arose2, reasonImagePullBackOff)
	}

	// Full recovery.
	o.onPodUpdate(nil, withRunning(testPod("pod-uid")))

	resolved := map[string]bus.ClusterData{}
	for range 2 {
		cd := decodeClusterData(t, sub)
		resolved[cd.Reason] = cd
	}
	assertNoEvent(t, sub) // exactly two resolves, nothing more

	for _, reason := range []string{reasonUnschedulable, reasonImagePullBackOff} {
		cd, ok := resolved[reason]
		if !ok {
			t.Errorf("no resolve event published for %q -- stranded", reason)
			continue
		}
		if !cd.Resolved {
			t.Errorf("%q Resolved = false, want true", reason)
		}
		if cd.UID != "pod-uid" {
			t.Errorf("%q UID = %q, want %q", reason, cd.UID, "pod-uid")
		}
	}
}

// TestPodMultiReasonDeleteResolvesEveryNarratedReason is
// TestPodMultiReasonRecoveryResolvesEveryNarratedReason's delete-path
// sibling -- Ruling 20 explicitly requires the same treatment there,
// respecting the narrated distinction (Important 3/Minor C) rather than
// resolving unconditionally the way the recovery path does.
func TestPodMultiReasonDeleteResolvesEveryNarratedReason(t *testing.T) {
	o, sub := newTestObserver(t)

	o.onPodUpdate(nil, withUnschedulable(testPod("pod-uid")))
	decodeClusterData(t, sub) // Unschedulable arises

	scheduled := withContainerWaiting(testPod("pod-uid"), "app", reasonImagePullBackOff)
	o.onPodUpdate(nil, scheduled)
	decodeClusterData(t, sub) // ImagePullBackOff arises

	o.onDelete(scheduled)

	removed := map[string]bus.ClusterData{}
	for range 2 {
		cd := decodeClusterData(t, sub)
		removed[cd.Reason] = cd
	}
	assertNoEvent(t, sub) // exactly two removals, nothing stranded

	for _, reason := range []string{reasonUnschedulable, reasonImagePullBackOff} {
		cd, ok := removed[reason]
		if !ok {
			t.Errorf("delete did not publish a removal for %q -- stranded", reason)
			continue
		}
		if !cd.Resolved {
			t.Errorf("%q Resolved = false, want true", reason)
		}
	}
}

// TestPodTroubleIsClearedWhenTheNamespaceInformerTearsDown pins Important 4
// (Task 5 fix round 1): one narrated pod, its event fully AWAITED before
// teardown, so nothing is ever in flight when the sweep runs. That is a real
// property (o.pods does get cleared here) but not the one that bites in
// practice -- see TestPodTroubleDoesNotStrandUnderConcurrentTeardown for the
// load shape (many pods, no synchronization point before an immediate
// terminal reconcile) that the round-2 re-review found still stranded
// entries on 23 of 25 attempts (Important B). Both tests are kept: this one
// pins the simple case cheaply, the other pins the case that actually
// mattered.
func TestPodTroubleIsClearedWhenTheNamespaceInformerTearsDown(t *testing.T) {
	client := fake.NewSimpleClientset()
	o, sub := newScopedPodTestObserver(t, client)

	pod := withContainerWaiting(testPod("pod-uid"), "app", reasonImagePullBackOff)
	if _, err := client.CoreV1().Pods(podTestNamespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	waitForClusterData(t, sub) // wait for the trouble to be narrated and recorded

	o.mu.Lock()
	before := len(o.pods)
	o.mu.Unlock()
	if before != 1 {
		t.Fatalf("o.pods before teardown = %d entries, want 1; test setup failure", before)
	}

	o.scoped.reconcile(terminalScope(podTestNamespace))

	o.mu.Lock()
	after := len(o.pods)
	stranded := make(map[stateKey]map[string]podCondition, len(o.pods))
	for k, v := range o.pods {
		stranded[k] = v
	}
	o.mu.Unlock()
	if after != 0 {
		t.Errorf("o.pods after the namespace's informer tore down = %d entries, want 0 -- stranded: %+v", after, stranded)
	}
	// Minor G (Task 5 fix round 2): clearNamespacePods deliberately publishes
	// nothing -- a torn-down informer means this process can no longer speak
	// about those pods, not that anything resolved or was removed. Assert it,
	// not just the doc comment's word for it.
	assertNoEvent(t, sub)
}

// TestPodTroubleDoesNotStrandUnderConcurrentTeardown is Important B (Task 5
// fix round 2). Round 1's sweep (clearNamespacePods, called once from
// stopNamespaceLocked) raced the informer's own in-flight deliveries:
// close(stop) does not stop a SharedIndexInformer's sharedProcessor
// synchronously, so a notification already queued kept being delivered to
// onPodAdd/onPodUpdate after the sweep had already run, writing straight
// back into o.pods. The re-review's own probe -- driven through a real
// informer, 40 broken pods created with NO synchronization point before an
// immediate terminal reconcile, repeated 25 times -- stranded entries on 23
// of 25 attempts.
// TestPodTroubleIsClearedWhenTheNamespaceInformerTearsDown cannot see this:
// it awaits its one event before tearing down, so nothing is ever actually
// in flight. This test reproduces the load shape that bites: many pods
// created back to back, then an immediate reconcile into Terminal, repeated
// enough times that a racy fix would show stranding with overwhelming
// probability. The fix (scopedInformers.withNamespaceLive, scoped.go) gates
// every Pod handler write on the namespace still being tracked, checked
// under the same lock the sweep itself uses -- mirroring
// internal/engine's epoch/aliveLocked(epoch) idiom -- so this passes
// regardless of how the race resolves, not because of the timing this test
// happens to exercise.
func TestPodTroubleDoesNotStrandUnderConcurrentTeardown(t *testing.T) {
	const podCount = 40
	const attempts = 25

	for attempt := 0; attempt < attempts; attempt++ {
		client := fake.NewSimpleClientset()
		o, _ := newScopedPodTestObserver(t, client)

		for i := 0; i < podCount; i++ {
			pod := withContainerWaiting(
				testPod(types.UID(fmt.Sprintf("pod-uid-%d-%d", attempt, i))),
				"app", reasonImagePullBackOff)
			pod.Name = fmt.Sprintf("worker-%d", i)
			if _, err := client.CoreV1().Pods(podTestNamespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
				t.Fatalf("attempt %d: Create() error = %v", attempt, err)
			}
		}

		// No synchronization point here -- this IS the shape that bites: an
		// immediate terminal reconcile with pod creation still (possibly) in
		// flight through the informer, matching a real Apply reaching
		// Terminal with dozens of pods mid-pull.
		o.scoped.reconcile(terminalScope(podTestNamespace))

		// Give any straggling delivery every opportunity to land before
		// checking -- a flaky pass here would UNDERSTATE the race, never
		// overstate it: we are trying to prove nothing lands, not that
		// everything lands quickly, and nothing in this test re-sweeps
		// afterward to paper over a late arrival.
		time.Sleep(20 * time.Millisecond)

		o.mu.Lock()
		after := len(o.pods)
		stranded := make(map[stateKey]map[string]podCondition, len(o.pods))
		for k, v := range o.pods {
			stranded[k] = v
		}
		o.mu.Unlock()
		if after != 0 {
			t.Fatalf("attempt %d: o.pods after concurrent teardown = %d entries, want 0 -- stranded: %+v", attempt, after, stranded)
		}
	}
}

// TestPodSeedDoesNotStrandUnderConcurrentTeardown is Important 2's Pod half
// (Task 6 fix round 1, review's control probe): removing seedPodBaseline's
// withNamespaceLive gate (numstat `2 2`) left the whole package green,
// because TestPodTroubleDoesNotStrandUnderConcurrentTeardown creates every
// pod AFTER HasSynced, so only onPodChange (a later Add) is ever exercised
// -- never the initial-list seed path seedPodBaseline is. The gap is
// inherited from Task 5, not introduced there; this closes it alongside the
// identical gap on the Event side (events_test.go's
// TestEventSeedDoesNotStrandUnderConcurrentTeardown), since both share the
// same root cause and the same fix shape.
//
// Pre-loads the fake clientset BEFORE reconcile ever starts the namespace,
// so every pod arrives via the informer's INITIAL list (seedPodBaseline,
// isInInitialList == true), then tears down immediately with no wait for
// HasSynced at all -- the initial list's own delivery goroutine can still be
// draining when the sweep runs.
func TestPodSeedDoesNotStrandUnderConcurrentTeardown(t *testing.T) {
	// 500, not Task 5's 40: unlike the live-Add stress test (which gets
	// natural pacing for free from 40 sequential Create() calls, each real
	// work on the calling goroutine while the informer's own goroutine
	// processes concurrently), every object here is pre-loaded before the
	// informer starts at all, so its ENTIRE initial list is one delivery
	// burst with no caller-side pacing. A short list can finish that whole
	// burst before this goroutine even gets scheduled again after starting
	// it -- empirically true at 40 -- so the count needs to be large enough
	// that SOME deliveries are still outstanding at the moment this test
	// deliberately races them against teardown below.
	const podCount = 500
	const attempts = 25

	for attempt := 0; attempt < attempts; attempt++ {
		preexisting := make([]runtime.Object, 0, podCount)
		for i := 0; i < podCount; i++ {
			pod := withContainerWaiting(
				testPod(types.UID(fmt.Sprintf("seed-pod-uid-%d-%d", attempt, i))),
				"app", reasonImagePullBackOff)
			pod.Name = fmt.Sprintf("seed-worker-%d", i)
			preexisting = append(preexisting, pod)
		}
		client := fake.NewSimpleClientset(preexisting...)
		o, _ := newScopedPodTestObserverWithoutSync(t, client)

		// Waits for the FIRST entry to land, then races the terminal
		// reconcile immediately -- via spinUntil, not waitFor. A fixed
		// sleep, or waitFor's own 10ms-ticker poll, either undershoots
		// (close(stop) can race the reflector goroutine's own first
		// scheduling, sometimes preventing it from ever starting a single
		// delivery, which would pass this test under a broken
		// seedPodBaseline for the wrong reason: nothing to strand, not
		// nothing stranded) or overshoots -- empirically, even a 500-object
		// batch can finish its ENTIRE initial-list delivery in well under
		// waitFor's 10ms poll granularity, so by the time a 10ms-spaced
		// check first observes len(o.pods) > 0, the whole batch has
		// typically already landed, closing the race. spinUntil's
		// sleep-free busy-poll is what catches the batch mid-flight instead
		// of only ever observing its beginning or its end. Waiting for
		// exactly "delivery has started" and then proceeding immediately is
		// what keeps the rest of the burst genuinely in flight when
		// teardown hits -- this IS this test's M5 proof-of-write check
		// (Task 6 fix round 1) and its race-preserving timing in one.
		spinUntil(t, "TestPodSeedDoesNotStrandUnderConcurrentTeardown: waiting for the first seeded entry", func() bool {
			o.mu.Lock()
			defer o.mu.Unlock()
			return len(o.pods) > 0
		})

		o.scoped.reconcile(terminalScope(podTestNamespace))

		time.Sleep(20 * time.Millisecond)

		o.mu.Lock()
		after := len(o.pods)
		stranded := make(map[stateKey]map[string]podCondition, len(o.pods))
		for k, v := range o.pods {
			stranded[k] = v
		}
		o.mu.Unlock()
		if after != 0 {
			t.Fatalf("attempt %d: o.pods after concurrent initial-list teardown = %d entries, want 0 -- stranded: %+v", attempt, after, stranded)
		}
	}
}

// newScopedPodTestObserverWithoutSync is newScopedPodTestObserver
// (top of this file) without the waitFor(t, entry.pod.HasSynced) call --
// TestPodSeedDoesNotStrandUnderConcurrentTeardown needs reconcile to start
// the namespace WITHOUT waiting for its initial list to finish draining, so
// the immediate terminal reconcile that follows can race it. Kept as its own
// function rather than a parameter on the existing helper: every OTHER
// caller of newScopedPodTestObserver wants the sync, and threading an
// unused-everywhere-else bool through it would obscure that.
func newScopedPodTestObserverWithoutSync(t *testing.T, client *fake.Clientset) (*Observer, <-chan bus.Event) {
	t.Helper()
	b := bus.New(64)
	sub, unsub := b.Subscribe(0)
	t.Cleanup(unsub)

	o := New(client, b, func() RunScope {
		return RunScope{RunID: "run-1", Namespaces: map[string]struct{}{podTestNamespace: {}}}
	})
	o.scoped.reconcile(scopeWith("run-1", podTestNamespace))
	t.Cleanup(o.scoped.stop)

	return o, sub
}

// spinUntil busy-polls cond with NO sleep between checks, until it returns
// true or a 2s deadline passes. Used instead of scoped_test.go's waitFor
// (a 10ms-ticker poll, fine for every other caller in this package) by every
// test in this package that needs to observe "at least one delivery has
// landed" without also giving the race it's probing time to close:
// empirically, even a several-hundred-object initial-list delivery burst, or
// forty individual live Create() calls, can finish well under 10ms, so a
// 10ms-spaced check reliably observes "everything already landed" rather
// than "delivery just started" -- closing the exact race those tests exist
// to probe. A sleep-free loop still lets the OS scheduler run the
// informer's reflector goroutine on another thread (GOMAXPROCS permitting)
// without this goroutine explicitly yielding; the mutex acquisition inside
// cond is itself a scheduling point. Measured at GOMAXPROCS=1 under -race
// (Task 6 fix round 2 re-review): ~2ms of spin per attempt against this 2s
// deadline, three orders of magnitude of headroom -- a runtime.Gosched()
// call would be harmless but is not needed.
//
// what names the caller in the deadline-exceeded failure message: this is a
// SHARED helper with multiple call sites (as of fix round 2: two seed-strand
// tests plus TestEventDedupeDoesNotStrandUnderConcurrentTeardown's own
// proof-of-write check), and a bare "condition not met within 2s" from
// inside spinUntil's own frame (t.Helper() attributes the FILE:LINE to the
// caller, but not which condition) is hard to place in a CI failure list
// with more than one caller.
func spinUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: condition not met within 2s", what)
		}
	}
}

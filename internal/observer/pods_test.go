package observer

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
func newScopedPodTestObserver(t *testing.T, client *fake.Clientset) <-chan bus.Event {
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

	return sub
}

// waitForEvent blocks for up to within for the next published event, failing
// the test on timeout instead of checking immediately: unlike
// handlers_internal_test.go's decodeClusterData (which calls handlers
// directly, so bus.Publish has already run by the time the call returns),
// these tests drive a real informer whose delivery is asynchronous relative
// to the fake clientset call that triggered it.
func waitForEvent(t *testing.T, sub <-chan bus.Event, within time.Duration) bus.Event {
	t.Helper()
	select {
	case e := <-sub:
		return e
	case <-time.After(within):
		t.Fatal("no event published within deadline")
		return bus.Event{}
	}
}

func waitForClusterData(t *testing.T, sub <-chan bus.Event, within time.Duration) bus.ClusterData {
	t.Helper()
	e := waitForEvent(t, sub, within)
	var cd bus.ClusterData
	if err := json.Unmarshal(e.Data, &cd); err != nil {
		t.Fatalf("Unmarshal(ClusterData) error = %v, raw = %s", err, e.Data)
	}
	return cd
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

func withRunning(pod *corev1.Pod, container string) *corev1.Pod {
	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{Name: container, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, Ready: true},
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
		name     string
		mutate   func(*corev1.Pod) *corev1.Pod
		reason   string
		wantSev  bus.Severity
		wantCont string
	}{
		{
			name:     "ImagePullBackOff",
			mutate:   func(p *corev1.Pod) *corev1.Pod { return withContainerWaiting(p, "app", reasonImagePullBackOff) },
			reason:   reasonImagePullBackOff,
			wantSev:  bus.SeverityWarn,
			wantCont: "app",
		},
		{
			name:     "ErrImagePull",
			mutate:   func(p *corev1.Pod) *corev1.Pod { return withContainerWaiting(p, "app", reasonErrImagePull) },
			reason:   reasonErrImagePull,
			wantSev:  bus.SeverityWarn,
			wantCont: "app",
		},
		{
			name:     "CrashLoopBackOff",
			mutate:   func(p *corev1.Pod) *corev1.Pod { return withContainerWaiting(p, "app", reasonCrashLoopBackOff) },
			reason:   reasonCrashLoopBackOff,
			wantSev:  bus.SeverityError,
			wantCont: "app",
		},
		{
			name:     "unschedulable",
			mutate:   withUnschedulable,
			reason:   reasonUnschedulable,
			wantSev:  bus.SeverityError,
			wantCont: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			sub := newScopedPodTestObserver(t, client)

			pod := tc.mutate(testPod("pod-uid"))
			if _, err := client.CoreV1().Pods(podTestNamespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
				t.Fatalf("Create() error = %v", err)
			}

			cd := waitForClusterData(t, sub, 2*time.Second)
			if cd.Kind != kindPod {
				t.Errorf("Kind = %q, want %q", cd.Kind, kindPod)
			}
			if cd.UID != "pod-uid" {
				t.Errorf("UID = %q, want %q", cd.UID, "pod-uid")
			}
			if cd.Reason != tc.reason {
				t.Errorf("Reason = %q, want %q", cd.Reason, tc.reason)
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
	sub := newScopedPodTestObserver(t, client)

	running := withRunning(testPod("pod-uid"), "app")
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
	sub := newScopedPodTestObserver(t, client)

	healthy := withRunning(testPod("pod-uid"), "app")
	if _, err := client.CoreV1().Pods(podTestNamespace).Update(context.Background(), healthy, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	cd := waitForClusterData(t, sub, 2*time.Second)
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
	sub := newScopedPodTestObserver(t, client)

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
	sub := newScopedPodTestObserver(t, client)

	pod := withContainerWaiting(testPod("pod-typed-uid"), "trainer", reasonImagePullBackOff)
	if _, err := client.CoreV1().Pods(podTestNamespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	cd := waitForClusterData(t, sub, 2*time.Second)
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
// row forever. Seeded via onPodAdd's initial-list path directly -- the only
// way this observer ever records a Pod's trouble -- rather than through a
// full informer, matching handlers_internal_test.go's own rationale for
// testing onDelete's effect by direct call: nothing observable over the bus
// distinguishes a working delete-clearing path from a stub one except the
// publish itself.
func TestPodDeleteClearsTrackedTrouble(t *testing.T) {
	o, sub := newTestObserver(t)
	pod := withContainerWaiting(testPod("pod-uid"), "app", reasonImagePullBackOff)

	o.onPodAdd(pod, true) // initial list: seeds the baseline, does not narrate
	assertNoEvent(t, sub)

	o.onDelete(pod)

	cd := decodeClusterData(t, sub)
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

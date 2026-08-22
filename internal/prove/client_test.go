package prove_test

import (
	"context"
	"errors"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/mchmarny/aicrme/internal/prove"
)

// existingJob is what a prior Apply for runID left in the cluster: same
// name, namespace, and labels Render itself would produce, so tests exercise
// the identity Delete/WaitAbsent/ListOwned actually key off, not a stand-in
// for it.
func existingJob(runID string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      prove.WorkloadName(runID),
			Namespace: prove.Namespace,
			Labels:    prove.Labels(runID),
		},
	}
}

// unrelatedJob sits in the SAME namespace and carries the SAME managed-by
// value, differing only in component. A ListOwned that matched on
// managed-by alone -- or built an OR instead of an AND across the ownership
// pair -- would still let this one through; only a selector requiring both
// keys excludes it.
func unrelatedJob() *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "someone-elses-job",
			Namespace: prove.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "aicrme",
				"aicrme.dev/component":         "something-else",
			},
		},
	}
}

func TestClientReadyWithLiveKube(t *testing.T) {
	if !prove.NewClient(fake.NewSimpleClientset()).Ready() {
		t.Error("Ready() = false, want true for a live kube client")
	}
}

// A caller (steps.proveStep.Run) must be able to tell a nil kube apart from
// a live one before issuing any other call -- every other method
// dereferences kube immediately and panics on nil rather than degrading.
func TestClientReadyWithNilKube(t *testing.T) {
	if prove.NewClient(nil).Ready() {
		t.Error("Ready() = true, want false for a nil kube client")
	}
}

// Idempotent: stopping an already-stopped workload succeeds. An operator who
// clicks Stop twice, or a reconciliation that races one, must not see an
// error.
func TestDeleteIsIdempotent(t *testing.T) {
	c := prove.NewClient(fake.NewSimpleClientset())
	if err := c.Delete(context.Background(), "run-abc"); err != nil {
		t.Errorf("Delete() on absent workload error = %v, want nil", err)
	}
}

func TestDeleteUsesForegroundPropagation(t *testing.T) {
	cs := fake.NewSimpleClientset(existingJob("run-abc"))
	var policy *metav1.DeletionPropagation
	cs.PrependReactor("delete", "jobs", func(a k8stesting.Action) (bool, runtime.Object, error) {
		policy = a.(k8stesting.DeleteActionImpl).DeleteOptions.PropagationPolicy
		return false, nil, nil
	})
	_ = prove.NewClient(cs).Delete(context.Background(), "run-abc")
	if policy == nil || *policy != metav1.DeletePropagationForeground {
		t.Errorf("propagation = %v, want Foreground -- background deletion returns before the pods are gone", policy)
	}
}

// A non-NotFound Delete error (RBAC narrowing, an apiserver 5xx) must
// surface, not be folded into the idempotent "already gone" case -- an
// operator's Stop must not report success over a workload Delete never
// actually touched.
func TestDeleteNonNotFoundErrorSurfaces(t *testing.T) {
	cs := fake.NewSimpleClientset(existingJob("run-abc"))
	cs.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(corev1.Resource("jobs"), "prove-run-abc", errors.New("no access"))
	})
	if err := prove.NewClient(cs).Delete(context.Background(), "run-abc"); err == nil {
		t.Fatal("Delete() error = nil, want the Forbidden error to surface")
	}
}

// WaitAbsent must not return while the object still exists. A cleanup that
// reports success early is how a "failed" run leaves GPUs allocated.
func TestWaitAbsentBlocksWhilePresent(t *testing.T) {
	c := prove.NewClient(fake.NewSimpleClientset(existingJob("run-abc")))
	err := c.WaitAbsent(context.Background(), "run-abc", 200*time.Millisecond)
	if err == nil {
		t.Fatal("WaitAbsent() returned nil while the workload still exists")
	}
}

func TestWaitAbsentReturnsOnceGone(t *testing.T) {
	c := prove.NewClient(fake.NewSimpleClientset())
	if err := c.WaitAbsent(context.Background(), "run-abc", time.Second); err != nil {
		t.Errorf("WaitAbsent() error = %v, want nil", err)
	}
}

// A WaitAbsent that merely slept out the full timeout before a single
// presence check would also pass the two tests above -- neither one ever
// puts a workload in front of it that starts present and later disappears.
// This test does: the job is deleted 50ms into a 2s wait, and the elapsed-
// time assertion falsifies any implementation that does not re-check, since
// such an implementation would not return until the full 2s had passed.
func TestWaitAbsentNoticesDeletionMidWait(t *testing.T) {
	cs := fake.NewSimpleClientset(existingJob("run-abc"))
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = cs.BatchV1().Jobs(prove.Namespace).
			Delete(context.Background(), prove.WorkloadName("run-abc"), metav1.DeleteOptions{})
	}()

	start := time.Now()
	err := prove.NewClient(cs).WaitAbsent(context.Background(), "run-abc", 2*time.Second)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("WaitAbsent() error = %v, want nil once the workload is deleted", err)
	}
	if elapsed > time.Second {
		t.Errorf("WaitAbsent() took %s to notice a mid-wait deletion, want well under the 2s timeout", elapsed)
	}
}

// A non-NotFound Get error must surface immediately rather than being
// treated as "still present, keep polling" until timeout -- an RBAC
// narrowing or apiserver outage should not masquerade as a slow gang
// teardown. Bounding elapsed time falsifies an implementation that folds
// this error into the poll loop instead of returning it.
func TestWaitAbsentGetErrorSurfacesImmediately(t *testing.T) {
	cs := fake.NewSimpleClientset(existingJob("run-abc"))
	cs.PrependReactor("get", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(corev1.Resource("jobs"), "prove-run-abc", errors.New("no access"))
	})

	start := time.Now()
	err := prove.NewClient(cs).WaitAbsent(context.Background(), "run-abc", 5*time.Second)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("WaitAbsent() error = nil, want the Forbidden error to surface")
	}
	if elapsed > time.Second {
		t.Errorf("WaitAbsent() took %s to surface a non-NotFound error, want it to return well before the 5s timeout", elapsed)
	}
}

// Reconciliation finds workloads by label, so a record-less console can
// still see what it left behind.
func TestListOwnedFindsByLabelNotByRecord(t *testing.T) {
	cs := fake.NewSimpleClientset(existingJob("run-abc"), unrelatedJob())
	got, err := prove.NewClient(cs).ListOwned(context.Background())
	if err != nil {
		t.Fatalf("ListOwned() error = %v", err)
	}
	if len(got) != 1 || got[0].RunID != "run-abc" {
		t.Errorf("ListOwned() = %+v, want exactly the aicrme-owned job for run-abc", got)
	}
}

func TestListOwnedEmptyWhenNoneOwned(t *testing.T) {
	got, err := prove.NewClient(fake.NewSimpleClientset()).ListOwned(context.Background())
	if err != nil {
		t.Fatalf("ListOwned() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListOwned() = %+v, want empty", got)
	}
}

// ListOwned must also report the right Name and Namespace, not just RunID --
// Task 8's adoption path names the run from these fields directly.
func TestListOwnedReportsNameAndNamespace(t *testing.T) {
	cs := fake.NewSimpleClientset(existingJob("run-xyz"))
	got, err := prove.NewClient(cs).ListOwned(context.Background())
	if err != nil {
		t.Fatalf("ListOwned() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListOwned() = %+v, want exactly one workload", got)
	}
	want := prove.OwnedWorkload{
		RunID:     "run-xyz",
		Name:      prove.WorkloadName("run-xyz"),
		Namespace: prove.Namespace,
	}
	if got[0] != want {
		t.Errorf("ListOwned()[0] = %+v, want %+v", got[0], want)
	}
}

func TestListOwnedErrorSurfaces(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("list", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(corev1.Resource("jobs"), "", errors.New("no access"))
	})
	if _, err := prove.NewClient(cs).ListOwned(context.Background()); err == nil {
		t.Fatal("ListOwned() error = nil, want the Forbidden error to surface")
	}
}

// Apply must actually create the object the rest of this package's identity
// depends on -- parsed and checked field by field, not just "err == nil",
// per Task 3's own review lesson about substring assertions proving nothing
// about the object's shape.
func TestApplyCreatesTheParsedWorkload(t *testing.T) {
	cs := fake.NewSimpleClientset()
	if err := prove.NewClient(cs).Apply(context.Background(), "run-abc"); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	got, err := cs.BatchV1().Jobs(prove.Namespace).
		Get(context.Background(), prove.WorkloadName("run-abc"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get() after Apply() error = %v", err)
	}
	if got.Labels["aicrme.dev/run-id"] != "run-abc" {
		t.Errorf("run-id label = %q, want run-abc", got.Labels["aicrme.dev/run-id"])
	}
	if got.Spec.Completions == nil || *got.Spec.Completions != 2 {
		t.Errorf("spec.completions = %v, want 2", got.Spec.Completions)
	}
	if got.Spec.Parallelism == nil || *got.Spec.Parallelism != 2 {
		t.Errorf("spec.parallelism = %v, want 2", got.Spec.Parallelism)
	}
}

// A retried Apply for the same run must not error: WorkloadName is
// deterministic, so the second call finds its own object already there.
func TestApplyIsIdempotentForTheSameRun(t *testing.T) {
	cs := fake.NewSimpleClientset()
	c := prove.NewClient(cs)
	if err := c.Apply(context.Background(), "run-abc"); err != nil {
		t.Fatalf("first Apply() error = %v", err)
	}
	if err := c.Apply(context.Background(), "run-abc"); err != nil {
		t.Errorf("second Apply() error = %v, want nil -- a retried Apply for the same run must be idempotent", err)
	}
}

func TestApplyCreateErrorSurfaces(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(corev1.Resource("jobs"), "prove-run-abc", errors.New("no access"))
	})
	if err := prove.NewClient(cs).Apply(context.Background(), "run-abc"); err == nil {
		t.Fatal("Apply() error = nil, want the Forbidden error to surface")
	}
}

func TestEnsureNamespaceCreatesIt(t *testing.T) {
	cs := fake.NewSimpleClientset()
	if err := prove.NewClient(cs).EnsureNamespace(context.Background()); err != nil {
		t.Fatalf("EnsureNamespace() error = %v", err)
	}
	if _, err := cs.CoreV1().Namespaces().Get(context.Background(), prove.Namespace, metav1.GetOptions{}); err != nil {
		t.Errorf("Get() namespace after EnsureNamespace() error = %v, want the namespace to exist", err)
	}
}

// A namespace left by a prior run of this same process is success, not an
// error -- EnsureNamespace runs on every Prove start, not once per process.
func TestEnsureNamespaceIsIdempotent(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: prove.Namespace},
	})
	if err := prove.NewClient(cs).EnsureNamespace(context.Background()); err != nil {
		t.Errorf("EnsureNamespace() on an existing namespace error = %v, want nil", err)
	}
}

// placedPod is what a scheduler (or, on a fake clientset, a test standing in
// for one) leaves behind the instant it binds a gang member: the same
// ownership labels Render gives every pod in the workload's template, plus
// Spec.NodeName set -- the field PlacedNodes actually reads. Phase defaults
// to Running (the common healthy case); tests exercising the liveness
// qualifier override it.
func placedPod(runID, name, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: prove.Namespace,
			Labels:    prove.Labels(runID),
		},
		Spec:   corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// A pod with no NodeName has not been scheduled yet and must not count as
// placed, even though it already exists and carries the right labels.
func TestPlacedNodesReturnsOnlyScheduledPods(t *testing.T) {
	pending := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "prove-run-abc-1", Namespace: prove.Namespace, Labels: prove.Labels("run-abc"),
		},
	}
	cs := fake.NewSimpleClientset(placedPod("run-abc", "prove-run-abc-0", "gpu-node-0"), pending)
	got, err := prove.NewClient(cs).PlacedNodes(context.Background(), "run-abc")
	if err != nil {
		t.Fatalf("PlacedNodes() error = %v", err)
	}
	if len(got) != 1 || got["prove-run-abc-0"] != "gpu-node-0" {
		t.Errorf("PlacedNodes() = %+v, want exactly the one scheduled pod", got)
	}
}

// A pod already bound to a node but not yet started is Pending, not
// Running -- a live scheduler leaves a pod in exactly this state for a
// window after binding, before any kubelet has started its containers.
// Excluding Pending would make placement detection slower than the signal
// it exists to read.
func TestPlacedNodesCountsPendingAsPlaced(t *testing.T) {
	pod := placedPod("run-abc", "prove-run-abc-0", "gpu-node-0")
	pod.Status.Phase = corev1.PodPending
	cs := fake.NewSimpleClientset(pod)
	got, err := prove.NewClient(cs).PlacedNodes(context.Background(), "run-abc")
	if err != nil {
		t.Fatalf("PlacedNodes() error = %v", err)
	}
	if len(got) != 1 {
		t.Errorf("PlacedNodes() = %+v, want the Pending-but-bound pod counted as placed", got)
	}
}

// Spec.NodeName survives into Succeeded and Failed -- reading it alone
// would report a dead gang member as placed, and with workload.yaml's
// backoffLimit: 0 a Failed pod is never replaced, so the Job is
// permanently dead even though the gang would read as fully placed.
// A failed gang member is not a placed one: workload.yaml sets
// backoffLimit: 0, so nothing will replace it, and counting it would report a
// permanently failed Job as a successfully running gang.
func TestPlacedNodesExcludesFailedPods(t *testing.T) {
	failed := placedPod("run-abc", "prove-run-abc-0", "gpu-node-0")
	failed.Status.Phase = corev1.PodFailed
	cs := fake.NewSimpleClientset(failed)
	got, err := prove.NewClient(cs).PlacedNodes(context.Background(), "run-abc")
	if err != nil {
		t.Fatalf("PlacedNodes() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("PlacedNodes() = %+v, want the failed pod excluded", got)
	}
}

// The other half of that distinction, and the one a fake clientset gave the
// wrong answer to for a whole task: a pod the substrate completed the instant
// it was bound is still a placement decision.
//
// This is not hypothetical. KWOK -- whose simulated GPU nodes are a
// prerequisite of the only path this console can demo on -- marks a pod
// Succeeded in the same second it binds it: measured on the demo cluster,
// both gang members bound at 10:28:39/40 with the Job reporting
// completionTime 10:28:40 and no observable Running window at any poll
// interval. While Succeeded was excluded alongside Failed, every simulated
// run timed out at three minutes reporting 0/2 placed, over a gang the
// scheduler had in fact placed immediately.
func TestPlacedNodesCountsAPodCompletedTheInstantItWasBound(t *testing.T) {
	succeeded := placedPod("run-abc", "prove-run-abc-1", "gpu-node-1")
	succeeded.Status.Phase = corev1.PodSucceeded
	cs := fake.NewSimpleClientset(succeeded)
	got, err := prove.NewClient(cs).PlacedNodes(context.Background(), "run-abc")
	if err != nil {
		t.Fatalf("PlacedNodes() error = %v", err)
	}
	if got["prove-run-abc-1"] != "gpu-node-1" {
		t.Errorf("PlacedNodes() = %+v, want the completed pod counted on gpu-node-1", got)
	}
}

func TestPlacedNodesEmptyWhenNoPods(t *testing.T) {
	got, err := prove.NewClient(fake.NewSimpleClientset()).PlacedNodes(context.Background(), "run-abc")
	if err != nil {
		t.Fatalf("PlacedNodes() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("PlacedNodes() = %+v, want empty", got)
	}
}

// A selector built from Labels(runID) -- including the run-id key -- must
// exclude another run's pods even though they share the ownership pair and
// the namespace.
func TestPlacedNodesOnlyMatchesTheGivenRun(t *testing.T) {
	cs := fake.NewSimpleClientset(
		placedPod("run-abc", "prove-run-abc-0", "gpu-node-0"),
		placedPod("run-xyz", "prove-run-xyz-0", "gpu-node-1"),
	)
	got, err := prove.NewClient(cs).PlacedNodes(context.Background(), "run-abc")
	if err != nil {
		t.Fatalf("PlacedNodes() error = %v", err)
	}
	if len(got) != 1 {
		t.Errorf("PlacedNodes() = %+v, want exactly one pod scoped to run-abc", got)
	}
}

func TestPlacedNodesErrorSurfaces(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(corev1.Resource("pods"), "", errors.New("no access"))
	})
	if _, err := prove.NewClient(cs).PlacedNodes(context.Background(), "run-abc"); err == nil {
		t.Fatal("PlacedNodes() error = nil, want the Forbidden error to surface")
	}
}

func TestEnsureNamespaceErrorSurfaces(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(corev1.Resource("namespaces"), prove.Namespace, errors.New("no access"))
	})
	if err := prove.NewClient(cs).EnsureNamespace(context.Background()); err == nil {
		t.Fatal("EnsureNamespace() error = nil, want the Forbidden error to surface")
	}
}

// EnsureAbsent is Stop's delete-then-confirm sequence as one callable unit,
// so Reset can require the same guarantee without going through Stop --
// whose stoppable() guard rejects both an ordinary failed run and a run
// already moved to StateResetting.
func TestEnsureAbsentDeletesAndConfirms(t *testing.T) {
	cs := fake.NewSimpleClientset(existingJob("run-abc"))
	c := prove.NewClient(cs)

	if err := c.EnsureAbsent(context.Background(), "run-abc", time.Second); err != nil {
		t.Fatalf("EnsureAbsent() error = %v", err)
	}
	if _, err := cs.BatchV1().Jobs(prove.Namespace).
		Get(context.Background(), prove.WorkloadName("run-abc"), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("workload Get() = %v, want NotFound", err)
	}
}

// Nothing to delete is success: a run that never reached Prove has no
// workload, and Reset must not treat that as a precondition failure.
func TestEnsureAbsentSucceedsWhenNothingWasEverApplied(t *testing.T) {
	if err := prove.NewClient(fake.NewSimpleClientset()).
		EnsureAbsent(context.Background(), "run-never-proved", time.Second); err != nil {
		t.Errorf("EnsureAbsent() error = %v, want nil", err)
	}
}

// A delete the API server refused has not made anything absent, and
// EnsureAbsent must not go on to report success. This is the half Reset
// depends on most: it is what stops a teardown uninstalling the components
// beneath a workload that is still holding GPUs.
func TestEnsureAbsentReportsAFailedDelete(t *testing.T) {
	cs := fake.NewSimpleClientset(existingJob("run-abc"))
	cs.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("etcdserver: request timed out")
	})

	err := prove.NewClient(cs).EnsureAbsent(context.Background(), "run-abc", time.Second)
	if err == nil {
		t.Fatal("EnsureAbsent() error = nil, want the delete failure surfaced")
	}
}

// A workload the API server never finishes removing is not absent either.
// Delete succeeding only means the cascade STARTED.
func TestEnsureAbsentReportsAWorkloadThatOutlivesTheWait(t *testing.T) {
	cs := fake.NewSimpleClientset(existingJob("run-abc"))
	// Delete reports success without removing anything, which is exactly
	// what a foreground delete blocked on a finalizer looks like.
	cs.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil
	})

	err := prove.NewClient(cs).EnsureAbsent(context.Background(), "run-abc", 50*time.Millisecond)
	if err == nil {
		t.Fatal("EnsureAbsent() error = nil, want a timeout -- Delete returning nil is not proof of absence")
	}
}

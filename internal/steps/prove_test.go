package steps_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
	"github.com/mchmarny/aicrme/internal/prove"
	"github.com/mchmarny/aicrme/internal/steps"
)

// testRunID is the one run ID every test in this file uses. A single
// constant rather than a literal repeated at each call site is what keeps
// placedPod's label set, the run passed to Run(), and the workload name
// asserted against all provably the same run.
const testRunID = "run-abc"

// placedPod is what a scheduler (or, on a fake clientset, this test standing
// in for one) leaves behind the instant it binds a gang member: testRunID's
// ownership labels plus Spec.NodeName set -- the exact field Prove's own
// placement detection reads (prove.Client.PlacedNodes). Phase is Running: a
// terminated pod with NodeName still set is a different case, covered at
// the prove.Client level (TestPlacedNodesExcludesTerminatedPods), not
// re-pinned here.
func placedPod(name, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: prove.Namespace,
			Labels:    prove.Labels(testRunID),
		},
		Spec:   corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func TestProveImplementsActiveStep(t *testing.T) {
	s := steps.NewProve(prove.NewClient(fake.NewSimpleClientset()), steps.ProveConfig{})
	as, ok := s.(engine.ActiveStep)
	if !ok || !as.LeavesWorkloadRunning() {
		t.Error("Prove must implement ActiveStep and leave its workload running")
	}
}

// C1: a Prove step wrapping a Client with no live cluster connection (kube
// nil, as main.go's dev-mode fallback produces outside a pod) must fail the
// run, not crash the process. Reviewed against a real panic: before the
// Client.Ready guard existed, this exact call
// (steps.NewProve(prove.NewClient(nil), ...).Run(...)) paniced with a nil
// pointer dereference on the first call into c.kube. The deferred recover
// turns that into a clean test failure instead of taking the whole test
// binary down, so a regression here is loud either way.
func TestProveFailsGracefullyWithoutLiveCluster(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Run() panicked: %v -- want a returned error instead", r)
		}
	}()
	run := newRun()
	run.ID = testRunID
	err := steps.NewProve(prove.NewClient(nil), steps.ProveConfig{}).
		Run(context.Background(), run, func(bus.Event) {})
	if err == nil {
		t.Fatal("Run() succeeded with no live cluster client, want an error")
	}
}

func TestProveMetadata(t *testing.T) {
	s := steps.NewProve(prove.NewClient(fake.NewSimpleClientset()), steps.ProveConfig{})
	if s.Phase() != engine.PhaseProve {
		t.Errorf("Phase() = %q, want %q", s.Phase(), engine.PhaseProve)
	}
	if got := s.Requires(); len(got) != 0 {
		t.Errorf("Requires() = %v, want none -- Prove adds no operator question", got)
	}
}

// Corrected from the brief's newRun("run-abc"): the real helper
// (discover_test.go) takes no arguments, so the run ID is set directly.
//
// Also adds two placed pods the brief's test omitted. A fake clientset runs
// no Job controller, so nothing ever creates gang pods on its own -- without
// them, Run's own "wait for placement" logic would legitimately time out
// and this test's err != nil assertion would fail. The brief's test as
// written cannot pass against a real implementation of the wait it is
// meant to exercise.
func TestProveAppliesWorkloadAndRecordsIdentity(t *testing.T) {
	cs := fake.NewSimpleClientset(
		placedPod("prove-run-abc-0", "gpu-node-0"),
		placedPod("prove-run-abc-1", "gpu-node-1"),
	)
	run := newRun()
	run.ID = testRunID
	err := steps.NewProve(prove.NewClient(cs), steps.ProveConfig{GangTimeout: time.Second}).
		Run(context.Background(), run, func(bus.Event) {})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if run.Workload.Name != prove.WorkloadName(testRunID) || run.Workload.Namespace != prove.Namespace {
		t.Errorf("Workload = %+v, want the rendered identity", run.Workload)
	}
}

// Run must actually ensure the namespace, not merely apply into one that
// happens to already exist because Apply's Create call on a fake clientset
// does not itself validate namespace existence the way a real API server
// would.
func TestProveEnsuresNamespaceBeforeApplying(t *testing.T) {
	cs := fake.NewSimpleClientset(
		placedPod("prove-run-abc-0", "gpu-node-0"),
		placedPod("prove-run-abc-1", "gpu-node-1"),
	)
	run := newRun()
	run.ID = testRunID
	if err := steps.NewProve(prove.NewClient(cs), steps.ProveConfig{GangTimeout: time.Second}).
		Run(context.Background(), run, func(bus.Event) {}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if _, err := cs.CoreV1().Namespaces().Get(context.Background(), prove.Namespace, metav1.GetOptions{}); err != nil {
		t.Errorf("Get() namespace after Run() error = %v, want the namespace to exist", err)
	}
}

// C2, spec section 8 row 1: a partial apply must not leave the workload
// behind. This reproduces the exact client-side-failure-after-server-
// accepted shape a network blip produces -- the reactor writes the Job into
// the fake's tracker (what a real API server would have done) and then
// reports the create call itself as failed (what the client actually saw).
func TestProveCleansUpWhenApplyFailsAfterCreating(t *testing.T) {
	cs := fake.NewSimpleClientset()
	gvr := batchv1.SchemeGroupVersion.WithResource("jobs")
	cs.PrependReactor("create", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      prove.WorkloadName(testRunID),
				Namespace: prove.Namespace,
				Labels:    prove.Labels(testRunID),
			},
		}
		if err := cs.Tracker().Create(gvr, job, prove.Namespace); err != nil {
			t.Fatalf("seeding the accepted-but-unacknowledged create failed: %v", err)
		}
		return true, nil, errors.New("connection reset by peer")
	})
	run := newRun()
	run.ID = testRunID
	err := steps.NewProve(prove.NewClient(cs), steps.ProveConfig{GangTimeout: 100 * time.Millisecond}).
		Run(context.Background(), run, func(bus.Event) {})
	if err == nil {
		t.Fatal("Run() succeeded though Apply failed")
	}
	if _, getErr := cs.BatchV1().Jobs(prove.Namespace).
		Get(context.Background(), prove.WorkloadName(testRunID), metav1.GetOptions{}); !apierrors.IsNotFound(getErr) {
		t.Error("workload still exists after Apply failed post-create -- it can still hold GPUs")
	}
}

// "One event per placement decision" (spec section 4) means exactly that:
// not zero, and not one per 20ms poll tick for the same pod.
func TestProveEmitsOneEventPerPlacement(t *testing.T) {
	cs := fake.NewSimpleClientset(
		placedPod("prove-run-abc-0", "gpu-node-0"),
		placedPod("prove-run-abc-1", "gpu-node-1"),
	)
	run := newRun()
	run.ID = testRunID
	var placements int
	err := steps.NewProve(prove.NewClient(cs), steps.ProveConfig{GangTimeout: time.Second}).
		Run(context.Background(), run, func(e bus.Event) {
			if e.Kind == bus.KindCluster {
				placements++
			}
		})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if placements != 2 {
		t.Errorf("placement events = %d, want exactly 2 (one per gang member)", placements)
	}
}

// I3: TestProveEmitsOneEventPerPlacement seeds both pods pre-placed, so
// awaitGang completes on its first poll and the dedupe guard never sees a
// second poll to prove itself against. This stages the second member's
// placement a few poll intervals later, forcing several polls that each
// re-see the first pod, so a missing (or broken) dedupe would emit it
// again on every one of them.
func TestProveDedupesPlacementEventsAcrossPolls(t *testing.T) {
	cs := fake.NewSimpleClientset(placedPod("prove-run-abc-0", "gpu-node-0"))
	go func() {
		time.Sleep(80 * time.Millisecond)
		_, _ = cs.CoreV1().Pods(prove.Namespace).Create(context.Background(),
			placedPod("prove-run-abc-1", "gpu-node-1"), metav1.CreateOptions{})
	}()

	run := newRun()
	run.ID = testRunID
	var placements int
	err := steps.NewProve(prove.NewClient(cs), steps.ProveConfig{GangTimeout: time.Second}).
		Run(context.Background(), run, func(e bus.Event) {
			if e.Kind == bus.KindCluster {
				placements++
			}
		})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if placements != 2 {
		t.Errorf("placement events over a staggered gang = %d, want exactly 2 (one per member, no poll-tick duplicates)", placements)
	}
}

// gangSize gates on BOTH members placing, not merely one: a run that
// declared success with a lone pod placed would report a gang-scheduled
// workload while only holding half the GPUs the demo claims.
func TestProveRequiresBothGangMembersPlaced(t *testing.T) {
	cs := fake.NewSimpleClientset(placedPod("prove-run-abc-0", "gpu-node-0"))
	run := newRun()
	run.ID = testRunID
	err := steps.NewProve(prove.NewClient(cs), steps.ProveConfig{GangTimeout: 100 * time.Millisecond}).
		Run(context.Background(), run, func(bus.Event) {})
	if err == nil {
		t.Fatal("Run() succeeded with only one of two gang members placed")
	}
	if _, getErr := cs.BatchV1().Jobs(prove.Namespace).
		Get(context.Background(), prove.WorkloadName(testRunID), metav1.GetOptions{}); !apierrors.IsNotFound(getErr) {
		t.Error("workload still exists after an incomplete gang timed out")
	}
}

// A gang that never places is a failure -- and the workload must be GONE
// before the step returns, because a pending gang can still place later.
func TestProveCleansUpWhenGangNeverPlaces(t *testing.T) {
	cs := fake.NewSimpleClientset()
	run := newRun()
	run.ID = testRunID
	// no pods ever become Running
	err := steps.NewProve(prove.NewClient(cs), steps.ProveConfig{GangTimeout: 100 * time.Millisecond}).
		Run(context.Background(), run, func(bus.Event) {})
	if err == nil {
		t.Fatal("Run() succeeded though the gang never placed")
	}
	if _, getErr := cs.BatchV1().Jobs(prove.Namespace).
		Get(context.Background(), prove.WorkloadName(testRunID), metav1.GetOptions{}); !apierrors.IsNotFound(getErr) {
		t.Error("workload still exists after a gang timeout -- it can still place and hold GPUs")
	}
}

// If cleanup itself fails, the error must say so rather than reporting a
// clean failure over an uncleaned cluster.
//
// Asserting the SPECIFIC "cleanup failed deleting" wording, not merely the
// substring "cleanup", is what makes this test pin the Delete branch the
// injected reactor exists to reach: a looser "contains cleanup" check also
// passes if Delete's own error is silently swallowed and WaitAbsent's own
// natural timeout supplies the word instead (Ruling 13 in the review) --
// that would report "cleanup failed waiting", a true but different claim
// about which call actually failed.
func TestProveReportsCleanupFailureDistinctly(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("delete refused")
	})
	run := newRun()
	run.ID = testRunID
	err := steps.NewProve(prove.NewClient(cs), steps.ProveConfig{GangTimeout: 50 * time.Millisecond}).
		Run(context.Background(), run, func(bus.Event) {})
	if err == nil || !strings.Contains(err.Error(), "cleanup failed deleting") {
		t.Errorf("Run() error = %v, want it to name the failed Delete specifically", err)
	}
}

// The other half of the same distinction (Ruling 13): an ordinary failure
// whose cleanup SUCCEEDED must never be reported as a cleanup failure, or
// an operator (or a future console alert) grepping the error for "cleanup"
// cannot tell a clean failure from a dirty one.
func TestProveOrdinaryFailureIsNotReportedAsCleanupFailure(t *testing.T) {
	cs := fake.NewSimpleClientset() // no reactors -- Delete and WaitAbsent both succeed
	run := newRun()
	run.ID = testRunID
	err := steps.NewProve(prove.NewClient(cs), steps.ProveConfig{GangTimeout: 100 * time.Millisecond}).
		Run(context.Background(), run, func(bus.Event) {})
	if err == nil {
		t.Fatal("Run() succeeded though the gang never placed")
	}
	if strings.Contains(err.Error(), "cleanup") {
		t.Errorf("Run() error = %v, a successful cleanup must not be reported as a cleanup failure", err)
	}
}

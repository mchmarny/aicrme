package steps_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
// placement detection reads (prove.Client.PlacedNodes).
func placedPod(name, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: prove.Namespace,
			Labels:    prove.Labels(testRunID),
		},
		Spec: corev1.PodSpec{NodeName: node},
	}
}

func TestProveImplementsActiveStep(t *testing.T) {
	s := steps.NewProve(prove.NewClient(fake.NewSimpleClientset()), steps.ProveConfig{})
	as, ok := s.(engine.ActiveStep)
	if !ok || !as.LeavesWorkloadRunning() {
		t.Error("Prove must implement ActiveStep and leave its workload running")
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
func TestProveReportsCleanupFailureDistinctly(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("delete refused")
	})
	run := newRun()
	run.ID = testRunID
	err := steps.NewProve(prove.NewClient(cs), steps.ProveConfig{GangTimeout: 50 * time.Millisecond}).
		Run(context.Background(), run, func(bus.Event) {})
	if err == nil || !strings.Contains(err.Error(), "cleanup") {
		t.Errorf("Run() error = %v, want it to name the failed cleanup", err)
	}
}

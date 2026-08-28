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

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
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

// A gang that never places has to SAY so, with the budget it was given and
// how much of the gang made it. Nothing pinned this wording before -- every
// timeout test above asserts only that Run failed -- which is how a real run
// came to record client-go's rate limiter refusing a call as its entire
// error, naming neither the timeout nor the 0/2 placement that was the
// actual diagnosis.
func TestProveGangTimeoutNamesTheDeadlineAndTheCount(t *testing.T) {
	cs := fake.NewSimpleClientset()
	run := newRun()
	run.ID = testRunID
	err := steps.NewProve(prove.NewClient(cs), steps.ProveConfig{GangTimeout: 100 * time.Millisecond}).
		Run(context.Background(), run, func(bus.Event) {})
	if err == nil {
		t.Fatal("Run() succeeded though the gang never placed")
	}
	for _, want := range []string{"did not place within", "100ms", "0/2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Run() error = %q, want it to contain %q", err.Error(), want)
		}
	}
}

// Nothing placed at all is a different failure from a gang that placed some
// members, and there is one known cause: a kai-scheduler left inconsistent by
// an earlier install: kai-scheduler's SchedulingShard survives an uninstall by
// design and owns the kai-scheduler-default Deployment, so a cluster installed
// into twice can be running the previous install's scheduler against a control
// plane replaced underneath it.
//
// THIS MESSAGE HAS BEEN WRONG TWICE, and each error cost real time, so the
// assertions below pin what is known rather than what was believed:
//
// It first prescribed deleting the shard and restarting the deployments, from
// a KWOK measurement. Measured NOT to work on real GKE H100s on 2026-08-26 --
// all six deployments rolled cleanly and the retry failed identically. Those
// commands must never come back, which the second loop below enforces.
//
// It then blamed the pod-grouper and said the failure follows a Reset. Both
// were wrong. The pod-grouper's `already exists` pair appears on healthy first
// installs too, and on 2026-08-28 this failed on a cluster that had NEVER been
// reset -- a second run had installed over the first. Asserting "pod-grouper"
// and "has not been reset", as this test used to, pinned the wrong diagnosis
// in place.
//
// What an operator needs at the end of a three-minute wait is a check they can
// run in five seconds, so that is what is asserted.
func TestProveGangTimeoutPointsAtTheKnownCauseWhenNothingPlaced(t *testing.T) {
	cs := fake.NewSimpleClientset()
	run := newRun()
	run.ID = testRunID
	err := steps.NewProve(prove.NewClient(cs), steps.ProveConfig{GangTimeout: 100 * time.Millisecond}).
		Run(context.Background(), run, func(bus.Event) {})
	if err == nil {
		t.Fatal("Run() succeeded though the gang never placed")
	}
	for _, want := range []string{"kai-scheduler", "SchedulingShard", "get deploy", "Reset"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Run() error = %q, want it to name the mechanism and the check (%q)", err.Error(), want)
		}
	}
	// The withdrawn diagnosis must not come back either. The pod-grouper
	// chatter is noise that appears on healthy installs, and "a cluster that
	// has not been reset is the only path that works" is false: Reset then
	// install is proven on real hardware, twice.
	for _, gone := range []string{"pod-grouper", "has not been reset"} {
		if strings.Contains(err.Error(), gone) {
			t.Errorf("Run() error = %q, still carries the withdrawn diagnosis %q", err.Error(), gone)
		}
	}
	// The disproven commands must not come back. This assertion is the only
	// thing standing between a future edit and re-shipping advice that was
	// measured not to work.
	for _, gone := range []string{"rollout restart", "delete schedulingshard"} {
		if strings.Contains(err.Error(), gone) {
			t.Errorf("Run() error = %q, still prescribes %q, which was measured not to clear the failure", err.Error(), gone)
		}
	}
}

// The same claim when the placement READ is what notices the deadline first.
//
// A List still in flight when the budget expires fails with the deadline's
// own error, and that used to be returned verbatim: measured on a real KWOK
// cluster, the run's recorded failure was "client rate limiter Wait returned
// an error: rate: Wait(n=1) would exceed context deadline" -- a true
// statement about plumbing that hid the fact that nothing had been placed at
// all. The reactor below reproduces that shape exactly: a read that outlives
// the budget and then fails.
func TestProveReportsATimedOutPlacementReadAsTheGangTimeout(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		time.Sleep(80 * time.Millisecond)
		return true, nil, errors.New("client rate limiter Wait returned an error: rate: Wait(n=1) would exceed context deadline")
	})
	run := newRun()
	run.ID = testRunID
	err := steps.NewProve(prove.NewClient(cs), steps.ProveConfig{GangTimeout: 50 * time.Millisecond}).
		Run(context.Background(), run, func(bus.Event) {})
	if err == nil {
		t.Fatal("Run() succeeded though the placement read failed after the deadline")
	}
	if !strings.Contains(err.Error(), "did not place within") {
		t.Errorf("Run() error = %q, want the gang timeout rather than the read's own error", err.Error())
	}
	if strings.Contains(err.Error(), "rate limiter") {
		t.Errorf("Run() error = %q, leaks the plumbing that happened to notice the deadline", err.Error())
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
	// Fix round 1's C3: the structural half of the same distinction. A
	// confirmed-clean cleanup must not satisfy engine.ErrUnconfirmedCleanup
	// either, or Ruling 12 would block Start over every ordinary Prove
	// failure, not just the unconfirmed ones.
	if errors.Is(err, engine.ErrUnconfirmedCleanup) {
		t.Errorf("Run() error = %v, want errors.Is(err, engine.ErrUnconfirmedCleanup) = false for a confirmed-clean cleanup", err)
	}
}

// TestCleanupFailureWrapsErrUnconfirmedCleanup pins the producer side of
// fix round 1's C3 cross-package contract: steps/prove.go's cleanup helper
// must wrap engine.ErrUnconfirmedCleanup (errors.Is-checkable) whenever
// EITHER of its own two cluster calls fails, not just carry text that
// happens to look right. Companion to
// TestProveReportsCleanupFailureDistinctly (which pins the operator-facing
// text) and TestRealCleanupFailureBlocksEngineStart below (which pins the
// consumer side, end to end).
func TestCleanupFailureWrapsErrUnconfirmedCleanup(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("delete refused")
	})
	run := newRun()
	run.ID = testRunID
	err := steps.NewProve(prove.NewClient(cs), steps.ProveConfig{GangTimeout: 50 * time.Millisecond}).
		Run(context.Background(), run, func(bus.Event) {})
	if err == nil {
		t.Fatal("Run() succeeded though Delete failed")
	}
	if !errors.Is(err, engine.ErrUnconfirmedCleanup) {
		t.Errorf("Run() error = %v, want errors.Is(err, engine.ErrUnconfirmedCleanup) = true", err)
	}
}

// TestRealCleanupFailureBlocksEngineStart is fix round 1's C3 regression,
// end to end. The engine used to key Ruling 12's guard off a substring of
// this package's error TEXT, and a one-character reword of that text (a
// 2-line diff) left the whole go test ./... suite green with the guard
// silently dead -- because nothing anywhere drove a REAL steps.NewProve
// cleanup failure through a REAL engine.Engine and checked that the guard
// actually fired. internal/engine's own tests cannot catch this class of
// regression: they hand-construct a fakeStep returning an error that
// WRAPS engine.ErrUnconfirmedCleanup directly (active_test.go's
// unconfirmedCleanupErr), which pins engine against itself, not against
// what this package actually produces. Only a test living here, importing
// engine (steps already does; the reverse would cycle), can close that gap.
// waitForEngineFailed polls e directly (no engine-package internals
// available from this black-box package) until id reaches StateFailed or 2
// seconds pass. Every test in this file that needs to wait wants exactly
// this state -- a hardcoded target, not a want parameter every call site
// happened to pass the same value for.
func waitForEngineFailed(t *testing.T, e *engine.Engine, id string) *engine.Run {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var got *engine.Run
	var err error
	for {
		got, err = e.Get(context.Background(), id)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if got.State == engine.StateFailed {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("run never reached state %q, last state %q", engine.StateFailed, got.State)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRealCleanupFailureBlocksEngineStart(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("delete refused")
	})
	step := steps.NewProve(prove.NewClient(cs), steps.ProveConfig{GangTimeout: 50 * time.Millisecond})
	b := bus.New(64)
	e := engine.New(b, engine.NewMemoryStore(), step)

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	got := waitForEngineFailed(t, e, run.ID)
	if !got.CleanupUnconfirmed {
		t.Fatalf("run.CleanupUnconfirmed = false after a real Delete-refusing cleanup failure, want true (Err = %q)", got.Err)
	}

	if _, startErr := e.Start(context.Background()); startErr == nil {
		t.Fatal("Start() succeeded after a real, unconfirmed steps.Prove cleanup failure -- Ruling 12's guard is dead")
	} else {
		var se *aicrerrors.StructuredError
		if !errors.As(startErr, &se) || se.Code != aicrerrors.ErrCodeConflict {
			t.Errorf("Start() error = %v, want ErrCodeConflict", startErr)
		}
	}
}

// TestRetryFailingAtEnsureNamespaceDoesNotClearRuling12Guard is fix round
// 2's N2 regression, reproduced end to end with the real steps.NewProve
// cleanup path -- matching the reviewer's own probe. Attempt 1's Delete is
// refused, leaving a real orphaned Job in the fake cluster. The RETRY fails
// at EnsureNamespace -- a real API call that runs BEFORE Apply, so it never
// reaches cleanup at all -- which fix round 1's implementation misread as
// "nothing to report", clearing Run.CleanupUnconfirmed over an orphan
// nothing had actually confirmed gone. EnsureNamespace is not a narrow or
// contrived case: it fails for the exact same class of reasons the first
// Delete did (a live API call that can be refused), so it is the
// CORRELATED failure, not an independent one.
func TestRetryFailingAtEnsureNamespaceDoesNotClearRuling12Guard(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("delete refused")
	})
	nsAttempts := 0
	cs.PrependReactor("create", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		nsAttempts++
		if nsAttempts == 1 {
			return false, nil, nil // let Start's own EnsureNamespace through normally
		}
		return true, nil, errors.New("namespace create refused")
	})
	step := steps.NewProve(prove.NewClient(cs), steps.ProveConfig{GangTimeout: 50 * time.Millisecond})
	b := bus.New(64)
	e := engine.New(b, engine.NewMemoryStore(), step)

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	got := waitForEngineFailed(t, e, run.ID)
	if !got.CleanupUnconfirmed {
		t.Fatalf("fixture run.CleanupUnconfirmed = false after a real Delete-refusing cleanup failure, want true (Err = %q)", got.Err)
	}
	// This test's own premise: a real orphaned Job, left behind because
	// Delete was refused.
	if _, getErr := cs.BatchV1().Jobs(prove.Namespace).
		Get(context.Background(), prove.WorkloadName(run.ID), metav1.GetOptions{}); getErr != nil {
		t.Fatalf("Get() error = %v, want the orphaned Job to exist as this test's own premise", getErr)
	}

	if _, retryErr := e.Retry(context.Background(), run.ID); retryErr != nil {
		t.Fatalf("Retry() error = %v", retryErr)
	}
	retried := waitForEngineFailed(t, e, run.ID)
	if !strings.Contains(retried.Err, "namespace") {
		t.Fatalf("retried run.Err = %q, want it to name the EnsureNamespace failure (fixture check)", retried.Err)
	}
	if !retried.CleanupUnconfirmed {
		t.Errorf("CleanupUnconfirmed = false after a retry that failed at EnsureNamespace (cleanup never reached), want true (sticky) -- Err = %q", retried.Err)
	}

	if _, startErr := e.Start(context.Background()); startErr == nil {
		t.Fatal("Start() succeeded after a retry that never confirmed the orphan is gone -- Ruling 12's guard is dead")
	} else {
		var se *aicrerrors.StructuredError
		if !errors.As(startErr, &se) || se.Code != aicrerrors.ErrCodeConflict {
			t.Errorf("Start() error = %v, want ErrCodeConflict", startErr)
		}
	}

	// The orphan is still exactly where it was -- nothing in this whole
	// sequence ever confirmed removing it.
	if _, getErr := cs.BatchV1().Jobs(prove.Namespace).
		Get(context.Background(), prove.WorkloadName(run.ID), metav1.GetOptions{}); getErr != nil {
		t.Errorf("orphaned Job no longer present after the retry (Get error = %v), but nothing in this run ever confirmed removing it", getErr)
	}
}

// TestRealConfirmedCleanupClearsRuling12GuardEndToEnd is fix round 3's
// NEW-1: engine.ErrCleanupConfirmed had an engine-side fixture only
// (internal/engine's TestRetryWithConfirmedCleanupClearsGuard, a hand-built
// error that pins the engine against itself) and no cross-package producer
// test -- the exact asymmetry TestRealCleanupFailureBlocksEngineStart
// exists to close for ErrUnconfirmedCleanup, on the OTHER sentinel. If
// steps/prove.go stopped wrapping ErrCleanupConfirmed on its success path,
// the guard would become clearable only through Stop, forever -- the wedge
// the reviewer independently probed and confirmed impossible (flag set,
// orphan removed out of band, three retries that all fail at
// EnsureNamespace leave the guard set and Start blocked, then Stop
// resolves it) -- reintroduced by a rewording nothing here would catch.
//
// Attempt 1's Delete is refused (a real orphaned Job is left behind,
// CleanupUnconfirmed becomes true). The RETRY's Delete and WaitAbsent both
// succeed this time (the reactor is one-shot), so cleanup() reaches its
// success path and wraps ErrCleanupConfirmed around the retry's own gang
// timeout -- and the guard must clear, driven through a real engine.Engine,
// not a hand-built error.
func TestRealConfirmedCleanupClearsRuling12GuardEndToEnd(t *testing.T) {
	cs := fake.NewSimpleClientset()
	deleteAttempts := 0
	cs.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		deleteAttempts++
		if deleteAttempts == 1 {
			return true, nil, errors.New("delete refused")
		}
		return false, nil, nil // let the retry's Delete through normally
	})
	step := steps.NewProve(prove.NewClient(cs), steps.ProveConfig{GangTimeout: 50 * time.Millisecond})
	b := bus.New(64)
	e := engine.New(b, engine.NewMemoryStore(), step)

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	got := waitForEngineFailed(t, e, run.ID)
	if !got.CleanupUnconfirmed {
		t.Fatalf("fixture run.CleanupUnconfirmed = false after a real Delete-refusing cleanup failure, want true (Err = %q)", got.Err)
	}
	if _, startErr := e.Start(context.Background()); startErr == nil {
		t.Fatal("fixture check: Start() succeeded before Retry, want the guard still blocking")
	}

	if _, retryErr := e.Retry(context.Background(), run.ID); retryErr != nil {
		t.Fatalf("Retry() error = %v", retryErr)
	}
	retried := waitForEngineFailed(t, e, run.ID)
	if retried.CleanupUnconfirmed {
		t.Errorf("CleanupUnconfirmed = true after a retry whose own Delete+WaitAbsent succeeded, want false (Err = %q)", retried.Err)
	}
	if strings.Contains(retried.Err, "cleanup") {
		t.Errorf("retried run.Err = %q, a confirmed-clean cleanup must not be reported as a cleanup failure (Ruling 13)", retried.Err)
	}

	if _, startErr := e.Start(context.Background()); startErr != nil {
		t.Errorf("Start() error = %v after a retry that confirmed the cleanup, want nil", startErr)
	}

	// The confirmation is real, not merely reported: the Job the retry's
	// own Delete removed is actually gone.
	if _, getErr := cs.BatchV1().Jobs(prove.Namespace).
		Get(context.Background(), prove.WorkloadName(run.ID), metav1.GetOptions{}); getErr == nil {
		t.Error("workload still present after a retry that reported its cleanup confirmed")
	}
}

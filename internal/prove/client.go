package prove

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
)

// Client applies, deletes, and rediscovers the Prove reference workload
// against a live cluster. It is the only thing in this package that talks
// to the API server -- manifest.go only ever renders bytes.
type Client struct {
	kube kubernetes.Interface
}

// NewClient wraps an existing clientset. Tests pass fake.NewSimpleClientset;
// production wiring passes the real one -- which can be nil outside a pod
// (rest.InClusterConfig fails on a developer laptop), so a caller must check
// Ready before issuing any other call.
func NewClient(kube kubernetes.Interface) *Client {
	return &Client{kube: kube}
}

// Ready reports whether this Client has a live cluster connection. Every
// other method dereferences kube immediately on call and panics on a nil
// one rather than degrading -- callers that cannot guarantee a non-nil kube
// (main.go's dev-mode fallback) must check this first and fail their own
// caller cleanly instead.
func (c *Client) Ready() bool { return c.kube != nil }

// OwnedWorkload is what ListOwned returns for a discovered workload: enough
// identity for the console to name it, offer Stop, or adopt it into a
// synthetic run (Task 8) -- never a copy of the object itself, because every
// caller here only needs to know it exists and which run it belongs to.
type OwnedWorkload struct {
	RunID     string
	Name      string
	Namespace string
}

// waitAbsentPollInterval bounds how quickly WaitAbsent notices a deletion
// has finished. It sits well under every timeout this package's own tests
// and the Prove step's GangTimeout use (as low as 50ms in Task 5's cleanup
// path), so a poll never eats a meaningful fraction of the caller's budget.
const waitAbsentPollInterval = 20 * time.Millisecond

// runIDLabelKey is the one label key this file reads back off a discovered
// object, and must agree with the key Labels (manifest.go) sets under the
// same name. Unlike the ownership pair (managed-by, component) -- which
// ownershipSelector derives FROM Labels below, so it cannot drift -- Labels
// exposes no accessor for this key alone: the run ID is exactly the value
// that varies per call, so it cannot be recovered by diffing two Labels()
// outputs against a shared key the way the ownership pair can be isolated.
const runIDLabelKey = "aicrme.dev/run-id"

// ownershipSelector matches every workload Prove owns, regardless of which
// run created it. Built from Labels itself with the run-scoped key removed,
// so a future addition to Labels' ownership pair is picked up here
// automatically instead of needing this file edited to match.
func ownershipSelector() string {
	set := Labels("") // run ID is irrelevant; only the ownership pair matters
	delete(set, runIDLabelKey)
	return labels.SelectorFromSet(set).String()
}

// EnsureNamespace creates the dedicated Prove namespace if it does not exist
// yet. Idempotent: an existing namespace -- this process's own prior run, or
// a fresh install that already created it -- is success, not an error.
func (c *Client) EnsureNamespace(ctx context.Context) error {
	_, err := c.kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: Namespace},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("prove: creating namespace %s: %w", Namespace, err)
	}
	return nil
}

// Apply renders and creates the workload for runID. It is idempotent for the
// same run: WorkloadName is deterministic and Render's output for a given
// run never changes, so a retried Apply finds its own object already there
// and treats that as success rather than erroring on AlreadyExists -- it
// never issues an Update, because a Job's placement-defining fields
// (completions, parallelism, selector) are immutable once created, and a
// repeat Apply for the same run has nothing to change them to.
func (c *Client) Apply(ctx context.Context, runID string) error {
	out, err := Render(runID, Namespace)
	if err != nil {
		return fmt.Errorf("prove: rendering workload for run %s: %w", runID, err)
	}
	var job batchv1.Job
	if decodeErr := yaml.Unmarshal(out, &job); decodeErr != nil {
		return fmt.Errorf("prove: decoding rendered workload for run %s: %w", runID, decodeErr)
	}
	_, err = c.kube.BatchV1().Jobs(Namespace).Create(ctx, &job, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("prove: applying workload for run %s: %w", runID, err)
	}
	return nil
}

// Delete removes the workload for runID with foreground propagation. Delete
// does not return until the API server has finished cascading the delete to
// every dependent (the pods): a Stop that reports success while background
// deletion is still tearing pods down -- and the GPUs they hold -- is the
// single worst outcome in this design.
//
// A missing workload is success: the caller's intent (nothing should be
// running) is already satisfied, so an operator clicking Stop twice, or a
// reconciliation racing one, never sees an error.
func (c *Client) Delete(ctx context.Context, runID string) error {
	foreground := metav1.DeletePropagationForeground
	err := c.kube.BatchV1().Jobs(Namespace).Delete(ctx, WorkloadName(runID), metav1.DeleteOptions{
		PropagationPolicy: &foreground,
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("prove: deleting workload for run %s: %w", runID, err)
	}
	return nil
}

// WaitAbsent blocks until the workload for runID is gone or timeout elapses.
// Foreground deletion (Delete, above) only guarantees the API server has
// STARTED cascading the delete; a caller that treats a successful Delete
// call as "gone" can still race a gang that places after cleanup was
// declared done. Polling Get until it 404s is what actually closes that
// window -- and doing so on a ticker, rather than sleeping out the full
// timeout before a single check, is what lets WaitAbsent notice a deletion
// that finishes well before timeout elapses.
func (c *Client) WaitAbsent(ctx context.Context, runID string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	name := WorkloadName(runID)
	ticker := time.NewTicker(waitAbsentPollInterval)
	defer ticker.Stop()

	for {
		_, err := c.kube.BatchV1().Jobs(Namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("prove: checking workload %s/%s for absence: %w", Namespace, name, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("prove: workload %s/%s still present after %s: %w", Namespace, name, timeout, ctx.Err())
		case <-ticker.C:
		}
	}
}

// PlacedNodes returns, for runID's gang, the node each already-scheduled AND
// still-live pod has been bound to, keyed by pod name. A pod absent from the
// result has either not been placed yet or has already terminated.
//
// Reading Spec.NodeName -- the field the scheduler itself writes the instant
// it binds a pod -- rather than Status.Phase alone is what makes placement
// itself trustworthy against a fake clientset that runs no kubelet and would
// never advance Phase past Pending: NodeName means "scheduled" on a real
// cluster and a faked one alike, with no controller needing to run for it to
// mean that. But NodeName alone is not sufficient: it survives into
// Succeeded and Failed, so without the Phase check below, a gang member that
// already died -- and, with workload.yaml's backoffLimit: 0, will never be
// replaced -- would still be counted as placed, reporting a permanently
// failed Job as a successfully running gang.
func (c *Client) PlacedNodes(ctx context.Context, runID string) (map[string]string, error) {
	list, err := c.kube.CoreV1().Pods(Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(Labels(runID)).String(),
	})
	if err != nil {
		return nil, fmt.Errorf("prove: listing pods for run %s: %w", runID, err)
	}
	out := make(map[string]string, len(list.Items))
	for _, pod := range list.Items {
		if pod.Spec.NodeName == "" {
			continue
		}
		if pod.Status.Phase != corev1.PodPending && pod.Status.Phase != corev1.PodRunning {
			continue
		}
		out[pod.Name] = pod.Spec.NodeName
	}
	return out, nil
}

// ListOwned finds every workload Prove owns by label, never by a persisted
// run record: terminal saves are best-effort and the run store can degrade
// to memory (internal/engine/cmstore.go), so a console that could only find
// its workload through the record would lose track of it exactly when the
// record was lost -- while the workload kept holding GPUs. Task 8's startup
// reconciliation depends on this being label-driven.
func (c *Client) ListOwned(ctx context.Context) ([]OwnedWorkload, error) {
	list, err := c.kube.BatchV1().Jobs(Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: ownershipSelector(),
	})
	if err != nil {
		return nil, fmt.Errorf("prove: listing owned workloads: %w", err)
	}
	out := make([]OwnedWorkload, 0, len(list.Items))
	for _, job := range list.Items {
		out = append(out, OwnedWorkload{
			RunID:     job.Labels[runIDLabelKey],
			Name:      job.Name,
			Namespace: job.Namespace,
		})
	}
	return out, nil
}

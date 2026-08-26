package prove

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	// extraTolerations are added to the reference workload's pod so it can
	// land on THIS cluster's GPU nodes. workload.yaml carries the two taints
	// that cover the common cases (KWOK's fake nodes, and nvidia.com/gpu),
	// and a platform team routinely uses neither: the first real GKE H100
	// cluster this console met taints its GPU pool dedicated=gpu-workload,
	// and kai-scheduler refused the gang with "2 node(s) had untolerated
	// taint(s)".
	//
	// Named taints only, never a catch-all Exists -- see workload.yaml's own
	// comment. This step's entire claim is that a GPU-aware scheduler chose
	// GPU nodes, and a pod that tolerates everything makes that claim
	// unfalsifiable.
	extraTolerations []corev1.Toleration
}

// NewClient wraps an existing clientset. Tests pass fake.NewSimpleClientset;
// production wiring passes the real one -- which can be nil outside a pod
// (rest.InClusterConfig fails on a developer laptop), so a caller must check
// Ready before issuing any other call.
func NewClient(kube kubernetes.Interface, extraTolerations ...corev1.Toleration) *Client {
	return &Client{kube: kube, extraTolerations: extraTolerations}
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

// waitAbsentTimeout bounds the wait when Apply replaces a drifted workload.
// Generous relative to a Job delete with no running pods, and short enough
// that a wedged finalizer surfaces as a failed Prove rather than a hang.
const waitAbsentTimeout = 2 * time.Minute

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

// Apply renders and creates the workload for runID.
//
// Idempotent for an unchanged spec: a matching hash annotation means the
// object in the cluster is the one this call wants, so it returns without
// writing. That matters -- a retried Apply against a gang that has already
// placed must not disturb it.
//
// A differing or absent hash means the opposite: the object was created by a
// differently-configured process, or by a binary predating SpecHashAnnotation.
// Neither is the workload this call is being asked for, so it is removed and
// recreated. Update is not an option: a Job's placement-defining fields
// (completions, parallelism, selector, and the whole pod template) are
// immutable once created.
func (c *Client) Apply(ctx context.Context, runID string) error {
	job, hash, err := c.render(runID)
	if err != nil {
		return err
	}

	existing, getErr := c.kube.BatchV1().Jobs(Namespace).Get(ctx, WorkloadName(runID), metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(getErr):
		// Nothing there. Fall through to the create below.
	case getErr != nil:
		return fmt.Errorf("prove: checking for an existing workload for run %s: %w", runID, getErr)
	case existing.Annotations[SpecHashAnnotation] == hash:
		return nil
	default:
		// EnsureAbsent rather than Delete: foreground deletion only
		// guarantees the API server has STARTED cascading, and a new gang
		// placed against pods that are still dying fails to schedule in a
		// way that reads as a placement bug rather than a teardown race.
		if err := c.EnsureAbsent(ctx, runID, waitAbsentTimeout); err != nil {
			return fmt.Errorf("prove: replacing the workload for run %s: %w", runID, err)
		}
	}

	if _, err := c.kube.BatchV1().Jobs(Namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		// A racing creator, or a Create the API server accepted while the
		// client saw a failure. Either way something with this name now
		// exists and this call did not put it there, so it is not treated as
		// success the way the old unconditional swallow did.
		return fmt.Errorf("prove: applying workload for run %s: %w", runID, err)
	}
	return nil
}

// render decodes the workload manifest, applies this Client's configuration,
// and returns the Job alongside a hash of it.
//
// The hash covers the object as THIS process would create it -- not one read
// back from the API server. Server-side defaulting fills a PodSpec with dozens
// of fields no client ever set, so hashing a retrieved Job would report drift
// on every call and turn "recreate on drift" into "always recreate".
func (c *Client) render(runID string) (*batchv1.Job, string, error) {
	out, err := Render(runID, Namespace)
	if err != nil {
		return nil, "", fmt.Errorf("prove: rendering workload for run %s: %w", runID, err)
	}
	var job batchv1.Job
	if decodeErr := yaml.Unmarshal(out, &job); decodeErr != nil {
		return nil, "", fmt.Errorf("prove: decoding rendered workload for run %s: %w", runID, decodeErr)
	}
	// Appended after decode rather than templated into workload.yaml: the
	// manifest is rendered by string replacement, and a YAML list spliced in
	// by textual substitution is a whole class of indentation bug for
	// something the typed object expresses directly.
	job.Spec.Template.Spec.Tolerations = append(
		job.Spec.Template.Spec.Tolerations, c.extraTolerations...)

	canonical, err := json.Marshal(job.Spec)
	if err != nil {
		return nil, "", fmt.Errorf("prove: hashing workload for run %s: %w", runID, err)
	}
	sum := sha256.Sum256(canonical)
	hash := hex.EncodeToString(sum[:])

	if job.Annotations == nil {
		job.Annotations = map[string]string{}
	}
	job.Annotations[SpecHashAnnotation] = hash
	return &job, hash, nil
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

// EnsureAbsent deletes runID's workload and does not return until the API
// server has confirmed it gone. It is the guarantee Stop makes, factored
// out so a second caller (engine.Reset) can require it without going
// through Stop's own state guard -- stoppable() rejects both an ordinary
// StateFailed run and a run already moved to StateResetting.
//
// Idempotent in both halves: Delete treats NotFound as success, and
// WaitAbsent returns immediately for an object that was never there. A run
// that never reached Prove therefore satisfies this trivially.
//
// Both halves are required. Delete alone only means the API server has
// STARTED cascading (see Delete and WaitAbsent), and a teardown that
// uninstalled components beneath a workload still holding GPUs is the
// failure this whole sequence exists to prevent.
func (c *Client) EnsureAbsent(ctx context.Context, runID string, timeout time.Duration) error {
	if err := c.Delete(ctx, runID); err != nil {
		return err
	}
	return c.WaitAbsent(ctx, runID, timeout)
}

// PlacedNodes returns, for runID's gang, the node each already-scheduled and
// not-failed pod has been bound to, keyed by pod name. A pod absent from the
// result has either not been placed yet or has already failed.
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
//
// The check excludes PodFailed only, which is narrower than it first was --
// and deliberately so for PodUnknown too, which the earlier phase test also
// excluded: Unknown means the node stopped reporting, not that the pod was
// never placed, so treating a brief control-plane partition as "the gang did
// not place" would fail a run over something the scheduler had already
// decided.
// Excluding Succeeded too looks equally safe and is not: KWOK marks a pod
// Succeeded in the same second it binds it -- measured on the demo cluster,
// where both gang members were bound at 10:28:39/40 and the Job reported
// completionTime 10:28:40, with no observable Running window at any poll
// interval. Since KWOK's simulated GPU nodes are a PREREQUISITE of the only
// path this console can demo on (a plain KWOK cluster cannot resolve a
// recipe at all), that exclusion made the Prove step time out at three
// minutes on every simulated run while the scheduler had in fact placed the
// gang perfectly. Failure is the thing worth excluding here; a pod the
// substrate completed instantly is not a failure, and the placement decision
// -- which is the whole claim this step makes on a simulated cluster -- had
// already been made and recorded on the object.
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
		if pod.Status.Phase == corev1.PodFailed {
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

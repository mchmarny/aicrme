// Package steps implements one engine.Step per phase of the run.
package steps

import (
	"context"
	"encoding/json"
	"time"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
	"github.com/mchmarny/aicrme/internal/gap"
	corev1 "k8s.io/api/core/v1"
)

// DiscoverConfig configures the AICR snapshot agent Job.
type DiscoverConfig struct {
	Namespace string
	Image     string
	// Kubeconfig is set explicitly rather than left to AICR's own resolution.
	// AgentConfig.Kubeconfig is documented as "the path (or empty for
	// in-cluster)", and empty was exactly right in a pod. Locally, empty means
	// AICR reads KUBECONFIG, else ~/.kube/config -- so Discover would snapshot
	// whatever the operator's ambient config points at while Apply installs
	// into the context they selected. That is a recipe generated for one
	// cluster and installed into another, with nothing in the timeline saying
	// so.
	Kubeconfig string
	// JobName and ServiceAccountName name the transient Job and the
	// ServiceAccount/Role/RoleBinding trio AICR's agent deployer creates for
	// it (see NewDiscover for why they cannot be left blank). Both are
	// deleted after each run: DiscoverConfig.Cleanup is always true below.
	JobName            string
	ServiceAccountName string
	Timeout            time.Duration
	Privileged         bool
	RequireGPU         bool

	// NodeSelector constrains where the agent Job schedules. Left nil in
	// production: aicr.Client.CollectSnapshot's own auto-targeting
	// (snapshotter.maybeInjectGPUNodeSelector) then biases placement onto a
	// real GPU node so the privileged hardware collectors have something to
	// read, which is exactly right there. It exists as a knob for simulated
	// clusters (KWOK), where every node advertising
	// nvidia.com/gpu.present=true is a fake node carrying the
	// kwok.x-k8s.io/node=fake:NoSchedule taint. The agent pod deliberately
	// carries no toleration for that taint, so left unpinned it stays
	// Pending on every auto-targeted fake node and Discover times out
	// loudly rather than completing -- Discover can never succeed on such a
	// cluster without this. (Tolerating the taint instead, so the agent
	// could land there, would be worse than a timeout: KWOK's controller
	// fakes Running/Succeeded status for anything scheduled onto such a
	// node with no real execution ever happening -- verified against the
	// kwok-controller v0.8.0 stage-fast.yaml pod-complete Stage -- so
	// Discover would falsely report success without the collector ever
	// having run. That path is deliberately not taken.) Setting
	// NodeSelector here bypasses the module's auto-targeting (it only
	// fires when the caller leaves NodeSelector unset) and pins the Job
	// onto a real node instead.
	NodeSelector map[string]string

	// DiscoverNetwork enables live l8k network-fabric discovery, populating
	// the snapshot's TypeNetworkTopology measurement — the signal the EFA
	// plugin gap needs (see internal/gap/rules.go) and that this console does
	// not yet key any rule on. Defaults off: per aicr.AgentConfig's doc, it is
	// NOT read-only — discovery writes nvidia.kubernetes-launch-kit.* node
	// labels and may patch NicClusterPolicy via server-side-apply. Reachable
	// here so a later task (or the Phase 4 EKS fixture capture) can turn it
	// on without touching this file again.
	DiscoverNetwork bool

	// Requests overrides the agent pod's container resource requests. Left
	// nil in production, where AICR's own defaults apply: the agent asks for
	// 1000m CPU (pkg/k8s/agent/job.go) because snapshotting a real cluster's
	// hardware is genuine work, and lowering that on a real install would
	// only make Discover slower.
	//
	// It exists for the same reason NodeSelector does -- a simulated cluster
	// cannot satisfy an assumption a real one can. The KWOK e2e pins the
	// agent onto the single real node (see NodeSelector above), and on a CI
	// runner that node cannot fit 1000m alongside the console and
	// kube-system: the agent pod stays Pending with "Insufficient cpu" and
	// Discover times out after ten minutes. This was invisible until the
	// e2e workflow first ran on a runner rather than a developer's laptop.
	//
	// Deliberately not exposed in values.yaml, same treatment as
	// AICRME_SNAPSHOT_NODE_SELECTOR: it is a test-path knob, and shrinking
	// the request on a real cluster is a way to make Discover worse.
	Requests corev1.ResourceList

	// ExtraTolerations are added to the agent Job alongside the built-in
	// nvidia.com/gpu one (see Run). Empty on Kind and KWOK.
	//
	// It exists because "the GPU taint" is not one taint. GKE's own docs use
	// nvidia.com/gpu=present:NoSchedule, but a cluster built by a platform
	// team routinely carries something else entirely -- the first real GKE
	// H100 cluster this console met tainted its GPU pool
	// dedicated=gpu-workload:NoSchedule, which the built-in toleration does
	// not match. The agent then cannot land on a GPU node, and the
	// accelerator is derived from an IN-POD PCI probe
	// (fingerprint's gpu.hardware.model), so the snapshot comes back with no
	// accelerator and the recipe does not resolve.
	//
	// Deliberately additive rather than a replacement, and deliberately not a
	// blanket Exists: see Run's comment for why a blanket toleration would
	// let the agent land on a KWOK fake node and report success having
	// collected nothing.
	ExtraTolerations []corev1.Toleration
}

type discover struct {
	client aicrclient.Snapshotter
	cfg    DiscoverConfig
}

// defaultAgentName is both the default Job name and the default
// ServiceAccount/Role/RoleBinding name AICR's own `aicr` CLI uses for the
// snapshot agent (pkg/cli/root.go's `name = "aicr"`, applied as the
// --job-name / --service-account-name flag defaults in pkg/cli/snapshot.go).
// aicr.Client.CollectSnapshot -- the Go entry point this console calls
// instead of the CLI -- applies no such default itself: an empty JobName or
// ServiceAccountName reaches the API server as `metadata.name: ""` and is
// rejected outright (see pkg/k8s/agent/job.go and rbac.go in the pinned
// module), failing Discover on every cluster, not just KWOK.
const defaultAgentName = "aicr"

// NewDiscover returns the Discover step. It runs automatically on first load —
// no decisions gate it.
func NewDiscover(c aicrclient.Snapshotter, cfg DiscoverConfig) engine.Step {
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Minute
	}
	if cfg.JobName == "" {
		cfg.JobName = defaultAgentName
	}
	if cfg.ServiceAccountName == "" {
		cfg.ServiceAccountName = defaultAgentName
	}
	return &discover{client: c, cfg: cfg}
}

// agentTolerations is the built-in nvidia.com/gpu toleration plus whatever the
// operator added.
//
// Never a blanket {Operator: Exists}, which is what AICR's own CLI defaults to.
// A blanket toleration also accepts kwok.x-k8s.io/node=fake:NoSchedule, and
// KWOK's controller reports Running/Succeeded for anything scheduled onto a
// fake node without executing it -- so the agent would land on a simulated GPU
// node and Discover would report success having collected nothing.
// DiscoverConfig.NodeSelector's doc records that trade deliberately; this
// keeps it. A timeout is bad; a false success is worse.
//
// Extras are appended rather than replacing the default so an operator adding
// their cluster's own GPU taint does not silently drop the common one.
func agentTolerations(extra []corev1.Toleration) []corev1.Toleration {
	out := make([]corev1.Toleration, 0, 1+len(extra))
	out = append(out, corev1.Toleration{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists})
	return append(out, extra...)
}

func (d *discover) Phase() engine.Phase { return engine.PhaseDiscover }
func (d *discover) Requires() []string  { return nil }

func (d *discover) Run(ctx context.Context, r *engine.Run, emit engine.Emit) error {
	emit(bus.Event{Kind: bus.KindLog, Message: "deploying cluster snapshot agent"})

	snap, err := d.client.CollectSnapshot(ctx, &aicr.AgentConfig{
		Kubeconfig:         d.cfg.Kubeconfig,
		Namespace:          d.cfg.Namespace,
		Image:              d.cfg.Image,
		JobName:            d.cfg.JobName,
		ServiceAccountName: d.cfg.ServiceAccountName,
		NodeSelector:       d.cfg.NodeSelector,
		// The GPU taint, and ONLY the GPU taint.
		//
		// On every managed cloud the GPUs sit on tainted nodes -- GKE taints
		// its GPU pools nvidia.com/gpu=present:NoSchedule, EKS and AKS do the
		// equivalent -- and nothing else in the chain supplies a toleration.
		// snapshotter.maybeInjectGPUNodeSelector biases the Job ONTO a GPU
		// node by injecting a nodeSelector and injects no toleration with it;
		// pkg/client/v1 defaults tolerations for validation only, never for
		// CollectSnapshot; and the agent assigns config.Tolerations straight
		// through. With none, the Job sits Pending until Discover's
		// ten-minute timeout -- as the first thing the demo does, on the one
		// cluster that matters.
		//
		// Deliberately NOT a bare {Operator: Exists}, which is what AICR's
		// own CLI defaults to and what an earlier draft of this put here. A
		// blanket toleration also accepts kwok.x-k8s.io/node=fake:NoSchedule,
		// and KWOK's controller fakes Running/Succeeded for anything
		// scheduled onto a fake node without executing it -- so the agent
		// would land on a simulated GPU node and Discover would report
		// success having collected nothing. The NodeSelector doc above
		// records that trade deliberately; this keeps it. A timeout is a bad
		// outcome, a false success is a worse one.
		//
		// Naming the key is what separates the two: simulated GPU nodes carry
		// BOTH taints, so refusing the kwok one keeps the agent off them
		// while the nvidia.com/gpu toleration lets it reach a real one.
		Tolerations:     agentTolerations(d.cfg.ExtraTolerations),
		Timeout:         d.cfg.Timeout,
		Privileged:      d.cfg.Privileged,
		RequireGPU:      d.cfg.RequireGPU,
		DiscoverNetwork: d.cfg.DiscoverNetwork,
		Requests:        d.cfg.Requests,
		Cleanup:         true,
	})
	if err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeUnavailable, "cluster snapshot failed", err)
	}

	// Persist the RAW agent bytes, not a re-serialization: a newer agent image
	// can emit fields this binary's Snapshot type does not model, and a typed
	// round trip drops them silently.
	r.Artifacts["snapshot.yaml"] = snap.Raw

	report := gap.Analyze(snap)
	encoded, err := json.Marshal(report)
	if err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "encoding capability report failed", err)
	}
	r.Artifacts["capability.json"] = encoded

	emit(bus.Event{Kind: bus.KindLog, Message: report.Headline, Data: encoded})
	for _, g := range report.Gaps {
		emit(bus.Event{Kind: bus.KindCluster, Level: bus.LevelWarn, Message: g.Title})
	}
	emit(bus.Event{Kind: bus.KindLog, Message: report.Punchline})
	return nil
}

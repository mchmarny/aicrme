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
)

// DiscoverConfig configures the AICR snapshot agent Job.
type DiscoverConfig struct {
	Namespace string
	Image     string
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
	// kwok.x-k8s.io/node=fake:NoSchedule taint -- KWOK's controller fakes
	// Running/Succeeded status for anything scheduled onto such a node with
	// no real execution ever happening (verified against the kwok-controller
	// v0.8.0 stage-fast.yaml pod-complete Stage), so tolerating that taint
	// to let the agent land there would make Discover report success
	// without ever running the real collector. Setting NodeSelector here
	// bypasses the module's auto-targeting (it only fires when the caller
	// leaves NodeSelector unset) and pins the Job onto a real node instead.
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

func (d *discover) Phase() engine.Phase { return engine.PhaseDiscover }
func (d *discover) Requires() []string  { return nil }

func (d *discover) Run(ctx context.Context, r *engine.Run, emit engine.Emit) error {
	emit(bus.Event{Kind: bus.KindLog, Message: "deploying cluster snapshot agent"})

	snap, err := d.client.CollectSnapshot(ctx, &aicr.AgentConfig{
		Namespace:          d.cfg.Namespace,
		Image:              d.cfg.Image,
		JobName:            d.cfg.JobName,
		ServiceAccountName: d.cfg.ServiceAccountName,
		NodeSelector:       d.cfg.NodeSelector,
		Timeout:            d.cfg.Timeout,
		Privileged:         d.cfg.Privileged,
		RequireGPU:         d.cfg.RequireGPU,
		DiscoverNetwork:    d.cfg.DiscoverNetwork,
		Cleanup:            true,
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

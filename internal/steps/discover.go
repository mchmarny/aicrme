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
	Namespace  string
	Image      string
	Timeout    time.Duration
	Privileged bool
	RequireGPU bool

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

// NewDiscover returns the Discover step. It runs automatically on first load —
// no decisions gate it.
func NewDiscover(c aicrclient.Snapshotter, cfg DiscoverConfig) engine.Step {
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Minute
	}
	return &discover{client: c, cfg: cfg}
}

func (d *discover) Phase() engine.Phase { return engine.PhaseDiscover }
func (d *discover) Requires() []string  { return nil }

func (d *discover) Run(ctx context.Context, r *engine.Run, emit engine.Emit) error {
	emit(bus.Event{Kind: bus.KindLog, Message: "deploying cluster snapshot agent"})

	snap, err := d.client.CollectSnapshot(ctx, &aicr.AgentConfig{
		Namespace:       d.cfg.Namespace,
		Image:           d.cfg.Image,
		Timeout:         d.cfg.Timeout,
		Privileged:      d.cfg.Privileged,
		RequireGPU:      d.cfg.RequireGPU,
		DiscoverNetwork: d.cfg.DiscoverNetwork,
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

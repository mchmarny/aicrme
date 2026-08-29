package steps

import (
	"context"
	"encoding/json"
	"time"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
)

// ValidateConfig configures the Validate step.
type ValidateConfig struct {
	// WorkDir is where the merged CTRF report is written, under
	// runs/<id>/validation.
	WorkDir string
	// Kubeconfig is the session kubeconfig the validator's Jobs run against.
	Kubeconfig string
	// Timeout bounds the whole validation. Zero uses defaultValidateTimeout.
	Timeout time.Duration
}

// defaultValidateTimeout bounds the deployment phase.
//
// Deliberately far below the SDK's 75-minute facade cap, which is sized for
// an all-phase run whose largest single check is a 65-minute inference-perf
// benchmark. This step runs only the cheap install/health phase, sits in the
// middle of a demo, and a run of it that has not finished in fifteen minutes
// is stuck rather than slow.
const defaultValidateTimeout = 15 * time.Minute

type validateStep struct {
	client aicrclient.API
	cfg    ValidateConfig
}

// NewValidate returns the Validate step.
//
// IT NEVER FAILS THE RUN. Every failure path records why and returns nil, the
// same posture snapshotOwnership takes and for the same reason: this is a
// report, and a report that could not be produced must not cost an install
// that succeeded. Prove still runs, and the success screen carries both
// claims.
func NewValidate(c aicrclient.API, cfg ValidateConfig) engine.Step {
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultValidateTimeout
	}
	return &validateStep{client: c, cfg: cfg}
}

func (v *validateStep) Phase() engine.Phase { return engine.PhaseValidate }
func (v *validateStep) Requires() []string  { return nil }

func (v *validateStep) Run(ctx context.Context, run *engine.Run, emit engine.Emit) error {
	if reason := skipReason(run); reason != "" {
		run.Validation = engine.Validation{Skipped: reason}
		emit(bus.Event{Kind: bus.KindLog, Level: bus.LevelWarn,
			Message: "validation skipped: " + reason})
		return nil
	}
	return nil
}

// skipReason reports why validation must not run, or "" when it may.
//
// totalGpus == 0 is internal/gap's own definition of a simulated cluster, not
// a heuristic invented here -- the same signal Prove uses to decide what it
// may claim.
func skipReason(run *engine.Run) string {
	raw := run.Artifacts["capability.json"]
	if len(raw) == 0 {
		return "no capability report, so this console cannot tell a real cluster from a simulated one"
	}
	var report struct {
		TotalGPUs int  `json:"totalGpus"`
		Analyzed  bool `json:"analyzed"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		return "the capability report is unreadable, so this console cannot tell a real cluster from a simulated one"
	}
	if !report.Analyzed {
		return "nothing was measured about this cluster"
	}
	if report.TotalGPUs == 0 {
		return "simulated cluster -- AICR's validator lands on KWOK's fake nodes and reports passes for checks that never ran"
	}
	return ""
}

var _ = aicr.PhaseDeployment // retained by Task 4

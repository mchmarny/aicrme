package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
)

// aicrValidationNamespace is where ValidateState creates its ServiceAccount,
// a per-run cluster-admin ClusterRoleBinding, and validator Jobs -- the
// default pkg/validator uses when this step's call (below) does not
// override it with aicr.WithValidationNamespace. RBAC and ConfigMaps are
// cleaned on a fresh context.Background() so they survive cancellation, but
// the Job itself is cleaned with the run context (so a deadline or SIGTERM
// leaves it), and nothing deletes the namespace. Unlike Apply, this step
// records no ownership for Reset to act on, so a failure here is entirely
// outside Reset's reach and absent from the residue inventory. The standing
// ruling is that the deployer owns its cleanup and orphans are printed, not
// chased -- this constant exists so the warning below can name where to
// look, not so this step can go clean it up itself.
const aicrValidationNamespace = "aicr-validation"

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
// checks run serially (pkg/client/v1/aicr.go's doc comment on the facade
// deadline), and recipes/validators/catalog.yaml's five `phase: deployment`
// entries sum to 24m serially: operator-health 2m, expected-resources 8m,
// gpu-operator-version 2m, check-nvidia-smi 10m, gke-gpu-nic-networks 2m.
// 30m leaves slack for the validator Job's own deploy and cleanup on top of
// that 24m worst case, still well below the SDK's 75-minute facade cap sized
// for an all-phase run's 65-minute inference-perf benchmark. A tighter cap
// here is not just slower demos: aicr.go's ValidateState discards partial
// results on a facade deadline (`results, err := v.ValidatePhases(...); if
// err != nil { return nil, err }`), so a deadline that fires mid-catalog
// throws away every check that had already finished.
const defaultValidateTimeout = 30 * time.Minute

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
		publish(emit, run, bus.LevelWarn, "validation skipped: "+reason)
		return nil
	}

	// Re-resolved rather than handed over, exactly as Bundle does and for the
	// same reason: ValidateState calls assertOwns and reads unexported state,
	// so it needs a RecipeResult THIS client produced. Every input is
	// persisted, and assertMatchesApproved proves the re-resolution did not
	// drift from what the operator approved.
	result, snap, reason := v.resolve(ctx, run)
	if reason != "" {
		run.Validation = engine.Validation{Skipped: reason}
		publish(emit, run, bus.LevelWarn, "validation skipped: "+reason)
		return nil
	}

	emit(bus.Event{Kind: bus.KindLog, Message: "validating the deployment"})

	results, err := v.client.ValidateState(ctx, result, snap,
		aicr.WithValidationPhases(aicr.PhaseDeployment),
		aicr.WithValidationKubeconfig(v.cfg.Kubeconfig),
		aicr.WithValidationRunID(run.ID),
		aicr.WithValidationCleanup(true),
		aicr.WithValidationTimeout(v.cfg.Timeout),
	)
	if err != nil {
		// This step created RBAC and Jobs in aicrValidationNamespace before
		// this error arrived, and this step -- unlike Apply -- has no
		// ownership record for Reset to clean up against. slog rather than
		// only the bus event: an operator watching this pod's own logs (the
		// channel that works even when the SPA never connects) gets the same
		// pointer to where to look. "may still hold", not "does hold": the
		// Job's own cleanup can have succeeded right up until the failure
		// that follows it, so claiming leftovers exist would itself be a
		// false-pass in the other direction.
		slog.Warn("validation failed; the namespace it used may still hold leftover objects",
			"run", run.ID, "namespace", aicrValidationNamespace, "error", err)
		reason := fmt.Sprintf("validation could not run: %s -- check the %s namespace for leftover objects this step could not confirm it removed",
			err.Error(), aicrValidationNamespace)
		run.Validation = engine.Validation{Skipped: reason}
		publish(emit, run, bus.LevelWarn, reason)
		return nil
	}

	run.Validation = engine.Validation{Phases: summarize(results)}
	if path, werr := v.writeReport(run.ID, results); werr == nil {
		run.Validation.ReportPath = path
	} else {
		emit(bus.Event{Kind: bus.KindLog, Level: bus.LevelWarn,
			Message: "validation ran but its report could not be written: " + werr.Error()})
	}
	// A verdict with failures is not routine narration -- it publishes at the
	// same warn level every skip already does, so "validation: 11 of 14
	// checks passed, 3 failed" does not render in the same neutral ink as
	// ordinary progress lines while the panel colors it red and the
	// timeline does not.
	level := bus.Level("")
	if anyPhaseFailed(run.Validation.Phases) {
		level = bus.LevelWarn
	}
	publish(emit, run, level, verdict(run.Validation.Phases))
	return nil
}

// anyPhaseFailed reports whether any validation phase recorded a failed
// check.
func anyPhaseFailed(phases []engine.PhaseSummary) bool {
	for _, p := range phases {
		if p.Failed > 0 {
			return true
		}
	}
	return false
}

// publish sends the outcome as a payload as well as a sentence. The console
// renders the structured verdict; the message is for the log beside it. A
// payload that will not marshal is not worth losing the message over, so a
// marshal error degrades to the plain event rather than returning.
func publish(emit engine.Emit, run *engine.Run, level bus.Level, message string) {
	ev := bus.Event{Kind: bus.KindLog, Level: level, Message: message}
	if data, err := json.Marshal(run.Validation); err == nil {
		ev.Data = data
	}
	emit(ev)
}

// resolve rebuilds the inputs ValidateState requires, or returns the reason
// it cannot.
func (v *validateStep) resolve(ctx context.Context, run *engine.Run) (*aicr.RecipeResult, *aicr.Snapshot, string) {
	approved := run.Artifacts["recipe.json"]
	if len(approved) == 0 {
		return nil, nil, "no approved recipe on this run"
	}
	snap, err := decodeSnapshot(run.Artifacts["snapshot.yaml"])
	if err != nil {
		return nil, nil, "the stored snapshot is unreadable: " + err.Error()
	}
	criteria, err := buildCriteria(v.client, snap, run.Decisions["intent"], run.Decisions["platform"])
	if err != nil {
		return nil, nil, "criteria could not be rebuilt: " + err.Error()
	}
	result, err := v.client.ResolveRecipeFromSnapshot(ctx, criteria, snap)
	if err != nil || result == nil {
		return nil, nil, "the recipe could not be re-resolved for validation"
	}
	if err := assertMatchesApproved(result, approved); err != nil {
		// Refused rather than validated. Attesting to a recipe that is not
		// the one installed is worse than not attesting at all.
		return nil, nil, "the re-resolved recipe drifted from the approved one: " + err.Error()
	}
	return result, snap, ""
}

// summarize flattens AICR's results into the record's own shape, so no AICR
// type is persisted.
func summarize(results []*aicr.PhaseResult) []engine.PhaseSummary {
	out := make([]engine.PhaseSummary, 0, len(results))
	for _, r := range results {
		if r == nil {
			continue
		}
		out = append(out, engine.PhaseSummary{
			Phase:   string(r.Phase),
			Status:  r.Status,
			Seconds: int(r.Duration.Round(time.Second).Seconds()),
			Tests:   r.Summary.Tests,
			Passed:  r.Summary.Passed,
			Failed:  r.Summary.Failed,
			Skipped: r.Summary.Skipped,
		})
	}
	return out
}

// verdict is the one-line result an operator reads in the timeline.
func verdict(phases []engine.PhaseSummary) string {
	var tests, passed, failed int
	for _, p := range phases {
		tests += p.Tests
		passed += p.Passed
		failed += p.Failed
	}
	if failed > 0 {
		return fmt.Sprintf("validation: %d of %d checks passed, %d failed", passed, tests, failed)
	}
	return fmt.Sprintf("validation: %d of %d checks passed", passed, tests)
}

// writeReport persists the merged CTRF payload beside the run's bundle.
func (v *validateStep) writeReport(runID string, results []*aicr.PhaseResult) (string, error) {
	dir := filepath.Join(v.cfg.WorkDir, "runs", runID, "validation")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// PhaseResult.RawReport is, per its own doc, "the marshaled CTRF JSON".
	// This step runs exactly one phase, so that payload IS the canonical CTRF
	// document -- writing it verbatim gives a file any CTRF tool can read,
	// with no merge step and nothing bespoke about the shape.
	//
	// When conformance and performance arrive, several phases will need
	// combining and aicr.Client.MergeReports is the tool for it (it stamps
	// the same combined document the CLI writes). Reaching for it now, with
	// one phase, would widen the client seam for a merge of one.
	var payload []byte
	for _, r := range results {
		if r != nil && len(r.RawReport) > 0 {
			payload = r.RawReport
			break
		}
	}
	if len(payload) == 0 {
		return "", errors.New("validation returned no CTRF payload")
	}
	path := filepath.Join(dir, "ctrf.json")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// skipReason reports why validation must not run, or "" when it may.
//
// Simulated is internal/gap's own definition of a simulated cluster, not a
// heuristic invented here: it is true when kwok-controller is running,
// regardless of how many GPUs the fake nodes report. Checked before
// TotalGPUs == 0, because a KWOK cluster with four fake accelerated nodes
// reports a perfectly healthy GPU count -- that combination is exactly what
// the old GPU-count-only check got wrong.
func skipReason(run *engine.Run) string {
	raw := run.Artifacts["capability.json"]
	if len(raw) == 0 {
		return "no capability report, so this console cannot tell a real cluster from a simulated one"
	}
	var report struct {
		TotalGPUs int  `json:"totalGpus"`
		Analyzed  bool `json:"analyzed"`
		Simulated bool `json:"simulated"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		return "the capability report is unreadable, so this console cannot tell a real cluster from a simulated one"
	}
	if !report.Analyzed {
		return "nothing was measured about this cluster"
	}
	if report.Simulated {
		return "simulated cluster -- kwok-controller is running its fake nodes, and AICR's validator lands on them and reports passes for checks that never ran"
	}
	if report.TotalGPUs == 0 {
		return "no GPU hardware to validate against"
	}
	return ""
}

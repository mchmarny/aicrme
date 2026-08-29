# aicrme Validate Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Run AICR's `deployment` validation phase against the cluster after Apply and before Prove, record the per-phase verdict on the run, and show it in the console — skipping honestly on a simulated cluster.

**Architecture:** A new `steps.Validate` at the already-reserved `engine.PhaseValidate`, wired between Apply and Prove. It re-resolves the recipe from the persisted snapshot using the machinery `steps.Bundle` already uses (`decodeSnapshot`, `buildCriteria`, `assertMatchesApproved`), calls `aicr.Client.ValidateState`, and records a per-phase summary on the run. It never fails the run: a validation that cannot run, drifts, errors, or reports failures is recorded and the run proceeds to Prove.

**Tech Stack:** Go 1.x, `github.com/NVIDIA/aicr v0.20.0`, React 19 + TypeScript + Tailwind v4, vitest.

**Spec:** `docs/superpowers/specs/2026-08-29-validate-and-evidence-design.md`

## Global Constraints

- **Pinned dependency:** `github.com/NVIDIA/aicr v0.20.0`. Do not bump it. `make check-aicr-pin` enforces it.
- **The step never fails the run.** Every failure path records and returns `nil`. This is spec'd behaviour, not defensive coding.
- **Default phase is `aicr.PhaseDeployment` only.** `conformance` and `performance` are out of scope for this plan — `performance` saturates every GPU and would starve Prove's gang.
- **Validation timeout is 15 minutes** (`aicr.WithValidationTimeout(15 * time.Minute)`), not the SDK's 75-minute default.
- **A simulated cluster is skipped, never passed.** Detection is `gap.Report.TotalGPUs == 0`, read from the `capability.json` artifact — `internal/gap`'s own definition, the same one Prove trusts.
- **Colours use the semantic tokens** in `web/src/index.css`: `text-pass`, `text-fail`, `text-warn`, `text-ink-faint`. Never Tailwind scale names.
- **Every commit must pass `make qualify`.**

## File Structure

| File | Responsibility |
|---|---|
| `internal/aicrclient/client.go` | Add the `Validator` role interface; add it to the `API` aggregate |
| `internal/aicrclient/fake.go` | Fake implements `ValidateState`, records calls and options |
| `internal/engine/run.go` | `Validation` / `PhaseSummary` types; `Run.Validation` field |
| `internal/steps/validate.go` | **New.** The step: skip check, re-resolve, validate, record |
| `internal/steps/validate_test.go` | **New.** Step behaviour |
| `internal/console/console.go` | Wire the step between Apply and Prove |
| `web/src/api.ts` | `Validation` type mirroring the Go shape |
| `web/src/components/Wizard.tsx` | Carry `validation` on `RunState` |
| `web/src/components/Prove.tsx` | Render the validation panel |

---

### Task 1: The `Validator` seam

**Files:**
- Modify: `internal/aicrclient/client.go` (interfaces block, ~line 47)
- Modify: `internal/aicrclient/fake.go`
- Test: `internal/aicrclient/client_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `aicrclient.Validator` with method `ValidateState(ctx context.Context, r *aicr.RecipeResult, s *aicr.Snapshot, opts ...aicr.ValidateOption) ([]*aicr.PhaseResult, error)`, folded into `aicrclient.API`. `aicrclient.Fake` gains fields `PhaseResults []*aicr.PhaseResult`, `ValidateErr error`, `ValidateCalls int`, `LastValidateOpts []aicr.ValidateOption`.

- [ ] **Step 1: Write the failing test**

In `internal/aicrclient/client_test.go`:

```go
// The Fake must satisfy the same aggregate the real client does, or a step
// written against API cannot be unit tested at all.
func TestFakeRecordsValidateCalls(t *testing.T) {
	want := []*aicr.PhaseResult{{Phase: aicr.PhaseDeployment, Status: "passed"}}
	f := &aicrclient.Fake{PhaseResults: want}

	got, err := f.ValidateState(context.Background(), nil, nil,
		aicr.WithValidationPhases(aicr.PhaseDeployment))
	if err != nil {
		t.Fatalf("ValidateState() error = %v", err)
	}
	if len(got) != 1 || got[0].Status != "passed" {
		t.Errorf("ValidateState() = %+v, want the configured results", got)
	}
	if f.ValidateCalls != 1 {
		t.Errorf("ValidateCalls = %d, want 1", f.ValidateCalls)
	}
	if len(f.LastValidateOpts) != 1 {
		t.Errorf("LastValidateOpts = %d, want the options recorded for assertion", len(f.LastValidateOpts))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/aicrclient/ -run TestFakeRecordsValidateCalls`
Expected: FAIL — `f.ValidateState undefined`.

- [ ] **Step 3: Add the interface and the fake**

In `internal/aicrclient/client.go`, after the `Bundler` interface:

```go
// Validator runs the recipe's validation phases against the live cluster.
//
// A role interface of its own, like every other seam here, rather than a
// method bolted onto an existing one: Validate is the only caller, and a
// step that needs to validate should not have to accept a bundler.
type Validator interface {
	ValidateState(ctx context.Context, r *aicr.RecipeResult, s *aicr.Snapshot,
		opts ...aicr.ValidateOption) ([]*aicr.PhaseResult, error)
}
```

Add `Validator` to the `API` interface's embedded list.

In `internal/aicrclient/fake.go`, add to the `Fake` struct:

```go
	PhaseResults     []*aicr.PhaseResult
	ValidateErr      error
	ValidateCalls    int
	LastValidateOpts []aicr.ValidateOption
```

and the method:

```go
func (f *Fake) ValidateState(_ context.Context, _ *aicr.RecipeResult, _ *aicr.Snapshot,
	opts ...aicr.ValidateOption) ([]*aicr.PhaseResult, error) {
	f.ValidateCalls++
	f.LastValidateOpts = opts
	if f.ValidateErr != nil {
		return nil, f.ValidateErr
	}
	return f.PhaseResults, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/aicrclient/`
Expected: PASS. The existing `var _ API = (*aicr.Client)(nil)` assertion proves the real client already satisfies the widened aggregate — if it does not compile, the signature above is wrong, not the client.

- [ ] **Step 5: Commit**

```bash
git add internal/aicrclient/
git commit -S -m "feat(aicrclient): a Validator seam for ValidateState"
```

---

### Task 2: The validation record

**Files:**
- Modify: `internal/engine/run.go` (near `Residue`, ~line 231)
- Test: `internal/engine/filestore_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `engine.Validation{Skipped string, ReportPath string, Phases []engine.PhaseSummary}` and `engine.PhaseSummary{Phase, Status string, Seconds int, Tests, Passed, Failed, Skipped int}`; field `Run.Validation Validation` with tag `json:"validation,omitzero"`.

- [ ] **Step 1: Write the failing test**

In `internal/engine/filestore_test.go`:

```go
// The verdict has to survive a restart. A recovered run that shows no
// validation would read as "never validated", which is a different claim
// from the one the record actually holds.
func TestFileStorePreservesValidation(t *testing.T) {
	store, err := engine.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore() error = %v", err)
	}
	run := &engine.Run{
		ID:    "run-with-validation",
		State: engine.StateDone,
		Validation: engine.Validation{
			ReportPath: "/tmp/runs/x/validation/deployment.json",
			Phases: []engine.PhaseSummary{{
				Phase: "deployment", Status: "passed",
				Seconds: 92, Tests: 14, Passed: 14,
			}},
		},
	}
	if err := store.Save(context.Background(), run); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := store.Load(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(got.Validation.Phases) != 1 || got.Validation.Phases[0].Passed != 14 {
		t.Errorf("Validation = %+v, want the saved summary", got.Validation)
	}
	if got.Validation.ReportPath == "" {
		t.Error("ReportPath is empty -- the CTRF file is unreachable after a restart")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/engine/ -run TestFileStorePreservesValidation`
Expected: FAIL — `unknown field Validation`.

- [ ] **Step 3: Add the types and the field**

In `internal/engine/run.go`, beside `Residue`:

```go
// Validation is what the Validate step found, or why it did not run.
//
// A SUMMARY, deliberately. aicr.PhaseResult carries RawReport and a full
// CTRF Report; this record is gzipped and about 30KB, and CTRF payloads
// would dominate it. The raw report is written to disk and named by
// ReportPath instead.
//
// ReportPath lives here rather than in ephemeralArtifacts. That field is
// dropped on encode, which is exactly why GET /api/runs/{id}/bundle answers
// 404 for a recovered run today -- a mistake worth not making twice.
type Validation struct {
	// Skipped is why validation did not run, empty when it did. A simulated
	// cluster and a drifted recipe both land here: neither is a pass, and
	// recording either as one would be the false-pass this step exists to
	// avoid.
	Skipped string `json:"skipped,omitempty"`
	// ReportPath is the merged CTRF report on disk.
	ReportPath string `json:"reportPath,omitempty"`
	// Phases is one entry per validation phase that ran, in the order AICR
	// ran them.
	Phases []PhaseSummary `json:"phases,omitempty"`
}

// PhaseSummary is one validation phase's outcome, flattened from
// aicr.PhaseResult so the record carries no AICR types.
type PhaseSummary struct {
	Phase   string `json:"phase"`
	Status  string `json:"status"`
	Seconds int    `json:"seconds"`
	Tests   int    `json:"tests"`
	Passed  int    `json:"passed"`
	Failed  int    `json:"failed"`
	Skipped int    `json:"skipped"`
}
```

And on `Run`, beside `Residue`:

```go
	// Validation is the Validate step's verdict. Zero when the step has not
	// run yet.
	Validation Validation `json:"validation,omitzero"`
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/engine/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/run.go internal/engine/filestore_test.go
git commit -S -m "feat(engine): record the validation verdict on the run"
```

---

### Task 3: The step skips a simulated cluster

**Files:**
- Create: `internal/steps/validate.go`
- Create: `internal/steps/validate_test.go`

**Interfaces:**
- Consumes: `aicrclient.API` (Task 1), `engine.Validation` (Task 2).
- Produces: `steps.NewValidate(c aicrclient.API, cfg steps.ValidateConfig) engine.Step`, with `ValidateConfig{WorkDir string, Kubeconfig string, Timeout time.Duration}`.

- [ ] **Step 1: Write the failing test**

In `internal/steps/validate_test.go`:

```go
// KWOK's fake nodes defeat the validator: it schedules with a blanket
// toleration, lands on a fake node, and KWOK fakes exit 0 without starting
// the container -- so every check reports "passed" having run nothing.
// Measured 2026-08-18: 14/14 false passes with nothing installed.
//
// The step therefore must not call ValidateState at all on a simulated
// cluster, and must record a skip rather than a pass.
func TestValidateSkipsASimulatedCluster(t *testing.T) {
	fake := &aicrclient.Fake{}
	step := steps.NewValidate(fake, steps.ValidateConfig{WorkDir: t.TempDir()})

	run := newRun()
	run.Artifacts["capability.json"] = []byte(`{"totalGpus":0,"usableGpus":0,"analyzed":true}`)

	if err := step.Run(context.Background(), run, func(bus.Event) {}); err != nil {
		t.Fatalf("Run() error = %v, want nil -- Validate never fails the run", err)
	}
	if fake.ValidateCalls != 0 {
		t.Errorf("ValidateCalls = %d, want 0 -- validating a simulated cluster produces a false pass", fake.ValidateCalls)
	}
	if run.Validation.Skipped == "" {
		t.Error("Skipped is empty -- a skipped validation must be recorded as skipped, never as a pass")
	}
	if len(run.Validation.Phases) != 0 {
		t.Errorf("Phases = %+v, want none recorded for a skipped run", run.Validation.Phases)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/steps/ -run TestValidateSkipsASimulatedCluster`
Expected: FAIL — `undefined: steps.NewValidate`.

- [ ] **Step 3: Write the step's skeleton and skip path**

Create `internal/steps/validate.go`:

```go
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/steps/ -run TestValidateSkipsASimulatedCluster`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/steps/validate.go internal/steps/validate_test.go
git commit -S -m "feat(steps): Validate skips a simulated cluster rather than false-passing"
```

---

### Task 4: The step validates a real cluster

**Files:**
- Modify: `internal/steps/validate.go`
- Modify: `internal/steps/validate_test.go`

**Interfaces:**
- Consumes: `steps.decodeSnapshot`, `steps.buildCriteria`, `steps.assertMatchesApproved` (all existing, package-private, used by `bundle.go`).
- Produces: nothing new.

- [ ] **Step 1: Write the failing test**

Append to `internal/steps/validate_test.go`:

```go
// The happy path, and the assertions that matter are the OPTIONS: the wrong
// phase set here is how performance validation ends up saturating the GPUs
// Prove needs.
func TestValidateRunsTheDeploymentPhaseAndRecordsIt(t *testing.T) {
	dir := t.TempDir()
	recipe := recipeFixture()
	fake := &aicrclient.Fake{
		Recipe: recipe,
		PhaseResults: []*aicr.PhaseResult{{
			Phase:    aicr.PhaseDeployment,
			Status:   "passed",
			Duration: 92 * time.Second,
			Summary:  aicr.ReportSummary{Tests: 14, Passed: 14},
		}},
	}
	step := steps.NewValidate(fake, steps.ValidateConfig{WorkDir: dir, Kubeconfig: "/tmp/kubeconfig"})

	run := newRunWithRealCluster(t, recipe)

	if err := step.Run(context.Background(), run, func(bus.Event) {}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if fake.ValidateCalls != 1 {
		t.Fatalf("ValidateCalls = %d, want 1", fake.ValidateCalls)
	}
	if run.Validation.Skipped != "" {
		t.Errorf("Skipped = %q, want empty on a real cluster", run.Validation.Skipped)
	}
	if len(run.Validation.Phases) != 1 {
		t.Fatalf("Phases = %+v, want one", run.Validation.Phases)
	}
	got := run.Validation.Phases[0]
	if got.Phase != "deployment" || got.Status != "passed" || got.Passed != 14 || got.Seconds != 92 {
		t.Errorf("PhaseSummary = %+v, want the flattened AICR result", got)
	}
	if run.Validation.ReportPath == "" {
		t.Error("ReportPath is empty -- the CTRF report was not written")
	}
	if _, err := os.Stat(run.Validation.ReportPath); err != nil {
		t.Errorf("report file missing: %v", err)
	}
}
```

Add this helper to the same file. It reuses the fixtures the Bundle tests
already use — `newRun()` (`discover_test.go`), `minimalSnapshot`
(`recommend_test.go`), `recipeFixture()` and `approvedFrom()`
(`bundle_test.go`) — all in this same external test package, so nothing needs
moving:

```go
// newRunWithRealCluster is a run whose artifacts describe a cluster WITH
// GPUs, so skipReason lets validation proceed, and whose approved recipe
// matches what the fake will re-resolve, so assertMatchesApproved passes.
func newRunWithRealCluster(t *testing.T, recipe *aicr.RecipeResult) *engine.Run {
	t.Helper()
	run := newRun()
	run.Decisions["intent"] = "training"
	run.Decisions["platform"] = "kubeflow"
	run.Artifacts["capability.json"] = []byte(`{"totalGpus":16,"usableGpus":16,"analyzed":true}`)
	run.Artifacts["snapshot.yaml"] = []byte(minimalSnapshot)
	run.Artifacts["recipe.json"] = approvedFrom(t, recipe)
	return run
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/steps/ -run TestValidateRunsTheDeploymentPhase`
Expected: FAIL — `ValidateCalls = 0`.

- [ ] **Step 3: Implement the validation path**

Replace the body of `Run` in `internal/steps/validate.go`:

```go
func (v *validateStep) Run(ctx context.Context, run *engine.Run, emit engine.Emit) error {
	if reason := skipReason(run); reason != "" {
		run.Validation = engine.Validation{Skipped: reason}
		emit(bus.Event{Kind: bus.KindLog, Level: bus.LevelWarn,
			Message: "validation skipped: " + reason})
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
		emit(bus.Event{Kind: bus.KindLog, Level: bus.LevelWarn,
			Message: "validation skipped: " + reason})
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
		run.Validation = engine.Validation{Skipped: "validation could not run: " + err.Error()}
		emit(bus.Event{Kind: bus.KindLog, Level: bus.LevelWarn,
			Message: "validation could not run: " + err.Error()})
		return nil
	}

	run.Validation = engine.Validation{Phases: summarize(results)}
	if path, werr := v.writeReport(run.ID, results); werr == nil {
		run.Validation.ReportPath = path
	} else {
		emit(bus.Event{Kind: bus.KindLog, Level: bus.LevelWarn,
			Message: "validation ran but its report could not be written: " + werr.Error()})
	}
	emit(bus.Event{Kind: bus.KindLog, Message: verdict(run.Validation.Phases)})
	return nil
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
```

Add `"errors"`, `"fmt"`, `"os"` and `"path/filepath"` to the imports and drop
the `var _ = aicr.PhaseDeployment` placeholder line from Task 3.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/steps/ -run TestValidate`
Expected: PASS, both tests.

- [ ] **Step 5: Commit**

```bash
git add internal/steps/validate.go internal/steps/validate_test.go
git commit -S -m "feat(steps): run the deployment validation phase and record its verdict"
```

---

### Task 5: Failures never fail the run

**Files:**
- Modify: `internal/steps/validate_test.go`

**Interfaces:**
- Consumes: Task 4's step. Produces: nothing.

- [ ] **Step 1: Write the failing tests**

```go
// A validation that errors, and a validation that reports failures, are two
// different things and NEITHER may fail the run. The install succeeded; the
// report is a report. Prove still has to run, because placement is the claim
// the demo is built on.
func TestValidateNeverFailsTheRun(t *testing.T) {
	t.Run("the call errors", func(t *testing.T) {
		recipe := recipeFixture()
		fake := &aicrclient.Fake{Recipe: recipe, ValidateErr: errors.New("apiserver said no")}
		step := steps.NewValidate(fake, steps.ValidateConfig{WorkDir: t.TempDir()})
		run := newRunWithRealCluster(t, recipe)

		if err := step.Run(context.Background(), run, func(bus.Event) {}); err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
		if run.Validation.Skipped == "" {
			t.Error("an errored validation must record why, or the screen shows nothing at all")
		}
	})

	t.Run("checks fail", func(t *testing.T) {
		recipe := recipeFixture()
		fake := &aicrclient.Fake{
			Recipe: recipe,
			PhaseResults: []*aicr.PhaseResult{{
				Phase: aicr.PhaseDeployment, Status: "failed",
				Summary: aicr.ReportSummary{Tests: 14, Passed: 11, Failed: 3},
			}},
		}
		step := steps.NewValidate(fake, steps.ValidateConfig{WorkDir: t.TempDir()})
		run := newRunWithRealCluster(t, recipe)

		if err := step.Run(context.Background(), run, func(bus.Event) {}); err != nil {
			t.Fatalf("Run() error = %v, want nil -- a failing check is a finding, not a broken run", err)
		}
		if run.Validation.Phases[0].Failed != 3 {
			t.Errorf("Failed = %d, want 3 recorded", run.Validation.Phases[0].Failed)
		}
		if run.Validation.Skipped != "" {
			t.Errorf("Skipped = %q, want empty -- validation ran, it just found problems", run.Validation.Skipped)
		}
	})
}

// Drift means this console cannot prove the recipe it would validate is the
// one that was installed. Refusing is the honest outcome; validating anyway
// would attest to the wrong thing.
func TestValidateRefusesADriftedRecipe(t *testing.T) {
	// The fake re-resolves recipeFixture(), but the run's approved recipe.json
	// describes a DIFFERENT component version -- the shape of an operator who
	// upgraded the binary between install and validate.
	recipe := recipeFixture()
	drifted := recipeFixture()
	drifted.Components[0].Version = "v9.9.9-not-what-was-installed"

	fake := &aicrclient.Fake{Recipe: recipe}
	step := steps.NewValidate(fake, steps.ValidateConfig{WorkDir: t.TempDir()})
	run := newRunWithRealCluster(t, drifted)

	if err := step.Run(context.Background(), run, func(bus.Event) {}); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if fake.ValidateCalls != 0 {
		t.Errorf("ValidateCalls = %d, want 0 -- a drifted recipe must not be validated", fake.ValidateCalls)
	}
	if !strings.Contains(run.Validation.Skipped, "drifted") {
		t.Errorf("Skipped = %q, want it to name the drift", run.Validation.Skipped)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/steps/ -run "TestValidateNeverFails|TestValidateRefuses"`
Expected: FAIL if any path returns an error or validates a drifted recipe.

- [ ] **Step 3: Fix whatever the tests catch**

The implementation from Task 4 should already satisfy these. If a test fails, the bug is real — fix `validate.go`, not the test.

- [ ] **Step 4: Run the package**

Run: `go test ./internal/steps/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/steps/validate_test.go internal/steps/validate.go
git commit -S -m "test(steps): Validate reports failures without failing the run"
```

---

### Task 6: Wire the step between Apply and Prove

**Files:**
- Modify: `internal/console/console.go` (`clusterWiring.steps`, ~line 635)
- Modify: `internal/console/connect_test.go` (`TestConnectBuildsEveryStepInOrder`)

**Interfaces:**
- Consumes: `steps.NewValidate` (Task 3/4). Produces: nothing.

- [ ] **Step 1: Update the ordering test**

In `internal/console/connect_test.go`, add `engine.PhaseValidate` between Apply and Prove in the `want` slice:

```go
	want := []engine.Phase{
		engine.PhaseDiscover, engine.PhaseRecommend, engine.PhaseBundle,
		engine.PhaseApply, engine.PhaseValidate, engine.PhaseProve,
	}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/console/ -run TestConnectBuildsEveryStepInOrder`
Expected: FAIL — 5 steps, want 6.

- [ ] **Step 3: Wire the step**

In `clusterWiring.steps`, between the `steps.NewApply(...)` and `steps.NewProve(...)` entries:

```go
		steps.NewValidate(w.aicr, steps.ValidateConfig{
			WorkDir: w.workDir,
			// The session kubeconfig, not the operator's: the validator
			// schedules Jobs, and it must run against the cluster this run
			// is pinned to rather than whatever the ambient context is.
			Kubeconfig: sessionKubeconfig,
		}),
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/console/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/console/
git commit -S -m "feat(console): run Validate between Apply and Prove"
```

---

### Task 7: The console shows the verdict

**Files:**
- Modify: `web/src/api.ts`
- Modify: `web/src/components/Wizard.tsx`
- Modify: `web/src/components/Prove.tsx`
- Test: `web/src/components/Prove.test.tsx`

**Interfaces:**
- Consumes: the JSON shape from Task 2.
- Produces: `Validation` and `PhaseSummary` TS types in `api.ts`; `RunState.validation?: Validation`.

- [ ] **Step 1: Write the failing tests**

In `web/src/components/Prove.test.tsx`:

```tsx
  // The verdict has to be on the screen the operator ends on, beside the
  // placement claim rather than instead of it.
  it('shows what validation found', () => {
    const run = runState({
      validation: {
        phases: [{ phase: 'deployment', status: 'passed', seconds: 92, tests: 14, passed: 14, failed: 0, skipped: 0 }],
      },
    })
    render(<Prove events={placementEvents()} run={run} busy={false} onStop={vi.fn()} />)

    const panel = screen.getByTestId('prove-validation')
    expect(panel.textContent).toMatch(/deployment/)
    expect(panel.textContent).toMatch(/14/)
  })

  // A skip is not a pass, and the screen must not let it read as one.
  it('says why validation was skipped rather than showing a verdict', () => {
    const run = runState({ validation: { skipped: 'simulated cluster -- AICR’s validator lands on fake nodes' } })
    render(<Prove events={placementEvents()} run={run} busy={false} onStop={vi.fn()} />)

    const panel = screen.getByTestId('prove-validation')
    expect(panel.textContent).toMatch(/skipped/i)
    expect(panel.textContent).toMatch(/simulated/i)
    expect(panel.textContent).not.toMatch(/passed/i)
  })

  // No validation at all is a third state, distinct from both.
  it('shows no validation panel when the step has not run', () => {
    render(<Prove events={placementEvents()} run={runState()} busy={false} onStop={vi.fn()} />)

    expect(screen.queryByTestId('prove-validation')).toBeNull()
  })
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd web && npx vitest run src/components/Prove.test.tsx`
Expected: FAIL — `prove-validation` not found.

- [ ] **Step 3: Add the types and the panel**

In `web/src/api.ts`:

```ts
/** PhaseSummary mirrors Go's engine.PhaseSummary. */
export interface PhaseSummary {
  phase: string
  status: string
  seconds: number
  tests: number
  passed: number
  failed: number
  skipped: number
}

/**
 * Validation mirrors Go's engine.Validation. `skipped` and `phases` are
 * mutually exclusive: a skip is never a pass, and the UI must not render one
 * as the other.
 */
export interface Validation {
  skipped?: string
  reportPath?: string
  phases?: PhaseSummary[]
}
```

In `web/src/components/Wizard.tsx`, add to `RunState`:

```ts
  /** validation is the Validate step's verdict, absent until it runs. */
  validation?: Validation
```

and populate it wherever `RunState` is built from a run record, beside `residue`.

In `web/src/components/Prove.tsx`, add the component and render it after `Summary`:

```tsx
/**
 * ValidationPanel reports what AICR's validator found, or why it did not run.
 *
 * A skip renders as a skip. On a simulated cluster the validator lands on
 * KWOK's fake nodes and reports passes for checks that never executed, so
 * "skipped" is the only honest thing this screen can say there -- the same
 * reason Prove labels a simulated placement rather than claiming throughput.
 */
function ValidationPanel({ validation }: { validation?: Validation }) {
  if (!validation || (!validation.skipped && !validation.phases?.length)) return null

  if (validation.skipped) {
    return (
      <div data-testid="prove-validation" className="text-xs text-ink-faint">
        <span className="text-warn">Validation skipped.</span> {validation.skipped}
      </div>
    )
  }

  return (
    <ul data-testid="prove-validation" className="space-y-1 font-mono text-xs">
      {validation.phases?.map(p => (
        <li key={p.phase} className="flex items-baseline gap-2">
          <span className={p.failed > 0 ? 'text-fail' : 'text-pass'}>
            {p.failed > 0 ? '✗' : '✓'}
          </span>
          <span className="text-ink">{p.phase}</span>
          <span className="text-ink-faint">
            {p.passed} of {p.tests} checks passed
            {p.failed > 0 ? `, ${p.failed} failed` : ''}
          </span>
        </li>
      ))}
    </ul>
  )
}
```

Render it inside the heading block, after the `Summary` line:

```tsx
        {active && <ValidationPanel validation={run.validation} />}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd web && npx vitest run src/components/Prove.test.tsx`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src
git commit -S -m "feat(console): show the validation verdict beside the placement claim"
```

---

### Task 8: Full gate and documentation

**Files:**
- Modify: `docs/STATE.md`

- [ ] **Step 1: Run the full gate**

Run: `make qualify`
Expected: `0 issues`, all Go and web tests pass, coverage above threshold.

- [ ] **Step 2: Update STATE.md**

In the "What works" table, add a row:

| Validation | real GKE H100s | `deployment` phase runs between Apply and Prove; simulated clusters skip with a stated reason |

And in open work item 3, replace the design-pending text with what shipped and what remains: `conformance` / `performance` as post-Stop actions, and evidence (increment 2).

- [ ] **Step 3: Commit**

```bash
git add docs/STATE.md
git commit -S -m "docs: validation runs between Apply and Prove"
```

- [ ] **Step 4: Do NOT push**

This work is on the `validate-step` branch and stays local. `ci` and `e2e`
trigger on pushes to `main` and on pull requests, and the GitHub Actions
budget is constrained — a full `e2e` run is six jobs. Verification here is
`make qualify` locally; CI runs when the branch is merged, which is the
operator's call, not this plan's.

- [ ] **Step 5: Assert the skip in the KWOK e2e**

The spec requires CI to assert the SKIP rather than a validation, because a
pass on KWOK is the false pass this whole step exists to avoid.

Add it to `test/e2e/prove.sh`, NOT apply-real.sh: prove.sh defines the
`run_json` and `fail` helpers this assertion uses (`prove.sh:55,79`) and
apply-real.sh defines neither. Place it after the first run reaches
`active` — after assertion 2 passes:

```bash
echo "--- assert: validation was SKIPPED on the simulated cluster, not passed"
VALIDATION="$(run_json "${RUN_ID}" | jq -c '.validation // {}')"
echo "validation record: ${VALIDATION}"
SKIPPED="$(echo "${VALIDATION}" | jq -r '.skipped // empty')"
[[ -n "${SKIPPED}" ]] \
  || fail "validation did not record a skip on a KWOK cluster: ${VALIDATION} -- a pass here is the documented false pass (validator lands on fake nodes, KWOK fakes exit 0)"
[[ "$(echo "${VALIDATION}" | jq '.phases // [] | length')" == "0" ]] \
  || fail "a skipped validation recorded phase results: ${VALIDATION}"
echo "validation skipped as designed: ${SKIPPED}"
```

Run `shellcheck -x -P test/e2e test/e2e/prove.sh` before committing.

```bash
git add test/e2e/prove.sh
git commit -S -m "test(e2e): assert validation skips a simulated cluster"
```

- [ ] **Step 6: Verify on real hardware**

Validation cannot be proven by the KWOK suite — that is the whole premise. On a real GPU cluster:

```bash
make build
HELM_REGISTRY_CONFIG=~/.config/containers/auth.json ./bin/aicrme
```

Drive a full run and confirm: the timeline shows `validating the deployment`, the Prove screen shows a per-phase verdict rather than a skip, and `<work-dir>/runs/<id>/validation/ctrf.json` exists and parses.

---

## Out of scope for this plan

- **`conformance` and `performance` as post-Stop actions.** They need an out-of-band engine operation modelled on `engine.Reset` (guards, backgrounding, state), plus an API route and a form. That is a separable subsystem and should be its own plan.
- **Evidence.** Increment 2 in the spec; depends on this plan's `[]*PhaseResult`.
- **Per-component attribution.** `ctrf.Builder` hardcodes `Suite` to the phase name; the spec records this as a known limit.

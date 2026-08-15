# aicrme Phase 2a Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extend the console's demo arc by one phase — from a resolved recipe to a real, streamed `helm` install driven by the bundle's own `deploy.sh`.

**Architecture:** A `Bundle` step re-resolves the approved recipe and writes an AICR bundle to an `emptyDir`. The run then parks on a one-key confirm decision. An `Apply` step hands the bundle directory to a new `internal/applier` package, which execs `bash deploy.sh` and converts its stable stdout markers into typed `bus.Event`s that a new cockpit renders as a component pipeline. Failure is fail-fast with a whole-script Retry, which requires a step cursor and an epoch guard in `internal/engine`.

**Tech Stack:** Go 1.x (see `.go-version`), `github.com/NVIDIA/aicr v0.19.0` (pinned), React + TypeScript + Vite + Tailwind, Helm, Kind + KWOK for e2e.

**Spec:** `docs/superpowers/specs/2026-08-15-aicrme-phase-2a-design.md`

## Global Constraints

- **AICR is pinned to `v0.19.0`.** Never bump it in this plan. `make check-aicr-pin` enforces that `go.mod`, `.settings.yaml` `dependencies.aicr`, and `cmd/aicrme/main.go`'s `defaultSnapshotAgentImage` tag all agree.
- **Coverage floor is 80%** (`.settings.yaml` `quality.coverage_threshold`). `make test-coverage` enforces it.
- **All tests run under `-race`** (`make test`).
- **`make qualify` must pass before every commit.** It is exactly what CI runs: `web lint lint-shell test-chart test-web test-coverage check-aicr-pin`.
- **Commits are signed (`git commit -S`), on `main`, with no `Co-Authored-By` and no sign-off.**
- **Errors use `github.com/NVIDIA/aicr/pkg/errors`** — `aicrerrors.New(code, msg)`, `aicrerrors.Wrap(code, msg, err)`, `aicrerrors.PropagateOrWrap(...)`. `internal/api`'s `writeErr` maps those codes to HTTP status.
- **Never publish unrecognized `deploy.sh` output to the bus.** It floods the 20 000-event replay ring and the bus drops live events for any subscriber more than 256 behind.
- **Prefer self-documenting code over comments**, but keep the codebase's existing habit of a comment wherever a non-obvious decision was made — explain *why*, never *what*.
- **Do not touch** the observer, the ConfigMap store, `StateActive`, Reset, Validate, or Prove. They are Phases 2b/3.

---

## File Structure

**New Go files**

| File | Responsibility |
|---|---|
| `internal/applier/applier.go` | Orchestrates one `deploy.sh` run: builds the `Spec`, wires the line sink, emits the failure event |
| `internal/applier/parse.go` | Pure `parseLine` marker grammar + the `ComponentData` / `FailureData` wire types |
| `internal/applier/exec.go` | `Exec` interface, the real `BashExec`, and the concurrency-safe `lineWriter` and `ring` |
| `internal/steps/criteria.go` | `buildCriteria` / `specificity` / `decodeSnapshot`, moved out of `recommend.go` so `Bundle` shares them |
| `internal/steps/bundle.go` | The `Bundle` step: re-resolve, assert-matches-approved, `MakeBundle` |
| `internal/steps/apply.go` | The `Apply` step: the `apply` confirm gate, delegating to `applier` |

**New web files**

| File | Responsibility |
|---|---|
| `web/src/pipeline.ts` | `deriveComponents` / `deriveFailure` over the event stream |
| `web/src/slowSteps.ts` | Static component → slow-step-explanation map |
| `web/src/components/Cockpit.tsx` | Gate / running / failed / done rendering |

**Modified**

`internal/aicrclient/client.go` (+`Bundler`), `internal/aicrclient/fake.go`, `internal/engine/run.go` (+`StepIndex`), `internal/engine/engine.go` (epoch + `Retry`), `internal/api/server.go`, `internal/api/runs.go`, `internal/api/auth.go`, `internal/gap/gap.go` (+`Analyzed`), `internal/steps/recommend.go`, `cmd/aicrme/main.go`, `charts/aicrme/values.yaml`, `charts/aicrme/templates/deployment.yaml`, `test/chart/contract.sh`, `test/e2e/lib.sh`, `.github/workflows/e2e.yaml`, `Makefile`, `web/src/api.ts`, `web/src/useEvents.ts`, `web/src/App.tsx`, `web/src/components/Wizard.tsx`, `web/src/components/Discover.tsx`.

---

## Task 1: Prove the dry-run e2e is viable, and capture the golden transcript

**Why first.** The spec names this the largest unproven risk in 2a. Every later task assumes `deploy.sh --dry-run` survives on Kind+KWOK and that the marker grammar looks exactly as read from the template. If it does not, that is a scope change to raise with the user, not to absorb silently. This task also answers spec Open Question 2 (bundle size on the emptyDir).

**Files:**
- Create (throwaway, **deleted in Step 8**): `cmd/probe/main.go`
- Create: `internal/applier/testdata/deploy-transcript-kwok.txt`
- Create: `docs/phase-2a-task-1-findings.md`

**Interfaces:**
- Consumes: nothing.
- Produces: `internal/applier/testdata/deploy-transcript-kwok.txt` — the real captured `deploy.sh` stdout that Task 4's golden test parses.

- [ ] **Step 1: Write the throwaway probe**

Create `cmd/probe/main.go`:

```go
// Command probe is a THROWAWAY Phase 2a Task 1 probe. It is deleted in this
// task's final commit; the transcript and findings it produces are the
// durable output. It answers what the plan cannot answer from source alone:
// whether MakeBundle produces a usable bundle from the committed
// simulated-H100 fixture, and how big that bundle is on disk.
package main

import (
	"context"
	"fmt"
	"os"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/fingerprint"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"gopkg.in/yaml.v3"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: probe <output-dir>")
		os.Exit(2)
	}
	outputDir := os.Args[1]

	raw, err := os.ReadFile("internal/steps/testdata/snapshot-kwok-h100.yaml")
	check(err)

	var inner snapshotter.Snapshot
	check(yaml.Unmarshal(raw, &inner))
	snap := aicr.WrapSnapshot(&inner)
	snap.Raw = raw

	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	check(err)
	defer func() { _ = client.Close() }()

	reg := client.CriteriaRegistry()
	criteria := aicr.WrapCriteria(fingerprint.FromMeasurements(snap.Unwrap().Measurements).ToCriteria(reg))
	intent, err := reg.ParseIntent("training")
	check(err)
	platform, err := reg.ParsePlatform("kubeflow")
	check(err)
	criteria.Intent = string(intent)
	criteria.Platform = string(platform)

	ctx := context.Background()
	result, err := client.ResolveRecipeFromSnapshot(ctx, criteria, snap)
	check(err)

	art, err := client.MakeBundle(ctx, result, aicr.BundleOptions{OutputDir: outputDir})
	check(err)

	fmt.Printf("components=%d files=%d bytes=%d hasErrors=%v dir=%s\n",
		len(result.Components), art.TotalFiles, art.TotalSize, art.HasErrors(), art.OutputDir)
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe failed:", err)
		os.Exit(1)
	}
}
```

- [ ] **Step 2: Generate a bundle and record its size**

Run from the repo root:

```bash
rm -rf /tmp/aicrme-probe-bundle
go run ./cmd/probe /tmp/aicrme-probe-bundle
du -sh /tmp/aicrme-probe-bundle
ls /tmp/aicrme-probe-bundle
```

Expected: a non-zero component count, `hasErrors=false`, a `deploy.sh` at the root, and `NNN-<name>/` directories. Record the `du -sh` figure — it decides the chart's `workDir.sizeLimit` in Task 3.

If `MakeBundle` errors, **stop and report to the user.** Do not work around it.

- [ ] **Step 3: Stand up a Kind + KWOK cluster with simulated H100 nodes**

`test/e2e/discover-recommend.sh` already does exactly this. Reuse it rather than re-deriving the node YAML:

```bash
kind create cluster --name aicrme-probe --wait 120s
curl -fsSL "https://github.com/kubernetes-sigs/kwok/releases/download/v0.8.0/kwok.yaml" | kubectl apply -f -
curl -fsSL "https://github.com/kubernetes-sigs/kwok/releases/download/v0.8.0/stage-fast.yaml" | kubectl apply -f -
kubectl -n kube-system rollout status deploy/kwok-controller --timeout=120s
```

Then apply the simulated nodes. Extract the `node_yaml` and `apply_kwok_nodes` shell functions from `test/e2e/discover-recommend.sh` (lines ~85-190) into a scratch file and source it, or run them by hand. Task 11 makes this sharing permanent; here it only needs to work once.

Verify: `kubectl get nodes` shows 2 system + 4 GPU nodes, all Ready.

- [ ] **Step 4: Dry-run `deploy.sh` and capture the transcript**

```bash
cd /tmp/aicrme-probe-bundle
NO_COLOR=1 DRY_RUN_FLAG=--dry-run KUBECONFIG_FLAG= HELM_DEBUG_FLAG= \
  bash deploy.sh --retries 0 2>&1 | tee /tmp/aicrme-probe-transcript.txt
echo "exit=${PIPESTATUS[0]}"
```

`--retries 0` keeps the probe fast: this run is about whether the markers appear and whether dry-run survives, not about retry behavior.

**This is the go/no-go gate.** Three outcomes:

- **Exit 0, markers present.** Proceed to Step 5.
- **Exit non-zero, but the failure is a chart that cannot render under `--dry-run`** (typically a chart templating a custom resource whose CRD is not yet installed). The markers are still valid and the transcript is still usable. Record the failing component in the findings, then proceed — Task 11's e2e will assert on progress reached, not on exit 0.
- **No `┌─` / `└─` markers at all, or the script dies in preflight.** **Stop and report to the user.** The parser premise is broken and the fallback is golden-files-only coverage with live verification deferred to Phase 4. That is the user's call.

- [ ] **Step 5: Verify the marker grammar matches the plan**

Confirm each of these appears in the transcript, byte for byte (note the **two spaces** on either side of `→`, and the **two leading spaces** on the retry line):

```bash
grep -n '^┌─ \[' /tmp/aicrme-probe-transcript.txt | head -3
grep -n '^└─ ✓' /tmp/aicrme-probe-transcript.txt | head -3
grep -c '^✓ Pre-flight checks passed$' /tmp/aicrme-probe-transcript.txt
```

Expected: header lines of the form `┌─ [1/13] gpu-operator  →  gpu-operator`, success lines `└─ ✓ gpu-operator installed`, and exactly one preflight-passed line.

If the literal shapes differ from the plan's Task 4 regexes, **the transcript wins** — update Task 4's regexes to match reality and note the discrepancy in the findings.

- [ ] **Step 6: Commit the transcript as a test fixture**

```bash
mkdir -p internal/applier/testdata
cp /tmp/aicrme-probe-transcript.txt internal/applier/testdata/deploy-transcript-kwok.txt
```

- [ ] **Step 7: Write the findings document**

Create `docs/phase-2a-task-1-findings.md` recording, with real measured numbers:

- component count and bundle size on disk (feeds Task 3's `sizeLimit`);
- whether `deploy.sh --dry-run` exited 0, and if not, which component failed and why;
- any deviation between the observed markers and the plan's Task 4 regexes;
- the exact `helm version` and `kubectl version` used, since the transcript is version-sensitive.

- [ ] **Step 8: Delete the probe, tear down, and commit**

```bash
rm -rf cmd/probe /tmp/aicrme-probe-bundle /tmp/aicrme-probe-transcript.txt
kind delete cluster --name aicrme-probe
make qualify
git add internal/applier/testdata/deploy-transcript-kwok.txt docs/phase-2a-task-1-findings.md
git commit -S -m "test(applier): capture a real deploy.sh dry-run transcript on KWOK

Phase 2a Task 1 probe. Proves the dry-run e2e premise before any applier
code is written, and captures the transcript the marker parser's golden
test reads. The probe program itself was throwaway and is not committed;
docs/phase-2a-task-1-findings.md records what it measured."
```

---

## Task 2: Bundle step

**Files:**
- Create: `internal/steps/criteria.go`
- Create: `internal/steps/bundle.go`
- Create: `internal/steps/bundle_test.go`
- Modify: `internal/steps/recommend.go` (remove the moved helpers)
- Modify: `internal/aicrclient/client.go` (add `Bundler` to `API`)
- Modify: `internal/aicrclient/fake.go` (implement `MakeBundle`)
- Modify: `cmd/aicrme/main.go` (wire `steps.NewBundle`)

**Interfaces:**
- Consumes: `aicrclient.API`, `engine.Step`, `engine.Run.Artifacts["snapshot.yaml"]` and `["recipe.json"]` (written by Discover and Recommend).
- Produces:
  - `aicrclient.Bundler` — `MakeBundle(ctx context.Context, r *aicr.RecipeResult, opts aicr.BundleOptions) (aicr.BundleArtifact, error)`
  - `steps.BundleConfig{ WorkDir string }`
  - `steps.NewBundle(c aicrclient.API, cfg BundleConfig) engine.Step`
  - `engine.Run.Artifacts["bundle.path"]` — the absolute bundle directory, consumed by Task 6's Apply step and Task 8's download route.
  - `steps.buildCriteria`, `steps.specificity`, `steps.decodeSnapshot` relocated to `criteria.go`, unexported, unchanged behavior.

- [ ] **Step 1: Move the shared helpers into `criteria.go`**

Create `internal/steps/criteria.go`. Move `criteriaAny`, `decodeSnapshot`, `buildCriteria`, and `specificity` out of `recommend.go` **verbatim, including their doc comments** — they carry hard-won reasoning about AICR issue #1888 and the `WrapSnapshot` requirement. Add a package-level note at the top:

```go
package steps

// Criteria derivation is shared by Recommend and Bundle. Bundle re-resolves
// the recipe rather than receiving a *aicr.RecipeResult handle from
// Recommend (MakeBundle requires a Client-owned one, which cannot travel
// through Run.Artifacts' []byte values), so both steps must derive criteria
// identically or the re-resolve could silently produce a different recipe
// than the one the user approved. steps.Bundle's assertMatchesApproved is
// the backstop that proves they did not.
```

- [ ] **Step 2: Run tests to verify the move changed nothing**

Run: `go test ./internal/steps/... -race`
Expected: PASS. This is a pure move — if anything fails, the move was not verbatim.

- [ ] **Step 3: Commit the move on its own**

```bash
git add internal/steps/criteria.go internal/steps/recommend.go
git commit -S -m "refactor(steps): extract criteria derivation for Bundle to share"
```

- [ ] **Step 4: Add `Bundler` to the AICR facade**

In `internal/aicrclient/client.go`, add above the `API` interface:

```go
// Bundler generates the on-disk deployer bundle for a resolved recipe. Kept
// as its own single-method interface for the same reason as the others: a
// step that only bundles should not be able to collect a snapshot.
type Bundler interface {
	MakeBundle(ctx context.Context, r *aicr.RecipeResult, opts aicr.BundleOptions) (aicr.BundleArtifact, error)
}
```

and add `Bundler` to the `API` interface's embedded list. The existing `var _ API = (*aicr.Client)(nil)` assertion then proves `*aicr.Client.MakeBundle` matches this signature at compile time.

- [ ] **Step 5: Teach the fake to bundle**

In `internal/aicrclient/fake.go`, add fields to `Fake`:

```go
	Artifact    aicr.BundleArtifact
	BundleErr   error

	BundleCalls   int
	LastBundleDir string
```

and the method:

```go
// MakeBundle records the call and the OutputDir it was given, then returns
// the configured Artifact. When Artifact is unset it returns a zero-value
// one so callers can assert on a non-nil result without scripting it, and
// it creates OutputDir so a caller that then stats the directory sees what
// a real bundle run would have left behind.
func (f *Fake) MakeBundle(_ context.Context, _ *aicr.RecipeResult, opts aicr.BundleOptions) (aicr.BundleArtifact, error) {
	f.BundleCalls++
	f.LastBundleDir = opts.OutputDir
	if f.BundleErr != nil {
		return nil, f.BundleErr
	}
	if opts.OutputDir != "" {
		if err := os.MkdirAll(opts.OutputDir, 0o755); err != nil {
			return nil, err
		}
	}
	if f.Artifact == nil {
		return &result.Output{OutputDir: opts.OutputDir}, nil
	}
	return f.Artifact, nil
}
```

Add imports `"os"` and `"github.com/NVIDIA/aicr/pkg/bundler/result"`.

- [ ] **Step 6: Write the failing Bundle tests**

Create `internal/steps/bundle_test.go`:

```go
package steps_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/steps"
)

// approvedFrom renders the recipe.json artifact Recommend would have
// written for r, so a Bundle test can assert the match guard against the
// same shape the real pipeline produces.
func approvedFrom(t *testing.T, r *aicr.RecipeResult) []byte {
	t.Helper()
	summary := steps.RecipeSummary{
		Name: r.Name, Version: r.Version, ComponentCount: len(r.Components),
		Components: make([]steps.ComponentSummary, 0, len(r.Components)),
	}
	for _, c := range r.Components {
		summary.Components = append(summary.Components, steps.ComponentSummary{
			Name: c.Name, Kind: c.Kind, Version: c.Version,
			Namespace: c.Namespace, Chart: c.Chart, Source: c.Source,
		})
	}
	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("marshal approved summary error = %v", err)
	}
	return encoded
}

func TestBundleGatesOnNoDecisions(t *testing.T) {
	step := steps.NewBundle(&aicrclient.Fake{}, steps.BundleConfig{WorkDir: t.TempDir()})
	if got := step.Requires(); len(got) != 0 {
		t.Errorf("Requires() = %v, want none -- Bundle runs automatically after Recommend", got)
	}
}

func TestBundleWritesBundlePathArtifact(t *testing.T) {
	recipe := recipeFixture()
	fake := &aicrclient.Fake{Recipe: recipe}
	workDir := t.TempDir()
	step := steps.NewBundle(fake, steps.BundleConfig{WorkDir: workDir})

	run := newRun()
	run.Decisions["intent"] = "training"
	run.Decisions["platform"] = "kubeflow"
	run.Artifacts["snapshot.yaml"] = []byte(minimalSnapshot)
	run.Artifacts["recipe.json"] = approvedFrom(t, recipe)

	if err := step.Run(context.Background(), run, func(bus.Event) {}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	want := filepath.Join(workDir, "runs", run.ID, "bundle")
	if got := string(run.Artifacts["bundle.path"]); got != want {
		t.Errorf("bundle.path = %q, want %q", got, want)
	}
	if fake.LastBundleDir != want {
		t.Errorf("MakeBundle OutputDir = %q, want %q", fake.LastBundleDir, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("bundle dir not created: %v", err)
	}
}

// The whole point of re-resolving instead of threading a handle from
// Recommend is that the user approved a specific component list. If the
// re-resolve drifts, bundling it anyway would install something the user
// never saw.
func TestBundleFailsClosedWhenReresolveDiffersFromApproved(t *testing.T) {
	approved := recipeFixture()
	drifted := recipeFixture()
	drifted.Components = drifted.Components[:1]

	fake := &aicrclient.Fake{Recipe: drifted}
	step := steps.NewBundle(fake, steps.BundleConfig{WorkDir: t.TempDir()})

	run := newRun()
	run.Decisions["intent"] = "training"
	run.Decisions["platform"] = "kubeflow"
	run.Artifacts["snapshot.yaml"] = []byte(minimalSnapshot)
	run.Artifacts["recipe.json"] = approvedFrom(t, approved)

	err := step.Run(context.Background(), run, func(bus.Event) {})
	if err == nil {
		t.Fatal("Run() error = nil, want a drift error")
	}
	if !strings.Contains(err.Error(), "component count") {
		t.Errorf("Run() error = %v, want it to name the count mismatch", err)
	}
	if fake.BundleCalls != 0 {
		t.Errorf("MakeBundle called %d times, want 0 -- must not bundle a drifted recipe", fake.BundleCalls)
	}
}

func TestBundleFailsClosedOnVersionDrift(t *testing.T) {
	approved := recipeFixture()
	drifted := recipeFixture()
	drifted.Components[0].Version = "v0.0.0-drifted"

	fake := &aicrclient.Fake{Recipe: drifted}
	step := steps.NewBundle(fake, steps.BundleConfig{WorkDir: t.TempDir()})

	run := newRun()
	run.Decisions["intent"] = "training"
	run.Decisions["platform"] = "kubeflow"
	run.Artifacts["snapshot.yaml"] = []byte(minimalSnapshot)
	run.Artifacts["recipe.json"] = approvedFrom(t, approved)

	err := step.Run(context.Background(), run, func(bus.Event) {})
	if err == nil || !strings.Contains(err.Error(), "gpu-operator") {
		t.Fatalf("Run() error = %v, want it to name the drifted component", err)
	}
}

func TestBundleRequiresRecommendToHaveRun(t *testing.T) {
	fake := &aicrclient.Fake{Recipe: recipeFixture()}
	step := steps.NewBundle(fake, steps.BundleConfig{WorkDir: t.TempDir()})

	run := newRun()
	run.Decisions["intent"] = "training"
	run.Decisions["platform"] = "kubeflow"
	run.Artifacts["snapshot.yaml"] = []byte(minimalSnapshot)
	// recipe.json deliberately absent.

	err := step.Run(context.Background(), run, func(bus.Event) {})
	if err == nil || !strings.Contains(err.Error(), "recipe.json") {
		t.Fatalf("Run() error = %v, want it to name the missing artifact", err)
	}
}
```

- [ ] **Step 7: Run the tests to verify they fail**

Run: `go test ./internal/steps/ -race -run TestBundle -v`
Expected: FAIL — `undefined: steps.NewBundle`.

- [ ] **Step 8: Implement the Bundle step**

Create `internal/steps/bundle.go`:

```go
package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
)

// BundleConfig configures where generated bundles land.
type BundleConfig struct {
	// WorkDir is the writable scratch root -- an emptyDir in the chart. Each
	// run gets <WorkDir>/runs/<runID>/bundle. Nothing here needs to outlive
	// the pod: the bundle is regenerated from the pinned, embedded catalog.
	WorkDir string
}

type bundle struct {
	client aicrclient.API
	cfg    BundleConfig
}

// NewBundle returns the Bundle step. It gates on no decisions: Recommend has
// already collected the only two the console asks for, and Bundle runs
// automatically so the confirm gate on Apply has a real bundle to show.
//
// Bundle RE-RESOLVES the recipe rather than receiving Recommend's
// *aicr.RecipeResult. aicr.Client.MakeBundle requires a Client-owned result
// (it calls assertOwns and reads unexported state), which cannot travel
// through engine.Run.Artifacts' []byte values. The alternative -- a holder
// shared between the two steps -- would be in-memory state the ConfigMap
// store in Phase 2b then has to lose across a pod restart, which is the
// exact case that store exists to fix. Re-resolving survives a restart for
// free: every input is persisted (the raw snapshot bytes, the two
// decisions) and the catalog is embedded in the pinned aicr module, so the
// resolve is deterministic. assertMatchesApproved proves it.
func NewBundle(c aicrclient.API, cfg BundleConfig) engine.Step {
	return &bundle{client: c, cfg: cfg}
}

func (b *bundle) Phase() engine.Phase { return engine.PhaseBundle }
func (b *bundle) Requires() []string  { return nil }

func (b *bundle) Run(ctx context.Context, run *engine.Run, emit engine.Emit) error {
	approved := run.Artifacts["recipe.json"]
	if len(approved) == 0 {
		return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			"recipe.json artifact is missing -- Recommend must run before Bundle")
	}

	snap, err := decodeSnapshot(run.Artifacts["snapshot.yaml"])
	if err != nil {
		return err
	}

	criteria, err := buildCriteria(b.client, snap, run.Decisions["intent"], run.Decisions["platform"])
	if err != nil {
		return err
	}

	result, err := b.client.ResolveRecipeFromSnapshot(ctx, criteria, snap)
	if err != nil {
		return aicrerrors.PropagateOrWrap(err, aicrerrors.ErrCodeInvalidRequest, "recipe re-resolution failed")
	}
	if result == nil {
		return aicrerrors.New(aicrerrors.ErrCodeInternal, "recipe re-resolution returned no result")
	}
	if err := assertMatchesApproved(result, approved); err != nil {
		return err
	}

	dir := filepath.Join(b.cfg.WorkDir, "runs", run.ID, "bundle")
	emit(bus.Event{Kind: bus.KindLog, Message: fmt.Sprintf("generating bundle for %d components", len(result.Components))})

	art, err := b.client.MakeBundle(ctx, result, aicr.BundleOptions{OutputDir: dir})
	if err != nil {
		return aicrerrors.PropagateOrWrap(err, aicrerrors.ErrCodeInternal, "bundle generation failed")
	}
	if art == nil {
		return aicrerrors.New(aicrerrors.ErrCodeInternal, "bundle generation returned no artifact")
	}
	// HasErrors covers per-bundler failures that Make itself reports as
	// non-fatal. Applying a partially generated bundle would fail later and
	// less legibly, so fail here instead.
	if art.HasErrors() {
		return aicrerrors.New(aicrerrors.ErrCodeInternal,
			fmt.Sprintf("bundle generation reported errors: %v", art.Errors))
	}

	run.Artifacts["bundle.path"] = []byte(dir)

	emit(bus.Event{
		Kind:    bus.KindLog,
		Message: fmt.Sprintf("bundle ready: %d files, %d bytes", art.TotalFiles, art.TotalSize),
	})
	return nil
}

// assertMatchesApproved proves the re-resolved recipe is the one the user
// approved on the Recommend screen. Bundle re-resolves rather than carrying
// a handle forward (see NewBundle), so this is the guard that makes that
// choice safe rather than merely convenient: without it, a catalog or
// derivation change between the two resolves would silently install a
// component set the user never saw.
func assertMatchesApproved(result *aicr.RecipeResult, approved []byte) error {
	var summary RecipeSummary
	if err := json.Unmarshal(approved, &summary); err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "stored recipe.json is unparseable", err)
	}
	if len(result.Components) != summary.ComponentCount {
		return aicrerrors.New(aicrerrors.ErrCodeInternal, fmt.Sprintf(
			"re-resolved recipe drifted from the approved one: component count %d, approved %d",
			len(result.Components), summary.ComponentCount))
	}
	for i, c := range result.Components {
		a := summary.Components[i]
		if c.Name != a.Name || c.Version != a.Version {
			return aicrerrors.New(aicrerrors.ErrCodeInternal, fmt.Sprintf(
				"re-resolved recipe drifted from the approved one at position %d: %s %s, approved %s %s",
				i, c.Name, c.Version, a.Name, a.Version))
		}
	}
	return nil
}
```

- [ ] **Step 9: Run the tests to verify they pass**

Run: `go test ./internal/steps/ ./internal/aicrclient/ -race`
Expected: PASS.

- [ ] **Step 10: Wire Bundle into the pipeline**

In `cmd/aicrme/main.go`, add a work-directory constant and pass the step. Below `defaultSnapshotAgentImage`:

```go
// defaultWorkDir is the writable scratch root. The chart mounts an emptyDir
// here (charts/aicrme/templates/deployment.yaml) and points TMPDIR, HOME,
// and the helm/kubectl cache variables at subdirectories of it, which is
// what lets the pod run with readOnlyRootFilesystem: true.
const defaultWorkDir = "/var/lib/aicrme"
```

and in the `engine.New(...)` call, after `steps.NewRecommend(client)`:

```go
		steps.NewBundle(client, steps.BundleConfig{
			WorkDir: envOr("AICRME_WORK_DIR", defaultWorkDir),
		}),
```

- [ ] **Step 11: Qualify and commit**

```bash
make qualify
git add internal/aicrclient/ internal/steps/ cmd/aicrme/main.go
git commit -S -m "feat(steps): Bundle step generating a real AICR bundle on disk

Re-resolves the recipe from the persisted snapshot and decisions rather
than threading Recommend's Client-owned *aicr.RecipeResult forward --
MakeBundle requires an owned handle, which cannot travel through
Run.Artifacts, and a step-to-step holder would be in-memory state the
ConfigMap store in 2b has to lose on restart. assertMatchesApproved is
what makes the re-resolve safe: it fails closed if the second resolve
drifts from the component list the user approved."
```

---

## Task 3: Chart workdir, cache env, and `readOnlyRootFilesystem`

**Files:**
- Modify: `charts/aicrme/values.yaml`
- Modify: `charts/aicrme/templates/deployment.yaml`
- Modify: `cmd/aicrme/main.go` (create the subdirectories at startup)
- Modify: `cmd/aicrme/main_test.go`
- Modify: `test/chart/contract.sh`

**Interfaces:**
- Consumes: `defaultWorkDir` from Task 2.
- Produces: a writable `/var/lib/aicrme` with `tmp/`, `home/`, `helm/{cache,config,data}/`, `kube/cache/`, and `runs/` present at startup; the pod runs `readOnlyRootFilesystem: true`.

- [ ] **Step 1: Write the failing startup test**

Add to `cmd/aicrme/main_test.go`:

```go
func TestEnsureWorkDirsCreatesEveryCacheDir(t *testing.T) {
	root := t.TempDir()
	if err := ensureWorkDirs(root); err != nil {
		t.Fatalf("ensureWorkDirs() error = %v", err)
	}
	for _, sub := range []string{"tmp", "home", "helm/cache", "helm/config", "helm/data", "kube/cache", "runs"} {
		if _, err := os.Stat(filepath.Join(root, sub)); err != nil {
			t.Errorf("missing %s: %v", sub, err)
		}
	}
}

func TestEnsureWorkDirsFailsOnUnwritableRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("setup error = %v", err)
	}
	if err := ensureWorkDirs(root); err == nil {
		t.Error("ensureWorkDirs() error = nil, want a failure -- an unwritable work dir must not start silently")
	}
}
```

Add imports `"os"` and `"path/filepath"` if absent.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./cmd/aicrme/ -race -run TestEnsureWorkDirs -v`
Expected: FAIL — `undefined: ensureWorkDirs`.

- [ ] **Step 3: Implement `ensureWorkDirs` and call it**

In `cmd/aicrme/main.go`, add:

```go
// workSubdirs are the directories the console and deploy.sh need writable.
// With readOnlyRootFilesystem: true, the emptyDir at AICRME_WORK_DIR is the
// only writable path in the container, so every tool that wants scratch
// space is pointed at a subdirectory of it by the chart's env block --
// bash's mktemp -d at TMPDIR, helm's three XDG-style caches, kubectl's
// discovery cache, and $HOME for anything that ignores all of the above.
// They are created here rather than by the chart because an emptyDir is
// mounted empty on every pod start.
var workSubdirs = []string{"tmp", "home", "helm/cache", "helm/config", "helm/data", "kube/cache", "runs"}

func ensureWorkDirs(root string) error {
	for _, sub := range workSubdirs {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o700); err != nil {
			return err
		}
	}
	return nil
}
```

Add `"path/filepath"` to the imports. In `main()`, immediately after the `slog.Info("starting aicrme", ...)` line:

```go
	workDir := envOr("AICRME_WORK_DIR", defaultWorkDir)
	if err := ensureWorkDirs(workDir); err != nil {
		slog.Error("work directory unusable", "dir", workDir, "error", err)
		os.Exit(1)
	}
```

and change the Bundle wiring from Task 2 to use the local `workDir` variable rather than re-reading the env.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./cmd/aicrme/ -race`
Expected: PASS.

- [ ] **Step 5: Add the chart's workDir knob**

In `charts/aicrme/values.yaml`, after the `resources:` block:

```yaml
# Scratch space for generated bundles plus the helm, kubectl, and bash temp
# directories deploy.sh needs. emptyDir, not a PVC: a bundle is regenerated
# deterministically from the pinned, embedded recipe catalog on every run,
# so nothing here needs to outlive the pod. sizeLimit is measured against a
# real bundle -- see docs/phase-2a-task-1-findings.md.
workDir:
  sizeLimit: 1Gi
```

Set `sizeLimit` from Task 1's measured `du -sh`, rounded generously upward — helm's chart cache grows past the bundle itself.

- [ ] **Step 6: Mount it and lock the root filesystem**

In `charts/aicrme/templates/deployment.yaml`, inside the container's `securityContext`, add:

```yaml
            # Every writable path the console and deploy.sh need is a
            # subdirectory of the work-dir emptyDir below, so nothing needs
            # to write to the image's own filesystem.
            readOnlyRootFilesystem: true
```

Add to the container's `env` list:

```yaml
            - name: AICRME_WORK_DIR
              value: /var/lib/aicrme
            # deploy.sh runs `mktemp -d`; helm and kubectl keep caches. With
            # readOnlyRootFilesystem the emptyDir is the only writable path,
            # so each tool is pointed into it explicitly. cmd/aicrme creates
            # these subdirectories at startup (an emptyDir mounts empty).
            - name: TMPDIR
              value: /var/lib/aicrme/tmp
            - name: HOME
              value: /var/lib/aicrme/home
            - name: HELM_CACHE_HOME
              value: /var/lib/aicrme/helm/cache
            - name: HELM_CONFIG_HOME
              value: /var/lib/aicrme/helm/config
            - name: HELM_DATA_HOME
              value: /var/lib/aicrme/helm/data
            - name: KUBECACHEDIR
              value: /var/lib/aicrme/kube/cache
```

Add after the container's `resources:` line:

```yaml
          volumeMounts:
            - name: work
              mountPath: /var/lib/aicrme
```

and at the pod-spec level, after the `containers:` block:

```yaml
      volumes:
        - name: work
          emptyDir:
            sizeLimit: {{ .Values.workDir.sizeLimit }}
```

- [ ] **Step 7: Add chart contract assertions**

In `test/chart/contract.sh`, following the file's existing assertion style, add checks that a default `helm template` render contains:

- `readOnlyRootFilesystem: true` on the container security context;
- an `emptyDir` volume named `work` mounted at `/var/lib/aicrme`;
- all seven env vars (`AICRME_WORK_DIR`, `TMPDIR`, `HOME`, `HELM_CACHE_HOME`, `HELM_CONFIG_HOME`, `HELM_DATA_HOME`, `KUBECACHEDIR`);
- the `sizeLimit` honoring `--set workDir.sizeLimit=2Gi`.

Read the file first and match its existing helper functions and failure-message style rather than inventing a new one.

- [ ] **Step 8: Verify chart and image**

```bash
make test-chart
helm template aicrme charts/aicrme | grep -A3 'readOnlyRootFilesystem'
```
Expected: `make test-chart` PASSes and the render shows the locked root filesystem.

- [ ] **Step 9: Qualify and commit**

```bash
make qualify
git add charts/aicrme/ cmd/aicrme/ test/chart/contract.sh
git commit -S -m "feat(chart): work-dir emptyDir, tool cache env, readOnlyRootFilesystem

The Phase 0-1 review deferred readOnlyRootFilesystem until the deploy.sh
wiring showed which cache dirs need to be writable. That list is now
known and is exactly the six variables set here, all pointed into one
emptyDir, so the console's root filesystem can finally be read-only."
```

---

## Task 4: The `deploy.sh` marker parser

**Files:**
- Create: `internal/applier/parse.go`
- Create: `internal/applier/parse_test.go`
- Create: `internal/applier/testdata/deploy-transcript-failure.txt`

**Interfaces:**
- Consumes: `internal/applier/testdata/deploy-transcript-kwok.txt` (Task 1).
- Produces:
  - `applier.ComponentData` and `applier.FailureData` — the JSON payloads on `bus.Event.Data`, consumed by Task 9's `web/src/pipeline.ts`.
  - `parseLine(line string) (bus.Event, bool)` — unexported, consumed by Task 5.

- [ ] **Step 1: Hand-author the failure transcript fixture**

Task 1's captured transcript is a dry run, so it contains no retry or failure markers. Create `internal/applier/testdata/deploy-transcript-failure.txt` from the template's exact `printf` formats (`pkg/bundler/deployer/helm/templates/deploy.sh.tmpl` lines 66-87, with every color variable empty under `NO_COLOR=1`). Note the two spaces around `→` and the two leading spaces on the `↺` line:

```

══ Pre-flight checks ══════════════════════════════════════════════
✓ Pre-flight checks passed

══ Deploying AICR components ══════════════════════════════════════

┌─ [1/3] cert-manager  →  cert-manager
│  Manual (approx, set KUBECONFIG_FLAG/DRY_RUN_FLAG/COMPONENT_WAIT_ARGS as needed): cd /bundle/001-cert-manager && bash install.sh
Release "cert-manager" does not exist. Installing it now.
└─ ✓ cert-manager installed

┌─ [2/3] kai-scheduler  →  kai-scheduler
│  Manual (approx, set KUBECONFIG_FLAG/DRY_RUN_FLAG/COMPONENT_WAIT_ARGS as needed): cd /bundle/002-kai-scheduler && bash install.sh
│  (async component — skipping --wait, keeping --timeout for hooks)
Error: INSTALLATION FAILED: timed out waiting for the condition
  --- Failed hook Job kai-scheduler-crd-upgrader diagnostics ---
  Cleaning up stale Helm hook Job kai-scheduler-crd-upgrader in kai-scheduler...
  --- End diagnostics for kai-scheduler-crd-upgrader ---
  ↺ kai-scheduler: attempt 1/1 failed, retrying in 5s...
Error: INSTALLATION FAILED: timed out waiting for the condition
└─ ✗ kai-scheduler FAILED (after 2 attempts)
⚠ kai-scheduler install failed, continuing (--best-effort)
✗ Deployment completed with failures (--best-effort): kai-scheduler
```

- [ ] **Step 2: Write the failing parser tests**

Create `internal/applier/parse_test.go`:

```go
package applier

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/mchmarny/aicrme/internal/bus"
)

func componentData(t *testing.T, e bus.Event) ComponentData {
	t.Helper()
	var d ComponentData
	if err := json.Unmarshal(e.Data, &d); err != nil {
		t.Fatalf("unmarshal ComponentData error = %v (data=%s)", err, e.Data)
	}
	return d
}

func TestParseLineMarkerGrammar(t *testing.T) {
	tests := []struct {
		name      string
		line      string
		wantKind  bus.Kind
		wantLevel bus.Level
		wantComp  string
		check     func(t *testing.T, e bus.Event)
	}{
		{
			name: "component header", line: "┌─ [3/16] gpu-operator  →  gpu-operator",
			wantKind: bus.KindComponent, wantLevel: bus.LevelInfo, wantComp: "gpu-operator",
			check: func(t *testing.T, e bus.Event) {
				d := componentData(t, e)
				if d.Index != 3 || d.Total != 16 || d.Namespace != "gpu-operator" || d.Status != StatusStarted {
					t.Errorf("ComponentData = %+v", d)
				}
			},
		},
		{
			name: "component installed", line: "└─ ✓ gpu-operator installed",
			wantKind: bus.KindComponent, wantLevel: bus.LevelInfo, wantComp: "gpu-operator",
			check: func(t *testing.T, e bus.Event) {
				if d := componentData(t, e); d.Status != StatusInstalled {
					t.Errorf("Status = %q, want %q", d.Status, StatusInstalled)
				}
			},
		},
		{
			name: "component failed", line: "└─ ✗ kai-scheduler FAILED (after 2 attempts)",
			wantKind: bus.KindComponent, wantLevel: bus.LevelError, wantComp: "kai-scheduler",
			check: func(t *testing.T, e bus.Event) {
				d := componentData(t, e)
				if d.Status != StatusFailed || d.Attempt != 2 {
					t.Errorf("ComponentData = %+v", d)
				}
			},
		},
		{
			name: "component retrying", line: "  ↺ kai-scheduler: attempt 1/5 failed, retrying in 20s...",
			wantKind: bus.KindComponent, wantLevel: bus.LevelWarn, wantComp: "kai-scheduler",
			check: func(t *testing.T, e bus.Event) {
				d := componentData(t, e)
				if d.Status != StatusRetrying || d.Attempt != 1 || d.MaxAttempts != 5 || d.RetryInSeconds != 20 {
					t.Errorf("ComponentData = %+v", d)
				}
			},
		},
		{
			name: "preflight passed", line: "✓ Pre-flight checks passed",
			wantKind: bus.KindPhase, wantLevel: bus.LevelInfo,
		},
		{
			name: "all installed", line: "✓ All components installed successfully.",
			wantKind: bus.KindPhase, wantLevel: bus.LevelInfo,
		},
		{
			name: "warn line", line: "⚠ kai-scheduler install failed, continuing (--best-effort)",
			wantKind: bus.KindLog, wantLevel: bus.LevelWarn,
		},
		{
			name: "fail line", line: "✗ Pre-flight checks failed. Fix the issues above before deploying.",
			wantKind: bus.KindLog, wantLevel: bus.LevelError,
		},
		{
			name: "async note", line: "│  (async component — skipping --wait, keeping --timeout for hooks)",
			wantKind: bus.KindLog, wantLevel: bus.LevelInfo,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseLine(tt.line)
			if !ok {
				t.Fatalf("parseLine(%q) not recognized", tt.line)
			}
			if got.Kind != tt.wantKind {
				t.Errorf("Kind = %q, want %q", got.Kind, tt.wantKind)
			}
			if got.Level != tt.wantLevel {
				t.Errorf("Level = %q, want %q", got.Level, tt.wantLevel)
			}
			if got.Component != tt.wantComp {
				t.Errorf("Component = %q, want %q", got.Component, tt.wantComp)
			}
			if tt.check != nil {
				tt.check(t, got)
			}
		})
	}
}

// Helm and kubectl output is the overwhelming majority of deploy.sh's
// stdout. Publishing it would flood the bus's replay ring and get slow
// subscribers dropped, so it must not parse into events at all.
func TestParseLineIgnoresNonMarkerOutput(t *testing.T) {
	for _, line := range []string{
		"",
		"Release \"cert-manager\" does not exist. Installing it now.",
		"NAME: gpu-operator",
		"  --- Failed hook Job kai-scheduler-crd-upgrader diagnostics ---",
		"│  Manual (approx, set KUBECONFIG_FLAG/DRY_RUN_FLAG/COMPONENT_WAIT_ARGS as needed): cd /bundle/001-x && bash install.sh",
		"══ Deploying AICR components ══════════════════════════════════════",
		"Error: INSTALLATION FAILED: timed out waiting for the condition",
	} {
		if got, ok := parseLine(line); ok {
			t.Errorf("parseLine(%q) recognized as %+v, want ignored", line, got)
		}
	}
}

// A header with an empty namespace is reachable: deploy.sh derives the
// namespace by awk-ing the component's install.sh, which yields an empty
// string if that grep ever misses.
func TestParseLineToleratesEmptyNamespace(t *testing.T) {
	got, ok := parseLine("┌─ [1/1] mystery  →  ")
	if !ok {
		t.Fatal("parseLine() not recognized")
	}
	if d := componentData(t, got); d.Name != "mystery" || d.Namespace != "" {
		t.Errorf("ComponentData = %+v", d)
	}
}

// The real captured transcript is the regression guard: it is what an AICR
// bump actually changes.
func TestParseTranscriptFixtures(t *testing.T) {
	tests := []struct {
		file          string
		wantStarted   bool
		wantInstalled bool
		wantFailed    bool
		wantRetrying  bool
	}{
		{file: "testdata/deploy-transcript-kwok.txt", wantStarted: true, wantInstalled: true},
		{file: "testdata/deploy-transcript-failure.txt", wantStarted: true, wantInstalled: true, wantFailed: true, wantRetrying: true},
	}

	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			raw, err := os.ReadFile(tt.file)
			if err != nil {
				t.Fatalf("read fixture error = %v", err)
			}
			seen := map[string]bool{}
			preflight := false
			for _, line := range strings.Split(string(raw), "\n") {
				e, ok := parseLine(line)
				if !ok {
					continue
				}
				if e.Kind == bus.KindPhase {
					preflight = true
				}
				if e.Kind == bus.KindComponent {
					seen[componentData(t, e).Status] = true
				}
			}
			if !preflight {
				t.Error("no phase event parsed -- the preflight marker is missing or changed")
			}
			if seen[StatusStarted] != tt.wantStarted {
				t.Errorf("started seen = %v, want %v", seen[StatusStarted], tt.wantStarted)
			}
			if seen[StatusInstalled] != tt.wantInstalled {
				t.Errorf("installed seen = %v, want %v", seen[StatusInstalled], tt.wantInstalled)
			}
			if seen[StatusFailed] != tt.wantFailed {
				t.Errorf("failed seen = %v, want %v", seen[StatusFailed], tt.wantFailed)
			}
			if seen[StatusRetrying] != tt.wantRetrying {
				t.Errorf("retrying seen = %v, want %v", seen[StatusRetrying], tt.wantRetrying)
			}
		})
	}
}
```

- [ ] **Step 3: Run to verify it fails**

Run: `go test ./internal/applier/ -race -v`
Expected: FAIL — the package does not compile (`undefined: parseLine`).

- [ ] **Step 4: Implement the parser**

Create `internal/applier/parse.go`:

```go
// Package applier executes a generated AICR bundle by driving the bundle's
// own deploy.sh, converting its stable output markers into console events.
//
// Why deploy.sh rather than each component's install.sh: deploy.sh carries
// correctness logic a per-component loop silently drops -- preflight for
// terminating namespaces, stale webhooks and orphaned CRD groups;
// per-component wait derivation; quadratic-backoff retry with helm hook-Job
// cleanup; and a post-install block that waits for every managed GPU node to
// reach nvidia.com/gpu-driver-upgrade-state=upgrade-done before restarting
// the DRA kubelet plugin. Skipping that last one strands DRA pods in
// ContainerCreating (AICR issue #973). Driving deploy.sh also keeps what the
// console runs byte-identical to what the user downloads.
package applier

import (
	"encoding/json"
	"regexp"
	"strconv"

	"github.com/mchmarny/aicrme/internal/bus"
)

// Component lifecycle values carried on ComponentData.Status.
const (
	StatusStarted   = "started"
	StatusInstalled = "installed"
	StatusFailed    = "failed"
	StatusRetrying  = "retrying"
)

// ComponentData is the Data payload on every KindComponent event. The SPA's
// web/src/pipeline.ts consumes this shape field for field.
type ComponentData struct {
	Name           string `json:"name"`
	Namespace      string `json:"namespace,omitempty"`
	Index          int    `json:"index,omitempty"`
	Total          int    `json:"total,omitempty"`
	Status         string `json:"status"`
	Attempt        int    `json:"attempt,omitempty"`
	MaxAttempts    int    `json:"maxAttempts,omitempty"`
	RetryInSeconds int    `json:"retryInSeconds,omitempty"`
}

// FailureData is the Data payload on the single KindError event the applier
// publishes when deploy.sh exits non-zero. Tail carries the last lines of
// raw output, which is where deploy.sh's own hook-Job and kai-scheduler
// diagnostic dumps live -- exactly what the failure screen must show.
type FailureData struct {
	Component string   `json:"component,omitempty"`
	ExitError string   `json:"exitError"`
	Tail      []string `json:"tail"`
}

// Marker patterns, transcribed from pkg/bundler/deployer/helm/templates/
// deploy.sh.tmpl at aicr v0.19.0 (_step_header, _step_ok, _step_fail,
// _step_retry, _ok, _warn_line, _fail). Every color variable expands empty
// because the applier exports NO_COLOR=1, so these match the bare text.
//
// The spacing is load-bearing and easy to "tidy" into breakage: the header
// has TWO spaces on each side of the arrow, and the retry line has TWO
// leading spaces. TestDeployTemplateUnchanged pins the template's sha256 so
// an upstream edit fails CI rather than silently emptying the timeline.
var (
	reHeader    = regexp.MustCompile(`^┌─ \[(\d+)/(\d+)\] (\S+)  →  (\S*)\s*$`)
	reInstalled = regexp.MustCompile(`^└─ ✓ (\S+) installed$`)
	reFailed    = regexp.MustCompile(`^└─ ✗ (\S+) FAILED \(after (\d+) attempts\)$`)
	reRetry     = regexp.MustCompile(`^ {2}↺ (\S+): attempt (\d+)/(\d+) failed, retrying in (\d+)s\.\.\.$`)
	reAsync     = regexp.MustCompile(`^│ {2}\((async component.*)\)$`)
	rePhaseOK   = regexp.MustCompile(`^✓ (Pre-flight checks passed|All components installed successfully\.)$`)
	reWarn      = regexp.MustCompile(`^⚠ (.+)$`)
	reFail      = regexp.MustCompile(`^✗ (.+)$`)
)

// parseLine maps one line of deploy.sh output to a console event. The bool
// is false for every line that is not a marker -- helm and kubectl output,
// banners, and diagnostic dumps. Those are deliberately NOT published: they
// are the overwhelming majority of the stream, and publishing them would
// exhaust the bus's replay ring and get live subscribers dropped for
// falling behind. Apply retains them in a bounded tail instead, and logs
// every line so `kubectl logs` keeps the complete transcript.
func parseLine(line string) (bus.Event, bool) {
	if m := reHeader.FindStringSubmatch(line); m != nil {
		return componentEvent(bus.LevelInfo, m[3], "installing "+m[3], ComponentData{
			Name:      m[3],
			Namespace: m[4],
			Index:     atoi(m[1]),
			Total:     atoi(m[2]),
			Status:    StatusStarted,
		}), true
	}
	if m := reInstalled.FindStringSubmatch(line); m != nil {
		return componentEvent(bus.LevelInfo, m[1], m[1]+" installed", ComponentData{
			Name: m[1], Status: StatusInstalled,
		}), true
	}
	if m := reFailed.FindStringSubmatch(line); m != nil {
		return componentEvent(bus.LevelError, m[1], m[1]+" failed after "+m[2]+" attempts", ComponentData{
			Name: m[1], Status: StatusFailed, Attempt: atoi(m[2]),
		}), true
	}
	if m := reRetry.FindStringSubmatch(line); m != nil {
		return componentEvent(bus.LevelWarn, m[1],
			m[1]+": attempt "+m[2]+" of "+m[3]+" failed, retrying in "+m[4]+"s",
			ComponentData{
				Name: m[1], Status: StatusRetrying,
				Attempt: atoi(m[2]), MaxAttempts: atoi(m[3]), RetryInSeconds: atoi(m[4]),
			}), true
	}
	if m := reAsync.FindStringSubmatch(line); m != nil {
		return bus.Event{Kind: bus.KindLog, Level: bus.LevelInfo, Message: m[1]}, true
	}
	if m := rePhaseOK.FindStringSubmatch(line); m != nil {
		return bus.Event{Kind: bus.KindPhase, Level: bus.LevelInfo, Message: m[1]}, true
	}
	if m := reWarn.FindStringSubmatch(line); m != nil {
		return bus.Event{Kind: bus.KindLog, Level: bus.LevelWarn, Message: m[1]}, true
	}
	if m := reFail.FindStringSubmatch(line); m != nil {
		return bus.Event{Kind: bus.KindLog, Level: bus.LevelError, Message: m[1]}, true
	}
	return bus.Event{}, false
}

func componentEvent(level bus.Level, component, message string, d ComponentData) bus.Event {
	// ComponentData holds only strings and ints, so Marshal cannot fail.
	encoded, _ := json.Marshal(d)
	return bus.Event{
		Kind:      bus.KindComponent,
		Level:     level,
		Component: component,
		Message:   message,
		Data:      encoded,
	}
}

// atoi is only ever called on a regexp capture group of \d+, so a parse
// failure is unreachable; zero is the honest value if that ever changes.
func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}
```

- [ ] **Step 5: Run to verify it passes**

Run: `go test ./internal/applier/ -race -v`
Expected: PASS, including both transcript fixtures.

If `TestParseTranscriptFixtures` fails on `deploy-transcript-kwok.txt`, the real markers differ from the plan's transcription. **The captured transcript wins** — fix the regexes, not the fixture.

- [ ] **Step 6: Qualify and commit**

```bash
make qualify
git add internal/applier/
git commit -S -m "feat(applier): deploy.sh marker parser

Maps the seven stable markers to typed events and deliberately drops
everything else: helm and kubectl output is most of the stream, and
publishing it would exhaust the bus replay ring and get live SSE
subscribers dropped for falling behind. Golden-tested against the real
KWOK dry-run transcript plus a hand-authored failure transcript that
covers the retry and failure markers a dry run never produces."
```

---

## Task 5: The executor, the diagnostic tail, and the template pin

**Files:**
- Create: `internal/applier/exec.go`
- Create: `internal/applier/applier.go`
- Create: `internal/applier/applier_test.go`
- Create: `internal/applier/template_test.go`

**Interfaces:**
- Consumes: `parseLine`, `ComponentData`, `FailureData` (Task 4).
- Produces:
  - `applier.Spec{ Dir string; Argv []string; Env []string }` — `Env` holds only the vars to ADD to the process environment.
  - `applier.Exec` interface — `Run(ctx context.Context, spec Spec, out io.Writer) error`
  - `applier.BashExec` — the production `Exec`.
  - `applier.New(e Exec) *Applier`
  - `applier.Options{ BundleDir string; Retries int; DryRun bool }`
  - `(*Applier).Apply(ctx context.Context, opts Options, emit func(bus.Event)) error` — consumed by Task 6.

- [ ] **Step 1: Write the failing executor tests**

Create `internal/applier/applier_test.go`:

```go
package applier

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/mchmarny/aicrme/internal/bus"
)

// fakeExec writes a canned transcript to out, then returns err -- the seam
// that lets every applier test run with no process and no cluster.
type fakeExec struct {
	transcript string
	err        error

	mu       sync.Mutex
	lastSpec Spec
	calls    int
}

func (f *fakeExec) Run(_ context.Context, spec Spec, out io.Writer) error {
	f.mu.Lock()
	f.lastSpec = spec
	f.calls++
	f.mu.Unlock()
	if _, err := io.WriteString(out, f.transcript); err != nil {
		return err
	}
	return f.err
}

func collect() (func(bus.Event), *[]bus.Event) {
	var mu sync.Mutex
	events := []bus.Event{}
	return func(e bus.Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e)
	}, &events
}

func TestApplyInvokesDeployScriptWithTheRightSpec(t *testing.T) {
	fake := &fakeExec{transcript: "✓ Pre-flight checks passed\n"}
	emit, _ := collect()

	err := New(fake).Apply(context.Background(), Options{BundleDir: "/bundle", Retries: 5}, emit)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	if fake.lastSpec.Dir != "/bundle" {
		t.Errorf("Dir = %q, want /bundle", fake.lastSpec.Dir)
	}
	wantArgv := []string{"bash", "deploy.sh", "--retries", "5"}
	if strings.Join(fake.lastSpec.Argv, " ") != strings.Join(wantArgv, " ") {
		t.Errorf("Argv = %v, want %v", fake.lastSpec.Argv, wantArgv)
	}
	env := strings.Join(fake.lastSpec.Env, "\n")
	for _, want := range []string{"NO_COLOR=1", "DRY_RUN_FLAG=", "KUBECONFIG_FLAG=", "HELM_DEBUG_FLAG="} {
		if !strings.Contains(env, want) {
			t.Errorf("Env missing %q, got %v", want, fake.lastSpec.Env)
		}
	}
	// --best-effort is deliberately absent: a half-installed platform that
	// reports success turns an applier failure into a confusing Validate
	// failure one phase later.
	if strings.Contains(strings.Join(fake.lastSpec.Argv, " "), "--best-effort") {
		t.Error("Argv contains --best-effort, want fail-fast")
	}
}

func TestApplyDryRunSetsTheFlag(t *testing.T) {
	fake := &fakeExec{}
	emit, _ := collect()

	if err := New(fake).Apply(context.Background(), Options{BundleDir: "/b", Retries: 0, DryRun: true}, emit); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if !strings.Contains(strings.Join(fake.lastSpec.Env, "\n"), "DRY_RUN_FLAG=--dry-run") {
		t.Errorf("Env = %v, want DRY_RUN_FLAG=--dry-run", fake.lastSpec.Env)
	}
}

func TestApplyPublishesMarkersAndNothingElse(t *testing.T) {
	fake := &fakeExec{transcript: strings.Join([]string{
		"✓ Pre-flight checks passed",
		"┌─ [1/2] cert-manager  →  cert-manager",
		`Release "cert-manager" does not exist. Installing it now.`,
		"NAME: cert-manager",
		"└─ ✓ cert-manager installed",
		"",
	}, "\n")}
	emit, events := collect()

	if err := New(fake).Apply(context.Background(), Options{BundleDir: "/b"}, emit); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if len(*events) != 3 {
		for _, e := range *events {
			t.Logf("event: %+v", e)
		}
		t.Fatalf("published %d events, want 3 -- helm output must not reach the bus", len(*events))
	}
}

func TestApplyPublishesFailureWithComponentAndTail(t *testing.T) {
	fake := &fakeExec{
		transcript: strings.Join([]string{
			"┌─ [2/3] kai-scheduler  →  kai-scheduler",
			"  --- Failed hook Job kai-scheduler-crd-upgrader diagnostics ---",
			"Error: INSTALLATION FAILED: timed out waiting for the condition",
			"└─ ✗ kai-scheduler FAILED (after 2 attempts)",
			"",
		}, "\n"),
		err: errors.New("exit status 1"),
	}
	emit, events := collect()

	err := New(fake).Apply(context.Background(), Options{BundleDir: "/b"}, emit)
	if err == nil {
		t.Fatal("Apply() error = nil, want the exec failure propagated")
	}

	last := (*events)[len(*events)-1]
	if last.Kind != bus.KindError || last.Level != bus.LevelError {
		t.Fatalf("last event = %+v, want a KindError at LevelError", last)
	}
	var d FailureData
	if uerr := json.Unmarshal(last.Data, &d); uerr != nil {
		t.Fatalf("unmarshal FailureData error = %v", uerr)
	}
	if d.Component != "kai-scheduler" {
		t.Errorf("Component = %q, want kai-scheduler", d.Component)
	}
	if !strings.Contains(strings.Join(d.Tail, "\n"), "Failed hook Job") {
		t.Errorf("Tail = %v, want deploy.sh's own diagnostics captured", d.Tail)
	}
}

// The tail is a bounded ring: a 20-minute real install emits far more
// output than any failure screen should carry.
func TestApplyTailIsBounded(t *testing.T) {
	var b strings.Builder
	for i := 0; i < tailLines*3; i++ {
		b.WriteString("noise line\n")
	}
	fake := &fakeExec{transcript: b.String(), err: errors.New("exit status 1")}
	emit, events := collect()

	_ = New(fake).Apply(context.Background(), Options{BundleDir: "/b"}, emit)

	var d FailureData
	if err := json.Unmarshal((*events)[len(*events)-1].Data, &d); err != nil {
		t.Fatalf("unmarshal error = %v", err)
	}
	if len(d.Tail) != tailLines {
		t.Errorf("len(Tail) = %d, want %d", len(d.Tail), tailLines)
	}
}

// A final line with no trailing newline must still parse -- deploy.sh's
// last line is not guaranteed to be newline-terminated when it dies.
func TestApplyFlushesAnUnterminatedFinalLine(t *testing.T) {
	fake := &fakeExec{transcript: "└─ ✓ cert-manager installed"}
	emit, events := collect()

	if err := New(fake).Apply(context.Background(), Options{BundleDir: "/b"}, emit); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if len(*events) != 1 {
		t.Fatalf("published %d events, want 1", len(*events))
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/applier/ -race -run TestApply -v`
Expected: FAIL — `undefined: New`, `undefined: Spec`.

- [ ] **Step 3: Implement the exec seam, line writer, and ring**

Create `internal/applier/exec.go`:

```go
package applier

import (
	"context"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// maxLineBytes caps how much is buffered for a single unterminated line, so
// a binary blob on stdout cannot grow the buffer without bound. Anything
// past the cap is flushed as its own line.
const maxLineBytes = 64 << 10

// killGrace is how long a canceled deploy.sh has to run its own INT/TERM
// trap (which removes the helm temp workdir) before the process is killed.
const killGrace = 10 * time.Second

// Spec is one process invocation. Env carries only the variables to ADD to
// the inherited environment, which keeps the golden assertions in
// applier_test.go readable -- BashExec appends them to os.Environ(), and
// os/exec resolves duplicate keys in favor of the last occurrence.
type Spec struct {
	Dir  string
	Argv []string
	Env  []string
}

// Exec runs one process, streaming its merged stdout and stderr to out.
// The single seam between the applier and the operating system: tests
// substitute a fake that writes a captured transcript.
type Exec interface {
	Run(ctx context.Context, spec Spec, out io.Writer) error
}

// BashExec runs the real process.
type BashExec struct{}

// Run streams merged stdout and stderr to out. On context cancellation it
// sends SIGTERM rather than SIGKILL so deploy.sh's own trap can remove the
// temp workdir it created, and only escalates after killGrace.
//
// Because Stdout and Stderr are the same non-*os.File writer, os/exec
// copies them on two separate goroutines, so out MUST be safe for
// concurrent use. lineWriter is.
func (BashExec) Run(ctx context.Context, spec Spec, out io.Writer) error {
	cmd := exec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = append(os.Environ(), spec.Env...)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = killGrace
	return cmd.Run()
}

// lineWriter splits everything written to it into lines and invokes fn once
// per complete line. fn is called while holding the mutex, which serializes
// it against the concurrent stdout and stderr copies and lets callers keep
// unsynchronized state inside fn.
type lineWriter struct {
	mu  sync.Mutex
	buf []byte
	fn  func(string)
}

func newLineWriter(fn func(string)) *lineWriter {
	return &lineWriter{fn: fn}
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	for _, b := range p {
		if b == '\n' {
			w.emitLocked()
			continue
		}
		if b == '\r' {
			continue
		}
		w.buf = append(w.buf, b)
		if len(w.buf) >= maxLineBytes {
			w.emitLocked()
		}
	}
	return len(p), nil
}

// Flush emits any trailing line that was never newline-terminated.
func (w *lineWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.emitLocked()
}

func (w *lineWriter) emitLocked() {
	if len(w.buf) == 0 {
		return
	}
	w.fn(string(w.buf))
	w.buf = w.buf[:0]
}

// ring retains the last n strings in O(1) per add.
type ring struct {
	items []string
	head  int
	count int
}

func newRing(n int) *ring { return &ring{items: make([]string, n)} }

func (r *ring) add(s string) {
	if r.count < len(r.items) {
		r.items[(r.head+r.count)%len(r.items)] = s
		r.count++
		return
	}
	r.items[r.head] = s
	r.head = (r.head + 1) % len(r.items)
}

func (r *ring) lines() []string {
	out := make([]string, 0, r.count)
	for i := 0; i < r.count; i++ {
		out = append(out, r.items[(r.head+i)%len(r.items)])
	}
	return out
}
```

- [ ] **Step 4: Implement `Apply`**

Create `internal/applier/applier.go`:

```go
package applier

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/mchmarny/aicrme/internal/bus"
)

// tailLines bounds the raw-output ring attached to a failure event. Large
// enough to hold deploy.sh's own hook-Job and kai-scheduler diagnostic
// dumps (each up to ~50 lines of describe output) plus the helm error that
// preceded them; small enough that the failure event stays a readable
// payload rather than a log file.
const tailLines = 200

// Options configures one Apply.
type Options struct {
	// BundleDir is the generated bundle root -- the directory holding
	// deploy.sh, written by steps.Bundle.
	BundleDir string
	// Retries is deploy.sh's per-component retry budget. Its own backoff is
	// quadratic (5s, 20s, 45s, 80s, 120s cap), and each attempt surfaces as
	// a warn event so the wait is visible rather than silent.
	Retries int
	// DryRun renders every component through helm without installing
	// anything. Used by the CI end-to-end test, which exercises the real
	// script and the real helm binary on a cluster with no GPUs.
	DryRun bool
}

// Applier runs one bundle's deploy.sh.
type Applier struct {
	exec Exec
}

// New returns an Applier over the given process seam.
func New(e Exec) *Applier { return &Applier{exec: e} }

// Apply runs deploy.sh to completion, publishing one event per recognized
// marker and, on failure, a single KindError carrying the failing component
// and a bounded tail of raw output.
//
// deploy.sh runs WITHOUT --best-effort. The first component to exhaust its
// retries ends the run. Continuing past a failure would finish on a cluster
// that looks installed and is not, and would convert a clear applier
// failure into a confusing Validate or Prove failure one phase later. The
// recovery path is engine.Retry re-running this whole step, which is safe
// because every install.sh is `helm upgrade --install` and deploy.sh's
// preflight and hook-Job cleanup run again.
func (a *Applier) Apply(ctx context.Context, opts Options, emit func(bus.Event)) error {
	tail := newRing(tailLines)

	// Written and read only from inside the lineWriter callback, which
	// lineWriter invokes under its own mutex, and read again after
	// exec.Run returns -- which happens after os/exec has joined both copy
	// goroutines, so that read is ordered after every write.
	var lastComponent string

	out := newLineWriter(func(line string) {
		tail.add(line)
		// Every line reaches the pod log even though most never reach the
		// bus, so `kubectl logs` retains the complete transcript.
		slog.Debug("deploy.sh", "line", line)

		ev, ok := parseLine(line)
		if !ok {
			return
		}
		if ev.Component != "" {
			lastComponent = ev.Component
		}
		emit(ev)
	})

	spec := Spec{
		Dir:  opts.BundleDir,
		Argv: []string{"bash", "deploy.sh", "--retries", strconv.Itoa(opts.Retries)},
		Env:  a.env(opts),
	}

	err := a.exec.Run(ctx, spec, out)
	out.Flush()
	if err == nil {
		return nil
	}

	// FailureData holds only strings, so Marshal cannot fail.
	data, _ := json.Marshal(FailureData{
		Component: lastComponent,
		ExitError: err.Error(),
		Tail:      tail.lines(),
	})
	emit(bus.Event{
		Kind:      bus.KindError,
		Level:     bus.LevelError,
		Component: lastComponent,
		Message:   "deploy.sh failed: " + err.Error(),
		Data:      data,
	})
	return aicrerrors.Wrap(aicrerrors.ErrCodeUnavailable, "bundle apply failed", err)
}

// env builds the variables deploy.sh and each install.sh read. The three
// *_FLAG variables are exported unconditionally, empty by default: the
// script's own `${DRY_RUN_FLAG:-}` expansions tolerate unset, but setting
// them explicitly means an operator's stray environment cannot leak a
// --dry-run (or a --debug) into a real customer install.
func (a *Applier) env(opts Options) []string {
	dryRun := ""
	if opts.DryRun {
		dryRun = "--dry-run"
	}
	return []string{
		"NO_COLOR=1",
		"DRY_RUN_FLAG=" + dryRun,
		"KUBECONFIG_FLAG=",
		"HELM_DEBUG_FLAG=",
	}
}
```

- [ ] **Step 5: Run to verify it passes**

Run: `go test ./internal/applier/ -race -v`
Expected: PASS.

- [ ] **Step 6: Write the template-pin test**

Create `internal/applier/template_test.go`:

```go
package applier

import (
	"crypto/sha256"
	"encoding/hex"
	"go/build"
	"os"
	"path/filepath"
	"testing"
)

// deployTemplateSHA256 pins pkg/bundler/deployer/helm/templates/deploy.sh.tmpl
// from the aicr module go.mod pins. The parser in parse.go transcribes that
// file's printf formats; nothing in the Go type system connects the two, so
// an upstream edit would otherwise silently empty the Apply timeline
// instead of failing. When this test fails on an aicr bump, re-read the
// template's output helpers against parse.go's regexes, update both this
// constant and the regexes, and re-capture
// testdata/deploy-transcript-kwok.txt.
const deployTemplateSHA256 = "df919af7e46d565d38fbf12927881ebeec1172227efac8962e4c00f035a8b519"

const deployTemplateRelPath = "pkg/bundler/deployer/helm/templates/deploy.sh.tmpl"

func TestDeployTemplateUnchanged(t *testing.T) {
	path := aicrModuleFile(t, deployTemplateRelPath)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s error = %v", path, err)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != deployTemplateSHA256 {
		t.Fatalf("deploy.sh.tmpl sha256 = %s, want %s\n"+
			"The template the marker parser transcribes has changed. Re-read its\n"+
			"output helpers against internal/applier/parse.go's regexes before\n"+
			"updating this constant.", got, deployTemplateSHA256)
	}
}

// aicrModuleFile resolves a path inside the pinned aicr module in the
// module cache. `go list -m` would be the obvious route but needs network
// access on a cold cache; build.Import resolves from GOMODCACHE alone.
func aicrModuleFile(t *testing.T, rel string) string {
	t.Helper()
	pkg, err := build.Default.Import("github.com/NVIDIA/aicr/pkg/errors", ".", build.FindOnly)
	if err != nil {
		t.Fatalf("locate aicr module error = %v", err)
	}
	root := filepath.Dir(filepath.Dir(pkg.Dir)) // .../aicr@vX.Y.Z
	return filepath.Join(root, filepath.FromSlash(rel))
}
```

- [ ] **Step 7: Run the pin test**

Run: `go test ./internal/applier/ -race -run TestDeployTemplateUnchanged -v`
Expected: PASS.

**Bite-proof it.** Change one character of `deployTemplateSHA256`, re-run, confirm FAIL with the guidance message, then restore. A pin that cannot fail is not a pin.

- [ ] **Step 8: Qualify and commit**

```bash
make qualify
git add internal/applier/
git commit -S -m "feat(applier): deploy.sh executor, bounded diagnostic tail, template pin

Runs without --best-effort: the first component to exhaust its retries
ends the run, because finishing on a cluster that looks installed and is
not turns an applier failure into a confusing Validate failure a phase
later. SIGTERM on cancel, not SIGKILL, so deploy.sh's own trap can
remove its helm temp workdir. lineWriter is mutex-guarded because
os/exec copies stdout and stderr on separate goroutines when they share
a non-*os.File writer.

TestDeployTemplateUnchanged pins the template's sha256 so an upstream
edit fails CI rather than silently emptying the Apply timeline."
```

---

## Task 6: Apply step and the confirm gate

**Files:**
- Create: `internal/steps/apply.go`
- Create: `internal/steps/apply_test.go`
- Modify: `cmd/aicrme/main.go`

**Interfaces:**
- Consumes: `applier.New`, `applier.Options`, `applier.BashExec`, `engine.Run.Artifacts["bundle.path"]`.
- Produces:
  - `steps.ApplyConfig{ Retries int; DryRun bool }`
  - `steps.NewApply(a *applier.Applier, cfg ApplyConfig) engine.Step` with `Requires() == []string{"apply"}`.

- [ ] **Step 1: Write the failing tests**

Create `internal/steps/apply_test.go`:

```go
package steps_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/mchmarny/aicrme/internal/applier"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/steps"
)

type recordingExec struct {
	transcript string
	err        error
	lastSpec   applier.Spec
}

func (r *recordingExec) Run(_ context.Context, spec applier.Spec, out io.Writer) error {
	r.lastSpec = spec
	_, _ = io.WriteString(out, r.transcript)
	return r.err
}

// The confirm gate: the console installs sixteen charts with cluster-admin,
// so the run must park for an explicit click first.
func TestApplyGatesOnTheApplyDecision(t *testing.T) {
	step := steps.NewApply(applier.New(&recordingExec{}), steps.ApplyConfig{})
	got := step.Requires()
	if len(got) != 1 || got[0] != "apply" {
		t.Fatalf("Requires() = %v, want [apply]", got)
	}
}

func TestApplyRunsDeployScriptFromTheBundleDir(t *testing.T) {
	exec := &recordingExec{transcript: "✓ Pre-flight checks passed\n"}
	step := steps.NewApply(applier.New(exec), steps.ApplyConfig{Retries: 5})

	run := newRun()
	run.Decisions["apply"] = "yes"
	run.Artifacts["bundle.path"] = []byte("/var/lib/aicrme/runs/abc/bundle")

	if err := step.Run(context.Background(), run, func(bus.Event) {}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if exec.lastSpec.Dir != "/var/lib/aicrme/runs/abc/bundle" {
		t.Errorf("Dir = %q, want the bundle path artifact", exec.lastSpec.Dir)
	}
}

func TestApplyRequiresBundleToHaveRun(t *testing.T) {
	step := steps.NewApply(applier.New(&recordingExec{}), steps.ApplyConfig{})

	run := newRun()
	run.Decisions["apply"] = "yes"
	// bundle.path deliberately absent.

	err := step.Run(context.Background(), run, func(bus.Event) {})
	if err == nil || !strings.Contains(err.Error(), "bundle.path") {
		t.Fatalf("Run() error = %v, want it to name the missing artifact", err)
	}
}

func TestApplyPropagatesDeployFailure(t *testing.T) {
	exec := &recordingExec{
		transcript: "└─ ✗ kai-scheduler FAILED (after 2 attempts)\n",
		err:        errors.New("exit status 1"),
	}
	step := steps.NewApply(applier.New(exec), steps.ApplyConfig{})

	run := newRun()
	run.Decisions["apply"] = "yes"
	run.Artifacts["bundle.path"] = []byte("/b")

	if err := step.Run(context.Background(), run, func(bus.Event) {}); err == nil {
		t.Fatal("Run() error = nil, want the deploy failure propagated so the engine fails the run")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/steps/ -race -run TestApply -v`
Expected: FAIL — `undefined: steps.NewApply`.

- [ ] **Step 3: Implement the Apply step**

Create `internal/steps/apply.go`:

```go
package steps

import (
	"context"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/mchmarny/aicrme/internal/applier"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
)

// ApplyConfig configures the deploy.sh invocation.
type ApplyConfig struct {
	Retries int
	DryRun  bool
}

type apply struct {
	applier *applier.Applier
	cfg     ApplyConfig
}

// NewApply returns the Apply step.
//
// It requires one decision, "apply", which is the console's confirm gate.
// The console installs the whole recipe with cluster-admin, so it must not
// begin mutating a cluster without an explicit click. This needs no new
// engine machinery: engine.awaitDecisions already parks the run in
// StateAwaitingDecision before a step whose Requires() is unsatisfied, and
// POST /api/runs/{id}/decide already supplies it. It does not break the
// spec's "exactly two decisions" promise -- intent and platform are
// choices, this is a confirmation -- and it is where the Review-and-verify
// screen lands when Phase 5 builds it.
func NewApply(a *applier.Applier, cfg ApplyConfig) engine.Step {
	return &apply{applier: a, cfg: cfg}
}

func (a *apply) Phase() engine.Phase { return engine.PhaseApply }
func (a *apply) Requires() []string  { return []string{"apply"} }

func (a *apply) Run(ctx context.Context, run *engine.Run, emit engine.Emit) error {
	dir := string(run.Artifacts["bundle.path"])
	if dir == "" {
		return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			"bundle.path artifact is missing -- Bundle must run before Apply")
	}

	emit(bus.Event{Kind: bus.KindLog, Message: "applying the bundle"})

	return a.applier.Apply(ctx, applier.Options{
		BundleDir: dir,
		Retries:   a.cfg.Retries,
		DryRun:    a.cfg.DryRun,
	}, func(e bus.Event) { emit(e) })
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/steps/ -race`
Expected: PASS.

- [ ] **Step 5: Wire Apply into the pipeline**

In `cmd/aicrme/main.go`, add a constant near `defaultWorkDir`:

```go
// defaultApplyRetries is deploy.sh's per-component retry budget, matching
// the script's own default. Its backoff is quadratic and each attempt
// surfaces as a warn event, so the wait is visible rather than silent.
const defaultApplyRetries = 5
```

and after `steps.NewBundle(...)` in the `engine.New(...)` call:

```go
		steps.NewApply(applier.New(applier.BashExec{}), steps.ApplyConfig{
			Retries: defaultApplyRetries,
			// Not exposed in values.yaml, deliberately -- same treatment as
			// AICRME_SNAPSHOT_NODE_SELECTOR. It exists so the CI end-to-end
			// test can exercise the real deploy.sh and the real helm binary
			// against a cluster with no GPUs without installing anything.
			DryRun: os.Getenv("AICRME_APPLY_DRY_RUN") == "true",
		}),
```

Add `"github.com/mchmarny/aicrme/internal/applier"` to the imports.

- [ ] **Step 6: Qualify and commit**

```bash
make qualify
git add internal/steps/ cmd/aicrme/main.go
git commit -S -m "feat(steps): Apply step behind a one-key confirm gate

Requires(\"apply\") reuses the engine's existing decision parking, so the
console shows the bundle and waits for a click before installing the
whole recipe with cluster-admin. AICRME_APPLY_DRY_RUN is a test-only
knob, kept out of values.yaml on purpose."
```

---

## Task 7: Engine step cursor, epoch guard, and Retry

**Files:**
- Modify: `internal/engine/run.go`
- Modify: `internal/engine/engine.go`
- Modify: `internal/engine/engine_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces:
  - `engine.Run.StepIndex int` (`json:"stepIndex"`) — index of the next step to run.
  - `(*Engine).Retry(runID string) (*Run, error)` — consumed by Task 8.

- [ ] **Step 1: Write the failing tests**

Add to `internal/engine/engine_test.go`:

```go
// A step that fails once and then succeeds -- the exact shape Retry exists
// for, since a mid-apply component failure is normal on real clusters.
type flakyStep struct {
	phase engine.Phase
	fails int
	runs  int
	mu    sync.Mutex
}

func (f *flakyStep) Phase() engine.Phase { return f.phase }
func (f *flakyStep) Requires() []string  { return nil }
func (f *flakyStep) Run(_ context.Context, _ *engine.Run, _ engine.Emit) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs++
	if f.runs <= f.fails {
		return errors.New("boom")
	}
	return nil
}

func (f *flakyStep) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs
}

func TestRetryResumesFromTheFailedStep(t *testing.T) {
	b := bus.New(64)
	first := newFakeStep(engine.PhaseDiscover)
	flaky := &flakyStep{phase: engine.PhaseApply, fails: 1}
	e := engine.New(b, engine.NewMemoryStore(), first, flaky)

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitState(t, e, run.ID, engine.StateFailed)

	if _, err := e.Retry(run.ID); err != nil {
		t.Fatalf("Retry() error = %v", err)
	}
	done := waitState(t, e, run.ID, engine.StateDone)

	if done.Err != "" {
		t.Errorf("Err = %q, want cleared on a successful retry", done.Err)
	}
	// The first step must NOT re-run: the cursor resumes at the step that
	// failed, so Discover's snapshot Job is not redeployed.
	if got := len(first.ran); got != 1 {
		t.Errorf("first step ran %d times, want 1", got)
	}
	if got := flaky.count(); got != 2 {
		t.Errorf("failed step ran %d times, want 2", got)
	}
}

func TestRetryRejectsARunThatIsNotFailed(t *testing.T) {
	b := bus.New(64)
	e := engine.New(b, engine.NewMemoryStore(), newFakeStep(engine.PhaseDiscover))

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitState(t, e, run.ID, engine.StateDone)

	if _, err := e.Retry(run.ID); err == nil {
		t.Error("Retry() error = nil, want a conflict on a completed run")
	}
}

func TestRetryRejectsAnUnknownRun(t *testing.T) {
	e := engine.New(bus.New(8), engine.NewMemoryStore(), newFakeStep(engine.PhaseDiscover))
	if _, err := e.Retry("nope"); err == nil {
		t.Error("Retry() error = nil, want not-found")
	}
}

// The epoch guard. Retry is the path that makes a second execute goroutine
// reachable for the SAME run, which is exactly what Start's isLive check
// cannot see: isLive answers "is a run live", not "is THIS goroutine still
// the one driving it". A retried run must therefore reach exactly one
// terminal state and publish exactly one terminal event -- a superseded
// goroutine writing state again would produce two.
func TestRetriedRunReachesExactlyOneTerminalState(t *testing.T) {
	b := bus.New(256)
	sub, unsubscribe := b.Subscribe(0)
	defer unsubscribe()

	flaky := &flakyStep{phase: engine.PhaseApply, fails: 1}
	e := engine.New(b, engine.NewMemoryStore(), flaky)

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitState(t, e, run.ID, engine.StateFailed)

	if _, err := e.Retry(run.ID); err != nil {
		t.Fatalf("Retry() error = %v", err)
	}
	done := waitState(t, e, run.ID, engine.StateDone)

	if done.StepIndex != 1 {
		t.Errorf("StepIndex = %d, want 1 (all steps consumed)", done.StepIndex)
	}

	// Drain what the bus has so far and count terminal events. A superseded
	// goroutine calling finish() again is the failure this catches.
	deadline := time.After(500 * time.Millisecond)
	terminal := 0
drain:
	for {
		select {
		case ev := <-sub:
			if ev.Message == "run done" {
				terminal++
			}
		case <-deadline:
			break drain
		}
	}
	if terminal != 1 {
		t.Errorf("published %d 'run done' events, want exactly 1 -- a superseded goroutine wrote state", terminal)
	}
}
```

Add `"sync"` to the test file's imports.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/engine/ -race -v`
Expected: FAIL — `undefined: e.Retry`.

- [ ] **Step 3: Add the step cursor to `Run`**

In `internal/engine/run.go`, add to the `Run` struct after `Pending`:

```go
	// StepIndex is the index of the next step to execute. It exists so a
	// failed run can be retried from the step that failed rather than from
	// the top: re-running Discover would redeploy the snapshot agent Job
	// and take minutes, and re-running Recommend would discard the
	// decisions the user already made. It advances only after a step
	// succeeds, so a failure leaves it pointing at the step to retry.
	StepIndex int `json:"stepIndex"`
```

`Clone` copies the struct by value, so it needs no change — but confirm that by reading it.

- [ ] **Step 4: Add the epoch and rewrite `execute`**

In `internal/engine/engine.go`:

Add to the `Engine` struct:

```go
	// epoch increments on every Start and Retry. Each execute goroutine
	// captures the value current when it launched and re-checks it before
	// every state write. Start's isLive check alone cannot cover this:
	// Retry deliberately relaunches execute for a run that already had a
	// goroutine, so "is a run live" and "is THIS goroutine still the one
	// driving it" are different questions.
	epoch uint64
```

Add the guard:

```go
// alive reports whether the goroutine holding this epoch is still the one
// driving the current run. Callers must NOT hold e.mu.
func (e *Engine) alive(epoch uint64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.epoch == epoch
}

// aliveLocked is alive for callers already holding e.mu.
func (e *Engine) aliveLocked(epoch uint64) bool { return e.epoch == epoch }
```

In `Start`, replace the run construction's tail so it stamps a new epoch and passes it to `execute`. Inside the existing `e.mu.Lock()` block, after `e.resume = make(chan struct{}, 1)`:

```go
	e.epoch++
	epoch := e.epoch
```

and change the launch to `go e.execute(context.WithoutCancel(ctx), epoch)`.

Rewrite `execute`:

```go
func (e *Engine) execute(ctx context.Context, epoch uint64) {
	for {
		e.mu.Lock()
		if !e.aliveLocked(epoch) {
			e.mu.Unlock()
			return
		}
		i := e.current.StepIndex
		e.mu.Unlock()

		if i >= len(e.steps) {
			break
		}
		step := e.steps[i]

		if !e.awaitDecisions(ctx, epoch, step) {
			return
		}
		if err := e.runStep(ctx, epoch, step); err != nil {
			return
		}

		e.mu.Lock()
		if !e.aliveLocked(epoch) {
			e.mu.Unlock()
			return
		}
		e.current.StepIndex = i + 1
		e.mu.Unlock()
	}
	e.finish(ctx, epoch, StateDone, "")
}
```

Thread `epoch` through `awaitDecisions`, `runStep`, and `finish`. In each, immediately after taking `e.mu`, return early (`false`, `nil`, or a bare `return`) if `!e.aliveLocked(epoch)`. In `runStep`, place the check both at the top and again in the merge-back block after `step.Run` returns — a step can run for twenty minutes, which is precisely the window in which it can be superseded.

- [ ] **Step 5: Implement `Retry`**

Add to `internal/engine/engine.go`:

```go
// Retry re-executes a failed run from the step that failed. Valid only from
// StateFailed.
//
// Safe to re-run the whole Apply step: every component's install.sh is
// `helm upgrade --install`, which is idempotent, and deploy.sh's own
// preflight and stale-hook-Job cleanup run again on the retry. Components
// that already installed are no-ops on the second pass.
func (e *Engine) Retry(runID string) (*Run, error) {
	e.mu.Lock()
	if e.current == nil || e.current.ID != runID {
		e.mu.Unlock()
		return nil, aicrerrors.New(aicrerrors.ErrCodeNotFound, "run not found: "+runID)
	}
	if e.current.State != StateFailed {
		e.mu.Unlock()
		return nil, aicrerrors.New(aicrerrors.ErrCodeConflict, "run is not in a failed state")
	}
	e.current.State = StateRunning
	e.current.Err = ""
	e.current.UpdatedAt = time.Now().UTC()
	e.resume = make(chan struct{}, 1)
	e.epoch++
	epoch := e.epoch
	snapshot := e.current.Clone()
	e.mu.Unlock()

	if err := e.store.Save(context.Background(), snapshot); err != nil {
		return nil, err
	}
	e.bus.Publish(bus.Event{
		RunID: runID, Kind: bus.KindPhase, Message: "run retrying",
	})
	go e.execute(context.Background(), epoch)
	return snapshot, nil
}
```

- [ ] **Step 6: Run to verify it passes**

Run: `go test ./internal/engine/ -race -v`
Expected: PASS.

- [ ] **Step 7: Bite-proof the epoch guard**

Delete the `aliveLocked` check from `runStep`'s merge-back block and re-run `go test ./internal/engine/ -race -count=5`. Confirm the superseded-run test fails or `-race` reports a problem, then restore the check. A guard no test can break is not a guard.

- [ ] **Step 8: Qualify and commit**

```bash
make qualify
git add internal/engine/
git commit -S -m "feat(engine): step cursor, epoch guard, and Retry

Retry resumes from the step that failed, so a mid-apply failure does not
redeploy the snapshot Job or discard the user's decisions. Safe to
re-run the whole Apply step because every install.sh is
'helm upgrade --install' and deploy.sh's preflight and hook-Job cleanup
run again.

The epoch guard is the other half: Retry deliberately relaunches execute
for a run that already had a goroutine, so Start's isLive check --
previously the only protection, flagged as latent in the Phase 2 handoff
-- stops being sufficient. Every state write now re-checks that this
goroutine is still the one driving the current run."
```

---

## Task 8: API — retry, bundle download, and the context latch

**Files:**
- Modify: `internal/api/server.go`
- Modify: `internal/api/runs.go`
- Modify: `internal/api/auth.go`
- Create: `internal/api/bundle.go`
- Create: `internal/api/bundle_test.go`
- Modify: `internal/api/runs_test.go`
- Modify: `cmd/aicrme/main.go`

**Interfaces:**
- Consumes: `engine.Retry` (Task 7), `Run.Artifacts["bundle.path"]` (Task 2).
- Produces:
  - `POST /api/runs/{id}/retry` → 200 with the run JSON; 404 unknown; 409 not failed.
  - `GET /api/runs/{id}/bundle` → `application/gzip` tarball.
  - `GET /api/session` → 204 when authenticated, 401 otherwise. Consumed by Task 10.
  - `api.Config.WorkDir string`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/api/runs_test.go` (matching the file's existing `httptest` helper style — read it first):

- `TestRetryReturnsTheRun` — a failed run retried returns 200.
- `TestRetryOnRunningRunConflicts` — 409.
- `TestRetryOnUnknownRunNotFound` — 404.
- `TestSessionProbeReturns204WhenAuthed` and `TestSessionProbeReturns401WhenNot`.

Create `internal/api/bundle_test.go`:

```go
package api_test

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A bundle.path artifact pointing outside the configured work dir must be
// refused. Nothing writes such a path today, but this handler turns an
// artifact value into a filesystem read, and "no caller does that yet" is
// not a boundary.
func TestBundleDownloadRefusesAPathOutsideWorkDir(t *testing.T) {
	// ... construct a server whose current run carries
	// Artifacts["bundle.path"] = "/etc", assert 400 and an empty body.
}

func TestBundleDownloadStreamsATarball(t *testing.T) {
	// ... write <workDir>/runs/<id>/bundle/deploy.sh, request the route,
	// gunzip + untar the response, assert deploy.sh is present with its
	// contents and its 0755 mode.
}

func TestBundleDownloadIsNotFoundBeforeBundleRuns(t *testing.T) {
	// ... a run with no bundle.path artifact yields 404.
}
```

Fill these in against the helper functions `internal/api`'s existing tests already provide; do not invent a parallel harness.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/api/ -race -v`
Expected: FAIL.

- [ ] **Step 3: Add `WorkDir` to `Config` and the routes**

In `internal/api/server.go`:

- add `WorkDir string` to `Config`, with a doc comment saying it is the containment boundary for `GET /api/runs/{id}/bundle`;
- reject an empty `WorkDir` in `New` the same way an empty password is rejected, and store it on `Server`;
- register in the `protected` mux:

```go
	protected.HandleFunc("GET /api/session", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	protected.HandleFunc("POST /api/runs/{id}/retry", s.handleRetry)
	protected.HandleFunc("GET /api/runs/{id}/bundle", s.handleBundle)
```

`GET /api/session` exists so the SPA can tell an expired session from a network blip: `EventSource` surfaces no HTTP status on error, so the console has no other way to learn its 8-hour session expired, and previously stuck on "reconnecting…" forever.

- [ ] **Step 4: Implement `handleRetry`**

In `internal/api/runs.go`:

```go
func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	run, err := s.engine.Retry(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}
```

- [ ] **Step 5: Implement `handleBundle`**

Create `internal/api/bundle.go` with a handler that:

1. `s.engine.Get(r.PathValue("id"))`, propagating its error through `writeErr`;
2. reads `run.Artifacts["bundle.path"]`; empty → `ErrCodeNotFound` ("bundle not generated yet");
3. resolves the path with `filepath.Clean` and requires it to be within `s.workDir` — compare against `filepath.Clean(s.workDir)+string(os.PathSeparator)` as a prefix, and reject with `ErrCodeInvalidRequest` otherwise;
4. sets `Content-Type: application/gzip` and `Content-Disposition: attachment; filename="aicrme-bundle-<runID>.tar.gz"`;
5. streams a `tar.gz` built with `archive/tar` over `filepath.WalkDir`, preserving each file's mode so `deploy.sh` stays executable, and using paths relative to the bundle directory;
6. logs a walk error via `slog` rather than trying to change the status code — headers are already sent by then.

- [ ] **Step 6: Close the 409 latch**

In `internal/api/runs.go`, change `handleCreateRun`:

```go
func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	// context.WithoutCancel: the run outlives this request by design --
	// Apply alone takes 10-20 minutes on real hardware. Handing the engine
	// the cancellable request context means the browser closing the tab
	// (or a proxy timing out) cancels the run mid-install, and a
	// store.Save failure under a canceled context would leave e.current
	// live and permanently 409 every new run.
	run, err := s.engine.Start(context.WithoutCancel(r.Context()))
	...
}
```

Add `"context"` to the imports.

- [ ] **Step 7: Run to verify they pass**

Run: `go test ./internal/api/ -race -v`
Expected: PASS.

- [ ] **Step 8: Pass `WorkDir` from main**

In `cmd/aicrme/main.go`'s `api.Config{...}`, add `WorkDir: workDir,`.

- [ ] **Step 9: Qualify and commit**

```bash
make qualify
git add internal/api/ cmd/aicrme/main.go
git commit -S -m "feat(api): retry, bundle download, session probe, and the run-context latch

GET /api/runs/{id}/bundle makes the confirm gate honest -- the user can
inspect exactly what they are about to approve -- and is contained to the
configured work dir rather than trusting an artifact value.

GET /api/session exists because EventSource surfaces no HTTP status on
error, so the console had no way to tell an expired 8-hour session from a
network blip and stuck on 'reconnecting...' forever.

handleCreateRun now starts the run under context.WithoutCancel: Apply
takes 10-20 minutes, so a closed tab would otherwise cancel a live
install, and a store.Save under a canceled context would latch e.current
and permanently 409 new runs."
```

---

## Task 9: The cockpit

**Files:**
- Create: `web/src/pipeline.ts`
- Create: `web/src/pipeline.test.ts`
- Create: `web/src/slowSteps.ts`
- Create: `web/src/components/Cockpit.tsx`
- Create: `web/src/components/Cockpit.test.tsx`
- Create: `web/src/fixtures/apply-run.json`
- Modify: `web/src/api.ts`
- Modify: `web/src/components/Wizard.tsx`

**Interfaces:**
- Consumes: `AicrEvent` (`web/src/useEvents.ts`), the `ComponentData` / `FailureData` JSON shapes from Task 4, the routes from Task 8.
- Produces:
  - `deriveComponents(events: AicrEvent[]): ComponentState[]`
  - `deriveFailure(events: AicrEvent[]): FailureInfo | null`
  - `retryRun(runId: string): Promise<Run>` and `bundleUrl(runId: string): string` in `api.ts`
  - `<Cockpit events run onDecide onRetry />`

- [ ] **Step 1: Record an apply-phase fixture**

Create `web/src/fixtures/apply-run.json` — an `AicrEvent[]` covering the whole Apply arc, hand-authored to match exactly what Task 4's parser emits: phase events for `bundle` and `apply`, a `decision` event for the gate, `component` events with `data` matching `ComponentData` (started → installed for two components, then started → retrying → failed for a third), and a terminal `error` event whose `data` matches `FailureData`.

Mirror the structure of the existing `web/src/fixtures/kwok-run.json`; read it first.

- [ ] **Step 2: Write the failing pipeline tests**

Create `web/src/pipeline.test.ts` asserting, against the fixture:

- `deriveComponents` returns one entry per component in first-seen order;
- a component seen `started` then `installed` ends at status `installed`;
- a component seen `started` → `retrying` → `failed` ends at `failed` and retains its attempt counts;
- `index`/`total` are carried from the header event;
- events from an older run id are excluded (same rule `deriveRunState` already applies in `Wizard.tsx`);
- `deriveFailure` returns the component, exit error, and tail from the terminal error event, and `null` when there is none.

- [ ] **Step 3: Run to verify they fail**

Run: `cd web && npm test -- pipeline`
Expected: FAIL — module not found.

- [ ] **Step 4: Implement `pipeline.ts`**

```ts
import type { AicrEvent } from './useEvents'

/**
 * ComponentData mirrors Go's applier.ComponentData (internal/applier/parse.go)
 * field for field. It is the Data payload on every KindComponent event.
 */
export interface ComponentData {
  name: string
  namespace?: string
  index?: number
  total?: number
  status: 'started' | 'installed' | 'failed' | 'retrying'
  attempt?: number
  maxAttempts?: number
  retryInSeconds?: number
}

/** FailureInfo mirrors Go's applier.FailureData. */
export interface FailureInfo {
  component?: string
  exitError: string
  tail: string[]
}

export interface ComponentState extends ComponentData {
  /** Wall-clock of the header event, so the UI can show elapsed time. */
  startedAt?: string
  /** Wall-clock of the terminal event for this component. */
  endedAt?: string
}
```

Implement `deriveComponents` as a single pass building a `Map<string, ComponentState>` keyed by name, preserving insertion order, filtered to the current run id (reuse the same last-event-wins rule `Wizard.tsx` documents), and `deriveFailure` as a reverse scan for the last `kind === 'error'` event carrying a `data` object with an `exitError` field.

Include a comment explaining that `total` is only present on the header event, so the pipeline's "N of M" comes from the most recent header seen rather than from `components.length` — a component that never started has no header and would otherwise undercount the total.

- [ ] **Step 5: Run to verify they pass**

Run: `cd web && npm test -- pipeline`
Expected: PASS.

- [ ] **Step 6: Add the slow-step map**

Create `web/src/slowSteps.ts`:

```ts
/**
 * Contextual slow-step explanations, surfaced BEFORE a known multi-minute
 * stall rather than after it. This is not decoration: every GPU cluster
 * install stalls somewhere, and an unexplained stall is precisely where a
 * demo audience concludes the tool is broken. Naming it before it happens
 * converts the worst moment into a credibility moment.
 *
 * Deliberately unquantified. Real per-node timings are calibrated in Phase
 * 4 against real hardware; inventing minute counts here would put a
 * fabricated number on the screen during a KWOK demo, which is worse than
 * saying less.
 */
const EXACT: Record<string, string> = {
  'gpu-operator':
    'The driver DaemonSet compiles the NVIDIA kernel module against each node’s running kernel, then loads it. This is the longest step of the install and it is supposed to look stalled.',
  'kai-scheduler':
    'Installed without --wait: its custom resources reconcile asynchronously, so Helm returning does not mean the scheduler is ready yet.',
}

const READINESS_SUFFIX = '-readiness'
const READINESS_NOTE =
  'A readiness gate. It polls the components installed before it until they actually pass, on a long deadline the bundler derives — so it holds here on purpose rather than failing fast.'

export function slowStepNote(component: string): string | undefined {
  if (component.endsWith(READINESS_SUFFIX)) return READINESS_NOTE
  return EXACT[component]
}
```

- [ ] **Step 7: Add the API calls**

In `web/src/api.ts`:

```ts
export async function retryRun(runId: string): Promise<Run> {
  const res = await fetch(`/api/runs/${encodeURIComponent(runId)}/retry`, { method: 'POST' })
  if (!res.ok) throw new ApiError(res.status, 'Failed to retry the run')
  return res.json()
}

/**
 * bundleUrl is a plain href rather than a fetch: the browser's own download
 * handling gets the filename from Content-Disposition, and the session
 * cookie rides along on the navigation.
 */
export function bundleUrl(runId: string): string {
  return `/api/runs/${encodeURIComponent(runId)}/bundle`
}
```

- [ ] **Step 8: Write the failing Cockpit tests**

Create `web/src/components/Cockpit.test.tsx`, following `Discover.test.tsx` and `Recommend.test.tsx` conventions, asserting:

- **Gate** — with `state === 'awaiting_decision'` and phase `apply`, the recipe's components render, a *Download bundle* link points at `/api/runs/<id>/bundle`, and clicking *Install* calls `onDecide` with `{ apply: 'yes' }`;
- **Running** — component rows render with status, and a `retrying` component shows its attempt count;
- **Slow-step callout** — an active `gpu-operator` renders its note; an active `cert-manager` renders none;
- **Failed** — the failing component, the exit error, and the tail all render, and clicking *Retry* calls `onRetry`;
- **Done** — a success line renders and no Retry button appears.

- [ ] **Step 9: Run to verify they fail**

Run: `cd web && npm test -- Cockpit`
Expected: FAIL — module not found.

- [ ] **Step 10: Implement `Cockpit.tsx`**

Four branches on `run.state` and the derived pipeline, using the existing Tailwind vocabulary (`bg-slate-950`, `text-slate-100`, `border-slate-800`, `text-emerald-400` for success, `text-amber-400` for warnings, `text-red-400` for errors). Keep it a presentation component: every derivation comes from `pipeline.ts`, every action is a prop.

Render the diagnostic tail inside a `<details>` fold, `<pre>`-formatted — it is long, and it should be available without dominating the screen.

- [ ] **Step 11: Route to the cockpit from `Wizard`**

In `web/src/components/Wizard.tsx`:

- import `Cockpit`, `retryRun`;
- add `handleRetry` alongside the existing `handleDecide`, with the same `setDecideError` treatment on failure;
- in the returned JSX, branch before the existing `run.phase === 'recommend'` check:

```tsx
        {run.phase === 'bundle' || run.phase === 'apply' ? (
          <Cockpit events={events} run={run} onDecide={handleDecide} onRetry={handleRetry} />
        ) : run.phase === 'recommend' ? (
          renderRecommend()
        ) : run.report ? (
          <Discover report={run.report} />
        ) : (
          <p className="text-slate-500 text-sm">Discovering the cluster…</p>
        )}
```

- widen the layout for the cockpit: the spec's "layout expands into the cockpit" beat. Drop the aside from `w-96` to `w-80` and remove any max-width on the main column when in cockpit mode.

`handleDecide` currently takes `{ intent, platform }`. Broaden its parameter to `Record<string, string>` so the gate can send `{ apply: 'yes' }` through the same path — `Recommend`'s existing call site keeps compiling unchanged.

- [ ] **Step 12: Run the full web suite**

Run: `cd web && npm test`
Expected: PASS, including the pre-existing `Wizard.test.tsx`.

- [ ] **Step 13: Qualify and commit**

```bash
make qualify
git add web/src/
git commit -S -m "feat(web): cockpit with confirm gate, component pipeline, and failure state

Four states off the event stream: the gate that must be clicked before
the console installs anything with cluster-admin, the running pipeline,
the failure screen carrying deploy.sh's own captured diagnostics behind a
fold, and done.

Slow-step callouts are deliberately unquantified -- real per-node timings
get calibrated in Phase 4, and a fabricated minute count on screen during
a KWOK demo is worse than saying less."
```

---

## Task 10: The two carried-over review findings

**Files:**
- Modify: `internal/gap/gap.go`
- Modify: `internal/gap/gap_test.go`
- Modify: `web/src/components/Discover.tsx`
- Modify: `web/src/components/Discover.test.tsx`
- Modify: `web/src/useEvents.ts`
- Modify: `web/src/useEvents.lifecycle.test.tsx`
- Modify: `web/src/App.tsx`

**Interfaces:**
- Consumes: `GET /api/session` (Task 8).
- Produces: `gap.Report.Analyzed bool` (`json:"analyzed"`); `useEvents(onUnauthorized?: () => void)`.

- [ ] **Step 1: Write the failing `Analyzed` test**

Add to `internal/gap/gap_test.go`:

```go
// A fully capable cluster and a cluster that was never measured both yield
// zero gaps. The console must not congratulate the user for the second.
func TestAnalyzeMarksWhetherASnapshotWasActuallyMeasured(t *testing.T) {
	if got := gap.Analyze(nil); got.Analyzed {
		t.Error("Analyzed = true for a nil snapshot, want false")
	}
	report := gap.Analyze(loadSnapshot(t, "testdata/snapshot-kwok-h100.yaml"))
	if !report.Analyzed {
		t.Error("Analyzed = false for a real snapshot, want true")
	}
}
```

Use whatever fixture-loading helper `gap_test.go` already defines rather than adding another; read the file first.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/gap/ -race -run TestAnalyzeMarks -v`
Expected: FAIL — `report.Analyzed undefined`.

- [ ] **Step 3: Add `Analyzed`**

In `internal/gap/gap.go`, add to `Report`:

```go
	// Analyzed is false when Analyze had no usable snapshot. Gaps is empty
	// in two very different situations -- every capability already present,
	// and nothing measured at all -- and the console must not show the
	// green "already capable" copy for the second. The headline strings
	// differ too, but keying UI on prose is how prose changes become bugs.
	Analyzed bool `json:"analyzed"`
```

Set `Analyzed: true` on the `report` literal in the measured path. The two degraded early returns leave it false.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/gap/ -race`
Expected: PASS.

- [ ] **Step 5: Distinguish the two cases in `Discover.tsx`**

Add `analyzed: boolean` to the `CapabilityReport` interface, and replace the single `gaps.length === 0` branch with three:

```tsx
      {!report.analyzed ? (
        <p data-testid="no-snapshot" className="text-amber-400">
          No cluster snapshot is available yet, so nothing has been measured — this is not a clean bill of health.
        </p>
      ) : gaps.length === 0 ? (
        <p data-testid="no-gaps" className="text-emerald-400">
          Every capability this workload needs is already installed — there is nothing left to close.
        </p>
      ) : (
        // ... existing gap list, unchanged
      )}
```

Add a `Discover.test.tsx` case asserting `no-snapshot` renders for `analyzed: false` and `no-gaps` does not, and update the existing no-gaps case to set `analyzed: true`.

- [ ] **Step 6: Run the web tests**

Run: `cd web && npm test -- Discover`
Expected: PASS.

- [ ] **Step 7: Write the failing 401 test**

Add to `web/src/useEvents.lifecycle.test.tsx` a case that:

- mocks `EventSource` so the test can fire `onerror`;
- mocks `fetch` to answer `/api/session` with 401;
- renders `useEvents(onUnauthorized)`;
- fires `onerror`;
- asserts `onUnauthorized` was called.

Add a second case where `/api/session` answers 204 and asserts `onUnauthorized` was **not** called — a network blip must not log the user out.

Follow the existing mocking style in that file rather than introducing a new one.

- [ ] **Step 8: Implement the 401 path**

In `web/src/useEvents.ts`, change the signature to `useEvents(onUnauthorized?: () => void)` and in `source.onerror`:

```ts
      source.onerror = () => {
        setConnected(false)
        // EventSource surfaces no HTTP status, so a dropped stream is
        // indistinguishable here from an expired session. After the 8-hour
        // expiry the console previously sat on "reconnecting…" forever with
        // no path back to the login screen. Probe a cheap authenticated
        // route to tell the two apart; anything other than a 401 is a
        // genuine blip and EventSource's own retry handles it.
        fetch('/api/session')
          .then(res => {
            if (res.status === 401 && !torndown) onUnauthorized?.()
          })
          .catch(() => {})
      }
```

Add `onUnauthorized` to the effect's dependency array, and note in a comment that callers must pass a stable reference (`useCallback`) or the stream reconnects on every render.

- [ ] **Step 9: Wire it into `App.tsx`**

In `App`, hoist `authed` handling so `Console` can clear it:

```tsx
      {authed ? <Console onUnauthorized={() => setAuthed(false)} /> : <Login onSuccess={() => setAuthed(true)} />}
```

In `Console`, accept the prop, wrap it in `useCallback`, and pass it to `useEvents`.

- [ ] **Step 10: Run the full suites**

Run: `cd web && npm test` and `go test ./... -race`
Expected: PASS.

- [ ] **Step 11: Qualify and commit**

```bash
make qualify
git add internal/gap/ web/src/
git commit -S -m "fix: distinguish unmeasured from capable, and recover from an expired session

Two findings the Phase 0-1 final review raised to the top of the Phase 2
list, fixed in files this phase already touches.

gap.Report.Analyzed: zero gaps means 'every capability present' OR
'nothing measured at all', and the console was showing the green
already-capable copy for both.

Nothing reset authed on a 401, so after the 8-hour session expiry the
console sat on 'reconnecting...' forever with no path back to the login
screen. EventSource exposes no HTTP status, so the stream error now
probes GET /api/session to tell an expired session from a network blip."
```

---

## Task 11: The dry-run end-to-end

**Files:**
- Modify: `test/e2e/lib.sh` (extract the KWOK cluster setup)
- Modify: `test/e2e/discover-recommend.sh` (consume the extracted helpers)
- Create: `test/e2e/apply-dryrun.sh`
- Modify: `.github/workflows/e2e.yaml`
- Modify: `Makefile`

**Interfaces:**
- Consumes: everything from Tasks 2-10, and `AICRME_APPLY_DRY_RUN` (Task 6).
- Produces: `make test-e2e-apply`; a CI job asserting the full arc reaches Apply and streams component progress.

- [ ] **Step 1: Extract the KWOK setup into `lib.sh`**

Move `node_yaml`, `apply_kwok_nodes`, and the KWOK controller install from `test/e2e/discover-recommend.sh` into `test/e2e/lib.sh` as `e2e_node_yaml`, `e2e_apply_kwok_nodes`, and `e2e_install_kwok`. Move `KWOK_VERSION`, `KWOK_K8S_VERSION`, `KWOK_REGION`, and `KWOK_ZONES` with them, keeping the existing comments about why the node values are inlined rather than sourced from the AICR repo.

- [ ] **Step 2: Verify the extraction changed nothing**

Run: `make lint-shell` and then the existing e2e end to end:
```bash
bash test/e2e/discover-recommend.sh
```
Expected: `PASS: discover-recommend e2e green`.

- [ ] **Step 3: Commit the extraction separately**

```bash
git add test/e2e/
git commit -S -m "refactor(e2e): share the KWOK cluster setup between e2e scripts"
```

- [ ] **Step 4: Write `apply-dryrun.sh`**

Create `test/e2e/apply-dryrun.sh`, structured on `discover-recommend.sh` (same `set -euo pipefail`, same `cleanup`/`trap` discipline, same `fail_run` and `dump_recent_events` diagnostics). It:

1. creates the Kind cluster, installs KWOK, applies the simulated H100 nodes (all via `lib.sh`);
2. builds and loads the image, installs the chart;
3. sets **both** env overrides in one `kubectl set env` and waits for the rollout:
   `AICRME_SNAPSHOT_NODE_SELECTOR=node-role.kubernetes.io/control-plane=` and `AICRME_APPLY_DRY_RUN=true`;
4. logs in, `POST /api/runs`, answers `{"intent":"training","platform":"kubeflow"}` on the first park;
5. polls until the run parks a **second** time, and asserts `.pending == ["apply"]` — this is the confirm gate, and asserting it is what proves the console does not install without a click;
6. `GET /api/runs/{id}/bundle`, asserts a non-empty `application/gzip` body containing `deploy.sh`:
   ```bash
   curl -fsS -b "${JAR}" "http://${ADDR}/api/runs/${RUN_ID}/bundle" -o "${TARBALL}"
   tar -tzf "${TARBALL}" | grep -qx 'deploy.sh'
   ```
7. `POST /api/runs/{id}/decide` with `{"apply":"yes"}`;
8. polls until `done` or `failed`, allowing generously for helm to fetch every chart (dry-run still downloads);
9. asserts from the SSE stream that **at least one component reached `installed`**:
   ```bash
   curl -fsS -b "${JAR}" --max-time 10 "http://${ADDR}/api/events?since=0" \
     | sed -n 's/^data: //p' \
     | jq -r 'select(.kind=="component") | .data.status' | sort -u
   ```

**On the terminal state:** if Task 1 found that some chart cannot render under `--dry-run`, assert on **progress reached** (step 9's `installed` count, and that the gate in step 5 fired) rather than on `state == done`, and say so in a comment naming the component Task 1 identified. Do not assert `done` if Task 1 proved it cannot be reached — a test that is green only because it stopped checking is worse than one that fails.

- [ ] **Step 5: Lint and run it**

Run: `make lint-shell && bash test/e2e/apply-dryrun.sh`
Expected: `PASS`.

This is the first full-arc run. Expect to iterate; when it fails, read the diagnostics the script dumps before the cluster is deleted.

- [ ] **Step 6: Add the Make target and the CI job**

In `Makefile`:

```makefile
.PHONY: test-e2e-apply
test-e2e-apply: ## Runs the Discover-to-Apply dry-run e2e on Kind+KWOK (needs Docker)
	./test/e2e/apply-dryrun.sh
```

In `.github/workflows/e2e.yaml`, add an `apply-dryrun` job mirroring the existing job's setup steps. While in the file, add the two hygiene items the Phase 0-1 review flagged, since this doubles the job count:

```yaml
concurrency:
  group: e2e-${{ github.ref }}
  cancel-in-progress: true
```

and `timeout-minutes: 45` on each job.

- [ ] **Step 7: Qualify and commit**

```bash
make qualify
git add test/e2e/ .github/workflows/e2e.yaml Makefile
git commit -S -m "test(e2e): full Discover-to-Apply arc on KWOK via deploy.sh --dry-run

deploy.sh exports DRY_RUN_FLAG and every generated install.sh
interpolates it, so CI can run the real bundle, the real deploy.sh, and
the real helm binary against a GPU-less cluster -- exercising the whole
marker grammar and the parser without installing anything.

Asserts the confirm gate fires (pending == [apply]) before any install
begins, that the bundle downloads as a tarball containing deploy.sh, and
that components reach 'installed' in the SSE stream. Adds the
concurrency group and timeouts the Phase 0-1 review flagged, now that
e2e runs two jobs."
```

---

## Task 12: Update the handoff record

**Files:**
- Modify: `docs/phase-2-handoff.md`
- Delete: `docs/phase-2a-task-1-findings.md` (fold its durable content in)

- [ ] **Step 1: Fold Task 1's findings into the handoff**

Move the measured bundle size, the dry-run outcome, and the helm/kubectl versions from `docs/phase-2a-task-1-findings.md` into `docs/phase-2-handoff.md`, then delete the findings file. Git is the record; a second scratch document is exactly what the handoff itself warns against keeping.

- [ ] **Step 2: Rewrite the handoff for 2b**

Update `docs/phase-2-handoff.md` so it describes what 2b inherits, not what 2a inherited:

- **What works today** — extend the demo arc to Apply, and name `test/e2e/apply-dryrun.sh` as the proof.
- **Constraints 2b inherits** — the ConfigMap store and the `bus.nextID` epoch problem (still open); `StateActive` (still unreachable, still Phase 3); the training workload (still unauthored); the observer (unstarted).
- **Resolved in 2a, remove from the deferred list** — `readOnlyRootFilesystem`, the `engine.Start` context / 409 latch, the epoch guard, the 401 `authed` reset, and the `Discover.tsx` capable-vs-no-snapshot conflation.
- **New for 2b** — the applier's marker parser is a maintenance liability pinned only by `TestDeployTemplateUnchanged`; the upstream `AICR_DEPLOY_EVENTS=jsonl` PR is what retires it. Record the image size after adding nothing new to it, and the emptyDir `sizeLimit` chosen.
- Keep every explicit non-goal section unchanged.

- [ ] **Step 3: Commit**

```bash
git add docs/
git commit -S -m "docs: refresh the handoff for Phase 2b

Folds Task 1's probe findings in and deletes the scratch file -- git is
the record. Removes the five deferred findings 2a actually closed, and
names the marker parser's maintenance cost as the thing the upstream
jsonl PR exists to retire."
```

---

## Self-Review

**Spec coverage.** Every section of `docs/superpowers/specs/2026-08-15-aicrme-phase-2a-design.md` maps to a task: §1 Bundle → Task 2; §2 workdir and `readOnlyRootFilesystem` → Task 3; §3 applier, marker grammar, unrecognized-line policy, template pin → Tasks 4-5; §4 failure policy → Tasks 5 (no `--best-effort`, tail) and 9 (failure screen); §5 engine step cursor, `Retry`, epoch guard → Task 7; §6 the three API additions and the 409 latch → Task 8; §7 cockpit, slow-step map, two carryovers → Tasks 9-10; §Testing → the test steps in every task plus Task 11; §"largest risk" dry-run probe → Task 1. Spec Open Question 2 (emptyDir sizing) is answered in Task 1 and applied in Task 3. Open Questions 1, 3, and 4 are explicitly deferred and carried into Task 12's rewritten handoff.

**Placeholder scan.** Tasks 8 (`bundle_test.go` bodies), 9 (Cockpit test bodies and the fixture), 10 (test bodies), and 11 (the e2e script) describe assertions rather than shipping literal code. That is deliberate in each case: those files must adopt the harness and helper functions their existing neighbours already define (`internal/api`'s `httptest` setup, `Discover.test.tsx`'s render conventions, `discover-recommend.sh`'s `cleanup`/`trap` discipline), and pasting a parallel harness here would produce worse code than reading the neighbour. Each such step names the file to read first and lists every assertion required. Everywhere a *new* type, interface, regex, or handler is introduced, the literal code is present.

**Type consistency.** `applier.ComponentData` / `applier.FailureData` (Task 4) are consumed under those exact names, with those exact JSON tags, by `web/src/pipeline.ts` (Task 9). `applier.Spec{Dir, Argv, Env}`, `applier.Exec.Run`, `applier.New`, and `applier.Options{BundleDir, Retries, DryRun}` (Task 5) are consumed verbatim by `steps.NewApply` (Task 6) and by both tasks' test fakes. `Run.Artifacts["bundle.path"]` is written in Task 2 and read in Tasks 6 and 8. `Run.StepIndex` and `Engine.Retry` (Task 7) are consumed in Task 8. `steps.RecipeSummary` / `steps.ComponentSummary` are reused unchanged from Phase 1 by Task 2's `assertMatchesApproved`. `gap.Report.Analyzed` (Task 10, Go) matches `CapabilityReport.analyzed` (Task 10, TS). `Config.WorkDir` (Task 8) and `BundleConfig.WorkDir` (Task 2) both derive from `cmd/aicrme`'s single `workDir` variable (Task 3).

**Known plan-authoring hazard.** An earlier draft of Task 7 Step 1 reached for an `Engine.Abandon` method that 2a has no reason to add, in order to force a second live goroutine. That was a plan defect and is fixed: the epoch guard is now proven through `Retry`, which is the path 2a actually introduces. The Phase 0-1 plan shipped four real defects in its own text, caught only by implementers — **if any step here asks for a symbol no task defines, stop and raise it rather than inventing the symbol.**

## Unresolved questions

1. **Does `deploy.sh --dry-run` complete on KWOK?** Task 1 answers it. A partial failure is workable (Task 11 asserts on progress); no markers at all is a scope change for the user.
2. **What `workDir.sizeLimit` is right?** Task 1 measures a real bundle; helm's chart cache, not the bundle, is likely to dominate.
3. **Does the image grow?** 2a adds no binaries, but it was already 55 MB compressed and it is the first thing a cluster pulls. Task 12 records the post-2a figure.
4. **Ownership and budget** — `approach.md` Open Question 1, untouched. It does not block 2a but decides whether Phase 4's real-hardware time exists.

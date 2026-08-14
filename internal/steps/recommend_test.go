package steps_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
	"github.com/mchmarny/aicrme/internal/steps"
	"gopkg.in/yaml.v3"
)

func recipeFixture() *aicr.RecipeResult {
	return &aicr.RecipeResult{
		Name:    "h100-eks-ubuntu-training",
		Version: "0.19.0",
		Components: []aicr.ComponentRef{
			{Name: "gpu-operator", Kind: "Helm", Version: "v26.3.3", Namespace: "gpu-operator",
				Chart: "gpu-operator", Source: "https://helm.ngc.nvidia.com/nvidia"},
			{Name: "kai-scheduler", Kind: "Helm", Version: "v0.14.1", Namespace: "kai-scheduler",
				Chart: "kai-scheduler", Source: "oci://ghcr.io/kai-scheduler/kai-scheduler"},
		},
	}
}

// minimalSnapshot is syntactically-valid, content-free Snapshot YAML for
// tests where only the presence of the artifact matters, not its
// measurements. TestRecommendResolveAgainstRealFixture below is what
// exercises the real captured measurements.
const minimalSnapshot = "apiVersion: aicr.nvidia.com/v1\nkind: Snapshot\n"

// loadH100Snapshot loads the KWOK fixture captured from a cluster carrying
// AICR's own simulated H100 nodes (kwok/profiles/eks/p5-h100.yaml in the
// AICR reference checkout, applied via kwok/scripts/apply-nodes.sh) --
// unlike testdata/snapshot-kwok.yaml (the original, real capture from a
// control-plane-only cluster with no worker nodes at all), this fixture's
// node topology carries real nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3
// labels the fingerprint package can actually derive an accelerator from.
// See task-10-report.md for exactly how this was captured.
func loadH100Snapshot(t *testing.T) *aicr.Snapshot {
	t.Helper()
	raw, err := os.ReadFile("testdata/snapshot-kwok-h100.yaml")
	if err != nil {
		t.Fatalf("fixture read error = %v", err)
	}
	var s snapshotter.Snapshot
	if err := yaml.Unmarshal(raw, &s); err != nil {
		t.Fatalf("parse fixture error = %v", err)
	}
	wrapped := aicr.WrapSnapshot(&s)
	wrapped.Raw = raw
	return wrapped
}

func TestRecommendRequiresExactlyTwoDecisions(t *testing.T) {
	step := steps.NewRecommend(&aicrclient.Fake{})
	got := step.Requires()
	want := []string{"intent", "platform"}
	if len(got) != len(want) {
		t.Fatalf("Requires() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Requires()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRecommendMapsDecisionsToCriteria(t *testing.T) {
	fake := &aicrclient.Fake{Recipe: recipeFixture()}
	step := steps.NewRecommend(fake)

	run := newRun()
	run.Decisions["intent"] = "training"
	run.Decisions["platform"] = "kubeflow"
	run.Artifacts["snapshot.yaml"] = []byte(minimalSnapshot)

	if err := step.Run(context.Background(), run, func(bus.Event) {}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if fake.LastCriteria == nil {
		t.Fatal("resolver was called without criteria")
	}
	if fake.LastCriteria.Intent != "training" {
		t.Errorf("Criteria.Intent = %q, want %q", fake.LastCriteria.Intent, "training")
	}
	if fake.LastCriteria.Platform != "kubeflow" {
		t.Errorf("Criteria.Platform = %q, want %q", fake.LastCriteria.Platform, "kubeflow")
	}
}

func TestRecommendStoresComponentSummary(t *testing.T) {
	fake := &aicrclient.Fake{Recipe: recipeFixture()}
	step := steps.NewRecommend(fake)

	run := newRun()
	run.Decisions["intent"] = "training"
	run.Decisions["platform"] = "kubeflow"
	run.Artifacts["snapshot.yaml"] = []byte(minimalSnapshot)

	if err := step.Run(context.Background(), run, func(bus.Event) {}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	var summary steps.RecipeSummary
	if err := json.Unmarshal(run.Artifacts["recipe.json"], &summary); err != nil {
		t.Fatalf("recipe.json decode error = %v", err)
	}
	if summary.ComponentCount != 2 {
		t.Errorf("ComponentCount = %d, want 2", summary.ComponentCount)
	}
	if summary.Components[0].Version == "" {
		t.Error("component version not carried — every version must be shown pinned")
	}
}

func TestRecommendPropagatesResolveFailure(t *testing.T) {
	fake := &aicrclient.Fake{ResolveErr: errors.New("no recipe for these coordinates")}
	step := steps.NewRecommend(fake)

	run := newRun()
	run.Decisions["intent"] = "training"
	run.Decisions["platform"] = "kubeflow"
	run.Artifacts["snapshot.yaml"] = []byte(minimalSnapshot)

	if err := step.Run(context.Background(), run, func(bus.Event) {}); err == nil {
		t.Fatal("Run() returned nil on a resolve failure")
	}
}

func TestRecommendFailsOnNilResult(t *testing.T) {
	// A Resolver returning (nil, nil) is not something aicr.Client does in
	// practice, but Resolver is an interface Recommend depends on, not a
	// concrete type — this guards against a future or alternate
	// implementation doing so silently.
	fake := &aicrclient.Fake{}
	step := steps.NewRecommend(fake)

	run := newRun()
	run.Decisions["intent"] = "training"
	run.Decisions["platform"] = "kubeflow"
	run.Artifacts["snapshot.yaml"] = []byte(minimalSnapshot)

	if err := step.Run(context.Background(), run, func(bus.Event) {}); err == nil {
		t.Fatal("Run() returned nil for a nil recipe result")
	}
}

func TestRecommendPhase(t *testing.T) {
	if got := steps.NewRecommend(&aicrclient.Fake{}).Phase(); got != engine.PhaseRecommend {
		t.Errorf("Phase() = %q, want %q", got, engine.PhaseRecommend)
	}
}

// TestRecommendFailsLoudlyWithoutSnapshot asserts the judgement call made in
// decodeSnapshot: Discover always runs before Recommend in the wired
// pipeline, so a missing snapshot.yaml artifact means something upstream
// broke, not a legitimate "no cluster info" case. Recommend must say so
// rather than silently resolving criteria-only against a nil snapshot,
// which the real aicr.Client rejects anyway ("snapshot is required (got
// nil)") — proceeding here would just defer that failure past this whole
// test file, since aicrclient.Fake never validates its arguments.
func TestRecommendFailsLoudlyWithoutSnapshot(t *testing.T) {
	fake := &aicrclient.Fake{Recipe: recipeFixture()}
	step := steps.NewRecommend(fake)

	run := newRun()
	run.Decisions["intent"] = "training"
	run.Decisions["platform"] = "kubeflow"
	// snapshot.yaml deliberately left unset.

	err := step.Run(context.Background(), run, func(bus.Event) {})
	if err == nil {
		t.Fatal("Run() returned nil with no snapshot.yaml artifact")
	}
	if fake.ResolveCalls != 0 {
		t.Error("resolver was called despite a missing snapshot")
	}
	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) || se.Code != aicrerrors.ErrCodeInvalidRequest {
		t.Errorf("error = %v, want a StructuredError with ErrCodeInvalidRequest", err)
	}
}

func TestRecommendFailsOnUnparseableSnapshot(t *testing.T) {
	fake := &aicrclient.Fake{Recipe: recipeFixture()}
	step := steps.NewRecommend(fake)

	run := newRun()
	run.Decisions["intent"] = "training"
	run.Decisions["platform"] = "kubeflow"
	run.Artifacts["snapshot.yaml"] = []byte("- this\n- is\n- a list, not a Snapshot\n")

	if err := step.Run(context.Background(), run, func(bus.Event) {}); err == nil {
		t.Fatal("Run() returned nil for an unparseable snapshot artifact")
	}
	if fake.ResolveCalls != 0 {
		t.Error("resolver was called with an unparseable snapshot")
	}
}

// TestRecommendRejectsEmptyDecisionValues covers the other half of the
// specificity judgement call: engine.Engine.awaitDecisions only checks that
// "intent" and "platform" are present as keys in Run.Decisions, never that
// their values are non-blank. AICR's facade does not itself guard against
// zero-specificity criteria (see TestRecommendResolveAgainstRealFixture),
// so Recommend is the only place left that can catch a blank decision
// before it reaches resolve().
func TestRecommendRejectsEmptyDecisionValues(t *testing.T) {
	tests := []struct {
		name     string
		intent   string
		platform string
	}{
		{"empty intent", "", "kubeflow"},
		{"empty platform", "training", ""},
		{"both empty", "", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := &aicrclient.Fake{Recipe: recipeFixture()}
			step := steps.NewRecommend(fake)

			run := newRun()
			run.Decisions["intent"] = tc.intent
			run.Decisions["platform"] = tc.platform
			run.Artifacts["snapshot.yaml"] = []byte(minimalSnapshot)

			if err := step.Run(context.Background(), run, func(bus.Event) {}); err == nil {
				t.Fatal("Run() returned nil for a blank decision value")
			}
			if fake.ResolveCalls != 0 {
				t.Error("resolver was called with a blank decision value")
			}
		})
	}
}

// TestRecommendResolveAgainstRealFixture pins the real, load-bearing
// behavior against the captured KWOK cluster: what AICR's facade actually
// does with Recommend's real criteria-derivation path (fingerprint-derived
// service/accelerator/os/nodes, intent+platform overlaid from the user).
// aicrclient.Fake never rejects anything, so a regression here would pass
// every other test in this file while failing on every real run.
//
// Empirically (aicr v0.19.0, embedded catalog, round 2 of this task):
// buildCriteria correctly derives service="kind" and nodes=1 from the KWOK
// fixture's own measurements (confirmed via a throwaway diagnostic script,
// not shipped) — the fingerprint-derivation wiring itself works. Resolution
// still fails for Criteria{Service:"kind", Intent:"training",
// Platform:"kubeflow", Nodes:1}, but for a precise, catalog-stated reason:
// intent=training and platform=kubeflow both require an accelerator (h100)
// for the "kind" service, and accelerator cannot be derived from a snapshot
// of a cluster with no GPU hardware — which is exactly what the KWOK demo
// path is (a simulated cluster, no GPU hardware, per the product spec). This
// is not a bug in criteria derivation; it is a real gap between AICR's
// catalog coverage and a no-GPU demo cluster. See task-10-report.md, "The
// KWOK specificity question, round 2" for the full empirical matrix across
// every (intent, platform) pair Task 11 plans to offer, and what it means
// for Task 13.
func TestRecommendResolveAgainstRealFixture(t *testing.T) {
	client, err := aicrclient.New()
	if err != nil {
		t.Fatalf("aicrclient.New() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	step := steps.NewRecommend(client)
	run := newRun()
	run.Decisions["intent"] = "training"
	run.Decisions["platform"] = "kubeflow"
	snap := loadSnapshot(t)
	run.Artifacts["snapshot.yaml"] = snap.Raw

	runErr := step.Run(context.Background(), run, func(bus.Event) {})
	if runErr == nil {
		t.Fatal("Run() succeeded resolving fingerprint-derived criteria against the KWOK fixture — " +
			"if AICR's catalog or KWOK's fixture changed, update this test and the Task 13 plan")
	}

	var se *aicrerrors.StructuredError
	if !errors.As(runErr, &se) || se.Code != aicrerrors.ErrCodeInvalidRequest {
		t.Errorf("error = %v, want a StructuredError with ErrCodeInvalidRequest", runErr)
	}
	if !strings.Contains(runErr.Error(), "accelerator") {
		t.Errorf("error = %v, want it to name the missing accelerator dimension "+
			"(the specific, diagnosable reason this fails, not a vague coverage error)", runErr)
	}
	t.Logf("observed resolve error against the real KWOK fixture: %v", runErr)
}

// TestRecommendFailsOnZeroSpecificityCriteria covers the specificity guard
// directly: a snapshot with no measurements at all (so nothing is
// fingerprint-derivable) plus intent/platform decisions explicitly set to
// the literal "any" sentinel (non-blank, so the earlier blank-decision check
// does not catch it) produces Criteria{Service:"any", Accelerator:"any",
// Intent:"any", OS:"any", Platform:"any", Nodes:0} -- Specificity()==0.
// AICR's facade does not reject this itself (see task-10-report.md); this
// test proves Recommend does, before ever calling the resolver.
func TestRecommendFailsOnZeroSpecificityCriteria(t *testing.T) {
	fake := &aicrclient.Fake{Recipe: recipeFixture()}
	step := steps.NewRecommend(fake)

	run := newRun()
	run.Decisions["intent"] = "any"
	run.Decisions["platform"] = "any"
	run.Artifacts["snapshot.yaml"] = []byte(minimalSnapshot)

	err := step.Run(context.Background(), run, func(bus.Event) {})
	if err == nil {
		t.Fatal("Run() returned nil for zero-specificity criteria")
	}
	if fake.ResolveCalls != 0 {
		t.Error("resolver was called with zero-specificity criteria")
	}
	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) || se.Code != aicrerrors.ErrCodeInvalidRequest {
		t.Errorf("error = %v, want a StructuredError with ErrCodeInvalidRequest", err)
	}
}

// kwokIntents and kwokPlatforms are the values Recommend's own facade
// actually accepts (verified against pkg/recipe/criteria.go's ParseIntent/
// ParsePlatform fast paths): training/inference, and dynamo/kubeflow/nim/
// runai/slurm plus "any" for "just the runtime, no specific platform" --
// the product spec's fourth user-facing platform option. An earlier round of
// this task used "none" here, which is not a value AICR's criteria registry
// recognizes at all; every pair that included it failed for that reason
// alone, which was hiding the real picture. These are the correct values.
var (
	kwokIntents   = []string{"training", "inference"}
	kwokPlatforms = []string{"dynamo", "kubeflow", "nim", "runai", "slurm", "any"}
)

// TestRecommendKWOKGPUlessFixtureMatrix pins the full, honest picture for
// the original KWOK fixture (a real capture from a control-plane-only
// cluster with no worker nodes at all -- see discover_test.go's
// loadSnapshot): every (intent, platform) pair fails except
// (inference, any), which needs no accelerator and so is the one
// combination a snapshot with zero derivable hardware can still satisfy.
// This is not a bug in Recommend -- see TestRecommendResolveAgainstRealFixture
// -- it is this fixture's real, permanent limitation: it was captured from a
// cluster with no GPU hardware and no simulated GPU nodes, so accelerator can
// never be derived from it. Contrast with TestRecommendResolvesAgainstSimulatedH100Fixture.
func TestRecommendKWOKGPUlessFixtureMatrix(t *testing.T) {
	client, err := aicrclient.New()
	if err != nil {
		t.Fatalf("aicrclient.New() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	snap := loadSnapshot(t)
	succeeds := map[string]bool{"inference/any": true}

	for _, intent := range kwokIntents {
		for _, platform := range kwokPlatforms {
			key := intent + "/" + platform
			wantErr := !succeeds[key]
			t.Run(key, func(t *testing.T) {
				step := steps.NewRecommend(client)
				run := newRun()
				run.Decisions["intent"] = intent
				run.Decisions["platform"] = platform
				run.Artifacts["snapshot.yaml"] = snap.Raw

				runErr := step.Run(context.Background(), run, func(bus.Event) {})
				if wantErr && runErr == nil {
					t.Fatalf("intent=%s platform=%s: Run() succeeded against the GPU-less KWOK fixture — "+
						"AICR's catalog changed; update task-10-report.md and re-evaluate the Task 13 plan",
						intent, platform)
				}
				if !wantErr && runErr != nil {
					t.Fatalf("intent=%s platform=%s: Run() error = %v, want success "+
						"(this pair needs no accelerator)", intent, platform, runErr)
				}
				t.Logf("intent=%s platform=%s: err=%v", intent, platform, runErr)
			})
		}
	}
}

// TestRecommendResolvesAgainstSimulatedH100Fixture is the answer to the
// question the GPU-less fixture cannot answer: with a real accelerator
// signal in the snapshot (AICR's own simulated H100 nodes, applied via
// AICR's own kwok/scripts/apply-nodes.sh -- see loadH100Snapshot and
// task-10-report.md), does Recommend's fingerprint-derived criteria
// actually resolve a real, sensibly-shaped recipe? For five of the twelve
// (intent, platform) pairs, yes -- pinned here with real client,
// real fixture, real resolution, and an assertion that every resolved
// component carries a name and a pinned version. The other seven still
// fail, for reasons that have nothing to do with accelerator (no catalog
// overlay combines service=kind with that platform at all); those are
// pinned too, so a change in either direction fails this test loudly.
func TestRecommendResolvesAgainstSimulatedH100Fixture(t *testing.T) {
	client, err := aicrclient.New()
	if err != nil {
		t.Fatalf("aicrclient.New() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	snap := loadH100Snapshot(t)
	succeeds := map[string]bool{
		"training/kubeflow": true,
		"training/slurm":    true,
		"training/any":      true,
		"inference/dynamo":  true,
		"inference/any":     true,
	}

	for _, intent := range kwokIntents {
		for _, platform := range kwokPlatforms {
			key := intent + "/" + platform
			wantErr := !succeeds[key]
			t.Run(key, func(t *testing.T) {
				step := steps.NewRecommend(client)
				run := newRun()
				run.Decisions["intent"] = intent
				run.Decisions["platform"] = platform
				run.Artifacts["snapshot.yaml"] = snap.Raw

				runErr := step.Run(context.Background(), run, func(bus.Event) {})
				if wantErr {
					if runErr == nil {
						t.Fatalf("intent=%s platform=%s: Run() succeeded against the simulated-H100 fixture — "+
							"AICR's catalog changed; update task-10-report.md and re-evaluate the Task 13 plan",
							intent, platform)
					}
					t.Logf("intent=%s platform=%s: err=%v", intent, platform, runErr)
					return
				}
				if runErr != nil {
					t.Fatalf("intent=%s platform=%s: Run() error = %v, want success", intent, platform, runErr)
				}

				var summary steps.RecipeSummary
				if err := json.Unmarshal(run.Artifacts["recipe.json"], &summary); err != nil {
					t.Fatalf("intent=%s platform=%s: recipe.json decode error = %v", intent, platform, err)
				}
				if summary.ComponentCount == 0 || len(summary.Components) == 0 {
					t.Fatalf("intent=%s platform=%s: resolved recipe has no components", intent, platform)
				}
				for _, c := range summary.Components {
					if c.Name == "" || c.Version == "" {
						t.Errorf("intent=%s platform=%s: component missing name or pinned version: %+v",
							intent, platform, c)
					}
				}
				t.Logf("intent=%s platform=%s: OK name=%s version=%s components=%d",
					intent, platform, summary.Name, summary.Version, summary.ComponentCount)
			})
		}
	}
}

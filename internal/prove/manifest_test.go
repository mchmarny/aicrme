package prove_test

import (
	"strings"
	"testing"

	"github.com/mchmarny/aicrme/internal/prove"
	"gopkg.in/yaml.v3"
)

func TestRenderCarriesOwnershipLabels(t *testing.T) {
	out, err := prove.Render("run-abc", prove.Namespace)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	var obj map[string]any
	if err := yaml.Unmarshal(out, &obj); err != nil {
		t.Fatalf("Render() produced invalid YAML: %v", err)
	}
	labels := obj["metadata"].(map[string]any)["labels"].(map[string]any)
	for k, want := range map[string]string{
		"app.kubernetes.io/managed-by": "aicrme",
		"aicrme.dev/run-id":            "run-abc",
		"aicrme.dev/component":         "prove-workload",
	} {
		if got, _ := labels[k].(string); got != want {
			t.Errorf("label %q = %q, want %q", k, got, want)
		}
	}
}

// Identity must survive a restart without the persisted record, so the name
// is derived from the run ID rather than generated.
func TestWorkloadNameIsStableForARunID(t *testing.T) {
	if a, b := prove.WorkloadName("run-abc"), prove.WorkloadName("run-abc"); a != b {
		t.Errorf("WorkloadName not stable: %q vs %q", a, b)
	}
	if a, b := prove.WorkloadName("run-abc"), prove.WorkloadName("run-xyz"); a == b {
		t.Errorf("WorkloadName collides across runs: %q", a)
	}
}

// Scalar allocation, NOT DRA. KWOK publishes no ResourceSlices and AICR
// disables full-GPU DRA advertising by default, so a resourceClaim here would
// never bind. Two pods, eight GPUs each.
func TestWorkloadRequestsScalarGPUsAndNotDRA(t *testing.T) {
	out, _ := prove.Render("run-abc", prove.Namespace)
	s := string(out)
	if !strings.Contains(s, "nvidia.com/gpu") {
		t.Error("workload does not request nvidia.com/gpu")
	}
	for _, forbidden := range []string{"resourceClaims", "resourceClaimTemplate", "ResourceClaim"} {
		if strings.Contains(s, forbidden) {
			t.Errorf("workload uses DRA (%q); Phase 3 is scalar-only", forbidden)
		}
	}
}

func TestRenderTargetsTheGivenNamespace(t *testing.T) {
	out, _ := prove.Render("run-abc", "somewhere-else")
	var obj map[string]any
	_ = yaml.Unmarshal(out, &obj)
	if got := obj["metadata"].(map[string]any)["namespace"]; got != "somewhere-else" {
		t.Errorf("namespace = %v, want somewhere-else", got)
	}
}

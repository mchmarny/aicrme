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

// The demo's whole claim is that the cluster can gang-place a multi-node GPU
// job: two pods, 8 GPUs each, kai-scheduler doing the placing. Fix round 1
// (review F1): TestWorkloadRequestsScalarGPUsAndNotDRA only substring-checks
// for "nvidia.com/gpu" -- a request of 1 GPU, or a single non-gang pod, still
// contains that substring and would pass it. This test parses the rendered
// object and pins completions, parallelism, schedulerName, and the GPU count
// individually, so a mutation to any one of them fails its own subtest.
func TestWorkloadGangSchedulesTwoPodsAtEightGPUsEachOnKaiScheduler(t *testing.T) {
	out, err := prove.Render("run-abc", prove.Namespace)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	var obj map[string]any
	if err := yaml.Unmarshal(out, &obj); err != nil {
		t.Fatalf("Render() produced invalid YAML: %v", err)
	}
	spec, ok := obj["spec"].(map[string]any)
	if !ok {
		t.Fatalf("spec missing or not a mapping: %#v", obj["spec"])
	}

	t.Run("completions", func(t *testing.T) {
		if got := spec["completions"]; got != 2 {
			t.Errorf("spec.completions = %v, want 2", got)
		}
	})
	t.Run("parallelism", func(t *testing.T) {
		if got := spec["parallelism"]; got != 2 {
			t.Errorf("spec.parallelism = %v, want 2", got)
		}
	})

	tmpl, ok := spec["template"].(map[string]any)
	if !ok {
		t.Fatalf("spec.template missing or not a mapping: %#v", spec["template"])
	}
	podSpec, ok := tmpl["spec"].(map[string]any)
	if !ok {
		t.Fatalf("spec.template.spec missing or not a mapping: %#v", tmpl["spec"])
	}

	t.Run("schedulerName", func(t *testing.T) {
		if got := podSpec["schedulerName"]; got != "kai-scheduler" {
			t.Errorf("spec.template.spec.schedulerName = %v, want kai-scheduler", got)
		}
	})

	t.Run("gpuRequestPerPod", func(t *testing.T) {
		containers, ok := podSpec["containers"].([]any)
		if !ok || len(containers) == 0 {
			t.Fatalf("spec.template.spec.containers missing or empty: %#v", podSpec["containers"])
		}
		container, ok := containers[0].(map[string]any)
		if !ok {
			t.Fatalf("containers[0] not a mapping: %#v", containers[0])
		}
		resources, ok := container["resources"].(map[string]any)
		if !ok {
			t.Fatalf("containers[0].resources missing or not a mapping: %#v", container["resources"])
		}
		limits, ok := resources["limits"].(map[string]any)
		if !ok {
			t.Fatalf("containers[0].resources.limits missing or not a mapping: %#v", resources["limits"])
		}
		if got := limits["nvidia.com/gpu"]; got != 8 {
			t.Errorf("nvidia.com/gpu limit = %v, want 8", got)
		}
	})

	// Measured, not theorized: without these two the gang is admitted and
	// grouped by kai-scheduler and then never placed, because every node
	// advertising nvidia.com/gpu on the demo path carries
	// kwok.x-k8s.io/node=fake:NoSchedule (test/e2e/lib.sh's e2e_node_yaml).
	// A real run on a KWOK cluster answered "no nodes with enough resources
	// were found: 4 node(s) had untolerated taint(s)" and Prove timed out,
	// cleaned up, and failed the run. No fake-clientset test can see this:
	// nothing in a fake cluster evaluates a taint.
	//
	// Pinned as a set, and pinned to KEYS rather than to "some tolerations
	// exist", because a catch-all toleration would satisfy the loose version
	// while destroying the claim this workload makes -- that a GPU-aware
	// scheduler chose GPU nodes, not that a pod was allowed anywhere.
	t.Run("tolerations", func(t *testing.T) {
		raw, ok := podSpec["tolerations"].([]any)
		if !ok {
			t.Fatalf("spec.template.spec.tolerations missing or not a list: %#v", podSpec["tolerations"])
		}
		got := map[string]map[string]any{}
		for i, item := range raw {
			tol, isMap := item.(map[string]any)
			if !isMap {
				t.Fatalf("tolerations[%d] not a mapping: %#v", i, item)
			}
			key, _ := tol["key"].(string)
			if key == "" {
				t.Fatalf("tolerations[%d] has no key -- a keyless toleration matches every taint: %#v", i, tol)
			}
			got[key] = tol
		}

		kwok, ok := got["kwok.x-k8s.io/node"]
		if !ok {
			t.Fatalf("no toleration for kwok.x-k8s.io/node; the simulated GPU nodes every demo and e2e run uses are unschedulable without it (have %v)", got)
		}
		if kwok["value"] != "fake" || kwok["effect"] != "NoSchedule" {
			t.Errorf("kwok toleration = %#v, want value=fake effect=NoSchedule", kwok)
		}

		gpu, ok := got["nvidia.com/gpu"]
		if !ok {
			t.Fatalf("no toleration for nvidia.com/gpu; a GKE GPU node pool is tainted with it (have %v)", got)
		}
		if gpu["operator"] != "Exists" || gpu["effect"] != "NoSchedule" {
			t.Errorf("nvidia.com/gpu toleration = %#v, want operator=Exists effect=NoSchedule", gpu)
		}
	})
}

// Fix round 1 (review F2): Render must build both label blocks from
// Labels(runID) rather than from static YAML text, so there is exactly one
// place that decides what the ownership selector looks for. This test would
// fail if the pod template's labels ever diverged from the Job's own -- the
// exact drift the structural fix (Render calling Labels once, injecting the
// same map into both blocks) rules out.
func TestPodTemplateCarriesTheSameOwnershipLabelsAsTheJob(t *testing.T) {
	out, err := prove.Render("run-abc", prove.Namespace)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	var obj map[string]any
	if err := yaml.Unmarshal(out, &obj); err != nil {
		t.Fatalf("Render() produced invalid YAML: %v", err)
	}
	podLabels := obj["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)["labels"].(map[string]any)
	for k, want := range map[string]string{
		"app.kubernetes.io/managed-by": "aicrme",
		"aicrme.dev/run-id":            "run-abc",
		"aicrme.dev/component":         "prove-workload",
	} {
		if got, _ := podLabels[k].(string); got != want {
			t.Errorf("pod template label %q = %q, want %q", k, got, want)
		}
	}
}

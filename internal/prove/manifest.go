// Package prove owns the reference workload the Prove step runs, and the
// identity that lets the console find it again after a restart.
//
// Identity rests on LABELS, not on the persisted run record. Terminal saves
// are best-effort and the store can degrade to memory (internal/engine/
// cmstore.go), so a console that could only find its workload via the record
// would lose track of it exactly when the record was lost -- while the
// workload kept holding GPUs.
package prove

import (
	_ "embed"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Namespace is dedicated and never shared with a recipe component, so Stop
// and reconciliation can reason about what they own.
const Namespace = "aicrme-prove"

//go:embed workload.yaml
var workloadYAML string

func Labels(runID string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": "aicrme",
		"aicrme.dev/run-id":            runID,
		"aicrme.dev/component":         "prove-workload",
	}
}

// WorkloadName is derived from the run ID rather than generated, so the same
// run always names the same object -- the property a restart depends on.
func WorkloadName(runID string) string { return "prove-" + runID }

// OwnedKinds enumerates what Prove creates. Stop and reconciliation act on
// this list, never on "everything in the namespace", so an object someone
// else put there is not collateral damage.
func OwnedKinds() []schema.GroupVersionResource {
	return []schema.GroupVersionResource{
		{Group: "batch", Version: "v1", Resource: "jobs"},
	}
}

func Render(runID, namespace string) ([]byte, error) {
	// PLACEHOLDER_NAMESPACE must precede PLACEHOLDER_NAME: PLACEHOLDER_NAME
	// is a prefix of PLACEHOLDER_NAMESPACE, and strings.Replacer resolves an
	// overlap by argument order, not by length. Listed the other way round,
	// every namespace substitution loses its "SPACE" suffix to the shorter,
	// earlier-listed pattern.
	r := strings.NewReplacer(
		"PLACEHOLDER_NAMESPACE", namespace,
		"PLACEHOLDER_NAME", WorkloadName(runID),
		"PLACEHOLDER_RUN_ID", runID,
	)
	out := r.Replace(workloadYAML)

	// A future edit to workload.yaml that adds a placeholder this replacer
	// does not know about must fail loudly here, not ship a manifest with a
	// literal PLACEHOLDER_ token baked into the field kai-scheduler or the
	// applier reads next.
	if strings.Contains(out, "PLACEHOLDER_") {
		return nil, fmt.Errorf("prove: workload.yaml has an unreplaced placeholder after rendering")
	}
	return []byte(out), nil
}

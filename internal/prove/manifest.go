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
	"sort"
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

// SpecHashAnnotation carries a hash of the workload as this process would
// create it. Apply compares it before deciding whether an existing object is
// the one it wants or a survivor of a differently-configured run.
//
// It exists because the workload name is derived from the run ID alone, so a
// retried Apply for the same run always addresses the same object -- and the
// rendered spec is no longer a pure function of the run. Client.extraTolerations
// comes from AICRME_GPU_TOLERATIONS, which is process configuration: two
// Applies of one run, from two differently-configured processes, legitimately
// want different Jobs. Without this the second is discarded in silence, which
// is how a toleration fix deployed mid-demo failed to reach the run it was
// deployed for -- observed on a real-cluster run, not reasoned about.
const SpecHashAnnotation = "aicrme.dev/spec-hash"

// OwnedKinds enumerates what Prove creates. Stop and reconciliation act on
// this list, never on "everything in the namespace", so an object someone
// else put there is not collateral damage.
//
// Not yet true in practice: Client.Delete, Client.WaitAbsent, and
// Client.ListOwned (client.go) hardcode the Job kind through the typed
// BatchV1().Jobs() surface rather than iterating this list. That is safe
// only as long as this returns exactly one entry. Spec §3 anticipates a
// second -- a Service and a ServiceAccount alongside the Job -- and the day
// this slice grows, those three methods must grow with it by hand, or
// whatever the new entry names goes un-deleted by Stop and undiscovered by
// reconciliation while this list keeps claiming otherwise.
func OwnedKinds() []schema.GroupVersionResource {
	return []schema.GroupVersionResource{
		{Group: "batch", Version: "v1", Resource: "jobs"},
	}
}

// labelBlock renders labels as YAML mapping lines at indent, sorted so
// output is deterministic.
func labelBlock(labels map[string]string, indent string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines := make([]string, len(keys))
	for i, k := range keys {
		lines[i] = indent + k + ": " + labels[k]
	}
	return strings.Join(lines, "\n")
}

// renderText is Render's implementation, taking the template as an argument
// so tests can exercise the unreplaced-placeholder guard against a
// deliberately incomplete template without touching the embedded
// workload.yaml.
func renderText(tmpl string, labels map[string]string, name, namespace string) (string, error) {
	// PLACEHOLDER_NAMESPACE must precede PLACEHOLDER_NAME: PLACEHOLDER_NAME
	// is a prefix of PLACEHOLDER_NAMESPACE, and strings.Replacer resolves an
	// overlap by argument order, not by length. Listed the other way round,
	// every namespace substitution loses its "SPACE" suffix to the shorter,
	// earlier-listed pattern. PLACEHOLDER_LABELS and PLACEHOLDER_POD_LABELS
	// share no prefix relationship with these or with each other, so their
	// position in this list does not matter.
	r := strings.NewReplacer(
		"PLACEHOLDER_NAMESPACE", namespace,
		"PLACEHOLDER_NAME", name,
		"PLACEHOLDER_LABELS", labelBlock(labels, "    "),
		"PLACEHOLDER_POD_LABELS", labelBlock(labels, "        "),
	)
	out := r.Replace(tmpl)

	// A future edit to workload.yaml that adds a placeholder this replacer
	// does not know about must fail loudly here, not ship a manifest with a
	// literal PLACEHOLDER_ token baked into the field kai-scheduler or the
	// applier reads next. This does NOT catch every possible substitution
	// defect -- an overlapping, partially-consumed token (the bug the
	// PLACEHOLDER_NAMESPACE/PLACEHOLDER_NAME ordering above fixed) leaves no
	// PLACEHOLDER_ substring behind at all, so this guard is narrower than it
	// looks: it only catches a wholly unregistered placeholder, not a
	// mis-ordered one.
	if strings.Contains(out, "PLACEHOLDER_") {
		return "", fmt.Errorf("prove: template has an unreplaced placeholder after rendering")
	}
	return out, nil
}

// Render builds the manifest from a single source of truth for identity:
// Labels(runID) supplies every label on both the Job and its pod template,
// so a selector built from Labels elsewhere (Task 4/8's ownership list) can
// never diverge from what actually landed on the objects it is meant to
// find.
func Render(runID, namespace string) ([]byte, error) {
	out, err := renderText(workloadYAML, Labels(runID), WorkloadName(runID), namespace)
	if err != nil {
		return nil, err
	}
	return []byte(out), nil
}

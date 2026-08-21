package prove

import (
	"strings"
	"testing"
)

// Fix round 1 (review F3): renderText's unreplaced-placeholder guard
// (manifest.go) sat in Render's untested 20%. This exercises it directly
// against a template carrying a placeholder no known token matches --
// exactly the case the guard exists for: a future workload.yaml edit that
// introduces a new PLACEHOLDER_ token without a matching substitution.
//
// It does NOT catch every substitution defect. An overlapping,
// partially-consumed token (the PLACEHOLDER_NAME/PLACEHOLDER_NAMESPACE bug
// fix round 0 found) leaves no PLACEHOLDER_ substring in the output at all,
// so it would slip past this guard too -- see renderText's own comment.
func TestRenderTextFailsOnAnUnregisteredPlaceholder(t *testing.T) {
	tmpl := "kind: Job\nmetadata:\n  name: PLACEHOLDER_MYSTERY\n"
	_, err := renderText(tmpl, Labels("run-abc"), WorkloadName("run-abc"), Namespace)
	if err == nil {
		t.Fatal("renderText() error = nil, want an error for an unregistered placeholder")
	}
	if !strings.Contains(err.Error(), "unreplaced placeholder") {
		t.Errorf("renderText() error = %q, want it to name the unreplaced placeholder", err)
	}
}

// The known placeholders still resolve correctly when renderText is called
// directly (not just through the embedded workload.yaml), so the guard test
// above is validating the failure path, not a template renderText can never
// actually satisfy.
func TestRenderTextSucceedsWhenEveryPlaceholderIsKnown(t *testing.T) {
	tmpl := "name: PLACEHOLDER_NAME\nnamespace: PLACEHOLDER_NAMESPACE\nlabels:\nPLACEHOLDER_LABELS\npod-labels:\nPLACEHOLDER_POD_LABELS\n"
	out, err := renderText(tmpl, Labels("run-abc"), WorkloadName("run-abc"), "somewhere-else")
	if err != nil {
		t.Fatalf("renderText() error = %v, want nil", err)
	}
	if strings.Contains(out, "PLACEHOLDER_") {
		t.Errorf("renderText() left a placeholder unresolved: %q", out)
	}
}

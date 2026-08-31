package clear

import (
	"context"
	"errors"
	"strings"
	"testing"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/mchmarny/aicrme/internal/aicrclient"
)

func surveyClient() *aicrclient.Fake {
	return &aicrclient.Fake{
		CatalogEntries: []aicr.CatalogEntry{{Name: "eks", Criteria: aicr.Criteria{Service: "eks"}}},
		Recipe: &aicr.RecipeResult{Components: []aicr.ComponentRef{
			{Name: "gpu-operator", Chart: "gpu-operator", Namespace: "gpu-operator"},
			{Name: "cert-manager", Chart: "cert-manager", Namespace: "cert-manager"},
			{Name: "nodewright-operator", Chart: "nodewright-operator", Namespace: "skyhook"},
		}},
	}
}

func surveyExec() *fakeExec {
	return &fakeExec{out: map[string]string{
		"helm list -A": `[
			{"name":"gpu-operator","namespace":"gpu-operator","revision":"1","updated":"2026-08-30T20:40:00Z","status":"deployed","chart":"gpu-operator-v26.3.3"},
			{"name":"cert-manager","namespace":"cert-manager","revision":"14","updated":"2026-08-30T20:41:00Z","status":"deployed","chart":"cert-manager-1.20.2"},
			{"name":"nodewright-operator","namespace":"skyhook","revision":"1","updated":"2026-08-30T20:42:00Z","status":"deployed","chart":"nodewright-operator-v0.17.1"},
			{"name":"postgres","namespace":"data","revision":"3","updated":"2026-08-30T20:43:00Z","status":"deployed","chart":"postgresql-16.1.0"}
		]`,
		"helm history gpu-operator":        `[{"revision":1,"updated":"2026-08-30T20:29:00Z","status":"deployed"}]`,
		"helm history cert-manager":        `[{"revision":1,"updated":"2026-01-02T09:00:00Z","status":"superseded"}]`,
		"helm history nodewright-operator": `[{"revision":1,"updated":"2026-08-30T20:30:00Z","status":"deployed"}]`,
		"kubectl get daemonset":            "gpu-operator\n",
	}}
}

func surveyor(e Exec, c aicrclient.API) *Surveyor { return &Surveyor{Exec: e, Client: c} }

// A release outside the universe must not appear at all. Listing somebody
// else's postgres, even unticked, invites an operator to remove a chart this
// console knows nothing about.
func TestSurveyOmitsReleasesOutsideTheUniverse(t *testing.T) {
	got, err := surveyor(surveyExec(), surveyClient()).Survey(context.Background(), "uid-1")
	if err != nil {
		t.Fatalf("Survey() error = %v", err)
	}
	for _, r := range got.Releases {
		if r.Name == "postgres" {
			t.Fatal("postgres is in the survey; it is not an AICR chart")
		}
	}
	if len(got.Releases) != 3 {
		t.Fatalf("got %d releases, want 3", len(got.Releases))
	}
}

func TestSurveyAppliesTheSelectionRuleEndToEnd(t *testing.T) {
	got, err := surveyor(surveyExec(), surveyClient()).Survey(context.Background(), "uid-1")
	if err != nil {
		t.Fatalf("Survey() error = %v", err)
	}
	if !got.Complete {
		t.Fatalf("Complete = false with every probe answering: %q", got.Incomplete)
	}
	if got.ClusterUID != "uid-1" {
		t.Errorf("ClusterUID = %q, want the connected cluster's", got.ClusterUID)
	}
	if got.DriverMode != DriverManaged {
		t.Errorf("DriverMode = %q, want managed", got.DriverMode)
	}
	by := byName(got.Releases)
	if !by["gpu-operator"].Recommended {
		t.Error("gpu-operator not recommended")
	}
	if by["cert-manager"].Recommended {
		t.Error("cert-manager recommended; first deployed in January")
	}
	if by["nodewright-operator"].Recommended || !by["nodewright-operator"].NodeLevel {
		t.Error("nodewright-operator must be flagged node-level and never recommended")
	}
}

// An incomplete universe means chart matching may have produced false
// negatives, so nothing may be recommended -- and the survey has to say why
// rather than looking like a confident empty answer.
func TestSurveyRecommendsNothingWhenTheUniverseIsIncomplete(t *testing.T) {
	c := surveyClient()
	c.CatalogEntries = append(c.CatalogEntries, aicr.CatalogEntry{Name: "gke", Criteria: aicr.Criteria{Service: "gke"}})
	c.ResolveErrs = map[string]error{"gke": errors.New("unresolvable")}

	got, err := surveyor(surveyExec(), c).Survey(context.Background(), "uid-1")
	if err != nil {
		t.Fatalf("Survey() error = %v", err)
	}
	if got.Complete {
		t.Fatal("Complete = true with an unresolvable catalog entry")
	}
	if got.Incomplete == "" {
		t.Error("Incomplete carries no explanation")
	}
	for _, r := range got.Releases {
		if r.Recommended {
			t.Errorf("%s recommended on an incomplete universe", r.Name)
		}
	}
}

// A release whose history is unreadable stays in the list -- dropping it would
// hide an installed component -- and makes the whole survey incomplete.
func TestSurveyKeepsAReleaseWhoseHistoryIsUnreadableAndGoesIncomplete(t *testing.T) {
	e := surveyExec()
	e.err = map[string]error{"helm history cert-manager": errors.New("release not found")}

	got, err := surveyor(e, surveyClient()).Survey(context.Background(), "uid-1")
	if err != nil {
		t.Fatalf("Survey() error = %v", err)
	}
	if got.Complete {
		t.Error("Complete = true with an unreadable history")
	}
	var found bool
	for _, r := range got.Releases {
		if r.Name == "cert-manager" {
			found = true
		}
		if r.Recommended {
			t.Errorf("%s recommended while evidence is incomplete", r.Name)
		}
	}
	if !found {
		t.Error("cert-manager dropped; an unjudgeable release must still be reported")
	}
}

// An unknown driver mode is itself incomplete: the operator is missing the
// warning that decides whether their nodes need rebooting.
func TestSurveyIsIncompleteWhenDriverModeIsUnknown(t *testing.T) {
	e := surveyExec()
	e.err = map[string]error{"kubectl get daemonset": errors.New("timeout")}

	got, err := surveyor(e, surveyClient()).Survey(context.Background(), "uid-1")
	if err != nil {
		t.Fatalf("Survey() error = %v", err)
	}
	if got.DriverMode != DriverUnknown {
		t.Errorf("DriverMode = %q, want unknown", got.DriverMode)
	}
	if got.Complete {
		t.Error("Complete = true with driver mode unknown")
	}
}

// Two releases can share a name in different namespaces. Keying anything on
// name alone silently drops one of them.
func TestSurveyKeepsTwoReleasesThatShareAName(t *testing.T) {
	e := surveyExec()
	e.out["helm list -A"] = `[
		{"name":"cert-manager","namespace":"cert-manager","revision":"1","updated":"2026-08-30T20:41:00Z","status":"deployed","chart":"cert-manager-1.20.2"},
		{"name":"cert-manager","namespace":"tenant-b","revision":"1","updated":"2026-08-30T20:41:00Z","status":"deployed","chart":"cert-manager-1.20.2"}
	]`
	e.out["helm history cert-manager"] = `[{"revision":1,"updated":"2026-08-30T20:29:00Z","status":"deployed"}]`

	got, err := surveyor(e, surveyClient()).Survey(context.Background(), "uid-1")
	if err != nil {
		t.Fatalf("Survey() error = %v", err)
	}
	if len(got.Releases) != 2 {
		t.Fatalf("got %d releases, want both namespaces", len(got.Releases))
	}
}

// The survey must never mutate, asserted over the whole assembled path.
func TestSurveyRunsNoMutatingCommand(t *testing.T) {
	e := surveyExec()
	// ReadOnly is what the production wiring installs; assert the assembled
	// path is compatible with it rather than trusting that it is.
	if _, err := surveyor(ReadOnly(e), surveyClient()).Survey(context.Background(), "uid-1"); err != nil {
		t.Fatalf("Survey() error = %v; the read-only guard rejected one of its own commands", err)
	}
	for _, argv := range e.argv {
		if !readOnlyCommands[argv[0]+" "+argv[1]] {
			t.Fatalf("survey ran a command outside the whitelist: %s", strings.Join(argv, " "))
		}
	}
}

// A failing helm is fatal. Reporting an empty cluster would be the one wrong
// answer an operator acts on destructively.
func TestSurveyFailsWhenReleasesCannotBeListed(t *testing.T) {
	e := surveyExec()
	e.err = map[string]error{"helm list -A": errors.New("helm: not found")}

	if _, err := surveyor(e, surveyClient()).Survey(context.Background(), "uid-1"); err == nil {
		t.Fatal("Survey succeeded with no helm")
	}
}

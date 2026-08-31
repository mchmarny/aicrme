package clear

import (
	"context"
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func byName(rs []Release) map[string]Release {
	m := map[string]Release{}
	for _, r := range rs {
		m[r.Name] = r
	}
	return m
}

// helm list reports LAST-updated, so a release at revision 14 touched this
// morning looks as new as one installed today -- which is why first-deployed
// is fetched separately at all.
func TestRecommendExcludesAReleaseThatPredatesTheInstall(t *testing.T) {
	got := byName(recommend([]Release{
		{Name: "gpu-operator", FirstDeployed: at("2026-08-30T20:29:00Z")},
		{Name: "kai-scheduler", FirstDeployed: at("2026-08-30T20:31:00Z")},
		{Name: "cert-manager", FirstDeployed: at("2026-01-02T09:00:00Z")},
	}, true))

	if !got["gpu-operator"].Recommended {
		t.Error("gpu-operator not recommended; it is part of the newest install")
	}
	if got["cert-manager"].Recommended {
		t.Error("cert-manager recommended; it predates the install by months")
	}
	if got["cert-manager"].Reason == "" {
		t.Error("no reason given; an operator cannot act on a silence")
	}
}

// A retry re-runs `helm upgrade --install`, making revision 2. Keying on
// `revision == 1` would drop exactly the component whose install was retried.
func TestRecommendIncludesARetriedComponentAtRevisionTwo(t *testing.T) {
	for _, r := range recommend([]Release{
		{Name: "gpu-operator", Revision: 1, FirstDeployed: at("2026-08-30T20:29:00Z")},
		{Name: "nvsentinel", Revision: 2, FirstDeployed: at("2026-08-30T20:35:00Z")},
	}, true) {
		if !r.Recommended {
			t.Errorf("%s (revision %d) not recommended; both are from the same install", r.Name, r.Revision)
		}
	}
}

// INCOMPLETE EVIDENCE RECOMMENDS NOTHING. Revision 1 dropped a release with an
// unreadable history from the comparison set, which left the "newest matched
// release" anchor computed from a subset -- so an older release could be
// recommended against an anchor that was wrong.
func TestRecommendRecommendsNothingWhenEvidenceIsIncomplete(t *testing.T) {
	got := recommend([]Release{
		{Name: "gpu-operator", FirstDeployed: at("2026-08-30T20:29:00Z")},
		{Name: "kai-scheduler", FirstDeployed: at("2026-08-30T20:31:00Z")},
	}, false)

	for _, r := range got {
		if r.Recommended {
			t.Errorf("%s recommended on incomplete evidence", r.Name)
		}
		if r.Reason == "" {
			t.Errorf("%s carries no reason for not being recommended", r.Name)
		}
	}
}

// A release with no first-deployed timestamp is itself incomplete evidence,
// and must not be silently excluded from the anchor.
func TestRecommendTreatsAMissingTimestampAsIncomplete(t *testing.T) {
	for _, r := range recommend([]Release{
		{Name: "gpu-operator", FirstDeployed: at("2026-08-30T20:29:00Z")},
		{Name: "cert-manager"}, // zero time: history unreadable
	}, true) {
		if r.Recommended {
			t.Errorf("%s recommended while a sibling has no first-deployed date", r.Name)
		}
	}
}

// Node-level effects are GRUB parameters and sysctls that no CR deletion
// reverts, so removing the operator leaves the node modified with nothing left
// that knows how to undo it.
func TestRecommendNeverRecommendsANodeLevelComponent(t *testing.T) {
	for _, r := range recommend([]Release{
		{Name: "gpu-operator", FirstDeployed: at("2026-08-30T20:29:00Z")},
		{Name: "nodewright-operator", Namespace: "skyhook", NodeLevel: true, FirstDeployed: at("2026-08-30T20:30:00Z")},
	}, true) {
		if r.NodeLevel && r.Recommended {
			t.Errorf("%s recommended; node-level effects cannot be reverted", r.Name)
		}
		if r.NodeLevel && r.Reason == "" {
			t.Error("node-level release carries no reason")
		}
	}
}

func TestIsNodeLevelMatchesNodewrightAndTheSkyhookNamespace(t *testing.T) {
	for _, tc := range []struct {
		component, namespace string
		want                 bool
	}{
		{"nodewright-operator", "skyhook", true},
		{"nodewright-customizations", "kube-system", true},
		{"anything", "skyhook", true},
		{"gpu-operator", "gpu-operator", false},
		{"cert-manager", "cert-manager", false},
	} {
		if got := isNodeLevel(tc.component, tc.namespace); got != tc.want {
			t.Errorf("isNodeLevel(%q, %q) = %v, want %v", tc.component, tc.namespace, got, tc.want)
		}
	}
}

func TestRecommendHandlesNoReleases(t *testing.T) {
	if got := recommend(nil, true); len(got) != 0 {
		t.Errorf("recommend(nil) = %v, want empty", got)
	}
}

func TestFirstDeployedReadsRevisionOneFromHelmHistory(t *testing.T) {
	e := &fakeExec{out: map[string]string{
		"helm history gpu-operator": `[
			{"revision":1,"updated":"2026-08-30T20:29:00Z","status":"superseded"},
			{"revision":2,"updated":"2026-08-30T21:00:00Z","status":"deployed"}
		]`,
	}}

	got, err := firstDeployed(context.Background(), e, "gpu-operator", "gpu-operator")
	if err != nil {
		t.Fatalf("firstDeployed() error = %v", err)
	}
	if !got.Equal(at("2026-08-30T20:29:00Z")) {
		t.Errorf("firstDeployed = %v, want revision 1's timestamp, not the latest", got)
	}
}

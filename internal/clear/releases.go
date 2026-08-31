package clear

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mchmarny/aicrme/internal/aicrclient"
)

// helmPageSize is helm's own default --max. Paging in the same size it already
// uses keeps each call's cost identical to an unpaged one.
const helmPageSize = 256

// helmRelease is helm's `list -o json` row.
//
// Revision is a string because that is what helm emits here -- and a NUMBER in
// `helm history -o json`, which is why the two have separate types rather than
// one that would have to lie about one of them.
type helmRelease struct {
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	Revision   string `json:"revision"`
	Updated    string `json:"updated"`
	Status     string `json:"status"`
	Chart      string `json:"chart"`
	AppVersion string `json:"app_version"`
}

// listReleases returns every live helm release on the cluster.
//
// PAGED, because helm defaults to --max 256 and a cluster past that would
// silently report a subset -- a component past the cut invisible in a survey
// whose whole job is completeness.
//
// STATUSES SELECTED EXPLICITLY, because helm 3 and helm 4 disagree on the
// default and helm 4 can include uninstalled history. Deployed, failed and
// pending are the live ones; a failed release still owns its objects and still
// blocks a reinstall, so it belongs in the survey.
//
// An error is fatal to the survey rather than an empty list: an empty survey
// renders as "this cluster is clean", the one wrong answer an operator would
// act on destructively.
func listReleases(ctx context.Context, e Exec) ([]helmRelease, error) {
	var all []helmRelease
	for offset := 0; ; offset += helmPageSize {
		argv := []string{
			"helm", "list", "-A", "-o", "json",
			"--deployed", "--failed", "--pending",
			"--max", strconv.Itoa(helmPageSize),
		}
		if offset > 0 {
			argv = append(argv, "--offset", strconv.Itoa(offset))
		}
		var buf bytes.Buffer
		if err := e.Run(ctx, argv, &buf); err != nil {
			return nil, fmt.Errorf("listing helm releases: %w", err)
		}
		var page []helmRelease
		if err := json.Unmarshal(buf.Bytes(), &page); err != nil {
			return nil, fmt.Errorf("parsing helm's release list: %w", err)
		}
		all = append(all, page...)
		if len(page) < helmPageSize {
			return all, nil
		}
	}
}

// looksLikeVersion reports whether s is plausibly a chart version.
//
// The guard on prefix matching. Without it "cert-manager-addon-1.0.0" reads as
// cert-manager at version "addon-1.0.0", and the survey offers to remove a
// third-party addon as if this console had installed it.
//
// Deliberately loose: charts version themselves inconsistently (1.20.2,
// v26.3.3, 1.0.0-rc1) and this only has to separate a version from a
// name fragment. A leading digit, optionally preceded by "v", does that.
func looksLikeVersion(s string) bool {
	if s == "" {
		return false
	}
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return false
	}
	return s[0] >= '0' && s[0] <= '9'
}

// matchChart resolves helm's "<chart>-<version>" field against the universe.
//
// Matched by prefix against known chart names rather than by splitting on the
// last dash -- both halves contain dashes -- and then validated: the remainder
// must look like a version. Longest match wins, so a chart whose name prefixes
// another cannot shadow it.
func matchChart(field string, charts map[string]aicrclient.Component) (aicrclient.Component, string, bool) {
	var best aicrclient.Component
	var version string
	found := false
	for chart, c := range charts {
		prefix := chart + "-"
		if !strings.HasPrefix(field, prefix) {
			continue
		}
		suffix := strings.TrimPrefix(field, prefix)
		if !looksLikeVersion(suffix) {
			continue
		}
		if found && len(best.Chart) >= len(chart) {
			continue
		}
		best, version, found = c, suffix, true
	}
	return best, version, found
}

// helmTimeLayouts are the two shapes helm emits. RFC3339 is what its JSON
// marshaller produces; the second is Go's default String() form, which some
// builds emit instead. Both are tried because the failure is silent and
// severe -- an unparsed date becomes the zero value, every release then falls
// inside the selection window, and the survey recommends removing all of them.
var helmTimeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02 15:04:05.999999999 -0700 MST",
}

func parseHelmTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range helmTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised helm timestamp %q", s)
}

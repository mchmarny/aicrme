package clear

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/mchmarny/aicrme/internal/aicrclient"
)

// fakeExec replays canned stdout per command prefix, keyed on the first three
// argv words. pageBy lets a test script successive `helm list` pages.
type fakeExec struct {
	out   map[string]string
	err   map[string]error
	pages []string
	argv  [][]string
}

func (f *fakeExec) Run(_ context.Context, argv []string, out io.Writer) error {
	f.argv = append(f.argv, argv)
	joined := strings.Join(argv, " ")
	key := strings.Join(argv[:min(3, len(argv))], " ")
	if e, ok := f.err[key]; ok {
		return e
	}
	if len(f.pages) > 0 && strings.HasPrefix(key, "helm list") {
		page := f.pages[0]
		f.pages = f.pages[1:]
		_, _ = io.WriteString(out, page)
		return nil
	}
	if s, ok := f.out[key]; ok {
		_, _ = io.WriteString(out, s)
		return nil
	}
	if s, ok := f.out[joined]; ok {
		_, _ = io.WriteString(out, s)
	}
	return nil
}

func universe() map[string]aicrclient.Component {
	return map[string]aicrclient.Component{
		"gpu-operator":          {Name: "gpu-operator", Chart: "gpu-operator", Namespace: "gpu-operator", Order: 3},
		"cert-manager":          {Name: "cert-manager", Chart: "cert-manager", Namespace: "cert-manager", Order: 9},
		"nvidia-dra-driver-gpu": {Name: "nvidia-dra-driver-gpu", Chart: "nvidia-dra-driver-gpu", Namespace: "nvidia-dra-driver", Order: 5},
	}
}

// The chart field is "<chart>-<version>" and BOTH halves contain dashes, so
// splitting on the last dash is wrong: "cert-manager-1.0.0-rc1" would yield
// chart "cert-manager-1.0.0". Matching against known chart names is exact.
func TestMatchChartHandlesDashesInBothHalves(t *testing.T) {
	for _, tc := range []struct{ field, chart, version string }{
		{"gpu-operator-v26.3.3", "gpu-operator", "v26.3.3"},
		{"nvidia-dra-driver-gpu-0.4.1", "nvidia-dra-driver-gpu", "0.4.1"},
		{"cert-manager-1.20.2", "cert-manager", "1.20.2"},
		{"cert-manager-1.0.0-rc1", "cert-manager", "1.0.0-rc1"},
	} {
		got, version, ok := matchChart(tc.field, universe())
		if !ok {
			t.Errorf("matchChart(%q) did not match", tc.field)
			continue
		}
		if got.Chart != tc.chart || version != tc.version {
			t.Errorf("matchChart(%q) = (%q, %q), want (%q, %q)", tc.field, got.Chart, version, tc.chart, tc.version)
		}
	}
}

// A DIFFERENT CHART THAT MERELY STARTS WITH A KNOWN NAME IS NOT THAT CHART.
// Prefix matching alone reads "cert-manager-addon-1.0.0" as cert-manager at
// version "addon-1.0.0" -- and would then offer to remove a third-party addon
// as if this console had installed it.
func TestMatchChartRejectsAChartThatMerelySharesAPrefix(t *testing.T) {
	for _, field := range []string{
		"cert-manager-addon-1.0.0",
		"gpu-operator-dashboards-2.0.0",
	} {
		if c, v, ok := matchChart(field, universe()); ok {
			t.Errorf("matchChart(%q) matched %q at version %q; the suffix is not a version", field, c.Chart, v)
		}
	}
}

func TestMatchChartRejectsAnUnknownChart(t *testing.T) {
	if _, _, ok := matchChart("postgresql-16.1.0", universe()); ok {
		t.Error("matched a chart outside the universe")
	}
}

func TestLooksLikeVersion(t *testing.T) {
	for _, tc := range []struct {
		s    string
		want bool
	}{
		{"1.20.2", true}, {"v26.3.3", true}, {"0.4.1", true}, {"1.0.0-rc1", true},
		{"addon-1.0.0", false}, {"dashboards-2.0.0", false}, {"", false}, {"latest", false},
	} {
		if got := looksLikeVersion(tc.s); got != tc.want {
			t.Errorf("looksLikeVersion(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
}

// Helm's JSON marshals RFC3339, but some builds emit Go's default layout.
// An unparsed date becomes the zero value, every release then falls inside the
// selection window, and the survey recommends removing all of them.
func TestParseHelmTimeAcceptsBothLayouts(t *testing.T) {
	for _, s := range []string{
		"2026-08-30T20:28:31.123456789Z",
		"2026-08-30 20:28:31.123456789 +0000 UTC",
	} {
		got, err := parseHelmTime(s)
		if err != nil {
			t.Errorf("parseHelmTime(%q) error = %v", s, err)
			continue
		}
		if got.Year() != 2026 || got.Month() != 8 || got.Day() != 30 {
			t.Errorf("parseHelmTime(%q) = %v, want 2026-08-30", s, got)
		}
	}
}

func TestParseHelmTimeRejectsGarbage(t *testing.T) {
	if _, err := parseHelmTime("not a time"); err == nil {
		t.Error("parseHelmTime accepted garbage")
	}
}

// helm list defaults to --max 256. A cluster with more releases than that
// would silently report a subset, and a component past the cut would be
// invisible in a survey whose whole job is completeness.
func TestListReleasesPagesUntilExhausted(t *testing.T) {
	full := make([]string, 0, 256)
	for i := range 256 {
		full = append(full, fmt.Sprintf(
			`{"name":"r%d","namespace":"ns","revision":"1","updated":"2026-08-30T20:29:00Z","status":"deployed","chart":"cert-manager-1.20.2"}`, i))
	}
	e := &fakeExec{pages: []string{
		"[" + strings.Join(full, ",") + "]",
		`[{"name":"last","namespace":"ns","revision":"1","updated":"2026-08-30T20:29:00Z","status":"deployed","chart":"cert-manager-1.20.2"}]`,
	}}

	got, err := listReleases(context.Background(), e)
	if err != nil {
		t.Fatalf("listReleases() error = %v", err)
	}
	if len(got) != 257 {
		t.Fatalf("got %d releases, want 257 -- the second page was not fetched", len(got))
	}
	if len(e.argv) != 2 {
		t.Fatalf("ran %d list commands, want 2 pages", len(e.argv))
	}
	if !strings.Contains(strings.Join(e.argv[1], " "), "--offset") {
		t.Errorf("second page did not use --offset: %v", e.argv[1])
	}
}

// helm 3 and helm 4 disagree on which statuses list returns by default, and
// helm 4 can include uninstalled history. Selecting explicitly is what makes
// the answer the same on both.
func TestListReleasesSelectsLiveStatusesExplicitly(t *testing.T) {
	e := &fakeExec{out: map[string]string{"helm list -A": "[]"}}

	if _, err := listReleases(context.Background(), e); err != nil {
		t.Fatalf("listReleases() error = %v", err)
	}
	joined := strings.Join(e.argv[0], " ")
	for _, want := range []string{"--deployed", "--failed", "--pending"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %q missing %s; helm 3 and 4 default differently", joined, want)
		}
	}
	if strings.Contains(joined, "--uninstalled") {
		t.Error("argv selects uninstalled releases; those are history, not installs")
	}
}

// An empty survey renders as "this cluster is clean", which is the one wrong
// answer an operator would act on destructively.
func TestListReleasesFailsRatherThanReportingAnEmptyCluster(t *testing.T) {
	e := &fakeExec{err: map[string]error{"helm list -A": errors.New("helm: not found")}}
	if _, err := listReleases(context.Background(), e); err == nil {
		t.Fatal("listReleases succeeded with no helm")
	}
}

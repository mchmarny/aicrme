package gap_test

import (
	"os"
	"strings"
	"testing"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/mchmarny/aicrme/internal/gap"
	"gopkg.in/yaml.v3"
)

func loadFixture(t *testing.T, name string) *aicr.Snapshot {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var s snapshotter.Snapshot
	if err := yaml.Unmarshal(raw, &s); err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	wrapped := aicr.WrapSnapshot(&s)
	wrapped.Raw = raw
	return wrapped
}

func TestAnalyzeKWOK(t *testing.T) {
	report := gap.Analyze(loadFixture(t, "snapshot-kwok.yaml"), gap.ClusterGPUs{})

	if report.Headline == "" {
		t.Error("Analyze produced no headline — Discover opens with a capability statement")
	}
	if report.TotalGPUs != 0 {
		t.Errorf("TotalGPUs = %d, want 0 on a KWOK cluster", report.TotalGPUs)
	}
	if report.UsableGPUs != 0 {
		t.Errorf("UsableGPUs = %d, want 0 on a KWOK cluster", report.UsableGPUs)
	}
	if report.Punchline == "" {
		t.Error("Analyze produced no punchline — the finale calls back to this number")
	}
}

func TestAnalyzeNilSnapshotIsSafe(t *testing.T) {
	report := gap.Analyze(nil, gap.ClusterGPUs{})
	if report.Headline == "" {
		t.Error("Analyze(nil) must still produce a renderable report")
	}
	if len(report.Gaps) != 0 {
		t.Errorf("Analyze(nil) produced %d gaps, want 0", len(report.Gaps))
	}
}

// TestAnalyzeBareSnapshotIsSafe covers the shape aicrclient.Fake{} returns
// when its Snapshot field is left unset: &aicr.Snapshot{} with no internal
// payload. Unwrap() on that reconstructs a minimal, non-nil
// snapshotter.Snapshot with a nil Measurements slice — Analyze must not panic
// ranging over it.
func TestAnalyzeBareSnapshotIsSafe(t *testing.T) {
	report := gap.Analyze(&aicr.Snapshot{}, gap.ClusterGPUs{})
	if report.Headline == "" {
		t.Error("Analyze(&aicr.Snapshot{}) must still produce a renderable report")
	}
	if len(report.Gaps) != 0 {
		t.Errorf("Analyze(&aicr.Snapshot{}) produced %d gaps, want 0", len(report.Gaps))
	}
}

func TestEveryGapNamesItsClosingComponent(t *testing.T) {
	report := gap.Analyze(loadFixture(t, "snapshot-kwok.yaml"), gap.ClusterGPUs{})
	if len(report.Gaps) == 0 {
		t.Fatal("no gaps fired against the KWOK fixture — expected gpu-driver to fire")
	}
	for _, g := range report.Gaps {
		if g.Component == "" {
			t.Errorf("gap %q names no closing component — the Discover screen must pre-explain Apply", g.ID)
		}
		if g.Title == "" {
			t.Errorf("gap %q has no title", g.ID)
		}
	}
}

// TestGapRulesFireAgainstFixture is the table-driven check Task 9 Step 5
// calls for: one row per rule the KWOK fixture actually proves. EFA plugin
// has no row — see the package comment on rules in internal/gap/rules.go for
// why it stays deferred to a real EKS fixture.
func TestGapRulesFireAgainstFixture(t *testing.T) {
	report := gap.Analyze(loadFixture(t, "snapshot-kwok.yaml"), gap.ClusterGPUs{})

	fired := map[string]bool{}
	for _, g := range report.Gaps {
		fired[g.ID] = true
	}

	tests := []struct {
		id        string
		wantFire  bool
		component string
	}{
		{id: "gpu-driver", wantFire: true, component: "gpu-operator"},
		{id: "device-plugin", wantFire: true, component: "gpu-operator"},
		{id: "gpu-metrics", wantFire: true, component: "gpu-operator"},
		{id: "gpu-scheduler", wantFire: true, component: "kai-scheduler"},
	}

	for _, tc := range tests {
		t.Run(tc.id, func(t *testing.T) {
			if fired[tc.id] != tc.wantFire {
				t.Errorf("gap %q fired = %v, want %v", tc.id, fired[tc.id], tc.wantFire)
			}
			for _, g := range report.Gaps {
				if g.ID == tc.id && g.Component != tc.component {
					t.Errorf("gap %q Component = %q, want %q", tc.id, g.Component, tc.component)
				}
			}
		})
	}
	if len(report.Gaps) != len(tests) {
		t.Errorf("Analyze produced %d gaps, want exactly %d (%v)", len(report.Gaps), len(tests), fired)
	}
}

// k8sSnapshot builds a synthetic snapshot carrying a single TypeK8s
// measurement with the given subtypes, so gpuOperatorAbsent's degraded-
// collector guard and gpuSchedulerAbsent's image-list check can be exercised
// with combinations the KWOK fixture alone cannot produce (it only ever shows
// the "GPU Operator genuinely absent" case).
func k8sSnapshot(subtypes ...measurement.Subtype) *aicr.Snapshot {
	return aicr.WrapSnapshot(&snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{
			{Type: measurement.TypeK8s, Subtypes: subtypes},
		},
	})
}

// TestDegradedK8sCollectorEmitsNoGuessedGaps is the explicit degraded-
// collector case the review called for: an empty K8s.policy is only evidence
// of a genuinely absent GPU Operator when K8s.image is non-empty, proving the
// collector actually ran. When both are empty — what
// pkg/collector/k8s/k8s.go's emptyK8sMeasurement produces on a client or
// discovery failure — device-plugin and gpu-metrics must not fire.
func TestDegradedK8sCollectorEmitsNoGuessedGaps(t *testing.T) {
	tests := []struct {
		name     string
		subtypes []measurement.Subtype
		wantIDs  map[string]bool
	}{
		{
			name: "healthy collector, GPU Operator genuinely absent",
			subtypes: []measurement.Subtype{
				{Name: "image", Data: map[string]measurement.Reading{"coredns": measurement.Str("v1.13.1")}},
				{Name: "policy", Data: map[string]measurement.Reading{}},
			},
			wantIDs: map[string]bool{"device-plugin": true, "gpu-metrics": true, "gpu-scheduler": true},
		},
		{
			name: "degraded collector: image and policy both empty",
			subtypes: []measurement.Subtype{
				{Name: "image", Data: map[string]measurement.Reading{}},
				{Name: "policy", Data: map[string]measurement.Reading{}},
			},
			wantIDs: map[string]bool{},
		},
		{
			name: "ClusterPolicy present: GPU Operator genuinely installed",
			subtypes: []measurement.Subtype{
				{Name: "image", Data: map[string]measurement.Reading{"k8s-device-plugin": measurement.Str("v0.17.4")}},
				{Name: "policy", Data: map[string]measurement.Reading{"devicePlugin.enabled": measurement.Str("true")}},
			},
			wantIDs: map[string]bool{"gpu-scheduler": true},
		},
		{
			name: "policy subtype missing entirely, not just collected-empty",
			subtypes: []measurement.Subtype{
				{Name: "image", Data: map[string]measurement.Reading{"coredns": measurement.Str("v1.13.1")}},
			},
			wantIDs: map[string]bool{"gpu-scheduler": true},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			report := gap.Analyze(k8sSnapshot(tc.subtypes...), gap.ClusterGPUs{})
			got := map[string]bool{}
			for _, g := range report.Gaps {
				got[g.ID] = true
			}
			for id := range tc.wantIDs {
				if !got[id] {
					t.Errorf("gap %q did not fire, want fire", id)
				}
			}
			for id := range got {
				if !tc.wantIDs[id] {
					t.Errorf("gap %q fired, want no fire", id)
				}
			}
		})
	}
}

// TestHeadlineWithoutProviderIsGrammatical locks in the Minor 4 fix: a
// snapshot whose K8s.node.provider is unreadable must not produce
// "This is a the cluster...".
func TestHeadlineWithoutProviderIsGrammatical(t *testing.T) {
	report := gap.Analyze(k8sSnapshot(measurement.Subtype{
		Name: "server",
		Data: map[string]measurement.Reading{measurement.KeyVersion: measurement.Str("1.30.0")},
	}), gap.ClusterGPUs{})
	if report.Headline == "" {
		t.Fatal("Headline is empty when provider is unreadable")
	}
	if strings.Contains(report.Headline, "a the cluster") {
		t.Errorf("Headline = %q, contains broken %q copy", report.Headline, "a the cluster")
	}
}

// gpuSnapshot builds a synthetic snapshot carrying a single TypeGPU
// "hardware" subtype, so usableGPUs' branches (hardware present, driver
// loaded) can be exercised beyond what the all-false KWOK fixture proves. It
// does not add a new rule or key — every key here is one Task 9 Step 1 already
// verified against the real snapshotter.
func gpuSnapshot(data map[string]measurement.Reading) *aicr.Snapshot {
	return aicr.WrapSnapshot(&snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{
			{
				Type:     measurement.TypeGPU,
				Subtypes: []measurement.Subtype{{Name: "hardware", Data: data}},
			},
		},
	})
}

func TestUsableGPUs(t *testing.T) {
	tests := []struct {
		name       string
		data       map[string]measurement.Reading
		wantTotal  int
		wantUsable int
	}{
		{
			name: "present and driver loaded",
			data: map[string]measurement.Reading{
				measurement.KeyGPUCount:        measurement.Int64(8),
				measurement.KeyGPUPresent:      measurement.Bool(true),
				measurement.KeyGPUDriverLoaded: measurement.Bool(true),
			},
			wantTotal:  8,
			wantUsable: 8,
		},
		{
			name: "present but driver not loaded",
			data: map[string]measurement.Reading{
				measurement.KeyGPUCount:        measurement.Int64(8),
				measurement.KeyGPUPresent:      measurement.Bool(true),
				measurement.KeyGPUDriverLoaded: measurement.Bool(false),
			},
			wantTotal:  8,
			wantUsable: 0,
		},
		{
			name: "counted but not present",
			data: map[string]measurement.Reading{
				measurement.KeyGPUCount:        measurement.Int64(8),
				measurement.KeyGPUPresent:      measurement.Bool(false),
				measurement.KeyGPUDriverLoaded: measurement.Bool(true),
			},
			wantTotal:  8,
			wantUsable: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			report := gap.Analyze(gpuSnapshot(tc.data), gap.ClusterGPUs{})
			if report.TotalGPUs != tc.wantTotal {
				t.Errorf("TotalGPUs = %d, want %d", report.TotalGPUs, tc.wantTotal)
			}
			if report.UsableGPUs != tc.wantUsable {
				t.Errorf("UsableGPUs = %d, want %d", report.UsableGPUs, tc.wantUsable)
			}
		})
	}
}

func TestPunchlineWithGPUs(t *testing.T) {
	report := gap.Analyze(gpuSnapshot(map[string]measurement.Reading{
		measurement.KeyGPUCount:        measurement.Int64(64),
		measurement.KeyGPUPresent:      measurement.Bool(true),
		measurement.KeyGPUDriverLoaded: measurement.Bool(true),
	}), gap.ClusterGPUs{})
	const want = "64 of 64 GPUs are usable by a workload today."
	if report.Punchline != want {
		t.Errorf("Punchline = %q, want %q", report.Punchline, want)
	}
}

// fullyCapableSnapshot builds a synthetic snapshot with a GPU measurement
// that clears usableGPUs' bar (present, driver loaded) and a K8s measurement
// that clears every rule's absence check: policy is collected and non-empty
// (so gpuOperatorAbsent is false, closing device-plugin and gpu-metrics) and
// image lists one of kaiSchedulerImageNames (so gpuSchedulerAbsent is false,
// closing gpu-scheduler). No fixture the repo has reaches this state — the
// KWOK captures are deliberately gap-heavy demo material — so this is
// synthetic, the same way gpuSnapshot() above is.
func fullyCapableSnapshot() *aicr.Snapshot {
	return aicr.WrapSnapshot(&snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{
			{
				Type: measurement.TypeGPU,
				Subtypes: []measurement.Subtype{{
					Name: "hardware",
					Data: map[string]measurement.Reading{
						measurement.KeyGPUCount:        measurement.Int64(8),
						measurement.KeyGPUPresent:      measurement.Bool(true),
						measurement.KeyGPUDriverLoaded: measurement.Bool(true),
					},
				}},
			},
			{
				Type: measurement.TypeK8s,
				Subtypes: []measurement.Subtype{
					{Name: "image", Data: map[string]measurement.Reading{
						"podgrouper": measurement.Str("v0.14.1"),
					}},
					{Name: "policy", Data: map[string]measurement.Reading{
						"devicePlugin.enabled": measurement.Str("true"),
					}},
				},
			},
		},
	})
}

// TestAnalyzeFullyCapableClusterHasNoGaps is the state Task 12's Discover
// screen crashed on: Report.Gaps carries no `omitempty` (its json tag is
// just `"gaps"`), so a cluster with every gap closed marshals it as JSON
// null, not `[]`. This pins that the Go side genuinely produces a nil slice
// here -- not just an empty one -- so a client-side fix has a real case to
// guard, and separately exercises punchline's non-zero-GPU branch
// (gap.go:85) end to end from a report with zero gaps, which
// TestPunchlineWithGPUs alone (a bare GPU-only synthetic snapshot) does not
// cover.
func TestAnalyzeFullyCapableClusterHasNoGaps(t *testing.T) {
	report := gap.Analyze(fullyCapableSnapshot(), gap.ClusterGPUs{})

	if report.Gaps != nil {
		t.Errorf("Gaps = %v, want nil -- a fully capable cluster has nothing left to close", report.Gaps)
	}
	const want = "8 of 8 GPUs are usable by a workload today."
	if report.Punchline != want {
		t.Errorf("Punchline = %q, want %q", report.Punchline, want)
	}
}

// A fully capable cluster and a cluster that was never measured both yield
// zero gaps. The console must not congratulate the user for the second.
func TestAnalyzeMarksWhetherASnapshotWasActuallyMeasured(t *testing.T) {
	if got := gap.Analyze(nil, gap.ClusterGPUs{}); got.Analyzed {
		t.Error("Analyzed = true for a nil snapshot, want false")
	}
	report := gap.Analyze(loadFixture(t, "snapshot-kwok-h100.yaml"), gap.ClusterGPUs{})
	if !report.Analyzed {
		t.Error("Analyzed = false for a real snapshot, want true")
	}
}

// probedGPUsPerNode is what one a3-megagpu-8g reports to an in-pod PCI probe,
// and the number the real cluster's console mistook for the whole cluster.
const probedGPUsPerNode = 8

// gpuHardware is the reading set a single-node PCI probe produces. Fixed at
// one node's worth deliberately: every test here is about what happens to that
// number afterwards, not about the number itself.
func gpuHardware() map[string]measurement.Reading {
	return map[string]measurement.Reading{
		measurement.KeyGPUCount:        measurement.Int64(probedGPUsPerNode),
		measurement.KeyGPUPresent:      measurement.Bool(true),
		measurement.KeyGPUDriverLoaded: measurement.Bool(true),
	}
}

// TestAnalyzePrefersTheNodeListOverTheSingleNodeProbe is the headline fix.
//
// The GPU measurement is an in-pod PCI probe of whichever node the agent
// landed on. On the real two-node H100 cluster it read 8, and the console
// announced "8 of 8 GPUs are usable" about a cluster holding 16. When the
// caller has walked the node list, that is simply better evidence.
func TestAnalyzePrefersTheNodeListOverTheSingleNodeProbe(t *testing.T) {
	report := gap.Analyze(gpuSnapshot(gpuHardware()),
		gap.ClusterGPUs{Nodes: 2, Total: 16, Usable: 16})

	if report.TotalGPUs != 16 || report.UsableGPUs != 16 {
		t.Errorf("got %d of %d, want 16 of 16 -- the node list outranks one node's probe",
			report.UsableGPUs, report.TotalGPUs)
	}
	if report.InferredGPUs {
		t.Error("InferredGPUs = true, but these numbers were counted rather than inferred")
	}
	if want := "16 of 16 GPUs are usable by a workload today."; report.Punchline != want {
		t.Errorf("Punchline = %q, want %q", report.Punchline, want)
	}
}

// TestAnalyzeCorrectsBothNumbersTogether is the trap this fix must not fall
// into.
//
// Correcting only the denominator -- total from the node list, usable still
// from the probe -- turns "8 of 8" into "8 of 16" and claims half the
// cluster's GPUs are unusable. That is a worse lie than the undercount it
// replaces, because it invents a fault that does not exist.
func TestAnalyzeCorrectsBothNumbersTogether(t *testing.T) {
	report := gap.Analyze(gpuSnapshot(gpuHardware()),
		gap.ClusterGPUs{Nodes: 2, Total: 16, Usable: 16})

	if report.UsableGPUs < report.TotalGPUs {
		t.Errorf("got %d of %d -- usable fell behind total, which reports a fault the cluster does not have",
			report.UsableGPUs, report.TotalGPUs)
	}
}

// TestAnalyzeInfersFromNodeCountBeforeTheDevicePlugin covers the cluster this
// console exists to onboard: GPU hardware present, no device plugin yet, so
// nvidia.com/gpu is advertised by nobody and the node list can only say how
// many GPU-shaped nodes there are. Multiplying the probe by that count is a
// guess, and the report says so rather than passing it off as counted.
func TestAnalyzeInfersFromNodeCountBeforeTheDevicePlugin(t *testing.T) {
	report := gap.Analyze(gpuSnapshot(gpuHardware()), gap.ClusterGPUs{Nodes: 2})

	if report.TotalGPUs != 16 || report.UsableGPUs != 16 {
		t.Errorf("got %d of %d, want 16 of 16 inferred from 8 x 2 nodes",
			report.UsableGPUs, report.TotalGPUs)
	}
	if !report.InferredGPUs {
		t.Error("InferredGPUs = false, but 8 x 2 is an assumption that every GPU node is identical")
	}
}

// TestAnalyzeFallsBackToTheProbeWhenNothingIsKnown keeps the old behavior for
// callers with no node list, which is what the zero ClusterGPUs means.
func TestAnalyzeFallsBackToTheProbeWhenNothingIsKnown(t *testing.T) {
	report := gap.Analyze(gpuSnapshot(gpuHardware()), gap.ClusterGPUs{})

	if report.TotalGPUs != 8 || report.UsableGPUs != 8 {
		t.Errorf("got %d of %d, want 8 of 8 from the probe alone", report.UsableGPUs, report.TotalGPUs)
	}
	if report.InferredGPUs {
		t.Error("InferredGPUs = true, but a single-node probe on one node is a measurement")
	}
}

// TestAnalyzeHeadlineUsesTheResolvedGPUCount closes the half of the fix that
// the first pass missed.
//
// punchline and headline are separate strings reading the same probe, and
// correcting only punchline produced a screen that said "This is a gke cluster
// with 8 GPUs" directly above "16 of 16 GPUs are usable by a workload today" --
// two sentences contradicting each other, which is worse than one wrong one.
// Caught on real hardware, not by this suite.
func TestAnalyzeHeadlineUsesTheResolvedGPUCount(t *testing.T) {
	report := gap.Analyze(gpuSnapshot(gpuHardware()),
		gap.ClusterGPUs{Nodes: 2, Total: 16, Usable: 16})

	if strings.Contains(report.Headline, "8 GPUs") {
		t.Errorf("Headline = %q, still quotes one node's probe", report.Headline)
	}
	if !strings.Contains(report.Headline, "16 GPUs") {
		t.Errorf("Headline = %q, want it to quote the cluster-wide 16", report.Headline)
	}
}

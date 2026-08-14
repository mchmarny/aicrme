package gap_test

import (
	"os"
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
	report := gap.Analyze(loadFixture(t, "snapshot-kwok.yaml"))

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
	report := gap.Analyze(nil)
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
	report := gap.Analyze(&aicr.Snapshot{})
	if report.Headline == "" {
		t.Error("Analyze(&aicr.Snapshot{}) must still produce a renderable report")
	}
	if len(report.Gaps) != 0 {
		t.Errorf("Analyze(&aicr.Snapshot{}) produced %d gaps, want 0", len(report.Gaps))
	}
}

func TestEveryGapNamesItsClosingComponent(t *testing.T) {
	report := gap.Analyze(loadFixture(t, "snapshot-kwok.yaml"))
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
// calls for: one row per rule the KWOK fixture actually proves. Rules for
// device plugin, GPU-aware scheduler, EFA plugin, and GPU metrics are not
// present in internal/gap/rules.go at all — see the package comment there —
// so they have no row here either.
func TestGapRulesFireAgainstFixture(t *testing.T) {
	report := gap.Analyze(loadFixture(t, "snapshot-kwok.yaml"))

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
			report := gap.Analyze(gpuSnapshot(tc.data))
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
	}))
	const want = "64 of 64 GPUs are usable by a workload today."
	if report.Punchline != want {
		t.Errorf("Punchline = %q, want %q", report.Punchline, want)
	}
}

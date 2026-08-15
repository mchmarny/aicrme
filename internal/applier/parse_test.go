package applier

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/mchmarny/aicrme/internal/bus"
)

func componentData(t *testing.T, e bus.Event) ComponentData {
	t.Helper()
	var d ComponentData
	if err := json.Unmarshal(e.Data, &d); err != nil {
		t.Fatalf("unmarshal ComponentData error = %v (data=%s)", err, e.Data)
	}
	return d
}

func TestParseLineMarkerGrammar(t *testing.T) {
	tests := []struct {
		name      string
		line      string
		wantKind  bus.Kind
		wantLevel bus.Level
		wantComp  string
		check     func(t *testing.T, e bus.Event)
	}{
		{
			name: "component header", line: "┌─ [3/16] gpu-operator  →  gpu-operator",
			wantKind: bus.KindComponent, wantLevel: bus.LevelInfo, wantComp: "gpu-operator",
			check: func(t *testing.T, e bus.Event) {
				d := componentData(t, e)
				if d.Index != 3 || d.Total != 16 || d.Namespace != "gpu-operator" || d.Status != StatusStarted {
					t.Errorf("ComponentData = %+v", d)
				}
			},
		},
		{
			name: "component installed", line: "└─ ✓ gpu-operator installed",
			wantKind: bus.KindComponent, wantLevel: bus.LevelInfo, wantComp: "gpu-operator",
			check: func(t *testing.T, e bus.Event) {
				if d := componentData(t, e); d.Status != StatusInstalled {
					t.Errorf("Status = %q, want %q", d.Status, StatusInstalled)
				}
			},
		},
		{
			name: "component failed", line: "└─ ✗ kai-scheduler FAILED (after 2 attempts)",
			wantKind: bus.KindComponent, wantLevel: bus.LevelError, wantComp: "kai-scheduler",
			check: func(t *testing.T, e bus.Event) {
				d := componentData(t, e)
				if d.Status != StatusFailed || d.Attempt != 2 {
					t.Errorf("ComponentData = %+v", d)
				}
			},
		},
		{
			name: "component retrying", line: "  ↺ kai-scheduler: attempt 1/5 failed, retrying in 20s...",
			wantKind: bus.KindComponent, wantLevel: bus.LevelWarn, wantComp: "kai-scheduler",
			check: func(t *testing.T, e bus.Event) {
				d := componentData(t, e)
				if d.Status != StatusRetrying || d.Attempt != 1 || d.MaxAttempts != 5 || d.RetryInSeconds != 20 {
					t.Errorf("ComponentData = %+v", d)
				}
			},
		},
		{
			name: "preflight passed", line: "✓ Pre-flight checks passed",
			wantKind: bus.KindPhase, wantLevel: bus.LevelInfo,
		},
		{
			name: "all installed", line: "✓ All components installed successfully.",
			wantKind: bus.KindPhase, wantLevel: bus.LevelInfo,
		},
		{
			name: "warn line", line: "⚠ kai-scheduler install failed, continuing (--best-effort)",
			wantKind: bus.KindLog, wantLevel: bus.LevelWarn,
		},
		{
			name: "fail line", line: "✗ Pre-flight checks failed. Fix the issues above before deploying.",
			wantKind: bus.KindLog, wantLevel: bus.LevelError,
		},
		{
			name: "async note", line: "│  (async component — skipping --wait, keeping --timeout for hooks)",
			wantKind: bus.KindLog, wantLevel: bus.LevelInfo,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseLine(tt.line)
			if !ok {
				t.Fatalf("parseLine(%q) not recognized", tt.line)
			}
			if got.Kind != tt.wantKind {
				t.Errorf("Kind = %q, want %q", got.Kind, tt.wantKind)
			}
			if got.Level != tt.wantLevel {
				t.Errorf("Level = %q, want %q", got.Level, tt.wantLevel)
			}
			if got.Component != tt.wantComp {
				t.Errorf("Component = %q, want %q", got.Component, tt.wantComp)
			}
			if tt.check != nil {
				tt.check(t, got)
			}
		})
	}
}

// Helm and kubectl output is the overwhelming majority of deploy.sh's
// stdout. Publishing it would flood the bus's replay ring and get slow
// subscribers dropped, so it must not parse into events at all.
func TestParseLineIgnoresNonMarkerOutput(t *testing.T) {
	for _, line := range []string{
		"",
		"Release \"cert-manager\" does not exist. Installing it now.",
		"NAME: gpu-operator",
		"  --- Failed hook Job kai-scheduler-crd-upgrader diagnostics ---",
		"│  Manual (approx, set KUBECONFIG_FLAG/DRY_RUN_FLAG/COMPONENT_WAIT_ARGS as needed): cd /bundle/001-x && bash install.sh",
		"══ Deploying AICR components ══════════════════════════════════════",
		"Error: INSTALLATION FAILED: timed out waiting for the condition",
	} {
		if got, ok := parseLine(line); ok {
			t.Errorf("parseLine(%q) recognized as %+v, want ignored", line, got)
		}
	}
}

// A header with an empty namespace is reachable: deploy.sh derives the
// namespace by awk-ing the component's install.sh, which yields an empty
// string if that grep ever misses.
func TestParseLineToleratesEmptyNamespace(t *testing.T) {
	got, ok := parseLine("┌─ [1/1] mystery  →  ")
	if !ok {
		t.Fatal("parseLine() not recognized")
	}
	if d := componentData(t, got); d.Name != "mystery" || d.Namespace != "" {
		t.Errorf("ComponentData = %+v", d)
	}
}

// The real captured transcript is the regression guard: it is what an AICR
// bump actually changes.
func TestParseTranscriptFixtures(t *testing.T) {
	tests := []struct {
		file          string
		wantStarted   bool
		wantInstalled bool
		wantFailed    bool
		wantRetrying  bool
	}{
		{file: "testdata/deploy-transcript-kwok.txt", wantStarted: true, wantInstalled: true},
		{file: "testdata/deploy-transcript-failure.txt", wantStarted: true, wantInstalled: true, wantFailed: true, wantRetrying: true},
	}

	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			raw, err := os.ReadFile(tt.file)
			if err != nil {
				t.Fatalf("read fixture error = %v", err)
			}
			seen := map[string]bool{}
			preflight := false
			for _, line := range strings.Split(string(raw), "\n") {
				e, ok := parseLine(line)
				if !ok {
					continue
				}
				if e.Kind == bus.KindPhase {
					preflight = true
				}
				if e.Kind == bus.KindComponent {
					seen[componentData(t, e).Status] = true
				}
			}
			if !preflight {
				t.Error("no phase event parsed -- the preflight marker is missing or changed")
			}
			if seen[StatusStarted] != tt.wantStarted {
				t.Errorf("started seen = %v, want %v", seen[StatusStarted], tt.wantStarted)
			}
			if seen[StatusInstalled] != tt.wantInstalled {
				t.Errorf("installed seen = %v, want %v", seen[StatusInstalled], tt.wantInstalled)
			}
			if seen[StatusFailed] != tt.wantFailed {
				t.Errorf("failed seen = %v, want %v", seen[StatusFailed], tt.wantFailed)
			}
			if seen[StatusRetrying] != tt.wantRetrying {
				t.Errorf("retrying seen = %v, want %v", seen[StatusRetrying], tt.wantRetrying)
			}
		})
	}
}

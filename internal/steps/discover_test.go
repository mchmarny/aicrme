package steps_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
	"github.com/mchmarny/aicrme/internal/gap"
	"github.com/mchmarny/aicrme/internal/steps"
	"gopkg.in/yaml.v3"
)

func newRun() *engine.Run {
	return &engine.Run{
		ID:        "test",
		Decisions: map[string]string{},
		Artifacts: map[string][]byte{},
	}
}

// loadSnapshot loads the real KWOK snapshot captured for Task 9's gap rules
// and wraps it exactly the way Client.CollectSnapshot does —
// fromInternalSnapshot(snap) then out.Raw = raw (pkg/client/v1/aicr.go) — so
// Fake.Snapshot carries both the raw bytes AND the parsed measurements gap.Analyze
// reads. A Raw-only *aicr.Snapshot{Raw: raw} literal parses cleanly but its
// Unwrap() yields no measurements at all (Unwrap only reads Raw's parsed
// sibling, the unexported internal field WrapSnapshot sets); scripting that
// shape here would silently defeat every test that checks gap output.
func loadSnapshot(t *testing.T) *aicr.Snapshot {
	t.Helper()
	raw, err := os.ReadFile("testdata/snapshot-kwok.yaml")
	if err != nil {
		t.Fatalf("fixture read error = %v", err)
	}
	var s snapshotter.Snapshot
	if err := yaml.Unmarshal(raw, &s); err != nil {
		t.Fatalf("parse fixture error = %v", err)
	}
	wrapped := aicr.WrapSnapshot(&s)
	wrapped.Raw = raw
	return wrapped
}

func TestDiscoverStoresRawSnapshot(t *testing.T) {
	snap := loadSnapshot(t)
	fake := &aicrclient.Fake{Snapshot: snap}
	step := steps.NewDiscover(fake, steps.DiscoverConfig{Namespace: "aicrme", Timeout: time.Minute})

	run := newRun()
	var events []bus.Event
	if err := step.Run(context.Background(), run, func(e bus.Event) { events = append(events, e) }); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if string(run.Artifacts["snapshot.yaml"]) != string(snap.Raw) {
		t.Error("stored snapshot is not the agent's raw bytes")
	}
	if fake.SnapshotCalls != 1 {
		t.Errorf("CollectSnapshot called %d times, want 1", fake.SnapshotCalls)
	}
	if len(events) == 0 {
		t.Error("Discover emitted no events")
	}
}

func TestDiscoverStoresCapabilityReport(t *testing.T) {
	fake := &aicrclient.Fake{Snapshot: loadSnapshot(t)}
	step := steps.NewDiscover(fake, steps.DiscoverConfig{Namespace: "aicrme"})

	run := newRun()
	if err := step.Run(context.Background(), run, func(bus.Event) {}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	encoded, ok := run.Artifacts["capability.json"]
	if !ok {
		t.Fatal("capability.json artifact was not written")
	}
	var report gap.Report
	if err := json.Unmarshal(encoded, &report); err != nil {
		t.Fatalf("capability.json does not decode as gap.Report: %v", err)
	}
	if report.Headline == "" {
		t.Error("stored capability report has no headline")
	}
	if len(report.Gaps) == 0 {
		t.Error("stored capability report has no gaps for the KWOK fixture")
	}
}

func TestDiscoverEmitsGapWarnings(t *testing.T) {
	fake := &aicrclient.Fake{Snapshot: loadSnapshot(t)}
	step := steps.NewDiscover(fake, steps.DiscoverConfig{Namespace: "aicrme"})

	var events []bus.Event
	if err := step.Run(context.Background(), newRun(), func(e bus.Event) { events = append(events, e) }); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	var sawWarn, sawHeadline, sawPunchline bool
	for _, e := range events {
		if e.Kind == bus.KindCluster && e.Level == bus.LevelWarn {
			sawWarn = true
		}
		if e.Kind == bus.KindLog && e.Message != "" {
			if e.Message == "deploying cluster snapshot agent" {
				continue
			}
			if !sawHeadline {
				sawHeadline = true
			} else {
				sawPunchline = true
			}
		}
	}
	if !sawWarn {
		t.Error("Discover emitted no KindCluster warn event for a fixture with gaps")
	}
	if !sawHeadline || !sawPunchline {
		t.Error("Discover did not emit both a headline log and a punchline log")
	}
}

// TestDiscoverPlumbsAgentConfig proves DiscoverConfig actually reaches the
// AgentConfig CollectSnapshot is called with — every field non-default so the
// test cannot pass against an accidentally empty *aicr.AgentConfig{}.
// Fake.LastAgentConfig only exists because CollectSnapshot used to discard
// its cfg argument entirely, which meant no test (including the six that
// existed before this one) could have caught Run() passing the wrong values,
// or none at all.
func TestDiscoverPlumbsAgentConfig(t *testing.T) {
	fake := &aicrclient.Fake{Snapshot: loadSnapshot(t)}
	cfg := steps.DiscoverConfig{
		Namespace:          "aicrme",
		Image:              "ghcr.io/nvidia/aicr:v0.19.0",
		JobName:            "custom-job",
		ServiceAccountName: "custom-sa",
		NodeSelector:       map[string]string{"node-role.kubernetes.io/control-plane": ""},
		Timeout:            3 * time.Minute,
		Privileged:         true,
		RequireGPU:         true,
		DiscoverNetwork:    true,
	}
	step := steps.NewDiscover(fake, cfg)

	if err := step.Run(context.Background(), newRun(), func(bus.Event) {}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	got := fake.LastAgentConfig
	if got == nil {
		t.Fatal("CollectSnapshot was called with a nil AgentConfig")
	}
	if got.Namespace != cfg.Namespace {
		t.Errorf("Namespace = %q, want %q", got.Namespace, cfg.Namespace)
	}
	if got.Image != cfg.Image {
		t.Errorf("Image = %q, want %q", got.Image, cfg.Image)
	}
	if got.JobName != cfg.JobName {
		t.Errorf("JobName = %q, want %q", got.JobName, cfg.JobName)
	}
	if got.ServiceAccountName != cfg.ServiceAccountName {
		t.Errorf("ServiceAccountName = %q, want %q", got.ServiceAccountName, cfg.ServiceAccountName)
	}
	if got.NodeSelector["node-role.kubernetes.io/control-plane"] != "" || len(got.NodeSelector) != 1 {
		t.Errorf("NodeSelector = %v, want %v", got.NodeSelector, cfg.NodeSelector)
	}
	if got.Timeout != cfg.Timeout {
		t.Errorf("Timeout = %v, want %v", got.Timeout, cfg.Timeout)
	}
	if got.Privileged != cfg.Privileged {
		t.Errorf("Privileged = %v, want %v", got.Privileged, cfg.Privileged)
	}
	if got.RequireGPU != cfg.RequireGPU {
		t.Errorf("RequireGPU = %v, want %v", got.RequireGPU, cfg.RequireGPU)
	}
	if got.DiscoverNetwork != cfg.DiscoverNetwork {
		t.Errorf("DiscoverNetwork = %v, want %v", got.DiscoverNetwork, cfg.DiscoverNetwork)
	}
	if !got.Cleanup {
		t.Error("Cleanup = false, want true — Discover must never leave the agent Job/RBAC behind")
	}
}

func TestDiscoverPropagatesFailure(t *testing.T) {
	boom := errors.New("agent job timed out")
	fake := &aicrclient.Fake{SnapshotErr: boom}
	step := steps.NewDiscover(fake, steps.DiscoverConfig{Namespace: "aicrme"})

	if err := step.Run(context.Background(), newRun(), func(bus.Event) {}); err == nil {
		t.Fatal("Run() returned nil on a collection failure")
	}
}

func TestDiscoverMetadata(t *testing.T) {
	step := steps.NewDiscover(&aicrclient.Fake{Snapshot: loadSnapshot(t)}, steps.DiscoverConfig{})
	if step.Phase() != engine.PhaseDiscover {
		t.Errorf("Phase() = %q, want %q", step.Phase(), engine.PhaseDiscover)
	}
	if len(step.Requires()) != 0 {
		t.Errorf("Requires() = %v, want empty — Discover runs automatically on first load", step.Requires())
	}
}

func TestDiscoverDefaultsTimeout(t *testing.T) {
	fake := &aicrclient.Fake{Snapshot: loadSnapshot(t)}
	// Timeout left zero: NewDiscover must default it rather than deploy the
	// agent Job with no wait bound at all.
	step := steps.NewDiscover(fake, steps.DiscoverConfig{Namespace: "aicrme"})

	if err := step.Run(context.Background(), newRun(), func(bus.Event) {}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	const wantDefault = 10 * time.Minute
	if fake.LastAgentConfig == nil {
		t.Fatal("CollectSnapshot was called with a nil AgentConfig")
	}
	if fake.LastAgentConfig.Timeout != wantDefault {
		t.Errorf("Timeout = %v, want default %v", fake.LastAgentConfig.Timeout, wantDefault)
	}
}

// TestDiscoverDefaultsAgentName proves JobName and ServiceAccountName are
// defaulted when left blank rather than reaching CollectSnapshot as "".
// aicr.Client.CollectSnapshot applies no default of its own for either field
// (unlike the `aicr` CLI, which defaults both via its own flag layer) — an
// empty JobName or ServiceAccountName reaches the API server as
// `metadata.name: ""` and is rejected outright, failing Discover on every
// cluster. See the identical reasoning on defaultAgentName in discover.go.
func TestDiscoverDefaultsAgentName(t *testing.T) {
	fake := &aicrclient.Fake{Snapshot: loadSnapshot(t)}
	// JobName and ServiceAccountName left zero-valued.
	step := steps.NewDiscover(fake, steps.DiscoverConfig{Namespace: "aicrme"})

	if err := step.Run(context.Background(), newRun(), func(bus.Event) {}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if fake.LastAgentConfig == nil {
		t.Fatal("CollectSnapshot was called with a nil AgentConfig")
	}
	if fake.LastAgentConfig.JobName == "" {
		t.Error("JobName is empty — CollectSnapshot would reach the API server as metadata.name: \"\"")
	}
	if fake.LastAgentConfig.ServiceAccountName == "" {
		t.Error("ServiceAccountName is empty — CollectSnapshot would reach the API server as metadata.name: \"\"")
	}
}

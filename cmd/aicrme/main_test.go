package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/mchmarny/aicrme/internal/engine"
	"github.com/mchmarny/aicrme/internal/steps"
)

const aicrModulePath = "github.com/NVIDIA/aicr"

// linkedAICRVersion returns the github.com/NVIDIA/aicr version this test
// binary was actually compiled against, read from the module graph the
// toolchain stamps into every binary it links. This is the same version
// go.mod records and `make check-aicr-pin` compares against .settings.yaml,
// but taken from the build itself rather than from a file that could have
// drifted from what was compiled.
//
// Only Deps is used, never info.Main.Version: for a plain `go build` with no
// VCS stamping the main module reads "(devel)", so deriving the console's own
// release tag at runtime is not reliable. Dependency versions carry no such
// caveat -- the module graph resolves them exactly.
func linkedAICRVersion(t *testing.T) string {
	t.Helper()
	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatal("debug.ReadBuildInfo() unavailable; cannot verify the snapshot agent image against the linked AICR module")
	}
	for _, dep := range info.Deps {
		if dep.Path != aicrModulePath {
			continue
		}
		if dep.Replace != nil {
			return dep.Replace.Version
		}
		return dep.Version
	}
	t.Fatalf("%s is not among this binary's module dependencies; the snapshot agent image pin cannot be verified", aicrModulePath)
	return ""
}

func splitImage(t *testing.T, image string) (repo, tag string) {
	t.Helper()
	repo, tag, ok := strings.Cut(image, ":")
	if !ok {
		t.Fatalf("defaultSnapshotAgentImage = %q, want an explicitly tagged repo:tag reference", image)
	}
	return repo, tag
}

// TestDefaultSnapshotAgentImageTracksLinkedAICRVersion is the guard that makes
// the image default load-bearing. Discover forwards it verbatim to the agent
// Job's container spec -- aicr.Client.CollectSnapshot, unlike the `aicr` CLI,
// applies no default of its own -- so deleting the constant or letting its tag
// drift from the compiled-in AICR client breaks Discover on every cluster.
// Before this test, both failure modes left the suite green.
func TestDefaultSnapshotAgentImageTracksLinkedAICRVersion(t *testing.T) {
	repo, tag := splitImage(t, defaultSnapshotAgentImage)

	if want := "ghcr.io/nvidia/aicr"; repo != want {
		t.Errorf("defaultSnapshotAgentImage repo = %q, want %q (the CLI's own defaultAgentImage mapping)", repo, want)
	}
	if want := linkedAICRVersion(t); tag != want {
		t.Errorf("defaultSnapshotAgentImage tag = %q, want %q (the linked %s version; see .settings.yaml dependencies.aicr and `make check-aicr-pin`)",
			tag, want, aicrModulePath)
	}
}

// TestDefaultSnapshotAgentImageIsNotFloating pins the failure mode a version
// comparison alone cannot catch: a mutable tag would make two deploys of the
// same console binary collect snapshots with different agents, so a run is no
// longer reproducible from its image reference.
func TestDefaultSnapshotAgentImageIsNotFloating(t *testing.T) {
	_, tag := splitImage(t, defaultSnapshotAgentImage)

	for _, floating := range []string{"", "latest", "main", "master", "edge", "dev", "devel", "nightly", "stable"} {
		if tag == floating {
			t.Errorf("defaultSnapshotAgentImage tag = %q, want an immutable release tag", tag)
		}
	}
	if !strings.HasPrefix(tag, "v") {
		t.Errorf("defaultSnapshotAgentImage tag = %q, want a v-prefixed release tag matching %s's module versions", tag, aicrModulePath)
	}
}

func TestParseNodeSelector(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want map[string]string
	}{
		{
			name: "unset leaves the module's GPU auto-targeting in charge",
			in:   "",
			want: nil,
		},
		{
			name: "single pair",
			in:   "kubernetes.io/os=linux",
			want: map[string]string{"kubernetes.io/os": "linux"},
		},
		{
			name: "multiple pairs",
			in:   "kubernetes.io/os=linux,topology.kubernetes.io/zone=us-east-1a",
			want: map[string]string{"kubernetes.io/os": "linux", "topology.kubernetes.io/zone": "us-east-1a"},
		},
		{
			name: "empty value is a valid label existence selector",
			in:   "node-role.kubernetes.io/control-plane=",
			want: map[string]string{"node-role.kubernetes.io/control-plane": ""},
		},
		{
			name: "surrounding whitespace is trimmed",
			in:   " kubernetes.io/os = linux , type = kwok ",
			want: map[string]string{"kubernetes.io/os": "linux", "type": "kwok"},
		},
		{
			name: "only the first separator splits, so values may contain =",
			in:   "key=a=b",
			want: map[string]string{"key": "a=b"},
		},
		{
			name: "malformed pair is dropped, valid neighbours survive",
			in:   "kubernetes.io/os=linux,missing-separator",
			want: map[string]string{"kubernetes.io/os": "linux"},
		},
		{
			name: "empty key is dropped",
			in:   "=linux,type=kwok",
			want: map[string]string{"type": "kwok"},
		},
		{
			name: "fully malformed value yields no selector at all",
			in:   "missing-separator",
			want: nil,
		},
		{
			name: "separators only yield no selector at all",
			in:   ",,",
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseNodeSelector(tc.in)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("parseNodeSelector(%q) = %v, want nil so DiscoverConfig.NodeSelector stays unset", tc.in, got)
				}
				return
			}
			if !maps.Equal(got, tc.want) {
				t.Errorf("parseNodeSelector(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestParseNodeSelectorWarnsOnUnparseableValue covers the case the map return
// value cannot express: a present-but-unparseable selector resolves to nil,
// which silently restores the very GPU auto-targeting the operator set the
// variable to disable. Dropping the pair is the right recovery, doing it
// silently is not.
func TestParseNodeSelectorWarnsOnUnparseableValue(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantWarn bool
	}{
		{name: "unset is not a mistake", in: "", wantWarn: false},
		{name: "well-formed value is silent", in: "type=kwok", wantWarn: false},
		{name: "empty value is well-formed and silent", in: "node-role.kubernetes.io/control-plane=", wantWarn: false},
		{name: "dropped pair warns", in: "type=kwok,missing-separator", wantWarn: true},
		{name: "fully malformed value warns", in: "missing-separator", wantWarn: true},
		{name: "empty key warns", in: "=linux", wantWarn: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureWarnings(t)
			parseNodeSelector(tc.in)
			got := logs.String()
			if tc.wantWarn && !strings.Contains(got, "AICRME_SNAPSHOT_NODE_SELECTOR") {
				t.Errorf("parseNodeSelector(%q) logged %q, want a warning naming AICRME_SNAPSHOT_NODE_SELECTOR", tc.in, got)
			}
			if !tc.wantWarn && got != "" {
				t.Errorf("parseNodeSelector(%q) logged %q, want silence", tc.in, got)
			}
		})
	}
}

// captureWarnings redirects the default logger for one subtest. parseNodeSelector
// runs before main installs its own handler, so it logs through slog's default.
func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return buf
}

func TestEnvOr(t *testing.T) {
	const key = "AICRME_TEST_ENV_OR"

	if got := envOr(key, defaultSnapshotAgentImage); got != defaultSnapshotAgentImage {
		t.Errorf("envOr(unset) = %q, want the fallback %q", got, defaultSnapshotAgentImage)
	}

	t.Setenv(key, "")
	if got := envOr(key, defaultSnapshotAgentImage); got != defaultSnapshotAgentImage {
		t.Errorf("envOr(empty) = %q, want the fallback %q -- an empty override must not reach the Job spec", got, defaultSnapshotAgentImage)
	}

	t.Setenv(key, "registry.example.com/mirror/aicr:v0.19.0")
	if got, want := envOr(key, defaultSnapshotAgentImage), os.Getenv(key); got != want {
		t.Errorf("envOr(set) = %q, want the override %q", got, want)
	}
}

func TestEnsureWorkDirsCreatesEveryCacheDir(t *testing.T) {
	root := t.TempDir()
	if err := ensureWorkDirs(root); err != nil {
		t.Fatalf("ensureWorkDirs() error = %v", err)
	}
	for _, sub := range []string{"tmp", "home", "helm/cache", "helm/config", "helm/data", "kube/cache", "runs"} {
		if _, err := os.Stat(filepath.Join(root, sub)); err != nil {
			t.Errorf("missing %s: %v", sub, err)
		}
	}
}

func TestEnsureWorkDirsFailsOnUnwritableRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("setup error = %v", err)
	}
	if err := ensureWorkDirs(root); err == nil {
		t.Error("ensureWorkDirs() error = nil, want a failure -- an unwritable work dir must not start silently")
	}
}

// recipeFixture marshals a real steps.RecipeSummary rather than hand-rolling
// a JSON literal, so a tag or shape change in the struct this test exists to
// pin shows up as a build-time or round-trip difference here too, instead of
// leaving a stale literal (and this test) green while production drifts.
func recipeFixture(t *testing.T, components ...steps.ComponentSummary) []byte {
	t.Helper()
	raw, err := json.Marshal(steps.RecipeSummary{
		Name:           "r",
		Version:        "1",
		ComponentCount: len(components),
		Components:     components,
	})
	if err != nil {
		t.Fatalf("marshal recipe fixture: %v", err)
	}
	return raw
}

func TestRecipeNamespacesFromArtifact(t *testing.T) {
	raw := recipeFixture(t,
		steps.ComponentSummary{Name: "a", Namespace: "gpu-operator"},
		steps.ComponentSummary{Name: "b", Namespace: "monitoring"},
	)

	got := recipeNamespaces(raw)
	for _, want := range []string{"gpu-operator", "monitoring"} {
		if _, ok := got[want]; !ok {
			t.Errorf("namespace %q missing from %v", want, got)
		}
	}
	if len(got) != 2 {
		t.Errorf("len = %d, want 2", len(got))
	}
}

func TestRecipeNamespacesToleratesMissingOrCorruptArtifact(t *testing.T) {
	if got := recipeNamespaces(nil); len(got) != 0 {
		t.Errorf("nil artifact = %v, want empty", got)
	}
	if got := recipeNamespaces([]byte("not json")); len(got) != 0 {
		t.Errorf("corrupt artifact = %v, want empty", got)
	}
}

// fakeRunReader is a runReader test double that counts artifact reads, so
// tests can assert the cache actually avoids the per-event engine round trip
// rather than just asserting on the returned value.
type fakeRunReader struct {
	id            string
	ok            bool
	run           *engine.Run
	artifactCalls int
}

func (f *fakeRunReader) CurrentID() (string, bool) { return f.id, f.ok }

func (f *fakeRunReader) Artifact(runID, key string) ([]byte, bool) {
	f.artifactCalls++
	if f.run == nil || f.run.ID != runID {
		return nil, false
	}
	v, ok := f.run.Artifacts[key]
	return v, ok
}

// TestNewRunScopeFnDoesNotCacheBeforeRecipeExists is the regression test for
// the bug this round fixes: caching an empty Namespaces the first time a run
// is asked about -- before Recommend has written recipe.json -- used to pin
// that empty set for the rest of the run (RunID already matched the cache on
// every later call), silently dropping every namespaced workload event for
// a window that spans Discover's 10-minute timeout plus the operator's
// awaiting-decision pause.
func TestNewRunScopeFnDoesNotCacheBeforeRecipeExists(t *testing.T) {
	fake := &fakeRunReader{
		id: "run-1", ok: true,
		run: &engine.Run{ID: "run-1", Artifacts: map[string][]byte{}},
	}
	scope := newRunScopeFn(fake)

	sc := scope()
	if sc.RunID != "run-1" {
		t.Fatalf("RunID = %q, want run-1", sc.RunID)
	}
	if len(sc.Namespaces) != 0 {
		t.Fatalf("Namespaces = %v, want empty before recipe.json exists", sc.Namespaces)
	}

	// Recommend writes recipe.json partway through the run.
	fake.run.Artifacts["recipe.json"] = recipeFixture(t, steps.ComponentSummary{Name: "a", Namespace: "gpu-operator"})

	sc = scope()
	if _, ok := sc.Namespaces["gpu-operator"]; !ok {
		t.Errorf("Namespaces = %v, want gpu-operator once recipe.json exists -- an empty pre-recipe scope must not stick", sc.Namespaces)
	}
}

// TestNewRunScopeFnCachesOnceRecipeExists guards the reason the cache exists
// at all: once a run's namespaces are resolved, repeated calls must not
// re-invoke Current(), which deep-copies every artifact.
func TestNewRunScopeFnCachesOnceRecipeExists(t *testing.T) {
	fake := &fakeRunReader{
		id: "run-1", ok: true,
		run: &engine.Run{ID: "run-1", Artifacts: map[string][]byte{
			"recipe.json": recipeFixture(t, steps.ComponentSummary{Name: "a", Namespace: "gpu-operator"}),
		}},
	}
	scope := newRunScopeFn(fake)

	for range 3 {
		scope()
	}

	if fake.artifactCalls != 1 {
		t.Errorf("Artifact() called %d times, want 1 -- resolved namespaces should be cached, not recomputed", fake.artifactCalls)
	}
}

// TestNewRunScopeFnRefreshesOnRunTransition guards against a stale-run leak:
// once the engine moves to a new run, the new run's scope must never carry
// the previous run's namespaces.
func TestNewRunScopeFnRefreshesOnRunTransition(t *testing.T) {
	fake := &fakeRunReader{
		id: "run-1", ok: true,
		run: &engine.Run{ID: "run-1", Artifacts: map[string][]byte{
			"recipe.json": recipeFixture(t, steps.ComponentSummary{Name: "a", Namespace: "gpu-operator"}),
		}},
	}
	scope := newRunScopeFn(fake)

	if sc := scope(); sc.RunID != "run-1" {
		t.Fatalf("RunID = %q, want run-1", sc.RunID)
	}

	fake.id = "run-2"
	fake.run = &engine.Run{ID: "run-2", Artifacts: map[string][]byte{
		"recipe.json": recipeFixture(t, steps.ComponentSummary{Name: "b", Namespace: "monitoring"}),
	}}

	sc := scope()
	if sc.RunID != "run-2" {
		t.Fatalf("RunID = %q, want run-2", sc.RunID)
	}
	if _, ok := sc.Namespaces["gpu-operator"]; ok {
		t.Errorf("Namespaces = %v, still carries run-1's gpu-operator", sc.Namespaces)
	}
	if _, ok := sc.Namespaces["monitoring"]; !ok {
		t.Errorf("Namespaces = %v, want monitoring (run-2's own recipe)", sc.Namespaces)
	}
}

// TestNewRunScopeFnNoCurrentRunReturnsZeroValueWithoutCloning covers the
// idle-engine path: CurrentID alone must decide there is nothing to scope,
// without any further trip into the engine.
func TestNewRunScopeFnNoCurrentRunReturnsZeroValueWithoutCloning(t *testing.T) {
	fake := &fakeRunReader{ok: false}
	scope := newRunScopeFn(fake)

	sc := scope()
	if sc.RunID != "" || len(sc.Namespaces) != 0 {
		t.Errorf("scope() = %+v, want the zero value when no run is current", sc)
	}
	if fake.artifactCalls != 0 {
		t.Errorf("Artifact() called %d times, want 0 -- CurrentID() alone should short-circuit", fake.artifactCalls)
	}
}

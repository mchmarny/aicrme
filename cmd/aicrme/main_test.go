package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/mchmarny/aicrme/internal/engine"
	"github.com/mchmarny/aicrme/internal/observer"
	"github.com/mchmarny/aicrme/internal/steps"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
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

// fakeAttributionReader is an attributionReader test double, letting the
// composition test drive engine.Attribution() independently of
// fakeRunReader's namespace-cache behavior -- the two are separate
// interfaces precisely because they are separate sources (Ruling 2).
type fakeAttributionReader struct {
	a     engine.Attribution
	calls int
}

func (f *fakeAttributionReader) Attribution() engine.Attribution {
	f.calls++
	return f.a
}

// TestNewObserverScopeFnComposesNamespacesAndAttribution pins the
// composition this task exists to build: Namespaces come from the cached
// recipe parsing (nsScope, i.e. newRunScopeFn), RunID/Component/Generation/
// Terminal come from the engine's attribution snapshot -- and both land in
// the one RunScope the observer actually reads, despite coming from two
// separate accessors that cannot be merged on the engine side (Ruling 2:
// Namespaces would require internal/engine to import internal/steps, which
// already imports internal/engine).
//
// This also doubles as the "matching RunIDs compose normally" case for
// Ruling 6's disagreement check (nsFake.id and attrFake.a.RunID are both
// "run-1" here): the check below must not reject the ordinary case where
// both sources already describe the same run. See
// TestNewObserverScopeFnRunIDDisagreementYieldsTheZeroScope for the
// mismatched case.
func TestNewObserverScopeFnComposesNamespacesAndAttribution(t *testing.T) {
	nsFake := &fakeRunReader{
		id: "run-1", ok: true,
		run: &engine.Run{ID: "run-1", Artifacts: map[string][]byte{
			"recipe.json": recipeFixture(t, steps.ComponentSummary{Name: "a", Namespace: "gpu-operator"}),
		}},
	}
	attrFake := &fakeAttributionReader{a: engine.Attribution{
		RunID:        "run-1",
		ActiveAction: "gpu-operator",
		Generation:   7,
		Terminal:     true,
	}}

	scope := newObserverScopeFn(attrFake, newRunScopeFn(nsFake))
	sc := scope()

	if sc.RunID != "run-1" {
		t.Errorf("RunID = %q, want run-1", sc.RunID)
	}
	if _, ok := sc.Namespaces["gpu-operator"]; !ok {
		t.Errorf("Namespaces = %v, want gpu-operator from the cached recipe parsing", sc.Namespaces)
	}
	if sc.Component != "gpu-operator" {
		t.Errorf("Component = %q, want gpu-operator from Attribution().ActiveAction", sc.Component)
	}
	if sc.Generation != 7 {
		t.Errorf("Generation = %d, want 7 from Attribution().Generation", sc.Generation)
	}
	if !sc.Terminal {
		t.Error("Terminal = false, want true from Attribution().Terminal (Ruling 8)")
	}
	if attrFake.calls != 1 {
		t.Errorf("Attribution() called %d times by one scope() call, want exactly 1", attrFake.calls)
	}
}

// TestNewObserverScopeFnRunIDDisagreementYieldsTheZeroScope pins Ruling 6.
// nsScope (Engine.CurrentID, under its own lock inside newRunScopeFn) and
// eng.Attribution() (a second, independent lock acquisition) can observe two
// different runs across a transition landing between the two calls -- here
// manufactured directly, rather than raced, by simply giving the two fakes
// disagreeing RunIDs. Merging the two reads anyway would pair one run's
// Namespaces with a different run's RunID/Component -- a wrong answer, and
// exactly the race RunScope's own doc comment forbids one layer down
// (internal/observer/observer.go). The fix returns the zero RunScope
// instead: this asserts on the WHOLE value via reflect.DeepEqual, not just
// RunID, so a partial merge (e.g. zeroing RunID but leaving Namespaces or
// Component from one of the two disagreeing sources) still fails.
func TestNewObserverScopeFnRunIDDisagreementYieldsTheZeroScope(t *testing.T) {
	nsFake := &fakeRunReader{
		id: "run-1", ok: true,
		run: &engine.Run{ID: "run-1", Artifacts: map[string][]byte{
			"recipe.json": recipeFixture(t, steps.ComponentSummary{Name: "a", Namespace: "gpu-operator"}),
		}},
	}
	attrFake := &fakeAttributionReader{a: engine.Attribution{
		RunID:        "run-2",
		ActiveAction: "kai-scheduler",
		Generation:   9,
	}}

	scope := newObserverScopeFn(attrFake, newRunScopeFn(nsFake))
	sc := scope()

	if !reflect.DeepEqual(sc, observer.RunScope{}) {
		t.Errorf("scope() = %+v, want the zero RunScope on RunID disagreement (nsScope=run-1, Attribution=run-2)", sc)
	}
	// The disagreement check compares values already read, not a
	// double-check call into eng -- a third read is exactly what the
	// coordinator's review flagged as the wrong shape to guard against here.
	if attrFake.calls != 1 {
		t.Errorf("Attribution() called %d times by one scope() call, want exactly 1", attrFake.calls)
	}
}

func TestParseResourceRequests(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want map[corev1.ResourceName]string
	}{
		{
			name: "unset leaves AICR's own defaults in charge",
			in:   "",
			want: nil,
		},
		{
			name: "single quantity",
			in:   "cpu=200m",
			want: map[corev1.ResourceName]string{corev1.ResourceCPU: "200m"},
		},
		{
			name: "multiple quantities",
			in:   "cpu=200m,memory=256Mi",
			want: map[corev1.ResourceName]string{corev1.ResourceCPU: "200m", corev1.ResourceMemory: "256Mi"},
		},
		{
			name: "surrounding whitespace is trimmed",
			in:   " cpu = 200m , memory = 256Mi ",
			want: map[corev1.ResourceName]string{corev1.ResourceCPU: "200m", corev1.ResourceMemory: "256Mi"},
		},
		{
			name: "an unparseable quantity is skipped, the rest still apply",
			in:   "cpu=not-a-quantity,memory=256Mi",
			want: map[corev1.ResourceName]string{corev1.ResourceMemory: "256Mi"},
		},
		{
			name: "a pair with no separator is skipped",
			in:   "cpu,memory=256Mi",
			want: map[corev1.ResourceName]string{corev1.ResourceMemory: "256Mi"},
		},
		{
			// nil, not an empty ResourceList: an empty list would be forwarded
			// to AICR as an explicit "request nothing" override, which is a
			// different and worse outcome than falling back to its defaults.
			name: "nothing usable falls back rather than requesting nothing",
			in:   "cpu=not-a-quantity",
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseResourceRequests(tt.in)
			if tt.want == nil {
				if got != nil {
					t.Fatalf("parseResourceRequests(%q) = %v, want nil", tt.in, got)
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("parseResourceRequests(%q) = %v, want %v", tt.in, got, tt.want)
			}
			for name, want := range tt.want {
				// Cmp, not String: the property is the quantity's value, and a
				// parse/serialize round trip may not preserve the spelling.
				q, ok := got[name]
				if !ok {
					t.Errorf("%s missing from %v", name, got)
					continue
				}
				if q.Cmp(resource.MustParse(want)) != 0 {
					t.Errorf("%s = %v, want %v", name, q, want)
				}
			}
		})
	}
}

// TestOwnerReferenceResolvesToTheDeployment guards the property the whole
// task exists for: the run ConfigMap's ownerReference must name the
// Deployment, never its ReplicaSet. A ReplicaSet is replaced on every
// rollout, and ownerReference garbage collection would then delete the run
// state as a side effect of upgrading the console. The fake clientset also
// carries a ReplicaSet with the *same name* -- the shape a same-name lookup
// across kinds could confuse this with -- so the test fails if the helper
// ever resolves anything but the typed Deployment object.
func TestOwnerReferenceResolvesToTheDeployment(t *testing.T) {
	const (
		ns   = "aicrme"
		name = "aicrme"
	)
	client := fake.NewSimpleClientset(
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID("deployment-uid")},
		},
		&appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID("replicaset-uid")},
		},
	)

	owner, err := resolveDeploymentOwner(context.Background(), client, ns, name)
	if err != nil {
		t.Fatalf("resolveDeploymentOwner() error = %v", err)
	}
	if owner.Kind != "Deployment" {
		t.Errorf("Kind = %q, want Deployment -- a ReplicaSet owner is garbage-collected on every rollout and would delete run state with it", owner.Kind)
	}
	if owner.APIVersion != "apps/v1" {
		t.Errorf("APIVersion = %q, want apps/v1", owner.APIVersion)
	}
	if owner.Name != name {
		t.Errorf("Name = %q, want %q", owner.Name, name)
	}
	if owner.UID != types.UID("deployment-uid") {
		t.Errorf("UID = %q, want the Deployment's UID (deployment-uid), not the same-named ReplicaSet's (replicaset-uid)", owner.UID)
	}
}

// TestOwnerReferenceResolutionFailsWhenTheDeploymentIsMissing guards the
// failure path newRunStore depends on to decide whether to degrade: a
// missing or unreachable Deployment must return an error, not a zero-value
// reference that would silently own the run ConfigMap by nothing at all.
func TestOwnerReferenceResolutionFailsWhenTheDeploymentIsMissing(t *testing.T) {
	client := fake.NewSimpleClientset()

	if _, err := resolveDeploymentOwner(context.Background(), client, "aicrme", "aicrme"); err == nil {
		t.Error("resolveDeploymentOwner() error = nil, want an error for a Deployment that does not exist")
	}
}

// TestNoClientKeepsTheMemoryStore is Ruling 4 made concrete: with a nil
// kubernetes.Interface, newRunStore must return a store that works with no
// cluster at all, and must say so. A ConfigMapStore wired with a nil
// kubernetes.Interface would panic the instant it touched kube.CoreV1(), so
// a successful Save/LoadCurrent round trip here is proof of degradation, not
// just a claim in a log line.
func TestNoClientKeepsTheMemoryStore(t *testing.T) {
	logs := captureWarnings(t)

	store := newRunStore(context.Background(), nil, "aicrme", "aicrme")

	run := &engine.Run{
		ID:        "0123456789abcdef",
		State:     engine.StateIdle,
		StartedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := store.Save(context.Background(), run); err != nil {
		t.Fatalf("Save() error = %v, want the in-memory store to accept a save with no cluster client", err)
	}
	got, err := store.LoadCurrent(context.Background())
	if err != nil {
		t.Fatalf("LoadCurrent() error = %v", err)
	}
	if got.ID != run.ID {
		t.Errorf("LoadCurrent().ID = %q, want %q", got.ID, run.ID)
	}

	if got := logs.String(); !strings.Contains(got, "no cluster client") {
		t.Errorf("logged %q, want a warning naming the degradation (\"no cluster client\")", got)
	}
}

// TestNewRunStoreDegradesWhenOwnerResolutionFails covers the other half of
// newRunStore's fallback: a live cluster client but a Deployment lookup that
// fails (RBAC, a control-plane blip, an unusual install order) must degrade
// exactly like the no-client case, not fail startup.
//
// The discriminator is deliberately not "Save succeeded" or a log substring
// match: the fake clientset accepts a Save against a ConfigMap carrying a
// zero-value OwnerReference with no validation at all, so a build that
// forgets the early return and falls through to a real ConfigMapStore
// anyway would still make Save succeed and would still log a line
// containing "aicrme" (present in nearly every message this package logs).
// That is exactly the case the spec calls out as worse than no persistence
// -- Kubernetes GC reaps a dependent whose owner UID does not resolve, so a
// malformed ownerReference gets the run ConfigMap deleted on the first
// sweep. Asserting against the fake clientset directly, that no ConfigMap
// named "aicrme-run" was ever created, is the one check a fallthrough to
// the real store cannot pass.
func TestNewRunStoreDegradesWhenOwnerResolutionFails(t *testing.T) {
	const ns = "aicrme"
	logs := captureWarnings(t)
	client := fake.NewSimpleClientset() // no Deployment object present

	store := newRunStore(context.Background(), client, ns, "aicrme")

	run := &engine.Run{
		ID:        "fedcba9876543210",
		State:     engine.StateIdle,
		StartedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := store.Save(context.Background(), run); err != nil {
		t.Fatalf("Save() error = %v, want the fallback store to accept a save", err)
	}

	if cm, getErr := client.CoreV1().ConfigMaps(ns).Get(context.Background(), "aicrme"+runStoreSuffix, metav1.GetOptions{}); getErr == nil {
		t.Fatalf("run ConfigMap %s/%s was created against the fake clientset = %+v, want the in-memory fallback used instead and no ConfigMap ever written",
			ns, "aicrme"+runStoreSuffix, cm)
	} else if !apierrors.IsNotFound(getErr) {
		t.Fatalf("unexpected error checking for the run ConfigMap: %v", getErr)
	}

	if got := logs.String(); !strings.Contains(got, "aicrme") {
		t.Errorf("logged %q, want a warning naming the Deployment lookup failure", got)
	}
}

// TestNewRunStoreBuildsAConfigMapStoreWithTheDeploymentOwner is the success
// path's own test: main.go:247's `return engine.NewConfigMapStore(...)` had
// zero coverage before this -- nothing exercised "valid client + resolvable
// Deployment -> real ConfigMap-backed store, built with the right name and
// the right owner." Asserting on the owner fields specifically, not just
// the ConfigMap's name, matters: a store that writes to the right name with
// the wrong owner is the data-loss case (a malformed ownerReference gets
// the object reaped by Kubernetes GC), and a name-only assertion would miss
// exactly that.
func TestNewRunStoreBuildsAConfigMapStoreWithTheDeploymentOwner(t *testing.T) {
	const (
		ns   = "aicrme"
		name = "aicrme"
	)
	client := fake.NewSimpleClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID("deployment-uid")},
	})

	store := newRunStore(context.Background(), client, ns, name)

	run := &engine.Run{
		ID:        "0123456789abcdef",
		State:     engine.StateIdle,
		StartedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := store.Save(context.Background(), run); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	wantName := name + runStoreSuffix
	cm, err := client.CoreV1().ConfigMaps(ns).Get(context.Background(), wantName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get(%s/%s) error = %v, want the run ConfigMap to have been created", ns, wantName, err)
	}
	if len(cm.OwnerReferences) != 1 {
		t.Fatalf("OwnerReferences = %v, want exactly one", cm.OwnerReferences)
	}
	owner := cm.OwnerReferences[0]
	if owner.Kind != "Deployment" {
		t.Errorf("owner.Kind = %q, want Deployment", owner.Kind)
	}
	if owner.APIVersion != "apps/v1" {
		t.Errorf("owner.APIVersion = %q, want apps/v1", owner.APIVersion)
	}
	if owner.Name != name {
		t.Errorf("owner.Name = %q, want %q", owner.Name, name)
	}
	if owner.UID != types.UID("deployment-uid") {
		t.Errorf("owner.UID = %q, want the Deployment's UID (deployment-uid) -- a store writing to the right name with the wrong owner is the data-loss case", owner.UID)
	}
}

// blockingHandler returns an http.Handler that blocks every request until
// release is closed, and a cleanup func that unblocks it and only then
// closes srv. httptest.Server.Close blocks until every outstanding request
// has completed, so calling it while a handler is still parked on <-release
// deadlocks the test -- the cleanup func here is the one place that
// ordering is allowed to matter, so it is centralized rather than left to
// defer stacking at each call site.
func blockingHandler() (handler http.Handler, release chan struct{}) {
	release = make(chan struct{})
	handler = http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	return handler, release
}

// TestNewRunStoreBoundsTheDeploymentLookupAgainstARealTransport is the
// bound's own regression test. cmstore.go's withCallTimeout wraps its
// callee in a goroutine+select specifically because client-go's fake
// clientset ignores ctx entirely (verified against
// k8s.io/client-go/gentype.FakeClient.Get -- see withCallTimeout's own
// doc). resolveDeploymentOwner takes the opposite approach and relies on
// the real transport honoring ctx directly, which is correct against a real
// apiserver but is not proven by any fake-clientset test: a future refactor
// that reintroduced an unbounded call here would leave every other test in
// this file green. This points a real *kubernetes.Clientset at an
// httptest.Server whose handler never answers -- the exact wedged-apiserver
// shape 2b-i's unbounded WaitForCacheSync crashlooped the whole console
// over -- and asserts newRunStore still returns within
// deploymentLookupTimeout's bound rather than hanging.
func TestNewRunStoreBoundsTheDeploymentLookupAgainstARealTransport(t *testing.T) {
	handler, release := blockingHandler()
	srv := httptest.NewServer(handler)
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	client, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatalf("kubernetes.NewForConfig() error = %v", err)
	}

	start := time.Now()
	store := newRunStore(context.Background(), client, "aicrme", "aicrme")
	elapsed := time.Since(start)

	// Generous slack over deploymentLookupTimeout: the property under test
	// is "bounded", not "bounded to the millisecond" -- CI schedulers add
	// jitter a tight bound would flake on.
	const slack = 5 * time.Second
	if elapsed > deploymentLookupTimeout+slack {
		t.Fatalf("newRunStore took %v against a server that never answers, want it bounded near deploymentLookupTimeout (%v)", elapsed, deploymentLookupTimeout)
	}
	if store == nil {
		t.Fatal("newRunStore() = nil, want the in-memory fallback")
	}
}

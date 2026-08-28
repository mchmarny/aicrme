package steps

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/mchmarny/aicrme/internal/applier"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

type fakeHelmLister struct {
	byNamespace map[string][]string
	errFor      map[string]error
}

func (f *fakeHelmLister) List(_ context.Context, namespace string) ([]string, error) {
	if err, ok := f.errFor[namespace]; ok {
		return nil, err
	}
	return f.byNamespace[namespace], nil
}

// A release that existed before Apply must be recorded, because Reset's
// entire ownership claim rests on this list.
func TestSnapshotOwnershipRecordsPreexistingReleases(t *testing.T) {
	h := &fakeHelmLister{byNamespace: map[string][]string{
		"gpu-operator":  {"gpu-operator", "somebody-elses-thing"},
		"kai-scheduler": {},
	}}
	kube := fake.NewSimpleClientset(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-operator", UID: "ns-uid-1"},
	})

	got := snapshotOwnership(context.Background(), h, kube, []string{"gpu-operator", "kai-scheduler"})

	wantReleases := []engine.ReleaseRef{
		{Name: "gpu-operator", Namespace: "gpu-operator"},
		{Name: "somebody-elses-thing", Namespace: "gpu-operator"},
	}
	if !reflect.DeepEqual(got.Releases, wantReleases) {
		t.Errorf("Releases = %#v, want %#v", got.Releases, wantReleases)
	}
}

// Existence, and only existence. The namespace's UID was recorded here too,
// back when Reset deleted namespaces and had to prove it was addressing the
// same OBJECT rather than a name someone else had recreated. Reset reports
// namespaces now, so the one thing still worth knowing is whether the run
// created this one or merely used it.
func TestSnapshotOwnershipRecordsWhetherTheNamespaceExisted(t *testing.T) {
	h := &fakeHelmLister{byNamespace: map[string][]string{}}
	kube := fake.NewSimpleClientset(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-operator", UID: "ns-uid-1"},
	})

	got := snapshotOwnership(context.Background(), h, kube, []string{"gpu-operator", "kai-scheduler"})

	byName := map[string]engine.NamespaceRef{}
	for _, ns := range got.Namespaces {
		byName[ns.Name] = ns
	}
	if ns := byName["gpu-operator"]; !ns.Existed {
		t.Errorf("gpu-operator = %#v, want Existed=true", ns)
	}
	if ns := byName["kai-scheduler"]; ns.Existed {
		t.Errorf("kai-scheduler = %#v, want Existed=false", ns)
	}
	// A namespace that does not exist is not a failure to observe it: it is
	// the observation, and the one that makes the namespace deletable later.
	if ns := byName["kai-scheduler"]; ns.SnapshotErr != "" {
		t.Errorf("kai-scheduler carries SnapshotErr %q -- NotFound is an answer, not an error", ns.SnapshotErr)
	}
}

// A snapshot that fails must not fail the install -- Apply is the long pole
// of the demo -- but it must be recorded, because every release in that
// namespace becomes unprovable and Reset has to skip it.
func TestSnapshotOwnershipRecordsPerNamespaceFailure(t *testing.T) {
	h := &fakeHelmLister{
		byNamespace: map[string][]string{"gpu-operator": {"gpu-operator"}},
		errFor:      map[string]error{"monitoring": errors.New("connection refused")},
	}
	kube := fake.NewSimpleClientset()

	got := snapshotOwnership(context.Background(), h, kube, []string{"gpu-operator", "monitoring"})

	byName := map[string]engine.NamespaceRef{}
	for _, ns := range got.Namespaces {
		byName[ns.Name] = ns
	}
	if byName["monitoring"].SnapshotErr == "" {
		t.Error("monitoring has no SnapshotErr -- an unprovable namespace must say so")
	}
	if byName["gpu-operator"].SnapshotErr != "" {
		t.Errorf("gpu-operator carries SnapshotErr %q -- one namespace's failure must not taint another",
			byName["gpu-operator"].SnapshotErr)
	}
}

// A namespace whose release list could not be read must not contribute
// releases to the evidence: a partial list reads as "these are the only
// releases that pre-existed", which is exactly the false negative that gets
// a bystander uninstalled.
//
// The healthy namespace is the positive control, and it is what keeps this
// test honest: asserting only the absence of monitoring's releases would
// pass against a snapshotOwnership that returned nothing at all.
func TestSnapshotOwnershipRecordsNoReleasesForANamespaceItCouldNotList(t *testing.T) {
	h := &fakeHelmLister{
		byNamespace: map[string][]string{"gpu-operator": {"gpu-operator"}},
		errFor:      map[string]error{"monitoring": errors.New("connection refused")},
	}

	got := snapshotOwnership(context.Background(), h, fake.NewSimpleClientset(),
		[]string{"gpu-operator", "monitoring"})

	want := []engine.ReleaseRef{{Name: "gpu-operator", Namespace: "gpu-operator"}}
	if !reflect.DeepEqual(got.Releases, want) {
		t.Errorf("Releases = %#v, want exactly %#v -- the readable namespace contributes, the unreadable one does not",
			got.Releases, want)
	}
}

// Outside a cluster both seams are nil (see cmd/aicrme/main.go's kube
// client). Recording an empty snapshot would claim every release was
// created by this run; recording a failure per namespace claims nothing
// was. Only the second is safe.
func TestSnapshotOwnershipFailsClosedWithoutClients(t *testing.T) {
	got := snapshotOwnership(context.Background(), nil, nil, []string{"gpu-operator", "kai-scheduler"})

	if len(got.Namespaces) != 2 {
		t.Fatalf("Namespaces = %#v, want one row per requested namespace", got.Namespaces)
	}
	for _, ns := range got.Namespaces {
		if ns.SnapshotErr == "" {
			t.Errorf("%s has no SnapshotErr -- with no client, nothing was observed", ns.Name)
		}
	}
}

// Sorted and deduplicated. Two components sharing a namespace (the recipe
// routinely has several) must not produce two rows, and the order must not
// depend on recipe.json's ordering -- a record diffed across two runs
// should read as a diff of the cluster, not of the iteration.
func TestSnapshotOwnershipIsSortedAndDeduplicated(t *testing.T) {
	h := &fakeHelmLister{byNamespace: map[string][]string{}}
	kube := fake.NewSimpleClientset()

	got := snapshotOwnership(context.Background(), h, kube,
		[]string{"monitoring", "gpu-operator", "monitoring", "cert-manager"})

	names := make([]string, 0, len(got.Namespaces))
	for _, ns := range got.Namespaces {
		names = append(names, ns.Name)
	}
	want := []string{"cert-manager", "gpu-operator", "monitoring"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("namespaces = %v, want %v", names, want)
	}
}

type recordingSpecExec struct {
	specs  []applier.Spec
	stdout string
	err    error
}

func (r *recordingSpecExec) Run(_ context.Context, spec applier.Spec, out io.Writer) error {
	r.specs = append(r.specs, spec)
	_, _ = io.WriteString(out, r.stdout)
	return r.err
}

// Every status is load-bearing: plain `helm list` hides failed, pending and
// superseded releases, and a release left in any of those states by an
// earlier attempt is still one this run did not create. Omitting them would
// make that release invisible to the snapshot and therefore fair game for
// Reset to uninstall.
//
// Asserted as the explicit flags rather than --all because the two are only
// equivalent under helm 3: helm 4 removed --all from `list` and rejects it.
// See helmLister.List for why that matters here.
func TestHelmListerAsksHelmForEveryReleaseState(t *testing.T) {
	e := &recordingSpecExec{stdout: "gpu-operator\nsomebody-elses-thing\n"}

	got, err := NewHelmLister(e).List(context.Background(), "gpu-operator")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}

	want := []string{"gpu-operator", "somebody-elses-thing"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("List() = %v, want %v", got, want)
	}
	if len(e.specs) != 1 {
		t.Fatalf("ran %d commands, want 1", len(e.specs))
	}
	wantArgv := []string{
		"helm", "list", "--namespace", "gpu-operator",
		"--deployed", "--failed", "--pending", "--superseded", "--uninstalled", "--uninstalling",
		"--short",
	}
	if !reflect.DeepEqual(e.specs[0].Argv, wantArgv) {
		t.Errorf("argv = %v, want %v", e.specs[0].Argv, wantArgv)
	}
}

// A helm that exits non-zero has not told us the namespace is empty.
func TestHelmListerReportsAFailureRatherThanAnEmptyList(t *testing.T) {
	e := &recordingSpecExec{stdout: "Error: Kubernetes cluster unreachable\n", err: errors.New("exit status 1")}

	got, err := NewHelmLister(e).List(context.Background(), "gpu-operator")
	if err == nil {
		t.Fatalf("List() = %v, error = nil, want an error", got)
	}
	if got != nil {
		t.Errorf("List() = %v, want nil alongside the error", got)
	}
	// The operator has to be able to act on this without the pod log.
	if !strings.Contains(err.Error(), "gpu-operator") {
		t.Errorf("error = %q, want it to name the namespace", err)
	}
}

// helm --short prints nothing at all for an empty namespace; a naive split
// on "\n" turns that into one release named "".
func TestHelmListerReturnsNothingForAnEmptyNamespace(t *testing.T) {
	for _, stdout := range []string{"", "\n", "  \n\n"} {
		got, err := NewHelmLister(&recordingSpecExec{stdout: stdout}).
			List(context.Background(), "kai-scheduler")
		if err != nil {
			t.Fatalf("List(%q) error = %v", stdout, err)
		}
		if len(got) != 0 {
			t.Errorf("List(%q) = %#v, want no releases", stdout, got)
		}
	}
}

// recipeNamespaces reads the durable artifact, not the bundle directory:
// Reset's evidence has to be gathered from something that survives the
// pod, and recipe.json is persisted in the run record while the bundle
// lives in an emptyDir.
func TestRecipeNamespacesReadsTheResolvedRecipe(t *testing.T) {
	run := &engine.Run{Artifacts: map[string][]byte{
		"recipe.json": []byte(`{"components":[
			{"name":"gpu-operator","namespace":"gpu-operator"},
			{"name":"nfd","namespace":"node-feature-discovery"},
			{"name":"no-namespace"}
		]}`),
	}}

	got := recipeNamespaces(run)

	want := []string{"gpu-operator", "node-feature-discovery"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("recipeNamespaces() = %v, want %v -- a component with no namespace has nothing to snapshot", got, want)
	}
}

// A run with no recipe.json (or an unreadable one) yields no namespaces,
// which yields no evidence, which makes Reset a no-op. That is the right
// direction to fail in, and it must not panic.
func TestRecipeNamespacesToleratesAMissingRecipe(t *testing.T) {
	for name, run := range map[string]*engine.Run{
		"absent":  {Artifacts: map[string][]byte{}},
		"garbage": {Artifacts: map[string][]byte{"recipe.json": []byte("{{{")}},
	} {
		if got := recipeNamespaces(run); len(got) != 0 {
			t.Errorf("%s: recipeNamespaces() = %v, want none", name, got)
		}
	}
}

// recipeRun builds a run whose recipe installs two components, and whose
// cluster already holds whatever the lister reports.
func recipeRun() *engine.Run {
	r := &engine.Run{
		ID:        "run-under-test",
		Decisions: map[string]string{"apply": "yes"},
		Artifacts: map[string][]byte{
			"bundle.path": []byte("/tmp/bundle"),
			"recipe.json": []byte(`{"components":[
				{"name":"cert-manager","namespace":"cert-manager"},
				{"name":"kai-scheduler","namespace":"kai-scheduler"}]}`),
		},
	}
	return r
}

func applyWith(h HelmLister) engine.Step {
	return NewApply(applier.New(&countingExec{}), ApplyConfig{
		Helm: h,
		Kube: fake.NewSimpleClientset(),
	})
}

// countingExec records how many times deploy.sh was actually spawned, which
// is the only proof that a refusal happened BEFORE the cluster was touched.
type countingExec struct{ runs int }

func (c *countingExec) Run(_ context.Context, _ applier.Spec, out io.Writer) error {
	c.runs++
	_, _ = io.WriteString(out, "✓ Pre-flight checks passed\n")
	return nil
}

// THE 2026-08-28 FAILURE, in one test.
//
// A run installed 16 components on a real cluster and reported 16/16. Its
// gang then failed to place, 0/2, because a SECOND run had installed over
// the first without a Reset in between: kai-scheduler's SchedulingShard
// survives a reinstall by design, the Deployment it owns is never recreated,
// and the cluster kept running the first install's scheduler pod against a
// control plane replaced underneath it. Apply reported complete success
// throughout.
func TestApplyRefusesToInstallOverAnExistingInstall(t *testing.T) {
	h := &fakeHelmLister{byNamespace: map[string][]string{
		"cert-manager":  {"cert-manager"},
		"kai-scheduler": {"kai-scheduler"},
	}}
	exec := &countingExec{}
	step := NewApply(applier.New(exec), ApplyConfig{Helm: h, Kube: fake.NewSimpleClientset()})

	err := step.Run(context.Background(), recipeRun(), func(bus.Event) {})

	if err == nil {
		t.Fatal("Apply installed over an existing install -- the run would report success and then not schedule")
	}
	// Refused BEFORE deploy.sh, not after: the whole point is that nothing
	// is touched.
	if exec.runs != 0 {
		t.Errorf("deploy.sh ran %d times, want 0 -- the refusal must precede any cluster mutation", exec.runs)
	}
	// The message has to be actionable: what is in the way, and the way out.
	for _, want := range []string{"cert-manager", "kai-scheduler", "Reset", "helm uninstall"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

// A bystander is somebody else's release sharing a recipe namespace. It must
// not make this run refuse -- the same distinction teardown draws when it
// declines to uninstall one.
func TestApplyIgnoresReleasesTheRecipeDoesNotInstall(t *testing.T) {
	h := &fakeHelmLister{byNamespace: map[string][]string{
		"cert-manager": {"somebody-elses-thing"},
	}}
	exec := &countingExec{}
	step := NewApply(applier.New(exec), ApplyConfig{Helm: h, Kube: fake.NewSimpleClientset()})

	if err := step.Run(context.Background(), recipeRun(), func(bus.Event) {}); err != nil {
		t.Fatalf("Apply refused over a bystander release: %v", err)
	}
	if exec.runs != 1 {
		t.Errorf("deploy.sh ran %d times, want 1", exec.runs)
	}
}

// THE TRAP IN THIS GUARD, and the reason it keys on the first attempt.
//
// Retry re-runs Apply, and by then the run's OWN partial install is on the
// cluster. A guard that looked again would refuse every retry of a run that
// failed at component 3 of 16 -- which is the ordinary failure this console
// exists to help with, and the Failed screen offers a Retry button for.
func TestApplyRetryIsNotASecondInstall(t *testing.T) {
	h := &fakeHelmLister{byNamespace: map[string][]string{}}
	exec := &countingExec{}
	step := NewApply(applier.New(exec), ApplyConfig{Helm: h, Kube: fake.NewSimpleClientset()})
	run := recipeRun()

	if err := step.Run(context.Background(), run, func(bus.Event) {}); err != nil {
		t.Fatalf("first Apply error = %v", err)
	}
	// The cluster now holds what this run installed, exactly as it would
	// after a partial failure.
	h.byNamespace["cert-manager"] = []string{"cert-manager"}
	h.byNamespace["kai-scheduler"] = []string{"kai-scheduler"}

	if err := step.Run(context.Background(), run, func(bus.Event) {}); err != nil {
		t.Fatalf("retry was refused as if it were a second install: %v", err)
	}
	if exec.runs != 2 {
		t.Errorf("deploy.sh ran %d times, want 2 -- the retry must proceed", exec.runs)
	}
}

// The first snapshot is the only true one, and a retry must not overwrite it.
//
// Re-snapshotting on retry records the run's own releases as things that
// pre-existed it, which hands them to teardown as somebody else's and leaves
// them installed at Reset -- the exact residue Reset exists to remove.
func TestApplyKeepsTheFirstOwnershipSnapshotAcrossARetry(t *testing.T) {
	h := &fakeHelmLister{byNamespace: map[string][]string{}}
	step := applyWith(h)
	run := recipeRun()

	if err := step.Run(context.Background(), run, func(bus.Event) {}); err != nil {
		t.Fatalf("first Apply error = %v", err)
	}
	if len(run.Ownership.Releases) != 0 {
		t.Fatalf("Releases = %v, want none recorded on a clean cluster", run.Ownership.Releases)
	}

	h.byNamespace["cert-manager"] = []string{"cert-manager"}
	if err := step.Run(context.Background(), run, func(bus.Event) {}); err != nil {
		t.Fatalf("retry error = %v", err)
	}
	if len(run.Ownership.Releases) != 0 {
		t.Errorf("Releases = %v after a retry, want none -- the run's own install is not something that pre-existed it",
			run.Ownership.Releases)
	}
}

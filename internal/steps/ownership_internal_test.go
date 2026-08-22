package steps

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/mchmarny/aicrme/internal/applier"
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

// Existence AND UID: a namespace deleted and recreated between Apply and
// Reset is a different object wearing the same name, and deleting it would
// be deleting someone else's.
func TestSnapshotOwnershipRecordsNamespaceExistenceAndUID(t *testing.T) {
	h := &fakeHelmLister{byNamespace: map[string][]string{}}
	kube := fake.NewSimpleClientset(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-operator", UID: "ns-uid-1"},
	})

	got := snapshotOwnership(context.Background(), h, kube, []string{"gpu-operator", "kai-scheduler"})

	byName := map[string]engine.NamespaceRef{}
	for _, ns := range got.Namespaces {
		byName[ns.Name] = ns
	}
	if ns := byName["gpu-operator"]; !ns.Existed || ns.UID != "ns-uid-1" {
		t.Errorf("gpu-operator = %#v, want Existed=true UID=ns-uid-1", ns)
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

// The UID that decides a namespace deletion is the one the namespace had
// when THIS RUN created it, which by definition does not exist until Apply
// has run. Without this read-back there is no way to tell the namespace
// Apply created from a different object someone else recreated at the same
// name in the interim (design section 5).
func TestConfirmCreatedNamespacesRecordsTheUIDApplyCreated(t *testing.T) {
	own := engine.Ownership{Namespaces: []engine.NamespaceRef{
		{Name: "kai-scheduler", Existed: false},
	}}
	kube := fake.NewSimpleClientset(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "kai-scheduler", UID: "created-uid-1"},
	})

	confirmCreatedNamespaces(context.Background(), kube, &own)

	if got := own.Namespaces[0].CreatedUID; got != "created-uid-1" {
		t.Errorf("CreatedUID = %q, want created-uid-1", got)
	}
}

// A namespace that already existed is never deleted, so reading a UID back
// for it would record evidence for a decision that is never made -- and
// worse, would make an adopted namespace look like a created one.
func TestConfirmCreatedNamespacesLeavesPreexistingOnesAlone(t *testing.T) {
	own := engine.Ownership{Namespaces: []engine.NamespaceRef{
		{Name: "gpu-operator", Existed: true, UID: "pre-existing-uid"},
	}}
	kube := fake.NewSimpleClientset(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-operator", UID: "pre-existing-uid"},
	})

	confirmCreatedNamespaces(context.Background(), kube, &own)

	if got := own.Namespaces[0].CreatedUID; got != "" {
		t.Errorf("CreatedUID = %q, want empty -- this run did not create it", got)
	}
}

// Three ways to arrive at no namespace: the component failed before
// creating it, the chart shipped its own Namespace manifest, or the read
// back failed. All three mean the same thing to Reset -- do not delete --
// and all three must leave CreatedUID empty rather than guessing.
func TestConfirmCreatedNamespacesLeavesTheUIDEmptyWhenItCannotConfirm(t *testing.T) {
	own := engine.Ownership{Namespaces: []engine.NamespaceRef{
		{Name: "never-created", Existed: false},
		{Name: "unsnapshotted", Existed: false, SnapshotErr: "connection refused"},
	}}

	confirmCreatedNamespaces(context.Background(), fake.NewSimpleClientset(), &own)

	for _, ns := range own.Namespaces {
		if ns.CreatedUID != "" {
			t.Errorf("%s: CreatedUID = %q, want empty", ns.Name, ns.CreatedUID)
		}
	}
}

// Nil client outside a cluster must not panic the Apply step at the very
// moment it has finished installing.
func TestConfirmCreatedNamespacesToleratesANilClient(t *testing.T) {
	own := engine.Ownership{Namespaces: []engine.NamespaceRef{{Name: "kai-scheduler"}}}

	confirmCreatedNamespaces(context.Background(), nil, &own)

	if own.Namespaces[0].CreatedUID != "" {
		t.Error("CreatedUID is set with no client to have read it")
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

// --all is load-bearing: `helm list` without it hides failed, pending and
// superseded releases, and a release left in any of those states by an
// earlier attempt is still one this run did not create. Omitting it would
// make that release invisible to the snapshot and therefore fair game for
// Reset to uninstall.
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
	wantArgv := []string{"helm", "list", "--namespace", "gpu-operator", "--all", "--short"}
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

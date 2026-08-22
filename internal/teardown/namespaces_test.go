package teardown_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/mchmarny/aicrme/internal/engine"
	"github.com/mchmarny/aicrme/internal/teardown"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// testNamespace is the namespace every test in this file reasons about.
const testNamespace = "kai-scheduler"

// The resource kinds the stub API server serves. Deliberately broader than
// the six workload kinds revision 1 of the design checked: the point of
// discovery is that a kind nobody thought of still counts.
var (
	gvrService        = schema.GroupVersionResource{Version: "v1", Resource: "services"}
	gvrSecret         = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	gvrConfigMap      = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	gvrServiceAccount = schema.GroupVersionResource{Version: "v1", Resource: "serviceaccounts"}
	gvrEvent          = schema.GroupVersionResource{Version: "v1", Resource: "events"}
	gvrRole           = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}
	gvrCronJob        = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "cronjobs"}
	gvrPDB            = schema.GroupVersionResource{Group: "policy", Version: "v1", Resource: "poddisruptionbudgets"}
	gvrWidget         = schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "widgets"}
)

var allGVRs = []schema.GroupVersionResource{
	gvrService, gvrSecret, gvrConfigMap, gvrServiceAccount, gvrEvent,
	gvrRole, gvrCronJob, gvrPDB, gvrWidget,
}

var listKinds = map[schema.GroupVersionResource]string{
	gvrService:        "ServiceList",
	gvrSecret:         "SecretList",
	gvrConfigMap:      "ConfigMapList",
	gvrServiceAccount: "ServiceAccountList",
	gvrEvent:          "EventList",
	gvrRole:           "RoleList",
	gvrCronJob:        "CronJobList",
	gvrPDB:            "PodDisruptionBudgetList",
	gvrWidget:         "WidgetList",
}

// kindFor is the singular kind behind a list kind, which unstructured
// objects have to carry for the dynamic fake to track them.
func kindFor(gvr schema.GroupVersionResource) string {
	return strings.TrimSuffix(listKinds[gvr], "List")
}

func object(gvr schema.GroupVersionResource, name string) *unstructured.Unstructured {
	apiVersion := gvr.Version
	if gvr.Group != "" {
		apiVersion = gvr.Group + "/" + gvr.Version
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion,
		"kind":       kindFor(gvr),
		"metadata":   map[string]any{"namespace": testNamespace, "name": name},
	}}
}

type stubDiscoverer struct {
	gvrs []schema.GroupVersionResource
	err  error
}

func (s stubDiscoverer) NamespacedResources() ([]schema.GroupVersionResource, error) {
	return s.gvrs, s.err
}

func newDyn(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, objs...)
}

func liveNamespace(name, uid string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, UID: k8stypes.UID(uid)}}
}

func noEmitNS(teardown.NamespaceOutcome) {}

// ownedNamespace is the evidence for a namespace this run created: absent
// from the pre-Apply snapshot, with the UID Apply's read-back recorded.
func ownedNamespace(createdUID string) engine.Ownership {
	return engine.Ownership{Namespaces: []engine.NamespaceRef{
		{Name: testNamespace, Existed: false, CreatedUID: createdUID},
	}}
}

// The whole rule in one test: created by this run, UID unchanged, nothing
// left in it.
func TestNamespacesDeletesOnlyWhatThisRunCreatedAndEmptied(t *testing.T) {
	kube := fake.NewSimpleClientset(liveNamespace("kai-scheduler", "uid-1"))

	out := teardown.Namespaces(context.Background(), kube,
		stubDiscoverer{gvrs: allGVRs}, newDyn(),
		[]string{"kai-scheduler"}, ownedNamespace("uid-1"), noEmitNS)

	if len(out) != 1 || !out[0].Deleted {
		t.Fatalf("outcomes = %+v, want kai-scheduler deleted", out)
	}
	if out[0].Skip != "" || out[0].Err != "" {
		t.Errorf("outcome = %+v, want no skip and no error", out[0])
	}
	_, err := kube.CoreV1().Namespaces().Get(context.Background(), "kai-scheduler", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("namespace Get() = %v, want NotFound -- the delete must actually have been issued", err)
	}
}

// A namespace that existed before Apply is never deleted, whatever is in
// it -- this console did not make it and does not get to unmake it.
func TestNamespacesKeepsOneThatExistedBeforeApply(t *testing.T) {
	kube := fake.NewSimpleClientset(liveNamespace("gpu-operator", "uid-1"))
	own := engine.Ownership{Namespaces: []engine.NamespaceRef{
		{Name: "gpu-operator", Existed: true, UID: "uid-1"},
	}}

	out := teardown.Namespaces(context.Background(), kube,
		stubDiscoverer{gvrs: allGVRs}, newDyn(),
		[]string{"gpu-operator"}, own, noEmitNS)

	if out[0].Deleted {
		t.Fatal("deleted a namespace that existed before this run")
	}
	if out[0].Skip == "" {
		t.Error("skip reason is empty -- a namespace left behind must be named")
	}
	if _, err := kube.CoreV1().Namespaces().Get(context.Background(), "gpu-operator", metav1.GetOptions{}); err != nil {
		t.Errorf("namespace Get() = %v, want it still present", err)
	}
}

// Same name, different object. A namespace deleted and recreated between
// Apply and Reset belongs to whoever recreated it.
func TestNamespacesKeepsOneWhoseUIDChanged(t *testing.T) {
	kube := fake.NewSimpleClientset(liveNamespace("kai-scheduler", "uid-SOMEONE-ELSE"))

	out := teardown.Namespaces(context.Background(), kube,
		stubDiscoverer{gvrs: allGVRs}, newDyn(),
		[]string{"kai-scheduler"}, ownedNamespace("uid-1"), noEmitNS)

	if out[0].Deleted {
		t.Fatal("deleted a namespace that is a different object than the one this run created")
	}
	if !strings.Contains(out[0].Skip, "uid-1") || !strings.Contains(out[0].Skip, "uid-SOMEONE-ELSE") {
		t.Errorf("Skip = %q, want it to name both UIDs so the operator can see the mismatch", out[0].Skip)
	}
}

// A namespace Apply never confirmed creating is not one this run can prove
// it owns: the component may have failed first, or the chart may have
// shipped its own Namespace manifest.
func TestNamespacesKeepsOneWhoseCreationWasNeverConfirmed(t *testing.T) {
	kube := fake.NewSimpleClientset(liveNamespace("kai-scheduler", "uid-1"))

	out := teardown.Namespaces(context.Background(), kube,
		stubDiscoverer{gvrs: allGVRs}, newDyn(),
		[]string{"kai-scheduler"}, ownedNamespace(""), noEmitNS)

	if out[0].Deleted {
		t.Fatal("deleted a namespace whose creation by this run was never confirmed")
	}
	if out[0].Skip == "" {
		t.Error("skip reason is empty")
	}
}

// Discovery-driven, not a hardcoded kind list: revision 1 checked six
// workload kinds and would have deleted a namespace holding Services,
// Secrets, ConfigMaps, RBAC, CronJobs, PDBs or any custom resource.
func TestNamespacesKeepsOneHoldingAResourceOfAnyKind(t *testing.T) {
	for name, gvr := range map[string]schema.GroupVersionResource{
		"Service":              gvrService,
		"Secret":               gvrSecret,
		"ConfigMap":            gvrConfigMap,
		"Role":                 gvrRole,
		"CronJob":              gvrCronJob,
		"PodDisruptionBudget":  gvrPDB,
		"a custom resource":    gvrWidget,
		"a ServiceAccount":     gvrServiceAccount,
		"an unrecognized kind": gvrWidget,
	} {
		t.Run(name, func(t *testing.T) {
			kube := fake.NewSimpleClientset(liveNamespace("kai-scheduler", "uid-1"))
			dyn := newDyn(object(gvr, "a-bystander"))

			out := teardown.Namespaces(context.Background(), kube,
				stubDiscoverer{gvrs: allGVRs}, dyn,
				[]string{"kai-scheduler"}, ownedNamespace("uid-1"), noEmitNS)

			if out[0].Deleted {
				t.Fatalf("deleted a namespace still holding a %s", name)
			}
			if !strings.Contains(out[0].Skip, "a-bystander") {
				t.Errorf("Skip = %q, want it to name what is still in the namespace", out[0].Skip)
			}
		})
	}
}

// The two objects Kubernetes itself puts in every namespace, plus the
// events every namespace accumulates. Counting any of them would make no
// namespace ever empty and Reset would delete nothing, ever -- which is a
// silent, total failure of the feature rather than a visible one.
func TestNamespacesIgnoresWhatKubernetesPutsInEveryNamespace(t *testing.T) {
	kube := fake.NewSimpleClientset(liveNamespace("kai-scheduler", "uid-1"))
	dyn := newDyn(
		object(gvrServiceAccount, "default"),
		object(gvrConfigMap, "kube-root-ca.crt"),
		object(gvrEvent, "kai-scheduler.17c9f2a1"),
	)

	out := teardown.Namespaces(context.Background(), kube,
		stubDiscoverer{gvrs: allGVRs}, dyn,
		[]string{"kai-scheduler"}, ownedNamespace("uid-1"), noEmitNS)

	if !out[0].Deleted {
		t.Errorf("outcome = %+v, want deleted -- these three are not bystanders", out[0])
	}
}

// The exclusions are by name, not by kind. A ServiceAccount someone else
// created is a bystander like any other, and excluding the whole kind is
// how this rule becomes unsafe again.
func TestNamespacesIgnoresOnlyTheExactBuiltInNames(t *testing.T) {
	for name, obj := range map[string]*unstructured.Unstructured{
		"a non-default ServiceAccount": object(gvrServiceAccount, "somebody-elses-sa"),
		"a non-root-CA ConfigMap":      object(gvrConfigMap, "somebody-elses-config"),
	} {
		t.Run(name, func(t *testing.T) {
			kube := fake.NewSimpleClientset(liveNamespace("kai-scheduler", "uid-1"))

			out := teardown.Namespaces(context.Background(), kube,
				stubDiscoverer{gvrs: allGVRs}, newDyn(obj),
				[]string{"kai-scheduler"}, ownedNamespace("uid-1"), noEmitNS)

			if out[0].Deleted {
				t.Fatalf("deleted a namespace holding %s", name)
			}
		})
	}
}

// Fail closed. An unanswered question is not an empty namespace.
func TestNamespacesKeepsOneItCannotInspect(t *testing.T) {
	kube := fake.NewSimpleClientset(liveNamespace("kai-scheduler", "uid-1"))

	out := teardown.Namespaces(context.Background(), kube,
		stubDiscoverer{err: errors.New("the server is currently unable to handle the request")},
		newDyn(), []string{"kai-scheduler"}, ownedNamespace("uid-1"), noEmitNS)

	if out[0].Deleted {
		t.Fatal("deleted a namespace whose contents could not be enumerated")
	}
	if out[0].Err == "" {
		t.Error("Err is empty -- a failure to inspect is reported, not passed over silently")
	}
}

// Same rule one layer down: discovery succeeded, but listing one of the
// kinds it named did not.
func TestNamespacesKeepsOneWhoseContentsCannotBeListed(t *testing.T) {
	kube := fake.NewSimpleClientset(liveNamespace("kai-scheduler", "uid-1"))
	dyn := newDyn()
	dyn.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("secrets is forbidden: RBAC: no policy matched")
	})

	out := teardown.Namespaces(context.Background(), kube,
		stubDiscoverer{gvrs: allGVRs}, dyn,
		[]string{"kai-scheduler"}, ownedNamespace("uid-1"), noEmitNS)

	if out[0].Deleted {
		t.Fatal("deleted a namespace one of whose kinds could not be listed")
	}
	if !strings.Contains(out[0].Err, "secrets") {
		t.Errorf("Err = %q, want it to name the kind that could not be listed", out[0].Err)
	}
}

// A chart that ships its own Namespace manifest has already had it removed
// by helm uninstall (AICR downgrades --create-namespace for exactly these).
func TestNamespacesTreatsAnAlreadyGoneNamespaceAsSuccess(t *testing.T) {
	out := teardown.Namespaces(context.Background(), fake.NewSimpleClientset(),
		stubDiscoverer{gvrs: allGVRs}, newDyn(),
		[]string{"kai-scheduler"}, ownedNamespace("uid-1"), noEmitNS)

	if out[0].Err != "" {
		t.Errorf("Err = %q, want none -- already gone is the desired end state", out[0].Err)
	}
	if out[0].Deleted {
		t.Error("Deleted = true, want false -- this Reset did not remove it, an uninstall already had")
	}
}

// A namespace with no evidence at all. Reached by a run recovered from a
// record written before ownership was tracked.
func TestNamespacesKeepsOneWithNoEvidence(t *testing.T) {
	kube := fake.NewSimpleClientset(liveNamespace("kai-scheduler", "uid-1"))

	out := teardown.Namespaces(context.Background(), kube,
		stubDiscoverer{gvrs: allGVRs}, newDyn(),
		[]string{"kai-scheduler"}, engine.Ownership{}, noEmitNS)

	if out[0].Deleted {
		t.Fatal("deleted a namespace with no ownership evidence whatsoever")
	}
	if out[0].Skip == "" {
		t.Error("skip reason is empty")
	}
}

// Discovery is one call per Reset, not one per namespace. Ten namespaces
// against a cluster with a slow aggregated API is the case that made this
// worth pinning (the plan's open question 2).
func TestNamespacesDiscoversOnceForTheWholeTeardown(t *testing.T) {
	kube := fake.NewSimpleClientset(
		liveNamespace("a", "uid-a"), liveNamespace("b", "uid-b"), liveNamespace("c", "uid-c"))
	d := &countingDiscoverer{gvrs: allGVRs}
	own := engine.Ownership{Namespaces: []engine.NamespaceRef{
		{Name: "a", CreatedUID: "uid-a"}, {Name: "b", CreatedUID: "uid-b"}, {Name: "c", CreatedUID: "uid-c"},
	}}

	teardown.Namespaces(context.Background(), kube, d, newDyn(), []string{"a", "b", "c"}, own, noEmitNS)

	if d.calls != 1 {
		t.Errorf("discovery ran %d times, want 1 for the whole teardown", d.calls)
	}
}

type countingDiscoverer struct {
	gvrs  []schema.GroupVersionResource
	calls int
}

func (c *countingDiscoverer) NamespacedResources() ([]schema.GroupVersionResource, error) {
	c.calls++
	return c.gvrs, nil
}

// Every outcome reaches the caller as it happens, for the same reason
// release outcomes do: a namespace delete waits on the API server's own
// cascade, and the returned slice arrives only after the last one.
func TestNamespacesEmitsEachOutcomeAsItHappens(t *testing.T) {
	kube := fake.NewSimpleClientset(liveNamespace("a", "uid-a"), liveNamespace("b", "uid-b"))
	own := engine.Ownership{Namespaces: []engine.NamespaceRef{
		{Name: "a", CreatedUID: "uid-a"}, {Name: "b", Existed: true},
	}}

	var emitted []teardown.NamespaceOutcome
	out := teardown.Namespaces(context.Background(), kube,
		stubDiscoverer{gvrs: allGVRs}, newDyn(), []string{"a", "b"}, own,
		func(o teardown.NamespaceOutcome) { emitted = append(emitted, o) })

	if len(emitted) != len(out) {
		t.Fatalf("emitted %d outcomes, returned %d", len(emitted), len(out))
	}
	for i := range out {
		if emitted[i] != out[i] {
			t.Errorf("emitted[%d] = %+v, returned %+v", i, emitted[i], out[i])
		}
	}
}

// resourceList is one group-version's worth of a discovery document.
func resourceList(groupVersion string, resources ...metav1.APIResource) *metav1.APIResourceList {
	return &metav1.APIResourceList{GroupVersion: groupVersion, APIResources: resources}
}

func namespacedResource(name string) metav1.APIResource {
	return metav1.APIResource{Name: name, Namespaced: true, Verbs: metav1.Verbs{"get", "list", "delete"}}
}

func fakeDiscovery(t *testing.T, lists ...*metav1.APIResourceList) (*fake.Clientset, *fakediscovery.FakeDiscovery) {
	t.Helper()
	cs := fake.NewSimpleClientset()
	d, ok := cs.Discovery().(*fakediscovery.FakeDiscovery)
	if !ok {
		t.Fatalf("Discovery() is %T, want *fakediscovery.FakeDiscovery", cs.Discovery())
	}
	d.Resources = lists
	return cs, d
}

// The adapter is what stands between the emptiness rule and a real API
// server, and nothing else in this package exercises it: the rule's own
// tests stub Discoverer out entirely.
func TestDiscovererReturnsEveryNamespacedListableKind(t *testing.T) {
	_, d := fakeDiscovery(t,
		resourceList("v1",
			namespacedResource("secrets"),
			// Cluster-scoped: it cannot occupy a namespace, and listing it
			// per namespace would report the whole cluster's worth.
			metav1.APIResource{Name: "nodes", Namespaced: false, Verbs: metav1.Verbs{"list"}},
			// A subresource, which is not an object at all.
			metav1.APIResource{Name: "pods/status", Namespaced: true, Verbs: metav1.Verbs{"get"}},
		),
		resourceList("example.com/v1", namespacedResource("widgets")),
	)

	got, err := teardown.NewDiscoverer(d).NamespacedResources()
	if err != nil {
		t.Fatalf("NamespacedResources() error = %v", err)
	}

	want := []schema.GroupVersionResource{
		{Version: "v1", Resource: "secrets"},
		{Group: "example.com", Version: "v1", Resource: "widgets"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NamespacedResources() = %v, want %v", got, want)
	}
}

// A CRD served at two versions returns the same objects either way, so
// listing both doubles the API calls for an identical answer.
func TestDiscovererReturnsOneVersionPerResource(t *testing.T) {
	_, d := fakeDiscovery(t,
		resourceList("example.com/v1", namespacedResource("widgets")),
		resourceList("example.com/v1beta1", namespacedResource("widgets")),
	)

	got, err := teardown.NewDiscoverer(d).NamespacedResources()
	if err != nil {
		t.Fatalf("NamespacedResources() error = %v", err)
	}
	if len(got) != 1 {
		t.Errorf("NamespacedResources() = %v, want one entry for example.com/widgets", got)
	}
}

// Fail closed at the source. A partial discovery document is the case
// where an aggregated API server is down -- and its kinds are the custom
// ones most likely to be holding something, so "I could not ask about this
// group" must never read as "this group has nothing".
func TestDiscovererReportsAPartialDiscoveryAsAFailure(t *testing.T) {
	cs, d := fakeDiscovery(t, resourceList("v1", namespacedResource("secrets")))
	cs.PrependReactor("get", "resource", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("unable to retrieve the complete list of server APIs: metrics.k8s.io/v1beta1")
	})

	got, err := teardown.NewDiscoverer(d).NamespacedResources()
	if err == nil {
		t.Fatalf("NamespacedResources() = %v, error = nil, want the partial discovery surfaced", got)
	}
	if got != nil {
		t.Errorf("NamespacedResources() = %v, want nil alongside the error", got)
	}
}

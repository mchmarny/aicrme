package teardown

import (
	"context"
	"fmt"
	"slices"

	"github.com/mchmarny/aicrme/internal/engine"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// Discoverer enumerates every namespaced resource kind the API server
// serves. Narrower than discovery.DiscoveryInterface on purpose, and not
// merely for taste: client-go's own FakeDiscovery implements
// ServerPreferredNamespacedResources as `return nil, nil`, so a test
// written against that interface would see a cluster with no kinds at all,
// find every namespace empty, and pass -- in the exact direction that
// deletes a namespace holding someone else's work. This seam is what makes
// the emptiness rule testable at all. NewDiscoverer is the production
// implementation.
type Discoverer interface {
	NamespacedResources() ([]schema.GroupVersionResource, error)
}

// NamespaceOutcome is what happened to one namespace. Deleted is the count
// the operator is shown; Skip and Err are why the others were left.
type NamespaceOutcome struct {
	Name string
	// Skip is why this namespace was deliberately left in place. Not a
	// failure: most namespaces a Reset considers are legitimately not its
	// to remove.
	Skip string
	// Err is why an attempt to remove it did not succeed, including a
	// failure to establish whether it was safe to try.
	Err     string
	Deleted bool
}

// builtInObjects are the objects Kubernetes itself puts in every namespace.
// Counting them would make no namespace ever empty, and Reset would delete
// nothing, ever -- a silent total failure of the feature rather than a
// visible one.
//
// Keyed by exact (resource, name), never by kind alone: a ServiceAccount
// somebody else created is a bystander like any other, and excluding the
// whole kind is precisely how revision 1's rule became unsafe. Each entry
// below is one object with one well-known name that one core controller
// creates unconditionally.
var builtInObjects = map[schema.GroupResource]string{
	// Created in every namespace by the service account controller.
	{Resource: "serviceaccounts"}: "default",
	// Published into every namespace by the root CA cert publisher.
	{Resource: "configmaps"}: "kube-root-ca.crt",
}

// eventResources are excluded wholesale rather than by name, because unlike
// the entries above their names are generated per occurrence. An Event is a
// record ABOUT an object rather than a workload in its own right, it is
// garbage collected on its own schedule, and it is destroyed with the
// namespace either way. Counting them would leave every namespace that has
// ever done anything looking permanently occupied.
var eventResources = []schema.GroupResource{
	{Resource: "events"},
	{Group: "events.k8s.io", Resource: "events"},
}

// Namespaces deletes each namespace this run created and left empty, and
// leaves -- with a stated reason -- every namespace it cannot prove all
// three of.
//
// The rule, in the order it is checked (design section 5):
//
//   - Absent from the pre-Apply snapshot. A namespace that existed before
//     this run is never deleted, whatever is in it.
//   - Its UID still matches the one read back after Apply created it. A
//     namespace deleted and recreated by someone else in the interim is a
//     different object wearing the same name.
//   - Empty, established by enumerating the API server's own namespaced
//     kinds rather than a hardcoded list, so a kind nobody thought of still
//     counts as a bystander.
//
// Fail closed throughout: any error discovering or listing leaves the
// namespace in place and is reported. An unanswered question is not an
// empty namespace.
//
// Discovery runs once for the whole teardown, not once per namespace: the
// document is the same for all of them, and a cluster with a slow
// aggregated API turns ten repeats into a visible stall.
func Namespaces(ctx context.Context, k kubernetes.Interface, d Discoverer, dyn dynamic.Interface,
	names []string, own engine.Ownership, emit func(NamespaceOutcome)) []NamespaceOutcome {

	evidence := make(map[string]engine.NamespaceRef, len(own.Namespaces))
	for _, ns := range own.Namespaces {
		evidence[ns.Name] = ns
	}

	// Deliberately not fatal on its own: a discovery failure only matters
	// for a namespace that gets as far as the emptiness check, and every
	// namespace that fails an earlier check should still report the honest
	// reason it was skipped rather than a discovery error it never reached.
	gvrs, discoveryErr := d.NamespacedResources()

	outcomes := make([]NamespaceOutcome, 0, len(names))
	for _, name := range names {
		out := namespaceOutcome(ctx, k, dyn, name, evidence[name], gvrs, discoveryErr)
		outcomes = append(outcomes, out)
		emit(out)
	}
	return outcomes
}

func namespaceOutcome(ctx context.Context, k kubernetes.Interface, dyn dynamic.Interface,
	name string, ref engine.NamespaceRef, gvrs []schema.GroupVersionResource, discoveryErr error) NamespaceOutcome {

	out := NamespaceOutcome{Name: name}

	switch {
	case ref.Name == "":
		out.Skip = "no record of what this namespace looked like before the install, so it is not provably this run's"
		return out
	case ref.SnapshotErr != "":
		out.Skip = "what already existed here could not be recorded before the install (" + ref.SnapshotErr + ")"
		return out
	case ref.Existed:
		out.Skip = "this namespace existed before the install, so it was used rather than created"
		return out
	case ref.CreatedUID == "":
		// The component may have failed before its namespace was created,
		// or the chart shipped its own Namespace manifest. Either way this
		// run never confirmed creating an object here.
		out.Skip = "this run's creation of this namespace was never confirmed, so it is not provably this run's"
		return out
	}

	live, err := k.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		// helm uninstall already removed it, which happens for every chart
		// that ships its own Namespace manifest -- AICR downgrades
		// --create-namespace for exactly those. The end state is the one
		// Reset wanted; it simply is not this call that produced it.
		out.Skip = "already removed, by the uninstall of the release that created it"
		return out
	case err != nil:
		out.Err = "reading the namespace failed: " + err.Error()
		return out
	}

	if string(live.UID) != ref.CreatedUID {
		// Both UIDs named, because "the UID changed" is not something an
		// operator can check afterward -- the object that would have proven
		// it is gone.
		out.Skip = fmt.Sprintf(
			"this is a different object than the one this run created (created %s, found %s), "+
				"so it belongs to whoever recreated it", ref.CreatedUID, live.UID)
		return out
	}

	if discoveryErr != nil {
		out.Err = "could not enumerate the kinds this namespace might hold, so it was left in place: " + discoveryErr.Error()
		return out
	}
	occupant, err := firstOccupant(ctx, dyn, name, gvrs)
	if err != nil {
		out.Err = "could not establish whether this namespace is empty, so it was left in place: " + err.Error()
		return out
	}
	if occupant != "" {
		out.Skip = "still holds " + occupant
		return out
	}

	if err := k.CoreV1().Namespaces().Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		out.Err = "deleting the namespace failed: " + err.Error()
		return out
	}
	out.Deleted = true
	return out
}

// firstOccupant returns a description of the first object found in the
// namespace, or "" if it holds nothing. It stops at the first one: the
// answer is a boolean, and enumerating the rest of a namespace that is
// already known to be occupied costs API calls for information nobody uses.
//
// Any list failure is returned as an error rather than treated as an empty
// kind. An RBAC denial, a partitioned API server and an unreachable
// aggregated API all look like "no objects" to a caller that ignores the
// error, and all three are the case where deleting would be worst.
func firstOccupant(ctx context.Context, dyn dynamic.Interface, namespace string,
	gvrs []schema.GroupVersionResource) (string, error) {

	for _, gvr := range gvrs {
		gr := gvr.GroupResource()
		if slices.Contains(eventResources, gr) {
			continue
		}
		list, err := dyn.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				// The kind was in the discovery document but is not served
				// here -- a CRD deleted between discovery and this call,
				// which a teardown makes likelier than usual.
				continue
			}
			return "", fmt.Errorf("listing %s: %w", gr.String(), err)
		}
		for _, item := range list.Items {
			if builtInObjects[gr] == item.GetName() {
				continue
			}
			return item.GetKind() + " " + item.GetName(), nil
		}
	}
	return "", nil
}

// discoverer adapts client-go's discovery client onto Discoverer.
type discoverer struct{ d discovery.DiscoveryInterface }

// NewDiscoverer returns the production Discoverer.
func NewDiscoverer(d discovery.DiscoveryInterface) Discoverer { return &discoverer{d: d} }

// NamespacedResources returns every namespaced, listable resource kind the
// API server serves, one entry per (group, resource).
//
// A partial discovery result is an error here rather than a shorter list.
// discovery.ErrGroupDiscoveryFailed is exactly the case where an
// aggregated API server is down, and its kinds are the custom ones most
// likely to be holding something -- treating "I could not ask about this
// group" as "this group has nothing" is the fail-open direction.
func (a *discoverer) NamespacedResources() ([]schema.GroupVersionResource, error) {
	_, lists, err := a.d.ServerGroupsAndResources()
	if err != nil {
		return nil, fmt.Errorf("discovering namespaced resources: %w", err)
	}

	var out []schema.GroupVersionResource
	seen := map[schema.GroupResource]bool{}
	for _, list := range lists {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			return nil, fmt.Errorf("parsing group version %q: %w", list.GroupVersion, err)
		}
		for _, r := range list.APIResources {
			// Subresources (status, scale) are not objects that can occupy
			// a namespace, and most cannot be listed at all.
			if !r.Namespaced || !slices.Contains(r.Verbs, "list") {
				continue
			}
			gr := schema.GroupResource{Group: gv.Group, Resource: r.Name}
			// One version per resource is enough: a CRD served at both v1
			// and v1beta1 returns the same objects either way, and listing
			// both doubles the call count for an identical answer.
			if seen[gr] {
				continue
			}
			seen[gr] = true
			out = append(out, gv.WithResource(r.Name))
		}
	}
	return out, nil
}

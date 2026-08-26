package steps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/mchmarny/aicrme/internal/applier"
	"github.com/mchmarny/aicrme/internal/engine"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// HelmLister reports the helm releases installed in one namespace. The
// seam exists so this package's tests never shell out; production wires a
// helm subprocess through the same Exec the applier uses.
type HelmLister interface {
	List(ctx context.Context, namespace string) ([]string, error)
}

// noClientErr is the SnapshotErr recorded when this process has no cluster
// client at all -- outside a pod, rest.InClusterConfig fails and
// cmd/aicrme/main.go carries a nil kubernetes.Interface deliberately. It is
// a snapshot failure rather than an empty snapshot on purpose: an empty
// snapshot asserts that nothing pre-existed, which would make every release
// this run touched look like one it created.
const noClientErr = "no cluster client in this process, so nothing could be observed"

// snapshotOwnership records what the cluster held before Apply installs
// anything: the releases already present per namespace, and whether each
// namespace existed (with its UID).
//
// It never returns an error. A namespace this cannot read is recorded with
// SnapshotErr set, which makes every release in it unprovable and therefore
// off limits to Reset -- the fail-closed direction. Failing the whole
// install because one snapshot call hiccuped would trade a certain cost for
// a hypothetical one, and Apply is the long pole of the demo.
//
// Sorted and deduplicated output, so a record diffed across two runs reads
// as a diff of the cluster rather than of recipe.json's ordering.
func snapshotOwnership(ctx context.Context, h HelmLister, k kubernetes.Interface, namespaces []string) engine.Ownership {
	names := slices.Clone(namespaces)
	slices.Sort(names)
	names = slices.Compact(names)

	own := engine.Ownership{}
	for _, ns := range names {
		releases, nsRef := snapshotNamespace(ctx, h, k, ns)
		own.Namespaces = append(own.Namespaces, nsRef)
		// Deliberately not appended when the namespace failed to list: a
		// partial release list reads as "these are the ONLY releases that
		// pre-existed here", which is the false negative that gets a
		// bystander uninstalled. The SnapshotErr on nsRef is what stops
		// teardown trusting any of it.
		for _, r := range releases {
			own.Releases = append(own.Releases, engine.ReleaseRef{Name: r, Namespace: ns})
		}
	}
	return own
}

// snapshotNamespace observes one namespace. The two halves fail
// independently -- helm and the API server are different remote calls --
// but either failure poisons the whole namespace, because ownership of a
// release needs both answers: which releases were already here, and whether
// the namespace itself was.
func snapshotNamespace(ctx context.Context, h HelmLister, k kubernetes.Interface, ns string) ([]string, engine.NamespaceRef) {
	ref := engine.NamespaceRef{Name: ns}
	if h == nil || k == nil {
		ref.SnapshotErr = noClientErr
		return nil, ref
	}

	releases, err := h.List(ctx, ns)
	if err != nil {
		ref.SnapshotErr = "listing helm releases failed: " + err.Error()
		return nil, ref
	}
	slices.Sort(releases)

	_, err = k.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		// Not an error: it is the observation that tells a later Reset this
		// run created the namespace, and the common case for a fresh install.
		return releases, ref
	case err != nil:
		ref.SnapshotErr = "reading the namespace failed: " + err.Error()
		return nil, ref
	}
	// Existence only. The object's UID used to be recorded here, and a second
	// pass after Apply recorded the UID of each namespace the run went on to
	// create, so a deletion could refuse a namespace that had been destroyed
	// and recreated at the same name in between. Reset no longer deletes
	// namespaces, so neither UID decides anything and both are gone.
	ref.Existed = true
	return releases, ref
}

// unsnapshotted names the namespaces whose ownership could not be
// established, in the order they were recorded (sorted). Empty is the
// healthy case.
func unsnapshotted(own engine.Ownership) []string {
	var out []string
	for _, ns := range own.Namespaces {
		if ns.SnapshotErr != "" {
			out = append(out, ns.Name)
		}
	}
	return out
}

// helmLister lists releases by running helm through the same process seam
// the applier uses, so there is one place in this binary that spawns a
// subprocess and one place that has to be stubbed in tests.
type helmLister struct{ exec applier.Exec }

// NewHelmLister returns the production HelmLister.
func NewHelmLister(e applier.Exec) HelmLister { return &helmLister{exec: e} }

// List returns every release in the namespace, in every state.
//
// Every state is load-bearing, not just the deployed ones: a release left
// failed, pending or superseded by an earlier attempt is still a release
// this run did not create, and hiding it from the snapshot would make it
// fair game for Reset to uninstall.
//
// Spelled as the explicit status filters rather than `--all`, which is
// shorter and means the same thing -- under helm 3. Helm 4 REMOVED --all
// from `list` (it lists every status by default) and rejects it outright:
// `Error: unknown flag: --all`. There is no pin to fall back on -- helm is
// whatever the operator has on PATH, resolved and recorded by preflight --
// and helm 4 is already the common case. The failure mode if this used
// --all would be quiet and bad: every namespace records a SnapshotErr,
// every release becomes unprovable, and Reset removes nothing while
// reporting itself clean. The status flags below exist in both majors and
// mean all-of-them in both. Found by test/e2e/reset.sh, whose own helm on
// the host was already 4.x.
//
// No Dir is set: helm reads its cache, config and data paths from the
// HELM_* and XDG variables the deployment already sets (see workSubdirs in
// cmd/aicrme/main.go), which BashExec inherits via os.Environ.
func (h *helmLister) List(ctx context.Context, namespace string) ([]string, error) {
	var buf bytes.Buffer
	spec := applier.Spec{
		Argv: []string{
			"helm", "list", "--namespace", namespace,
			"--deployed", "--failed", "--pending", "--superseded", "--uninstalled", "--uninstalling",
			"--short",
		},
		Env: []string{"NO_COLOR=1"},
	}
	if err := h.exec.Run(ctx, spec, &buf); err != nil {
		// The namespace and helm's own stderr both go in: this error becomes
		// a NamespaceRef.SnapshotErr the operator reads at Reset time, long
		// after the pod log has rolled.
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeUnavailable,
			fmt.Sprintf("listing helm releases in %s failed: %s", namespace, strings.TrimSpace(buf.String())), err)
	}

	var out []string
	for line := range strings.SplitSeq(buf.String(), "\n") {
		// --short prints nothing at all for an empty namespace, and a naive
		// split turns that into one release named "".
		if name := strings.TrimSpace(line); name != "" {
			out = append(out, name)
		}
	}
	return out, nil
}

// recipeNamespaces is the namespace set Apply is about to install into,
// read from recipe.json rather than from the bundle directory. Reset needs
// this evidence to survive the pod, and recipe.json is persisted in the run
// record while the bundle lives in an emptyDir that dies with it.
//
// A run with no readable recipe.json yields nothing, which yields no
// evidence, which makes a later Reset a no-op rather than a guess.
func recipeNamespaces(run *engine.Run) []string {
	var summary RecipeSummary
	if err := json.Unmarshal(run.Artifacts["recipe.json"], &summary); err != nil {
		return nil
	}
	var out []string
	for _, c := range summary.Components {
		if c.Namespace != "" {
			out = append(out, c.Namespace)
		}
	}
	return out
}

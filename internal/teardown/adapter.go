package teardown

import (
	"context"
	"io"
	"time"

	"github.com/mchmarny/aicrme/internal/applier"
	"github.com/mchmarny/aicrme/internal/engine"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// defaultUninstallTimeout is the per-release budget handed to helm's
// --timeout, covering the --wait that follows the uninstall.
//
// Provisional at five minutes, and named here rather than inlined so the
// figure is one edit away from a real measurement. The GPU operator is the
// pole: its uninstall waits on validating webhooks being withdrawn and on
// the driver daemonset's pods terminating, neither of which is instant on a
// cluster under load.
const defaultUninstallTimeout = 5 * time.Minute

// Engine adapts this package onto engine.Teardown, so internal/engine never
// has to import it -- this package already imports engine, for the
// ComponentState and Ownership every decision here is founded on, and the
// dependency has to run one way.
type Engine struct {
	exec Exec
	kube kubernetes.Interface
	disc Discoverer
	dyn  dynamic.Interface
}

// NewEngineTeardown returns the production engine.Teardown.
func NewEngineTeardown(e Exec, kube kubernetes.Interface, d Discoverer, dyn dynamic.Interface) *Engine {
	return &Engine{exec: e, kube: kube, disc: d, dyn: dyn}
}

// Releases implements engine.Teardown.
func (a *Engine) Releases(ctx, cancel context.Context, comps []engine.ComponentState,
	own engine.Ownership, emit func(engine.ResidueItem)) []engine.ResidueItem {

	out := Releases(ctx, cancel, a.exec, comps, own, Options{Timeout: defaultUninstallTimeout},
		func(o ReleaseOutcome) { emit(releaseItem(o)) })

	items := make([]engine.ResidueItem, 0, len(out))
	for _, o := range out {
		items = append(items, releaseItem(o))
	}
	return items
}

// Namespaces implements engine.Teardown.
func (a *Engine) Namespaces(ctx context.Context, names []string, own engine.Ownership,
	emit func(engine.ResidueItem)) []engine.ResidueItem {

	out := Namespaces(ctx, a.kube, a.disc, a.dyn, names, own,
		func(o NamespaceOutcome) { emit(namespaceItem(o)) })

	items := make([]engine.ResidueItem, 0, len(out))
	for _, o := range out {
		items = append(items, namespaceItem(o))
	}
	return items
}

// releaseItem projects one outcome onto the engine's inventory shape.
// Removed is derived rather than carried: a release with neither a skip
// reason nor an error is one the uninstall command ran against and helm
// accepted, which is the only definition of removed this package has.
func releaseItem(o ReleaseOutcome) engine.ResidueItem {
	return engine.ResidueItem{
		Kind:      engine.KindRelease,
		Name:      o.Name,
		Namespace: o.Namespace,
		Removed:   o.Skip == "" && o.Err == "",
		Skip:      o.Skip,
		Err:       o.Err,
	}
}

func namespaceItem(o NamespaceOutcome) engine.ResidueItem {
	return engine.ResidueItem{
		Kind:    engine.KindNamespace,
		Name:    o.Name,
		Removed: o.Deleted,
		Skip:    o.Skip,
		Err:     o.Err,
	}
}

// BashExec adapts applier.BashExec onto this package's narrower Exec.
//
// Not a direct assignment: applier.Exec takes a Spec (a working directory,
// an environment, an argv), this one takes an argv alone, so applier.BashExec
// does not satisfy Exec and the conversion has to be explicit. That
// narrowness is deliberate -- see Exec's own comment -- and this is the one
// place the two meet.
type BashExec struct{}

// Run runs argv through the applier's real process seam, so a helm
// uninstall gets the same process-group signal handling, the same SIGTERM
// grace period, and the same inherited HELM_* environment that deploy.sh's
// own helm invocations do.
func (BashExec) Run(ctx context.Context, argv []string, out io.Writer) error {
	return applier.BashExec{}.Run(ctx, applier.Spec{Argv: argv, Env: []string{"NO_COLOR=1"}}, out)
}

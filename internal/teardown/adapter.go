package teardown

import (
	"context"
	"io"
	"time"

	"github.com/mchmarny/aicrme/internal/applier"
	"github.com/mchmarny/aicrme/internal/engine"
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
//
// One dependency, the process seam. It held a clientset, a discovery client
// and a dynamic client until namespace deletion was removed: deciding whether
// a namespace was empty enough to delete needed all three, and reporting a
// namespace needs none of them.
type Engine struct {
	exec Exec
}

// NewEngineTeardown returns the production engine.Teardown.
func NewEngineTeardown(e Exec) *Engine {
	return &Engine{exec: e}
}

// Releases implements engine.Teardown.
func (a *Engine) Releases(ctx, cancel context.Context, comps []engine.ComponentState,
	own engine.Ownership, emit func(engine.ResidueItem)) []engine.ResidueItem {

	out := Releases(ctx, cancel, a.exec, comps, own, Options{Timeout: defaultUninstallTimeout},
		func(o ReleaseOutcome) {
			for _, it := range outcomeItems(o) {
				emit(it)
			}
		})

	items := make([]engine.ResidueItem, 0, len(out))
	for _, o := range out {
		items = append(items, outcomeItems(o)...)
	}
	return items
}

// outcomeItems projects one component's teardown onto the inventory: the
// release row, then a row per object purged after it.
//
// The object rows follow their release rather than being collected into a
// section of their own, because that is the order the operator watches them
// resolve in and the order that makes them legible -- "kai-scheduler removed,
// and these four things it left behind removed with it".
func outcomeItems(o ReleaseOutcome) []engine.ResidueItem {
	items := make([]engine.ResidueItem, 0, 1+len(o.Objects))
	items = append(items, releaseItem(o))
	for _, ob := range o.Objects {
		items = append(items, objectItem(ob))
	}
	return items
}

// objectItem projects one purged object onto the inventory shape.
//
// Name carries the resource as well as the object's own name. The shape has
// no field for a resource type, and "object default" would name three
// different things in kai's table alone; "queue.scheduling.run.ai/default-queue"
// names exactly one, and is also directly pasteable into a kubectl command
// when a purge failed and the operator has to finish it by hand.
func objectItem(o ObjectOutcome) engine.ResidueItem {
	return engine.ResidueItem{
		Kind:      engine.KindObject,
		Name:      o.Resource + "/" + o.Name,
		Namespace: o.Namespace,
		Removed:   o.Err == "",
		Err:       o.Err,
	}
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

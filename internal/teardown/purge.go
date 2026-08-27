package teardown

import (
	"context"
	"io"
	"time"
)

// purgeTimeout bounds one `kubectl delete` of one object.
//
// Short on purpose, and much shorter than the per-release uninstall budget:
// these are single objects with no dependents and no --wait semantics worth
// speaking of, so a delete that has not returned in a minute is wedged on a
// finalizer rather than working slowly. Failing then is better than holding a
// teardown open -- the object is reported as residue and the operator gets a
// name to act on.
const purgeTimeout = 60 * time.Second

// purgeTarget is one object a component's chart creates and then instructs
// helm not to remove.
type purgeTarget struct {
	// Resource is fully qualified (plural.group) so kubectl resolves it
	// without guessing. A bare kind would be ambiguous across CRDs, and an
	// ambiguous delete is the one kind of mistake this package must not make.
	Resource string
	// Name is a specific object, never a label selector and never --all.
	// That is the whole ownership argument: the chart creates THESE names,
	// so these are the ones this console installed. A Queue an operator
	// wrote by hand has a different name and is untouched.
	Name string
	// Namespace is empty for a cluster-scoped object.
	Namespace string
}

// componentPurges maps a component name to the objects its chart leaves
// behind after `helm uninstall`.
//
// This is the only per-component knowledge in a package that is otherwise
// entirely generic, and it is deliberately a short, named, auditable list
// rather than a rule. The generic alternative -- delete every custom resource
// whose CRD this run installed -- is unbounded: it would take an operator's
// own Queues, and every workload's PodGroups, along with the four objects
// that actually matter.
//
// kai-scheduler is the only entry, and every object in it survives a helm
// uninstall BY DESIGN rather than by accident:
//
//	SchedulingShard/default       helm.sh/resource-policy: keep
//	Queue/default-parent-queue    helm.sh/resource-policy: keep
//	Queue/default-queue           helm.sh/resource-policy: keep
//	Config/kai-config             a pre-install hook, and hook resources are
//	                              not part of the release manifest at all
//
// The shard is the one that causes the failure this table exists for. It owns
// the kai-scheduler-default Deployment, which helm therefore does not own
// either; a reinstall finds the shard already present and matching, never
// recreates the Deployment, and the cluster goes on running the PREVIOUS
// install's scheduler pod against a control plane that has been replaced
// underneath it. That pod does not schedule new gangs. Measured on KWOK: a
// gang placed in 8s on a fresh cluster, never on a reset one, and in 2s once
// these four objects were removed between the uninstall and the reinstall.
//
// Ordering is not load-bearing here -- these four have no interdependency
// that a delete has to respect -- but the shard is first because it is the
// one whose removal the next install actually depends on.
var componentPurges = map[string][]purgeTarget{
	"kai-scheduler": {
		{Resource: "schedulingshard.kai.scheduler", Name: "default"},
		{Resource: "queue.scheduling.run.ai", Name: "default-queue"},
		{Resource: "queue.scheduling.run.ai", Name: "default-parent-queue"},
		{Resource: "config.kai.scheduler", Name: "kai-config", Namespace: "kai-scheduler"},
	},
}

// ObjectOutcome is what happened to one purged object. Err empty means it is
// gone -- or was never there, which --ignore-not-found makes the same answer
// and is what lets a second Reset run clean instead of erroring.
type ObjectOutcome struct {
	Resource  string
	Name      string
	Namespace string
	Err       string
}

// purgeArgv is the delete for one target.
//
// --ignore-not-found for idempotence: a re-Reset after a partial one is the
// normal recovery path, and on the second pass every one of these is already
// gone. No --wait: these objects have no dependents to cascade to, and the
// delete returning is the whole signal.
//
// No --context and no --kubeconfig. The console freezes the operator's chosen
// context into a session kubeconfig and sets KUBECONFIG for the whole process
// (internal/console/console.go), which every subprocess inherits -- the same
// mechanism that pins the helm uninstall this delete follows. Passing a
// context here would be a second source of truth for the one thing that must
// not disagree.
func purgeArgv(t purgeTarget) []string {
	argv := []string{"kubectl", "delete", t.Resource, t.Name}
	if t.Namespace != "" {
		argv = append(argv, "-n", t.Namespace)
	}
	return append(argv, "--ignore-not-found", "--timeout="+purgeTimeout.String())
}

// purge removes the objects component holds in the table, and reports one
// outcome per object it attempted.
//
// Called only after a CONFIRMED uninstall -- see its single call site in
// Releases. A release that was skipped belongs to somebody else and so do its
// objects; a release whose uninstall failed may still have controllers
// running, and deleting the objects those controllers own invites them
// straight back, possibly with a finalizer nothing left alive will clear.
//
// A failure does not stop the rest, matching Releases' own policy for the
// same reason: three objects removed is strictly better than one, and every
// one left behind is reported by name so the operator can finish the job.
//
// cancel is checked between commands and never during one, which is the
// property Releases guarantees for uninstalls and which matters at least as
// much here: these run against a cluster the operator is about to be handed
// back.
func purge(ctx, cancel context.Context, e Exec, component string) []ObjectOutcome {
	targets := componentPurges[component]
	if len(targets) == 0 {
		return nil
	}

	out := make([]ObjectOutcome, 0, len(targets))
	for _, t := range targets {
		o := ObjectOutcome{Resource: t.Resource, Name: t.Name, Namespace: t.Namespace}
		if err := e.Run(ctx, purgeArgv(t), io.Discard); err != nil {
			o.Err = err.Error()
		}
		out = append(out, o)
		if cancel.Err() != nil {
			return out
		}
	}
	return out
}

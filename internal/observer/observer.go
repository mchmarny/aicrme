// Package observer converts Kubernetes cluster state changes into typed
// console events, so a long Apply is visible between deploy.sh's
// component-boundary markers rather than silent for minutes at a time.
//
// It aggregates; it never relays. An informer's UpdateFunc fires on any
// field change -- managedFields, annotations, status heartbeats -- and the
// bus drops live events for any subscriber more than 256 behind
// (internal/bus.subscriberBuffer). So each handler computes a small
// normalized state and publishes only when that state changes.
package observer

import (
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/mchmarny/aicrme/internal/bus"
)

// resyncPeriod is 0 deliberately: periodic resync would re-deliver every
// object as an update, and while change-detection would drop those, doing
// the work at all is pointless when informers already watch.
const resyncPeriod = 0

// RunScope is the engine state the observer needs, taken as one atomic
// snapshot. Reading the run ID and the namespaces separately would let
// attribution and filtering come from different runs across a race.
//
// Component and Generation mirror engine.Attribution's own fields (Task 2,
// internal/engine/attribution.go) rather than being fetched through a second
// accessor. main is what combines the two sources into this one struct
// (see cmd/aicrme/main.go's newObserverScopeFn): Namespaces from 2b-ii's
// cached recipe parsing, RunID/Component/Generation from Engine.Attribution().
// They cannot be combined on the engine side -- Namespaces come from parsing
// recipe.json into steps.RecipeSummary, and internal/steps imports
// internal/engine, so an engine accessor that also returned Namespaces would
// be an import cycle. Folding both into one struct here, composed by one func in main, is what
// lets publish still call its scope accessor exactly once per event despite
// the two underlying sources.
type RunScope struct {
	RunID string
	// Namespaces are the resolved recipe's namespaces. Empty means no run
	// has resolved one yet, and namespaced workloads are filtered out
	// entirely -- Nodes are cluster-scoped and always pass.
	Namespaces map[string]struct{}
	// Component is the deployment action currently installing, straight from
	// engine.Attribution.ActiveAction: a TEMPORAL cursor, not a claim of
	// ownership. A cluster event stamped with it means "observed while
	// Component installs", never "belongs to Component" -- deploy.sh.tmpl's
	// own note (~line 488) warns that cluster convergence continues
	// asynchronously after the script exits. Empty outside Apply, between
	// actions, or once the run reaches a terminal state; that is a
	// first-class outcome (spec Section 1), not an error.
	Component string
	// Generation mirrors engine.Attribution.Generation, so a consumer that
	// needs to detect a stale read can, without a second call into the
	// engine.
	Generation uint64
	// Terminal mirrors engine.Attribution.Terminal (Ruling 8): RunID's run
	// has reached a state Engine.finish actually sets (StateDone or
	// StateFailed). It is read fresh off engine.Attribution() on every scope
	// call, so it can never disagree with RunID -- unlike RunID and
	// Namespaces going empty (which happens only on Discard or a new Start,
	// see scoped.go's package doc), Terminal is how a consumer of RunScope
	// learns a specific run is over without waiting for the run to be
	// replaced or discarded. scopedInformers (scoped.go) is that consumer:
	// it treats Terminal as authoritative and re-derives it on every
	// reconcile, rather than remembering a past terminal read, which is what
	// lets a retried run (same RunID, Terminal flips back to false) resume
	// being watched instead of staying wedged.
	Terminal bool
}

type stateKey struct {
	kind      string
	namespace string
	name      string
	uid       types.UID
}

// Observer watches a small set of resources and narrates changes.
type Observer struct {
	client kubernetes.Interface
	bus    *bus.Bus
	scope  func() RunScope
	// scoped owns the Pod/Event informers, which -- unlike DaemonSets,
	// Deployments and Nodes below -- cannot be built once at Start: they are
	// namespace-scoped to a recipe that has not resolved yet when Start runs
	// (scoped.go).
	scoped *scopedInformers

	mu sync.Mutex
	// workload holds DaemonSet/Deployment readiness summaries.
	workload map[stateKey]string
	// gpuQty holds Node nvidia.com/gpu allocatable. Kept separate and
	// compared with Quantity.Cmp rather than string equality, since 8,
	// 8000m and "8" are the same quantity with different serializations.
	gpuQty map[stateKey]resource.Quantity
	// pods holds every narrated trouble Reason (pods.go's podCondition) per
	// tracked Pod, keyed by Reason within each Pod's own entry -- a SET, not
	// a single value (Ruling 20, Task 5 fix round 3). Ruling 14(b)/17 only
	// ever remembered the CURRENT single condition, so a Pod that narrated
	// Unschedulable and then ImagePullBackOff (an ordinary Apply sequence:
	// pends on capacity, gets scheduled, then its image pull stalls) lost
	// track of Unschedulable the moment ImagePullBackOff overwrote it --
	// nothing ever resolved it, stranding an Error-severity condition on a
	// pod that went on to become fully healthy. onPodChange's full-recovery
	// path now resolves every entry in a Pod's set, not just the last one;
	// onDelete does the same, filtered by narrated. Separate from workload
	// because its value type differs (a set of Reason/Container pairs, not
	// a display string) and because a healthy pod is never given an entry
	// at all -- see podCondition's own doc comment. A nil inner map (no
	// entry for a Pod at all) and an absent key both mean "nothing tracked"
	// -- callers range over it directly rather than checking for nil first.
	pods map[stateKey]map[string]podCondition
	// events holds every (resource, Reason) pair this observer has already
	// recorded from the Event informer, keyed the same way pods above is --
	// eventInvolvedKey (events.go) reads ev.InvolvedObject, not the Event API
	// object's own identity, so a FailedScheduling Warning and a Pod trouble
	// condition on the SAME Pod are keyed identically even though they come
	// from two different informers. The inner map's VALUE is eventDedupe
	// (events.go), a smaller cousin of podCondition: unlike a Pod's tracked
	// trouble, an Event dedupe entry carries no severity/detail of its own to
	// recompute later, but it DOES need the two things resolveEventsLocked
	// requires -- narrated (Task 6 fix round 2, Important 1(new)): true if
	// onEventChange actually published this reason as arising, false if
	// seedEventBaseline only recorded it silently from an informer's initial
	// list, read by resolveEventsLocked's narratedOnly parameter to
	// reproduce onPodChange/onDelete's own narrated-filtering asymmetry
	// (podCondition's doc comment) on the Event side instead of diverging
	// from it -- a delete must not manufacture a "removed" event for a
	// Warning no consumer was ever shown; and container, echoed back on
	// resolution so a row doesn't lose which container failed when the
	// condition clears. Cleared by handlers.go's onDelete (Event case,
	// per-entry, on the Event API object's own TTL eviction) or
	// clearNamespaceEvents (per-namespace, on informer teardown).
	events map[stateKey]map[string]eventDedupe
}

// New returns an Observer. A nil client yields a no-op: the console's whole
// Discover-to-Apply arc works without cluster telemetry, so failing to build
// a client must degrade rather than prevent startup.
//
// o is built as a variable, not returned directly from one struct literal,
// because scopedHandlers.pod below needs bound method values (o.onPodAdd
// etc.) that close over o itself -- those can only be taken once o exists as
// an addressable value. o.pods/o.events are initialized before that point, in
// the same literal as workload/gpuQty, so the handlers never see a nil map
// once the informers they are registered on start delivering.
func New(client kubernetes.Interface, b *bus.Bus, scope func() RunScope) *Observer {
	o := &Observer{
		client:   client,
		bus:      b,
		scope:    scope,
		workload: make(map[stateKey]string),
		gpuQty:   make(map[stateKey]resource.Quantity),
		pods:     make(map[stateKey]map[string]podCondition),
		events:   make(map[stateKey]map[string]eventDedupe),
	}
	o.scoped = newScopedInformers(client, scopedHandlers{
		pod: cache.ResourceEventHandlerDetailedFuncs{
			AddFunc:    o.onPodAdd,
			UpdateFunc: o.onPodUpdate,
			DeleteFunc: o.onDelete,
		},
		event: cache.ResourceEventHandlerDetailedFuncs{
			AddFunc:    o.onEventAdd,
			UpdateFunc: o.onEventUpdate,
			DeleteFunc: o.onDelete,
		},
	}, o.clearNamespaceState)
	return o
}

// clearNamespacePods drops every tracked Pod trouble condition in ns. Wired
// as scopedInformers' onNamespaceStop callback, invoked whenever ns's
// factories are torn down -- run termination, a scope change that drops ns,
// or process shutdown (Important 4, Task 5 fix round 1). Without this,
// o.pods entries for a torn-down namespace are stranded permanently: Task
// 4's teardown is immediate on RunScope.Terminal, so a pod deleted after
// that point is never delivered to onDelete, and nothing else would ever
// evict the entry.
//
// Deliberately does not publish anything: tearing down does not mean the
// pods it was watching are fixed, removed, or in any particular state --
// this process simply stopped being able to say anything more about them.
// Publishing a resolution here would be inventing a claim this observer
// cannot back, the same failure mode Ruling 14/Important 3 just fixed
// elsewhere in this file.
func (o *Observer) clearNamespacePods(ns string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for key := range o.pods {
		if key.namespace == ns {
			delete(o.pods, key)
		}
	}
}

// clearNamespaceEvents drops every tracked Event dedupe entry in ns -- the
// Event-informer counterpart to clearNamespacePods immediately above, wired
// into the same onNamespaceStop callback (clearNamespaceState) so a
// namespace's Pod and Event state clear together whenever its factories tear
// down.
//
// This is also what bounds o.events across a RUN boundary (spec Section 3's
// "bounded by run generation" requirement, Task 6): scoped.reconcile tears
// down every namespace under the OLD run (stopAllLocked) before starting the
// new run's namespaces, on EITHER of the two branches that reach it (M4,
// Task 6 fix round 1) -- whenever RunScope.RunID changes to a DIFFERENT run
// (scoped.go:183), or whenever RunScope.Terminal is true, meaning no run is
// current at all (scoped.go:177). The Terminal branch is not a redundant
// restatement of the RunID one: engine.Retry (internal/engine/engine.go)
// reuses the SAME RunID after a failure, so a retry's teardown-then-restart
// is driven by Terminal flipping true and then back to false for that
// identical RunID, never by RunID changing -- the earlier version of this
// comment named only the RunID branch, which is incomplete for exactly the
// Retry case a reader of this package's doc comment (scoped.go, which
// discusses Retry directly above this file) will already be thinking about.
// Both branches reach this sweep before startNamespace builds any new
// factory for the namespace, within the SAME reconcile call -- so this sweep
// always runs, for every namespace, strictly between an old run's dedupe
// state and a new (or retried) run's first Event notification for that
// namespace. engine.Attribution.Generation itself is NOT what this reads: it advances on
// every ActiveAction transition WITHIN a run (internal/engine/attribution.go,
// setActiveAction/clearActiveAction), roughly twice per deployment action --
// keying eviction off it directly would clear o.events ~28 times over one
// ordinary 14-action run, discarding dedupe state the run's own later actions
// still needed and defeating the coalescing this map exists to respect.
// RunID is the field that actually marks a run boundary (Ruling 6/8,
// scoped.go's package doc), and scoped.go's own reconcile already tears down
// and rebuilds on exactly that signal -- riding it here, rather than reading
// Generation a second way, is what makes "a new run does not inherit the
// previous run's dedupe state" hold without also making dedupe reset mid-run.
//
// Deliberately does not publish anything, matching clearNamespacePods: a
// torn-down informer means this process can no longer speak to whether an
// already-narrated Warning is still happening, not that it resolved.
func (o *Observer) clearNamespaceEvents(ns string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for key := range o.events {
		if key.namespace == ns {
			delete(o.events, key)
		}
	}
}

// clearNamespaceState composes clearNamespacePods and clearNamespaceEvents
// into the single onNamespaceStop callback newScopedInformers accepts
// (scoped.go) -- one namespace tearing down clears both kinds of
// namespace-scoped state it was feeding, not just one.
func (o *Observer) clearNamespaceState(ns string) {
	o.clearNamespacePods(ns)
	o.clearNamespaceEvents(ns)
}

// Start registers handlers and starts the informers. It returns once caches
// have synced (or the stop channel closes); handlers then run until stopCh.
func (o *Observer) Start(stopCh <-chan struct{}) error {
	if o.client == nil {
		slog.Warn("observer disabled: no Kubernetes client available")
		return nil
	}

	// Independent of the cluster-wide factory below: scoped.run drives its
	// own goroutine for the process's lifetime, reconciling Pod/Event
	// factories against o.scope() as runs come and go, and tearing them
	// down the moment a run reaches a terminal state (scoped.go, Ruling 3).
	// It never blocks this call -- see scopedInformers.reconcile's own doc
	// comment for why.
	go o.scoped.run(o.scope, o.bus, stopCh, reconcileInterval)

	factory := informers.NewSharedInformerFactory(o.client, resyncPeriod)

	dsInf := factory.Apps().V1().DaemonSets().Informer()
	if err := o.register(dsInf, o.onDaemonSet); err != nil {
		return err
	}

	deployInf := factory.Apps().V1().Deployments().Informer()
	if err := o.register(deployInf, o.onDeployment); err != nil {
		return err
	}

	nodeInf := factory.Core().V1().Nodes().Informer()
	if err := o.register(nodeInf, o.onNode); err != nil {
		return err
	}

	factory.Start(stopCh)
	for typ, ok := range factory.WaitForCacheSync(stopCh) {
		if !ok {
			slog.Warn("observer cache did not sync", "type", typ.String())
		}
	}
	return nil
}

// register wires one informer's handlers and its watch-error handler. A
// silently-dead informer is worse than no observer: the timeline simply
// stops with no indication that it has.
func (o *Observer) register(inf cache.SharedIndexInformer, onUpdate func(any)) error {
	if err := inf.SetWatchErrorHandler(func(_ *cache.Reflector, err error) {
		slog.Warn("observer watch failed", "error", err)
	}); err != nil {
		return err
	}
	_, err := inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    o.onAdd,
		UpdateFunc: func(_, newObj any) { onUpdate(newObj) },
		DeleteFunc: o.onDelete,
	})
	return err
}

// publish attaches cd to the event as its typed Data payload, stamping both
// Event.At and ClusterData.At from the same now() call so the two never
// disagree about when the transition happened.
//
// o.scope() is called exactly ONCE here and used for both the namespace
// filter and the RunID/Component stamp. Reading it a second time -- once for
// the filter, once for the stamp -- is the natural-looking way to write this
// and the wrong one: a second read can land on the far side of a run or
// active-action transition, filtering an event against one snapshot's
// namespaces while stamping it with a different snapshot's action. See
// TestPublishDoesNotStampAcrossARunTransition.
func (o *Observer) publish(ns, msg string, cd bus.ClusterData) {
	sc := o.scope()
	if ns != "" {
		if _, ok := sc.Namespaces[ns]; !ok {
			return
		}
	}
	cd.At = time.Now().UTC()
	// ClusterData holds only strings, ints, bools and a time.Time, so Marshal
	// cannot fail.
	data, _ := json.Marshal(cd)
	o.bus.Publish(bus.Event{
		RunID:     sc.RunID,
		Kind:      bus.KindCluster,
		Level:     bus.LevelInfo,
		At:        cd.At,
		Component: sc.Component,
		Message:   msg,
		Data:      data,
	})
}

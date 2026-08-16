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
type RunScope struct {
	RunID string
	// Namespaces are the resolved recipe's namespaces. Empty means no run
	// has resolved one yet, and namespaced workloads are filtered out
	// entirely -- Nodes are cluster-scoped and always pass.
	Namespaces map[string]struct{}
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

	mu sync.Mutex
	// workload holds DaemonSet/Deployment readiness summaries.
	workload map[stateKey]string
	// gpuQty holds Node nvidia.com/gpu allocatable. Kept separate and
	// compared with Quantity.Cmp rather than string equality, since 8,
	// 8000m and "8" are the same quantity with different serializations.
	gpuQty map[stateKey]resource.Quantity
}

// New returns an Observer. A nil client yields a no-op: the console's whole
// Discover-to-Apply arc works without cluster telemetry, so failing to build
// a client must degrade rather than prevent startup.
func New(client kubernetes.Interface, b *bus.Bus, scope func() RunScope) *Observer {
	return &Observer{
		client:   client,
		bus:      b,
		scope:    scope,
		workload: make(map[stateKey]string),
		gpuQty:   make(map[stateKey]resource.Quantity),
	}
}

// Start registers handlers and starts the informers. It returns once caches
// have synced (or the stop channel closes); handlers then run until stopCh.
func (o *Observer) Start(stopCh <-chan struct{}) error {
	if o.client == nil {
		slog.Warn("observer disabled: no Kubernetes client available")
		return nil
	}

	factory := informers.NewSharedInformerFactory(o.client, resyncPeriod)

	dsInf := factory.Apps().V1().DaemonSets().Informer()
	if err := o.register(dsInf, o.onDaemonSet); err != nil {
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

func (o *Observer) publish(ns, msg string) {
	sc := o.scope()
	if ns != "" {
		if _, ok := sc.Namespaces[ns]; !ok {
			return
		}
	}
	o.bus.Publish(bus.Event{
		RunID:   sc.RunID,
		Kind:    bus.KindCluster,
		Level:   bus.LevelInfo,
		At:      time.Now().UTC(),
		Message: msg,
	})
}

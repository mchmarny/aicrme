package observer

import (
	"log/slog"
	"sync"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/mchmarny/aicrme/internal/bus"
)

// The observer starts at pod start, before any run exists -- the recipe's
// namespaces come from recipe.json, which Recommend writes partway through a
// run (docs/superpowers/specs/2026-08-17-aicrme-phase-2b-iii-design.md
// Section 3). So Pods and Events, unlike the three cluster-scoped informers
// in observer.go, cannot be watched from a factory built once at Start: this
// file gives them a lifecycle that starts once a scope resolves and tears
// down when the run that resolved it ends.
//
// informers.WithNamespace takes exactly one namespace (verified against the
// pinned client-go v0.36.3, informers/factory.go:99 -- "factory.namespace =
// namespace" is a single field, last write wins, not a set), so following a
// multi-namespace recipe means one SharedInformerFactory per namespace,
// roughly ten for the current recipe, not one namespace-filtered factory.

// factoryEntry is one namespace's Pod/Event watch: its own
// SharedInformerFactory plus the two informers materialized on it, and the
// stop channel that owns their lifetime independently of the process-wide
// stopCh the three cluster-scoped informers in observer.go share. Handlers
// are not registered here -- that is Tasks 5 and 6's job; this type only
// makes the informers exist and keeps their lifetime correct.
type factoryEntry struct {
	factory informers.SharedInformerFactory
	pod     cache.SharedIndexInformer
	event   cache.SharedIndexInformer
	stop    chan struct{}
}

// scopedInformers owns the Pod/Event factories for whichever run is
// currently in scope. mu guards every field below it: reconcile can run
// concurrently with itself in production, since run() re-reconciles on every
// KindPhase event and nothing serializes those against each other beyond
// this lock.
type scopedInformers struct {
	client kubernetes.Interface

	mu sync.Mutex
	// runID is the run these entries belong to, or "" when nothing is
	// scoped.
	runID string
	// terminated is the last runID stopIfRunID tore down. main's RunScope
	// composition (cmd/aicrme/main.go's newObserverScopeFn) has no way to
	// say "this run is over" -- engine.Attribution and CurrentID keep
	// reporting a terminated run's own RunID and Namespaces unchanged until
	// a new run starts or the old one is discarded (internal/engine's
	// Engine.Start replaces e.current; Discard sets it nil; nothing else
	// does). Without this guard, any later reconcile call that still
	// observes the just-terminated run's scope would restart the very
	// factories stopIfRunID just closed. Cleared the moment a genuinely
	// different RunID is reconciled, since the guard exists only to keep a
	// terminated run's OWN scope from reviving.
	terminated string
	entries    map[string]*factoryEntry
}

func newScopedInformers(client kubernetes.Interface) *scopedInformers {
	return &scopedInformers{client: client, entries: make(map[string]*factoryEntry)}
}

// reconcile starts and stops per-namespace factories so the live set matches
// sc. It never blocks: the only call that reaches the network (a namespace's
// initial List) happens inside startNamespace's own goroutine, never on this
// call's goroutine -- the same posture 2b-ii's async observer.Start uses,
// applied here because reconcile runs repeatedly over a run's lifetime
// rather than once at process start, so no caller can be trusted to
// remember to wrap it in a goroutine itself.
//
// A RunID change -- including the zero RunScope an unresolved or
// disagreeing read produces (Ruling 6, observer.go's RunScope doc comment)
// -- tears down every existing factory before applying the new set:
// namespaces are only meaningful within the run that resolved them, so
// carrying a prior run's factory forward under a different RunID would watch
// namespaces that run's recipe never named.
func (s *scopedInformers) reconcile(sc RunScope) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if sc.RunID == "" || len(sc.Namespaces) == 0 || sc.RunID == s.terminated {
		s.stopAllLocked()
		s.runID = ""
		return
	}

	if s.runID != sc.RunID {
		s.stopAllLocked()
		s.runID = sc.RunID
		s.terminated = ""
	}

	for ns, e := range s.entries {
		if _, ok := sc.Namespaces[ns]; !ok {
			close(e.stop)
			delete(s.entries, ns)
		}
	}
	for ns := range sc.Namespaces {
		if _, ok := s.entries[ns]; ok {
			continue // already watching this namespace -- idempotent, no double-start
		}
		s.entries[ns] = s.startNamespace(ns)
	}
}

// startNamespace builds one namespace's factory and materializes its Pod and
// Event informers, then starts them on their own goroutine so a wedged API
// server's initial List cannot stall reconcile's caller -- the bite-proof
// this file exists to satisfy (TestScopedInformerStartDoesNotBlock). Callers
// must hold s.mu; the goroutine launched here touches only the factory and
// stop values it closes over, never s itself, so it needs no lock of its
// own.
func (s *scopedInformers) startNamespace(ns string) *factoryEntry {
	stop := make(chan struct{})
	factory := informers.NewSharedInformerFactoryWithOptions(s.client, resyncPeriod, informers.WithNamespace(ns))
	pod := factory.Core().V1().Pods().Informer()
	event := factory.Core().V1().Events().Informer()

	go func() {
		factory.Start(stop)
		for typ, ok := range factory.WaitForCacheSync(stop) {
			if !ok {
				slog.Warn("scoped observer cache did not sync", "namespace", ns, "type", typ.String())
			}
		}
	}()

	return &factoryEntry{factory: factory, pod: pod, event: event, stop: stop}
}

// stopAllLocked closes and releases every tracked namespace's factory.
// Callers must hold s.mu and are responsible for what the teardown means for
// s.runID/s.terminated afterward -- this only empties s.entries.
func (s *scopedInformers) stopAllLocked() {
	for ns, e := range s.entries {
		close(e.stop)
		delete(s.entries, ns)
	}
}

// stop tears down every namespace's factory unconditionally. Used on process
// shutdown (run()'s stopCh closing), where there is no specific run to
// compare against.
func (s *scopedInformers) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopAllLocked()
	s.runID = ""
}

// stopIfRunID tears down every factory only if runID is still the run this
// instance has scoped -- Ruling 3's "stop the moment a run reaches a
// terminal state" (docs/superpowers/sdd/2026-08-17-aicrme-phase-2b-iii/progress.md),
// with no grace window. The comparison mirrors internal/engine's
// epoch-guard pattern: a terminal signal for a run this instance has already
// moved on from (reconcile already tore down and started a different run's
// factories) must be a no-op, not a destructive one -- see
// TestStopIfRunIDIgnoresAStaleRunID.
func (s *scopedInformers) stopIfRunID(runID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runID != runID {
		return
	}
	s.stopAllLocked()
	s.runID = ""
	s.terminated = runID
}

// phaseRunDone, phaseRunFailed and phaseRunActive are the KindPhase messages
// internal/engine's Engine.finish publishes ("run " + string(state)) for
// every state finish is ever called with (engine.go). Duplicated as string
// literals rather than importing internal/engine.State, the same shape
// handlers.go's componentStatusStarted already uses to avoid coupling this
// package to the engine's types -- and not a new contract, since
// web/src/components/Wizard.tsx's deriveRunState already keys UI state off
// these exact same literals. phaseRunActive has no caller in engine.go today
// (run.go's own doc comment reserves StateActive for the Prove workload the
// engine does not yet drive to finish with), but is matched here anyway: the
// cost of matching an unreachable state is zero, and the cost of silently
// missing it the day that path is wired up is a Pod/Event watch that never
// tears down.
const (
	phaseRunDone   = "run done"
	phaseRunFailed = "run failed"
	phaseRunActive = "run active"
)

func isTerminalRunMessage(msg string) bool {
	switch msg {
	case phaseRunDone, phaseRunFailed, phaseRunActive:
		return true
	default:
		return false
	}
}

// run drives the scoped lifecycle for the observer's whole process lifetime.
// It subscribes to the SAME bus the observer publishes cluster telemetry to
// -- Engine.finish publishes a run's terminal KindPhase event on that same
// bus, synchronously under Publish's own lock (internal/bus/bus.go), so this
// goroutine observes it as soon as any other subscriber does. That is what
// makes teardown immediate rather than bounded by a poll interval: main's
// RunScope alone cannot signal "this run just ended" (see s.terminated's doc
// comment), so this listens for the one signal that actually fires at that
// instant instead of inferring it from RunScope's other fields.
//
// Every non-terminal KindPhase event triggers a reconcile against the
// current scope -- cheap and idempotent if nothing changed -- because
// namespaces resolve partway through a run (Recommend writing recipe.json)
// with no dedicated event marking that moment; the next phase-complete
// marker is what wakes this goroutine to notice.
func (s *scopedInformers) run(scope func() RunScope, b *bus.Bus, stopCh <-chan struct{}) {
	sub, unsub := b.Subscribe(0)
	defer unsub()
	for {
		select {
		case <-stopCh:
			s.stop()
			return
		case e, ok := <-sub:
			if !ok {
				s.stop()
				return
			}
			if e.Kind != bus.KindPhase {
				continue
			}
			if isTerminalRunMessage(e.Message) {
				s.stopIfRunID(e.RunID)
				continue
			}
			s.reconcile(scope())
		}
	}
}

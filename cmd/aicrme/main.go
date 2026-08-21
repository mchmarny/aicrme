// Command aicrme serves the AI Cluster Runtime demo console.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/api"
	"github.com/mchmarny/aicrme/internal/applier"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
	"github.com/mchmarny/aicrme/internal/observer"
	"github.com/mchmarny/aicrme/internal/prove"
	"github.com/mchmarny/aicrme/internal/steps"
	"github.com/mchmarny/aicrme/internal/version"
	"github.com/mchmarny/aicrme/internal/web"
)

// replayCapacity bounds the event ring. A full real-hardware run emits a few
// thousand events; this keeps the whole timeline replayable to a late tab.
const replayCapacity = 20000

// defaultSnapshotAgentImage is the snapshot agent image used when
// AICRME_SNAPSHOT_IMAGE is unset. Must track go.mod's github.com/NVIDIA/aicr
// version (also pinned in .settings.yaml under dependencies.aicr) -- a stale
// tag here is the first thing that breaks on a fresh customer cluster, since
// Discover cannot fall back to anything else.
const defaultSnapshotAgentImage = "ghcr.io/nvidia/aicr:v0.19.0"

// defaultWorkDir is the writable scratch root. The chart mounts an emptyDir
// here (charts/aicrme/templates/deployment.yaml) and points TMPDIR, HOME,
// and the helm/kubectl cache variables at subdirectories of it, which is
// what lets the pod run with readOnlyRootFilesystem: true.
const defaultWorkDir = "/var/lib/aicrme"

// defaultApplyRetries is deploy.sh's per-component retry budget, matching
// the script's own default. Its backoff is quadratic and each attempt
// surfaces as a warn event, so the wait is visible rather than silent.
const defaultApplyRetries = 5

// runShutdownTimeout bounds how long shutdown waits for an in-flight run to
// stop. The worst case is killGrace (internal/applier/exec.go, 10s: the
// process-group SIGTERM -> SIGKILL escalation) plus terminalSaveTimeout
// (internal/engine/engine.go, 5s: the detached terminal-state write once the
// step returns) -- roughly 15s. cmd.WaitDelay does not add a second window on
// top of that: os/exec starts its timer the instant cmd.Cancel returns, the
// same moment the escalation goroutine starts its own, so the two race
// concurrently rather than run back to back (see os/exec's watchCtx and the
// WaitDelay doc comment: "starts when either the associated Context is done
// or a call to Wait observes that the child process has exited, whichever
// occurs first"). 15s was the right estimate but zero slack; 30s gives real
// headroom and still fits inside the chart's terminationGracePeriodSeconds of
// 45 alongside the concurrent HTTP drain -- test/chart/contract.sh pins
// runShutdownTimeout against killGrace + terminalSaveTimeout, and both
// against the grace period, so they cannot drift apart silently.
const runShutdownTimeout = 30 * time.Second

// httpShutdownTimeout bounds the HTTP drain. Runs concurrently with the
// above, so the pod's total shutdown budget is the larger of the two, not
// their sum -- which is what lets both fit inside
// terminationGracePeriodSeconds.
const httpShutdownTimeout = 10 * time.Second

// workSubdirs are the directories the console and deploy.sh need writable.
// With readOnlyRootFilesystem: true, the emptyDir at AICRME_WORK_DIR is the
// only writable path in the container, so every tool that wants scratch
// space is pointed at a subdirectory of it by the chart's env block --
// bash's mktemp -d at TMPDIR, helm's three XDG-style caches, kubectl's
// discovery cache, and $HOME for anything that ignores all of the above.
// They are created here rather than by the chart because an emptyDir is
// mounted empty on every pod start.
var workSubdirs = []string{"tmp", "home", "helm/cache", "helm/config", "helm/data", "kube/cache", "runs"}

func ensureWorkDirs(root string) error {
	for _, sub := range workSubdirs {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o700); err != nil {
			return err
		}
	}
	return nil
}

// recipeNamespaces extracts the namespaces the resolved recipe installs
// into. A missing or unparseable artifact yields an empty set, which the
// observer treats as "filter every namespaced workload out" -- the
// fail-quiet direction, since narrating unrelated cluster activity is worse
// than narrating nothing.
func recipeNamespaces(raw []byte) map[string]struct{} {
	out := map[string]struct{}{}
	if len(raw) == 0 {
		return out
	}
	var summary steps.RecipeSummary
	if err := json.Unmarshal(raw, &summary); err != nil {
		slog.Warn("recipe.json unparseable; observer will not narrate workloads", "error", err)
		return out
	}
	for _, c := range summary.Components {
		if c.Namespace != "" {
			out[c.Namespace] = struct{}{}
		}
	}
	return out
}

// runReader narrows *engine.Engine to what newRunScopeFn needs, so its
// caching logic -- the part with real behavior to get wrong -- can be
// exercised with a fake instead of a live Engine. Neither method clones a
// whole Run: this accessor runs on the observer's per-event path.
type runReader interface {
	CurrentID() (string, bool)
	Artifact(runID, key string) ([]byte, bool)
}

// newRunScopeFn returns the namespace half of the observer's accessor --
// newObserverScopeFn composes it with the engine's attribution cursor into
// the single func the observer actually calls (see newObserverScopeFn's doc
// comment for why they stay two sources composed once, rather than one).
// It caches by run ID and refreshes only when that changes. Neither the
// cached path nor the miss path may call Engine.Current(), which deep-copies
// every artifact including the raw snapshot (tens of KB): observer.publish
// resolves the scope before it can apply the namespace filter, so on a busy
// cluster this runs for state changes on every Deployment and DaemonSet,
// in scope or not. CurrentID reads the ID under the engine lock without
// cloning, and Artifact copies exactly the one artifact this needs.
//
// The cache is populated only once recipe.json exists. Recommend does not
// run until the operator supplies the intent/platform decisions
// (recommend.Requires), and that wait -- plus Discover's own 10-minute
// timeout ahead of it -- can span most of a run. If the accessor cached an
// empty Namespaces the first time it was asked inside that window, every
// later call for the same run ID would keep returning it (RunID already
// matches the cache), so every namespaced workload event for the rest of
// the run would be silently dropped by observer.publish with no error
// anywhere. Caching only once the artifact is non-empty means a pre-recipe
// call recomputes on every invocation until Recommend writes it, then locks
// in the real value from then on -- safe because recipe.json, once written,
// is never mutated again.
func newRunScopeFn(eng runReader) func() observer.RunScope {
	var (
		mu     sync.Mutex
		cached observer.RunScope
	)
	return func() observer.RunScope {
		id, ok := eng.CurrentID()
		if !ok {
			return observer.RunScope{}
		}
		mu.Lock()
		defer mu.Unlock()
		if cached.RunID == id {
			return cached
		}
		sc := observer.RunScope{RunID: id}
		raw, _ := eng.Artifact(id, "recipe.json")
		if len(raw) == 0 {
			return sc // recipe not resolved yet -- caching this would pin an empty scope for the whole run
		}
		sc.Namespaces = recipeNamespaces(raw)
		cached = sc
		return sc
	}
}

// attributionReader narrows *engine.Engine to the one cheap accessor
// newObserverScopeFn needs: one lock acquisition over a handful of scalars,
// no artifact clone, no store I/O (internal/engine/attribution.go).
type attributionReader interface {
	Attribution() engine.Attribution
}

// newObserverScopeFn composes nsScope (built by newRunScopeFn) with eng's
// attribution snapshot into the single accessor observer.New takes. This is
// the composition Task 3 exists to build, and it is composed here -- in
// main, not in either internal/observer or internal/engine -- for a reason
// that is a hard constraint, not a preference:
//
// engine.Attribution deliberately does NOT carry Namespaces. Namespaces come
// from parsing recipe.json into steps.RecipeSummary, and internal/steps
// imports internal/engine -- so an engine-side accessor that also returned
// Namespaces would be an import cycle (Ruling 2,
// docs/superpowers/specs/2026-08-17-aicrme-phase-2b-iii-design.md Section
// 2). main already holds both the engine and 2b-ii's cached namespace
// parsing (newRunScopeFn), so it is the one place that can see both without
// creating that cycle. Do not "simplify" this into a single engine-side
// snapshot later; that is the cycle this shape exists to avoid.
//
// RunID and Component are taken from eng.Attribution() together, not from
// nsScope's own (cached) RunID: Attribution() reads RunID and ActiveAction
// under the engine's one lock, so the two always describe the same instant.
// Pairing Component with nsScope's RunID instead would let them describe two
// different reads taken microseconds apart across a run transition -- narrow,
// but avoidable for free by picking the read that is already atomic.
//
// nsScope() and eng.Attribution() are themselves two INDEPENDENT lock
// acquisitions -- nsScope calls Engine.CurrentID, which takes and releases
// e.mu on its own, before this func separately takes and releases e.mu again
// via Attribution(). A run transition landing between those two calls would
// otherwise pair one run's Namespaces with a different run's RunID/Component
// -- precisely the race RunScope's own doc comment forbids, one layer
// higher up (observer.go: "reading the run ID and the namespaces separately
// would let attribution and filtering come from different runs across a
// race"). The sc.RunID != a.RunID check below is what closes that gap: on
// disagreement this returns the zero RunScope rather than merging the two
// reads (Ruling 6). Merging is ruled out because it is the one option
// guaranteed to produce a WRONG answer -- an event stamped with one run's
// action but filtered by another run's namespaces. A retry loop is the
// wrong shape for a per-watch-event path. The zero RunScope costs nothing:
// unattributed is already a first-class outcome in this design (spec
// Section 1), and the disagreement window is a single transition instant,
// not a sustained state.
//
// The result is called by the observer exactly once per watch event
// (observer.Observer.publish), so eng.Attribution() -- itself cheap by
// construction -- is also called exactly once per event: one call in here,
// one call by the observer into this func. The disagreement check compares
// values already in hand (sc.RunID, a.RunID); it must never justify a third
// call into eng to "double check".
//
// sc.Terminal is copied from a.Terminal for the same reason Component is:
// Attribution() computes it fresh from e.current.State under the engine's
// one lock (isTerminal(e.current.State), internal/engine/attribution.go), so
// it can never disagree with the RunID it travels with here (Ruling 8). This
// is what lets the scoped Pod/Event informer lifecycle
// (internal/observer/scoped.go) treat RunScope as the single, authoritative
// answer to "is this run over" -- no separate at-most-once signal, no
// per-run memory of a prior terminal read, so a retried run (same RunID,
// State back to StateRunning) is simply not terminal on the very next read.
// The disagreement branch above still means "tear down" for that lifecycle
// without needing Terminal set: it returns RunID == "", which
// scopedInformers.reconcile already treats as no scope.
func newObserverScopeFn(eng attributionReader, nsScope func() observer.RunScope) func() observer.RunScope {
	return func() observer.RunScope {
		sc := nsScope()
		a := eng.Attribution()
		if sc.RunID != a.RunID {
			return observer.RunScope{}
		}
		sc.RunID = a.RunID
		sc.Component = a.ActiveAction
		sc.Generation = a.Generation
		sc.Terminal = a.Terminal
		return sc
	}
}

// runStoreSuffix distinguishes the run store's ConfigMap from the chart's
// own {{ include "aicrme.fullname" . }} ConfigMap (AICRME_TLS /
// AICRME_NAMESPACE -- charts/aicrme/templates/configmap.yaml), following the
// naming convention charts/aicrme/templates/secret.yaml already uses for the
// auth Secret ("<fullname>-auth"). It must be a distinct, runtime-created
// object: a templated one would revert to the chart's rendered content on
// every helm upgrade, wiping state an in-flight Apply is actively
// checkpointing (docs/superpowers/specs/2026-08-17-aicrme-phase-2b-ii-design.md,
// "It must not be the chart's ConfigMap, and must not be templated at all").
const runStoreSuffix = "-run"

// deploymentLookupTimeout bounds the one-time Deployment Get newRunStore
// issues to resolve the run ConfigMap's ownerReference. Everything the
// resulting store does afterward is self-bounded by cmStoreCallTimeout
// (internal/engine/cmstore.go), but this call happens before that store
// exists, so it has to bound itself: a wedged API server here must degrade
// to the in-memory store, not hang startup indefinitely the way 2b-i's
// unbounded WaitForCacheSync did.
const deploymentLookupTimeout = 10 * time.Second

// resolveDeploymentOwner returns an ownerReference to the named Deployment,
// so the run ConfigMap survives every pod and ReplicaSet churn along a
// rollout but is garbage-collected the moment the release itself is
// uninstalled. The chart sets AICRME_DEPLOYMENT_NAME to the exact
// {{ include "aicrme.fullname" . }} value it names the Deployment object
// with (charts/aicrme/templates/deployment.yaml), so one Get resolves the
// object this pod actually belongs to. Deliberately not a walk of the pod's
// own ownerReferences chain (Pod -> ReplicaSet -> Deployment): that would
// cost a second API call for the ReplicaSet -- itself transient and reaped
// on every rollout, exactly the object this reference must never target --
// for no benefit over asking for the Deployment directly.
func resolveDeploymentOwner(ctx context.Context, kube kubernetes.Interface, namespace, name string) (metav1.OwnerReference, error) {
	dep, err := kube.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return metav1.OwnerReference{}, err
	}
	return metav1.OwnerReference{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       dep.Name,
		UID:        dep.UID,
	}, nil
}

// newRunStore resolves the ConfigMap-backed run store, or falls back to an
// in-memory one. kube is nil outside a cluster (rest.InClusterConfig fails
// on a developer laptop) -- `make build && ./bin/aicrme` outside a cluster
// stays a supported development path, so this is expected and logs at Warn
// (the kube client construction above already explains why kube is nil for
// the telemetry side; this is the persistence-specific consequence, not a
// second diagnosis of the same cause).
//
// A resolution error with a live client is a different animal and logs at
// Error, not Warn: it means a pod holding cluster-admin cannot look up its
// own Deployment (RBAC, a control-plane blip, an unusual install order that
// starts the pod before the Deployment object is visible to its own API
// server), and /healthz reports identically healthy either way -- this log
// line is the only signal an operator gets that the durability this whole
// phase exists to provide has silently gone missing.
//
// Per Ruling 4, this is the only place that ever chooses the store;
// Engine.store is set once in New and, on the unreadable-record path,
// reassigned only by Recover itself.
func newRunStore(ctx context.Context, kube kubernetes.Interface, namespace, deploymentName string) engine.Store {
	if kube == nil {
		slog.Warn("no cluster client; run state will not survive a pod restart")
		return engine.NewMemoryStore()
	}
	lookupCtx, cancel := context.WithTimeout(ctx, deploymentLookupTimeout)
	defer cancel()
	owner, err := resolveDeploymentOwner(lookupCtx, kube, namespace, deploymentName)
	if err != nil {
		slog.Error("resolving the console Deployment for the run store's owner reference failed despite a live cluster client; run state will not survive a pod restart",
			"deployment", deploymentName, "namespace", namespace, "error", err)
		return engine.NewMemoryStore()
	}
	return engine.NewConfigMapStore(kube, namespace, deploymentName+runStoreSuffix, owner)
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	slog.Info("starting aicrme", "version", version.String())

	// Created before any of the fatal startup checks below, so the run
	// store's Deployment lookup and eng.Recover (both later, once the
	// engine exists) share shutdown's own cancellation. defer stop() is
	// deliberately not called here: every other startup check in this
	// function follows the same rule (see the kube client construction
	// below) -- a defer registered before a later os.Exit would never run,
	// which is exactly what gocritic's exitAfterDefer flags. stop() is
	// deferred once every fatal check has passed.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	workDir := envOr("AICRME_WORK_DIR", defaultWorkDir)
	if err := ensureWorkDirs(workDir); err != nil {
		slog.Error("work directory unusable", "dir", workDir, "error", err)
		os.Exit(1)
	}

	static, err := web.Static()
	if err != nil {
		slog.Error("embedded SPA unavailable", "error", err)
		os.Exit(1)
	}

	client, err := aicrclient.New()
	if err != nil {
		slog.Error("AICR client init failed", "error", err)
		os.Exit(1)
	}

	b := bus.New(replayCapacity)

	// A failure here must not stop the console: the entire Discover-to-Apply
	// arc works without cluster telemetry, and `make build && ./bin/aicrme`
	// outside a cluster is a supported development path. Same degrade-with-a-
	// warning posture as parseNodeSelector. No defer is registered by this
	// block, so its position relative to the fatal checks above and below it
	// does not matter the way it would for one that did. This warns about
	// telemetry only -- newRunStore below logs its own, more specific
	// warning about persistence for the same nil kube, so this does not
	// repeat that half of the consequence.
	var kube kubernetes.Interface
	// rest.InClusterConfig does no network I/O -- env vars and two file
	// reads -- so inside a pod kube is essentially always non-nil here; it
	// is Start below, not this block, that first talks to the API server.
	if cfg, cfgErr := rest.InClusterConfig(); cfgErr != nil {
		slog.Warn("no in-cluster config; live cluster telemetry disabled", "error", cfgErr)
	} else if c, clientErr := kubernetes.NewForConfig(cfg); clientErr != nil {
		slog.Warn("kubernetes client init failed; live cluster telemetry disabled", "error", clientErr)
	} else {
		kube = c
	}

	namespace := envOr("AICRME_NAMESPACE", "aicrme")
	runStore := newRunStore(ctx, kube, namespace, envOr("AICRME_DEPLOYMENT_NAME", "aicrme"))

	// Same nil kube as the telemetry warning above and newRunStore's own --
	// the third and most consequential-sounding of the three, since a run
	// that reaches Prove without it fails outright rather than merely
	// degrading. Prove itself already guards against a nil client (Client.Ready,
	// checked first in proveStep.Run) and fails just that one run rather than
	// the process, so this is visibility for the operator, not the guard.
	if kube == nil {
		slog.Warn("no cluster client; any run that reaches Prove will fail until aicrme runs in-cluster")
	}
	// One instance, shared between the Prove step below and eng.SetProveClient
	// further down -- see that call's comment for why a second construction
	// site is worth avoiding even though both would be equivalent today.
	proveClient := prove.NewClient(kube)

	eng := engine.New(b, runStore,
		steps.NewDiscover(client, steps.DiscoverConfig{
			Namespace: namespace,
			// aicr.Client.CollectSnapshot forwards Image verbatim to the Job
			// spec's container -- unlike the `aicr` CLI, the Go client applies
			// no fallback of its own (verified against pkg/client/v1 and
			// pkg/k8s/agent/job.go in the pinned module). An empty string here
			// reaches the API server as `image: ""`, which container
			// validation rejects outright, so Discover would fail before the
			// Job is even scheduled. defaultSnapshotAgentImage reproduces the
			// same ghcr.io/nvidia/aicr:<version> mapping the CLI's own
			// defaultAgentImage() uses (pkg/cli/root.go), pinned to this
			// console's aicr dependency (go.mod / .settings.yaml
			// dependencies.aicr) rather than derived at runtime.
			Image: envOr("AICRME_SNAPSHOT_IMAGE", defaultSnapshotAgentImage),
			// Unset (nil) on every real deployment: aicr.Client.CollectSnapshot
			// then auto-targets a real GPU node itself (see the NodeSelector
			// doc on steps.DiscoverConfig). AICRME_SNAPSHOT_NODE_SELECTOR
			// exists only so a KWOK-simulated cluster (this console's own
			// e2e test) can pin the agent Job off the tainted, fake-executing
			// simulated GPU nodes and onto a real one.
			NodeSelector: parseNodeSelector(os.Getenv("AICRME_SNAPSHOT_NODE_SELECTOR")),
			// Unset (nil) on every real deployment, where AICR's own 1000m
			// CPU default applies. Exists so the KWOK e2e can fit the agent
			// onto the one real node it is pinned to -- see the Requests
			// field's doc on steps.DiscoverConfig.
			Requests:   parseResourceRequests(os.Getenv("AICRME_SNAPSHOT_REQUESTS")),
			Privileged: true,
			Timeout:    10 * time.Minute,
		}),
		steps.NewRecommend(client),
		steps.NewBundle(client, steps.BundleConfig{
			WorkDir: workDir,
		}),
		steps.NewApply(applier.New(applier.BashExec{}), steps.ApplyConfig{
			Retries: defaultApplyRetries,
			// Not exposed in values.yaml, deliberately -- same treatment as
			// AICRME_SNAPSHOT_NODE_SELECTOR. It exists so the CI end-to-end
			// test can exercise the real deploy.sh and the real helm binary
			// against a cluster with no GPUs without installing anything.
			DryRun: os.Getenv("AICRME_APPLY_DRY_RUN") == "true",
		}),
		// The final step: a run that reaches here and returns without error
		// ends at StateActive rather than StateDone (engine.ActiveStep), with
		// the reference workload deliberately left running. proveClient
		// wraps the same in-cluster kube client the observer uses for
		// telemetry, nil outside a pod -- see the Warn above for why that no
		// longer risks the process.
		steps.NewProve(proveClient, steps.ProveConfig{
			// My ruling (design doc's own open question): provisional, to be
			// revisited against a real make demo once gang placement latency
			// on a live cluster is measured rather than guessed.
			GangTimeout: 3 * time.Minute,
		}),
	)
	// Stop (Task 7) is the only way a run leaves StateActive, and it needs
	// the same client the Prove step above just used to create the
	// workload -- one instance, shared, rather than a second one wrapping
	// the same kube: both are stateless beyond that one field, but a second
	// construction site is a second place for the two to silently diverge
	// if either ever grows real state. Set after New, not threaded through
	// it: engine.New's signature is shared by every test in this binary's
	// dependency tree (internal/engine and internal/api together construct
	// it at ~80 call sites), and Stop is the only caller that needs a
	// cluster client at all.
	eng.SetProveClient(proveClient)

	srv, err := api.New(api.Config{
		Username:   envOr("AICRME_USERNAME", "admin"),
		Password:   os.Getenv("AICRME_PASSWORD"),
		SessionTTL: 8 * time.Hour,
		LoginRate:  10,
		TLS:        os.Getenv("AICRME_TLS") == "true",
		AICR:       client,
		WorkDir:    workDir,
	}, b, eng, static)
	if err != nil {
		slog.Error("server configuration invalid", "error", err)
		os.Exit(1)
	}

	// Recovery must complete before httpSrv.ListenAndServe below: a window
	// in which the console answers requests while a recovered run is not yet
	// installed is a window in which the SPA's automatic POST /api/runs on
	// load wins the race and silently replaces it. This does not reintroduce
	// 2b-i's startup-hang class -- every ConfigMap call Recover makes is
	// bounded by cmStoreCallTimeout (internal/engine/cmstore.go) and its own
	// load retry is bounded and ctx-aware (internal/engine/recover.go) -- so
	// this call always returns. ErrStepConfig is the one error it returns:
	// a step slice recovery cannot make sense of, which is a programming
	// error in this binary's own wiring above, not a runtime condition, so
	// it is fatal rather than degraded. Everything else Recover handles
	// internally and falls through to a degraded start; StoreUnreadable()
	// exists purely so this can log that, per Ruling 4 -- Recover already
	// performed the only corrective action available (swapping to a fresh
	// memory store) by the time this returns.
	if err := eng.Recover(ctx); err != nil {
		slog.Error("engine step configuration cannot be recovered against", "error", err)
		os.Exit(1)
	}
	if eng.StoreUnreadable() {
		slog.Warn("persisted run checkpoint was unreadable or failed validation; starting without it")
	}

	// Immediately after Recover and, like it, before ListenAndServe below:
	// the record Recover just installed (or failed to find) is only half the
	// state -- the workload it describes outlives this process independently,
	// and the store can lose the record while the workload keeps holding
	// GPUs. Reconciling settles the two against each other and, in the case
	// that matters most, adopts a workload with no surviving record so the
	// operator gets a Stop button back rather than a cluster only kubectl can
	// clean up.
	//
	// Never fatal: an unreachable API server here costs the console its
	// bearings on a leftover workload, which is worth a loud warning and not
	// worth refusing to start over. The call itself is bounded (one List) and
	// deliberately decides nothing when that List fails -- see its own doc
	// comment for why a failed list must not be read as "gone".
	if err := eng.ReconcileWorkloads(ctx, proveClient); err != nil {
		slog.Warn("could not reconcile reference workloads left in the cluster; one may still be running untracked",
			"error", err)
	}

	// Every fatal startup check is above this point; only degrade-and-warn
	// paths remain below. Deferring stop() and client.Close() only now is
	// what keeps this function clean under gocritic's exitAfterDefer -- a
	// defer registered before a later os.Exit would silently never run.
	defer stop()
	defer func() { _ = client.Close() }()

	// Started in a goroutine, not called inline: Start ends by blocking on
	// the informer factory's WaitForCacheSync, which carries no deadline of
	// its own -- it returns only once every watched type has synced or
	// obsStop closes, and obsStop closes only when main returns (the defer
	// above). A synchronous call here would mean an unreachable, partitioned,
	// or merely slow API server blocks httpSrv.ListenAndServe() from ever
	// running -- and the chart's probes are already counting against that.
	// The startupProbe governs until it first succeeds (initialDelaySeconds:
	// 5, periodSeconds: 5, failureThreshold: 11 --
	// charts/aicrme/templates/deployment.yaml), which kills the pod at 55s;
	// once startup has succeeded the livenessProbe takes over and kills on
	// its third consecutive failure, at 25s. Either way the result of
	// blocking here is a permanent CrashLoopBackOff of the whole console,
	// caused by the one subsystem that is supposed to be optional. (A pod
	// dies on the failureThreshold-th CONSECUTIVE failure, so the last probe
	// that can still save it fires at initialDelaySeconds + periodSeconds x
	// (failureThreshold - 1) -- one period earlier than the arithmetic an
	// earlier version of this comment used, which named a fourth liveness
	// probe that never runs.) Handlers are registered
	// before the informer factory starts, so events flow whether or not this
	// goroutine's Start call has returned yet; its return value is consumed
	// only for this warning.
	obsStop := make(chan struct{})
	defer close(obsStop)
	obs := observer.New(kube, b, newObserverScopeFn(eng, newRunScopeFn(eng)))
	go func() {
		if startErr := obs.Start(obsStop); startErr != nil {
			slog.Warn("observer failed to start; continuing without cluster telemetry", "error", startErr)
		}
	}()

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: SSE streams are long-lived by design.
	}

	go func() {
		slog.Info("listening", "addr", *addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server failed", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")

	// Drain first: canceling the run lands it in StateFailed, which isLive
	// does not consider live, so an unguarded POST /api/runs during the wait
	// below would start a run that shutdown then kills mid-flight.
	srv.Drain()

	// HTTP drain and engine cleanup run concurrently. The invariant is "do
	// not return before the deploy.sh process tree is reaped" -- not "do not
	// begin HTTP shutdown first". aicrme is PID 1 under the image's
	// ENTRYPOINT with no init, so returning from main tears down the whole
	// PID namespace and SIGKILLs helm mid-release. Helm handles SIGTERM
	// itself and marks the release failed; killed outright it leaves the
	// release stranded in pending-install, which blocks the next
	// `helm upgrade --install` until someone runs `helm rollback` by hand.
	// deploy.sh's own INT/TERM trap is not what needs the time -- it is an
	// `rm -rf` of the helm temp workdir and returns immediately.
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		httpCtx, cancelHTTP := context.WithTimeout(context.Background(), httpShutdownTimeout)
		defer cancelHTTP()
		_ = httpSrv.Shutdown(httpCtx)
	}()

	go func() {
		defer wg.Done()
		engCtx, cancelEng := context.WithTimeout(context.Background(), runShutdownTimeout)
		defer cancelEng()
		if err := eng.CancelAndWait(engCtx); err != nil {
			slog.Error("in-flight run did not stop cleanly", "error", err)
		}
	}()

	wg.Wait()
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// parseNodeSelector turns a "key=value,key2=value2" list into a node
// selector map, or nil for an empty string -- so an unset
// AICRME_SNAPSHOT_NODE_SELECTOR leaves DiscoverConfig.NodeSelector nil and
// the aicr module's own real-cluster GPU auto-targeting applies unchanged.
// A malformed pair (no "=", or an empty key) is skipped rather than failing
// startup: one mistyped entry should degrade to "no override", not crash
// the console. But skipping is not the same as staying silent about it: a
// value that is present yet unparseable silently re-enables the very
// auto-targeting this knob exists to disable (see the NodeSelector doc on
// steps.DiscoverConfig -- on a KWOK cluster that means Discover pends on a
// tainted, non-tolerated fake node and times out loudly rather than ever
// completing, not a silent success, but it is still a confusing failure an
// operator should be able to trace back to a typo), so every dropped pair,
// and a fully malformed value that resolves to no selector at all, is
// logged at Warn.
// parseResourceRequests parses a "cpu=200m,memory=256Mi" list into a
// ResourceList for the snapshot agent's container requests. It degrades the
// same way parseNodeSelector does -- an unparseable entry is skipped with a
// warning and the rest still apply, because dropping one malformed pair is
// better than failing startup over a knob only the e2e sets.
//
// Returning nil for an empty or fully-unparseable value is what hands control
// back to AICR's own defaults, which is the correct production behavior.
func parseResourceRequests(s string) corev1.ResourceList {
	if s == "" {
		return nil
	}
	out := corev1.ResourceList{}
	for _, pair := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(pair, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			slog.Warn("AICRME_SNAPSHOT_REQUESTS: skipping unparseable pair",
				"pair", pair, "value", s)
			continue
		}
		q, err := resource.ParseQuantity(strings.TrimSpace(v))
		if err != nil {
			slog.Warn("AICRME_SNAPSHOT_REQUESTS: skipping unparseable quantity",
				"key", k, "pair", pair, "error", err)
			continue
		}
		out[corev1.ResourceName(k)] = q
	}
	if len(out) == 0 {
		slog.Warn("AICRME_SNAPSHOT_REQUESTS set but produced no usable requests; "+
			"the snapshot agent falls back to AICR's own defaults", "value", s)
		return nil
	}
	return out
}

func parseNodeSelector(s string) map[string]string {
	if s == "" {
		return nil
	}
	out := make(map[string]string)
	for _, pair := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(pair, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			slog.Warn("AICRME_SNAPSHOT_NODE_SELECTOR: skipping unparseable pair",
				"pair", pair, "value", s)
			continue
		}
		out[k] = strings.TrimSpace(v)
	}
	if len(out) == 0 {
		slog.Warn("AICRME_SNAPSHOT_NODE_SELECTOR set but produced no usable selector; "+
			"Discover falls back to the module's own GPU auto-targeting",
			"value", s)
		return nil
	}
	return out
}

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

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/api"
	"github.com/mchmarny/aicrme/internal/applier"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
	"github.com/mchmarny/aicrme/internal/observer"
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
// stop. One cancellation can spend two of the applier's killGrace windows
// back to back (internal/applier/exec.go): 10s for the process-group SIGTERM
// -> SIGKILL escalation, then up to another 10s of cmd.WaitDelay if a
// descendant that escaped the process group is still holding the stdout pipe
// open. 15s did not cover that. 30s does, and still fits inside the chart's
// terminationGracePeriodSeconds of 45 alongside the concurrent HTTP drain --
// test/chart/contract.sh pins the two against each other so they cannot
// drift apart silently.
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

// newRunScopeFn returns an accessor the observer calls on every watch event.
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

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	slog.Info("starting aicrme", "version", version.String())

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
	eng := engine.New(b, engine.NewMemoryStore(),
		steps.NewDiscover(client, steps.DiscoverConfig{
			Namespace: envOr("AICRME_NAMESPACE", "aicrme"),
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
			Privileged:   true,
			Timeout:      10 * time.Minute,
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
	)

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
	defer func() { _ = client.Close() }()

	// A failure here must not stop the console: the entire Discover-to-Apply
	// arc works without cluster telemetry, and `make build && ./bin/aicrme`
	// outside a cluster is a supported development path. Same degrade-with-a-
	// warning posture as parseNodeSelector. Placed after every remaining
	// os.Exit in main: `defer close(obsStop)` below would not run if an
	// exit happened after it (gocritic's exitAfterDefer), and every startup
	// failure that still exits is checked above this point.
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

	// Started in a goroutine, not called inline: Start ends by blocking on
	// the informer factory's WaitForCacheSync, which carries no deadline of
	// its own -- it returns only once every watched type has synced or
	// obsStop closes, and obsStop closes only when main returns (the defer
	// above). A synchronous call here would mean an unreachable, partitioned,
	// or merely slow API server blocks httpSrv.ListenAndServe() from ever
	// running, so the chart's liveness probe (initialDelaySeconds: 5,
	// periodSeconds: 10, default failureThreshold: 3 --
	// charts/aicrme/templates/deployment.yaml) kills the pod roughly every
	// 35s: a permanent CrashLoopBackOff of the whole console, caused by the
	// one subsystem that is supposed to be optional. Handlers are registered
	// before the informer factory starts, so events flow whether or not this
	// goroutine's Start call has returned yet; its return value is consumed
	// only for this warning.
	obsStop := make(chan struct{})
	defer close(obsStop)
	obs := observer.New(kube, b, newRunScopeFn(eng))
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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

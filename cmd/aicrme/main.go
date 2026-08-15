// Command aicrme serves the AI Cluster Runtime demo console.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/api"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
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

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	slog.Info("starting aicrme", "version", version.String())

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
	)

	srv, err := api.New(api.Config{
		Username:   envOr("AICRME_USERNAME", "admin"),
		Password:   os.Getenv("AICRME_PASSWORD"),
		SessionTTL: 8 * time.Hour,
		LoginRate:  10,
		TLS:        os.Getenv("AICRME_TLS") == "true",
		AICR:       client,
	}, b, eng, static)
	if err != nil {
		slog.Error("server configuration invalid", "error", err)
		os.Exit(1)
	}
	defer func() { _ = client.Close() }()

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
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
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

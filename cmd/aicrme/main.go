// Command aicrme serves the AI Cluster Runtime demo console from the
// operator's own machine.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/mchmarny/aicrme/internal/console"
	"github.com/mchmarny/aicrme/internal/version"
)

func main() {
	var opts console.Options
	flag.StringVar(&opts.Addr, "addr", "127.0.0.1:0",
		"listen address; must resolve to loopback. Port 0 lets the OS pick.")
	flag.StringVar(&opts.Kubeconfig, "kubeconfig", "",
		"path to a kubeconfig; unset falls through to the default loading rules, which honor KUBECONFIG")
	flag.StringVar(&opts.Context, "context", "",
		"kubeconfig context to preselect; the operator can still change it before connecting")
	open := flag.Bool("open", true, "open the default browser at the tokenized URL")
	flag.StringVar(&opts.WorkDir, "work-dir", defaultWorkDir(),
		"scratch and state directory; AICRME_WORK_DIR overrides the default")
	flag.Parse()
	opts.OpenBrowser = *open

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	slog.Info("starting aicrme", "version", version.String())

	// SIGINT and SIGTERM cancel ctx, which is the only thing that stops Run.
	// Run's own shutdown reaps the deploy.sh process tree before returning --
	// see its comment on why that ordering is not negotiable.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	// stop() is called explicitly rather than deferred: the error path exits,
	// and os.Exit does not run deferred functions.
	err := console.Run(ctx, opts)
	stop()
	if err != nil {
		slog.Error("aicrme failed", "error", err)
		os.Exit(1)
	}
}

// defaultWorkDir is AICRME_WORK_DIR, else ~/.aicrme. A home directory that
// cannot be resolved falls back to the current directory rather than failing:
// the console still works, and the operator sees where state landed in the
// startup log.
func defaultWorkDir() string {
	if v := os.Getenv("AICRME_WORK_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		slog.Warn("could not resolve a home directory; using ./.aicrme", "error", err)
		return ".aicrme"
	}
	return filepath.Join(home, ".aicrme")
}

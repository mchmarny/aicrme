// Command aicrme serves the AI Cluster Runtime demo console from the
// operator's own machine.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/go-logr/logr"
	"k8s.io/klog/v2"

	"github.com/mchmarny/aicrme/internal/console"
	"github.com/mchmarny/aicrme/internal/steps"
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
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	opts.OpenBrowser = *open

	// Before the logger is configured and before anything reaches for a
	// kubeconfig: asking a downloaded binary what it is must not require a
	// cluster, a home directory, or a running server. Homebrew's formula test
	// runs exactly this.
	if *showVersion {
		printVersion(os.Stdout)
		return
	}

	// AICR logs each validation check through slog's DEFAULT logger, so the
	// handler installed here is the one that sees them. Teeing it lets the
	// Validate step narrate a phase the SDK otherwise runs in total silence.
	progress := steps.NewProgressHandler(
		newQuietHandler(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	slog.SetDefault(slog.New(progress))
	opts.Progress = progress

	// client-go logs through klog, which writes to stderr on its own and knows
	// nothing about the handler above -- so its reflector churn arrived
	// unleveled and unfiltered, interleaved with the console's own output.
	// Routing it through the same handler is what lets quietHandler see it.
	klog.SetLogger(logr.FromSlogHandler(slog.Default().Handler()))
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

// printVersion writes the build identity as one line. The leading program name
// is what makes the output self-describing when it is pasted into a bug report,
// and it is what the Homebrew formula's test asserts on.
func printVersion(w io.Writer) {
	fmt.Fprintf(w, "aicrme %s\n", version.String())
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

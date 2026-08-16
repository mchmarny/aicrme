// Package applier executes a generated AICR bundle by driving the bundle's
// own deploy.sh, converting its stable output markers into console events.
//
// Why deploy.sh rather than each component's install.sh: deploy.sh carries
// correctness logic a per-component loop silently drops -- preflight for
// terminating namespaces, stale webhooks and orphaned CRD groups;
// per-component wait derivation; quadratic-backoff retry with helm hook-Job
// cleanup; and a post-install block that waits for every managed GPU node to
// reach nvidia.com/gpu-driver-upgrade-state=upgrade-done before restarting
// the DRA kubelet plugin. Skipping that last one strands DRA pods in
// ContainerCreating (AICR issue #973). Driving deploy.sh also keeps what the
// console runs byte-identical to what the user downloads.
package applier

import (
	"encoding/json"
	"regexp"
	"strconv"

	"github.com/mchmarny/aicrme/internal/bus"
)

// Component lifecycle values carried on ComponentData.Status.
const (
	// StatusStarted marks a component's install step beginning.
	StatusStarted = "started"
	// StatusInstalled marks a component that finished installing successfully.
	StatusInstalled = "installed"
	// StatusFailed marks a component that exhausted its retries.
	StatusFailed = "failed"
	// StatusRetrying marks a component about to retry after a failed attempt.
	StatusRetrying = "retrying"
)

// ComponentData is the Data payload on every KindComponent event. The SPA's
// web/src/pipeline.ts consumes this shape field for field.
type ComponentData struct {
	Name           string `json:"name"`
	Namespace      string `json:"namespace,omitempty"`
	Index          int    `json:"index,omitempty"`
	Total          int    `json:"total,omitempty"`
	Status         string `json:"status"`
	Attempt        int    `json:"attempt,omitempty"`
	MaxAttempts    int    `json:"maxAttempts,omitempty"`
	RetryInSeconds int    `json:"retryInSeconds,omitempty"`
}

// FailureData is the Data payload on the single KindError event the applier
// publishes when deploy.sh exits non-zero. Tail carries the last lines of
// raw output, which is where deploy.sh's own hook-Job and kai-scheduler
// diagnostic dumps live -- exactly what the failure screen must show.
type FailureData struct {
	Component string   `json:"component,omitempty"`
	ExitError string   `json:"exitError"`
	Tail      []string `json:"tail"`
}

// Marker patterns, transcribed from pkg/bundler/deployer/helm/templates/
// deploy.sh.tmpl at aicr v0.19.0 (_step_header, _step_ok, _step_fail,
// _step_retry, _ok, _warn_line, _fail). Every color variable expands empty
// because the applier exports NO_COLOR=1, so these match the bare text.
//
// The spacing is load-bearing and easy to "tidy" into breakage: the header
// has TWO spaces on each side of the arrow, and the retry line has TWO
// leading spaces. TestDeployTemplateUnchanged pins the template's sha256 so
// an upstream edit fails CI rather than silently emptying the timeline.
var (
	reHeader    = regexp.MustCompile(`^┌─ \[(\d+)/(\d+)\] (\S+)  →  (\S*)\s*$`)
	reInstalled = regexp.MustCompile(`^└─ ✓ (\S+) installed$`)
	reFailed    = regexp.MustCompile(`^└─ ✗ (\S+) FAILED \(after (\d+) attempts\)$`)
	reRetry     = regexp.MustCompile(`^ {2}↺ (\S+): attempt (\d+)/(\d+) failed, retrying in (\d+)s\.\.\.$`)
	reAsync     = regexp.MustCompile(`^│ {2}\((async component.*)\)$`)
	rePhaseOK   = regexp.MustCompile(`^✓ (Pre-flight checks passed|All components installed successfully\.)$`)
	reWarn      = regexp.MustCompile(`^⚠ (.+)$`)
	reFail      = regexp.MustCompile(`^✗ (.+)$`)
)

// parseLine maps one line of deploy.sh output to a console event. The bool
// is false for every line that is not a marker -- helm and kubectl output,
// banners, and diagnostic dumps. Those are deliberately NOT published: they
// are the overwhelming majority of the stream, and publishing them would
// exhaust the bus's replay ring and get live subscribers dropped for
// falling behind. Apply retains them in a bounded tail instead, and logs
// every line so `kubectl logs` keeps the complete transcript.
func parseLine(line string) (bus.Event, bool) {
	if m := reHeader.FindStringSubmatch(line); m != nil {
		return componentEvent(bus.LevelInfo, m[3], "installing "+m[3], ComponentData{
			Name:      m[3],
			Namespace: m[4],
			Index:     atoi(m[1]),
			Total:     atoi(m[2]),
			Status:    StatusStarted,
		}), true
	}
	if m := reInstalled.FindStringSubmatch(line); m != nil {
		return componentEvent(bus.LevelInfo, m[1], m[1]+" installed", ComponentData{
			Name: m[1], Status: StatusInstalled,
		}), true
	}
	if m := reFailed.FindStringSubmatch(line); m != nil {
		return componentEvent(bus.LevelError, m[1], m[1]+" failed after "+m[2]+" attempts", ComponentData{
			Name: m[1], Status: StatusFailed, Attempt: atoi(m[2]),
		}), true
	}
	if m := reRetry.FindStringSubmatch(line); m != nil {
		return componentEvent(bus.LevelWarn, m[1],
			m[1]+": attempt "+m[2]+" of "+m[3]+" failed, retrying in "+m[4]+"s",
			ComponentData{
				Name: m[1], Status: StatusRetrying,
				Attempt: atoi(m[2]), MaxAttempts: atoi(m[3]), RetryInSeconds: atoi(m[4]),
			}), true
	}
	if m := reAsync.FindStringSubmatch(line); m != nil {
		return bus.Event{Kind: bus.KindLog, Level: bus.LevelInfo, Message: m[1]}, true
	}
	if m := rePhaseOK.FindStringSubmatch(line); m != nil {
		return bus.Event{Kind: bus.KindPhase, Level: bus.LevelInfo, Message: m[1]}, true
	}
	if m := reWarn.FindStringSubmatch(line); m != nil {
		return bus.Event{Kind: bus.KindLog, Level: bus.LevelWarn, Message: m[1]}, true
	}
	if m := reFail.FindStringSubmatch(line); m != nil {
		return bus.Event{Kind: bus.KindLog, Level: bus.LevelError, Message: m[1]}, true
	}
	return bus.Event{}, false
}

func componentEvent(level bus.Level, component, message string, d ComponentData) bus.Event {
	// ComponentData holds only strings and ints, so Marshal cannot fail.
	encoded, _ := json.Marshal(d)
	return bus.Event{
		Kind:      bus.KindComponent,
		Level:     level,
		Component: component,
		Message:   message,
		Data:      encoded,
	}
}

// atoi is only ever called on a regexp capture group of \d+, so a parse
// failure is unreachable; zero is the honest value if that ever changes.
func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

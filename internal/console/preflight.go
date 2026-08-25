package console

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

// preflightTimeout bounds each version probe. These are local executables
// answering a --version flag; a second is already generous, and the ceiling
// exists so a wedged binary on PATH cannot hang startup.
const preflightTimeout = 5 * time.Second

// Toolchain is what this machine resolved for each executable the run depends
// on, keyed by tool name. It is recorded on the run and travels in the
// evidence bundle.
type Toolchain map[string]string

// tool describes one executable and how to ask it its version.
type tool struct {
	name string
	// args is the version subcommand. Kept per-tool because there is no
	// convention: helm wants a template, kubectl wants a client-only flag,
	// and bash prints a paragraph.
	args []string
	// required marks a tool the run cannot proceed without.
	required bool
	// degrades names what stops working when an optional tool is absent, so
	// the warning says something an operator can act on.
	degrades string
	// parse reduces the command's output to a version string.
	parse func(string) string
}

// tools is the set the deleted image supplied. Dockerfile:44 was
// `apk add --no-cache bash ca-certificates curl jq tar` plus a helm and
// kubectl fetch, and the comment above it named why: the console shells out
// to the bundle's deploy.sh, which needs bash, helm, kubectl, and jq.
//
// curl and tar are deliberately absent: the Dockerfile used them to fetch
// helm and kubectl and the same RUN removed them. Neither is a runtime
// dependency. CA certificates are a host property rather than a PATH lookup,
// and a machine with no trust store fails at the first HTTPS call with a
// clear TLS error.
var tools = []tool{
	// bash is not optional and not sh: applier.Apply builds
	// Argv: []string{"bash", "deploy.sh", ...} -- an explicit interpreter,
	// not a shebang -- so a machine without bash fails at exec with a message
	// about a missing file rather than a missing shell. deploy.sh is
	// AICR-generated and this repo does not control whether it stays
	// POSIX-clean.
	{name: "bash", args: []string{"--version"}, required: true, parse: firstLine},
	{name: "helm", args: []string{"version", "--template", "{{.Version}}"}, required: true, parse: strings.TrimSpace},
	{name: "kubectl", args: []string{"version", "--client", "-o", "json"}, required: true, parse: kubectlVersion},
	{name: "jq", args: []string{"--version"}, required: false, degrades: "deploy.sh's webhook preflight", parse: firstLine},
}

// preflight resolves every executable the run shells out to and records what
// it found.
//
// Missing is fatal for bash, helm and kubectl; missing jq is a warning.
// Version skew is a warning in every case, and every resolved version is
// recorded on the run and surfaced in the evidence bundle.
//
// Refusing to start because an operator has helm 3.20 rather than the 3.19.0
// the deleted Dockerfile pinned would make the tool unusable for the
// reproducibility it was meant to protect. For a product whose output is
// evidence, the honest way to serve "correctness must be reproducible" is to
// RECORD the toolchain that produced the result, not to block on it -- and
// today, with the version baked into an image, nothing ever asks.
//
// Helm 4 specifically is not a blocker: steps.helmLister.List has used
// explicit per-status flags rather than --all since e36b015, "list helm
// releases in a way both helm majors accept".
func preflight(ctx context.Context) (Toolchain, error) {
	found := Toolchain{}
	for _, t := range tools {
		path, err := exec.LookPath(t.name)
		if err != nil {
			if t.required {
				return nil, fmt.Errorf("%s is required and was not found on PATH: %w", t.name, err)
			}
			slog.Warn("optional tool not found on PATH; continuing with reduced function",
				"tool", t.name, "degrades", t.degrades)
			continue
		}
		version, err := probeVersion(ctx, path, t)
		if err != nil {
			// Resolved but unwilling to answer. Not fatal: the executable is
			// there and deploy.sh will use it either way, and refusing to
			// start over an unparseable --version would be exactly the
			// blocking this function argues against. The run records what
			// little is known.
			slog.Warn("could not read a tool's version; recording it as unknown",
				"tool", t.name, "path", path, "error", err)
			version = "unknown"
		}
		found[t.name] = version
	}
	slog.Info("toolchain", "resolved", found)
	return found, nil
}

func probeVersion(ctx context.Context, path string, t tool) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, preflightTimeout)
	defer cancel()

	//nolint:gosec // G204: path comes from exec.LookPath over a fixed tool name, and args are compile-time literals.
	out, err := exec.CommandContext(ctx, path, t.args...).Output()
	if err != nil {
		return "", err
	}
	version := t.parse(string(out))
	if version == "" {
		return "", fmt.Errorf("%s printed no version", t.name)
	}
	return version, nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
}

// kubectlVersion reads gitVersion out of `kubectl version --client -o json`,
// falling back to the raw output. The fallback matters: `-o json` is not
// universal across kubectl's own history, and a version this cannot parse is
// still better recorded verbatim than dropped.
func kubectlVersion(s string) string {
	var payload struct {
		ClientVersion struct {
			GitVersion string `json:"gitVersion"`
		} `json:"clientVersion"`
	}
	if err := json.Unmarshal([]byte(s), &payload); err == nil && payload.ClientVersion.GitVersion != "" {
		return payload.ClientVersion.GitVersion
	}
	return firstLine(s)
}

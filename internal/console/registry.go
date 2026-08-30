package console

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// helmCredentialHelperPrefix is the naming convention helm and docker share:
// a config naming credsStore "osxkeychain" invokes docker-credential-osxkeychain.
//
// The suppression below is a false positive: this is a binary-name prefix,
// and gosec's G101 matches on the word "credential" appearing in the name.
//
//nolint:gosec // G101: a binary-name prefix, not a credential.
const helmCredentialHelperPrefix = "docker-credential-"

// helmRegistryConfig is the subset of helm's registry credentials file this
// checks. It is docker's config.json format, and helm keeps its OWN copy of it
// -- $HELM_REGISTRY_CONFIG, by default under helm's config home, NOT
// ~/.docker/config.json. Pointing DOCKER_CONFIG somewhere does not affect it,
// which is the first thing anyone tries.
type helmRegistryConfig struct {
	CredsStore  string            `json:"credsStore"`
	CredHelpers map[string]string `json:"credHelpers"`
}

// checkHelmCredentialHelpers reports a credential helper that helm's registry
// config names but that does not exist on this machine, or "" when there is
// nothing to say.
//
// # WHY THIS IS WORTH A STARTUP PROBE
//
// helm resolves credentials for an oci:// chart through this config BEFORE it
// tries an anonymous pull. A config naming a helper binary that is not
// installed therefore fails EVERY OCI chart, including public ones that need
// no credentials at all:
//
//	Error: failed to perform "FetchReference" on source:
//	GET "https://ghcr.io/v2/nvidia/nodewright/charts/nodewright/manifests/v0.17.1":
//	exec: "docker-credential-osxkeychain": executable file not found in $PATH
//
// Observed on a real GKE install on 2026-08-28: a machine with no Docker
// Desktop, whose helm config still carried credsStore osxkeychain from a
// previous install. Apply died at component 3 of 16, five minutes in, after
// six identical retries, naming a Docker binary the operator never asked for
// and a registry that had refused nothing. The condition is knowable at
// startup, in one PATH lookup, before anything is installed.
//
// It reads the config helm ITSELF would read, via `helm env`, so an operator
// who has already worked around this by exporting HELM_REGISTRY_CONFIG is
// checked against the file they redirected to rather than the default.
//
// Every failure here returns "": a missing or unparseable registry config is
// the ordinary state of a machine that has never pulled a private chart, and
// this is a courtesy probe, not a gate.
func checkHelmCredentialHelpers(ctx context.Context) string {
	return credentialHelperProblem(helmRegistryConfigPath(ctx))
}

// credentialHelperProblem is the half that holds the logic, split from the
// half that shells out to `helm env` so it is testable against a fixture file
// without a helm binary on PATH.
func credentialHelperProblem(path string) string {
	if path == "" {
		return ""
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path comes from `helm env`, not from a request.
	if err != nil {
		return ""
	}
	var cfg helmRegistryConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return ""
	}

	// A helper is named once by credsStore (every registry) and possibly per
	// registry by credHelpers. Collect the distinct names and report on the
	// ones that are not installed.
	named := map[string][]string{}
	if cfg.CredsStore != "" {
		named[cfg.CredsStore] = append(named[cfg.CredsStore], "every registry")
	}
	for registry, helper := range cfg.CredHelpers {
		named[helper] = append(named[helper], registry)
	}

	var problems []string
	for helper, scopes := range named {
		if _, err := exec.LookPath(helmCredentialHelperPrefix + helper); err == nil {
			continue
		}
		sort.Strings(scopes)
		problems = append(problems, fmt.Sprintf("%s%s (%s)",
			helmCredentialHelperPrefix, helper, strings.Join(scopes, ", ")))
	}
	if len(problems) == 0 {
		return ""
	}
	sort.Strings(problems)

	// The message names the file, because that is the part nobody guesses --
	// it is not ~/.docker/config.json -- and both fixes, because which one is
	// right depends on whether the operator wants those credentials back.
	return fmt.Sprintf(
		"helm's registry config (%s) names a credential helper this machine does not have: %s. "+
			"Every oci:// chart will fail before helm tries an anonymous pull, including public ones. "+
			"Install the helper, or point HELM_REGISTRY_CONFIG at a config without it "+
			"(podman writes a usable one at ~/.config/containers/auth.json).",
		path, strings.Join(problems, "; "))
}

// helmRegistryConfigPath asks helm where its registry config lives, rather
// than reproducing the lookup. helm resolves it from HELM_REGISTRY_CONFIG, an
// XDG-ish config home, and a per-platform default; a reimplementation here
// would be a second source of truth that silently disagrees on the machine
// that matters.
func helmRegistryConfigPath(ctx context.Context) string {
	helm, err := exec.LookPath("helm")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, preflightTimeout)
	defer cancel()

	//nolint:gosec // G204: path comes from exec.LookPath over a fixed name, args are literals.
	out, err := exec.CommandContext(ctx, helm, "env", "HELM_REGISTRY_CONFIG").Output()
	if err != nil {
		return ""
	}
	// `helm env <KEY>` prints the bare value, but older helm prints the whole
	// KEY="value" environment regardless of the argument. Handle both.
	line := firstLine(string(out))
	if _, value, ok := strings.Cut(line, "="); ok {
		line = value
	}
	return strings.Trim(strings.TrimSpace(line), `"`)
}

// sanitizedRegistryConfigName is where the rewritten config is written, under
// the work directory beside everything else this run owns.
const sanitizedRegistryConfigName = "helm-registry.json"

// repairRegistryConfig writes a copy of helm's registry config with the
// unusable credential helpers removed, and returns its path.
//
// WHY THIS IS AICRME'S JOB
// The probe above is accurate and its advice works -- an operator who exports
// HELM_REGISTRY_CONFIG gets a clean run. But aicrme spawns every helm process
// itself, so it can hand them a working config instead of asking a human to.
// Telling someone to fix their environment for a problem you are already
// holding the fix for is a poor trade, and it is a step between them and a
// working cluster on the very first screen.
//
// It is a REWRITE, not a replacement. credsStore and the specific credHelpers
// entries naming missing binaries are dropped; `auths` and everything else are
// preserved verbatim, so credentials for a private registry that actually work
// keep working. Pointing helm at an empty config would fix the public charts
// and silently break the private ones, which is a worse failure than the one
// being fixed because it appears later and looks like a permissions problem.
//
// Only the helpers that are MISSING are removed. A machine with a working
// osxkeychain helper is untouched.
func repairRegistryConfig(srcPath, workDir string) (string, error) {
	raw, err := os.ReadFile(srcPath) //nolint:gosec // G304: path comes from `helm env`, not from a request.
	if err != nil {
		return "", err
	}
	// Decoded as a generic map rather than helmRegistryConfig: this file is
	// docker's format and carries fields aicrme has no business understanding,
	// and a round-trip through a typed struct would drop every one of them.
	var cfg map[string]any
	if decodeErr := json.Unmarshal(raw, &cfg); decodeErr != nil {
		return "", decodeErr
	}

	if store, ok := cfg["credsStore"].(string); ok && !helperInstalled(store) {
		delete(cfg, "credsStore")
	}
	if helpers, ok := cfg["credHelpers"].(map[string]any); ok {
		for registry, helper := range helpers {
			if name, ok := helper.(string); ok && !helperInstalled(name) {
				delete(helpers, registry)
			}
		}
		if len(helpers) == 0 {
			delete(cfg, "credHelpers")
		}
	}

	out, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	// MkdirAll first: this runs during the startup probe, BEFORE the work
	// directory is created for the session, so writing straight in failed with
	// ENOENT and the repair silently fell back to the warning it replaces.
	// Caught by a local smoke run 2026-08-30 rather than on a cluster.
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return "", err
	}
	dst := filepath.Join(workDir, sanitizedRegistryConfigName)
	// 0600: this is a copy of a credentials file.
	if err := os.WriteFile(dst, out, 0o600); err != nil {
		return "", err
	}
	return dst, nil
}

func helperInstalled(name string) bool {
	_, err := exec.LookPath(helmCredentialHelperPrefix + name)
	return err == nil
}

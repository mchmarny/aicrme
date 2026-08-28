package console

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRegistryConfig writes a helm registry config and returns its path.
func writeRegistryConfig(t *testing.T, cfg helmRegistryConfig) string {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshalling the fixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}
	return path
}

// The probe is split so the file-reading half is testable without a helm
// binary: helmRegistryConfigPath shells out, checkHelmCredentialHelpers does
// not. These exercise the half that holds the logic, through a small seam that
// takes the path directly.
func problemFor(t *testing.T, cfg helmRegistryConfig) string {
	t.Helper()
	return credentialHelperProblem(writeRegistryConfig(t, cfg))
}

// TestFlagsACredentialHelperThatIsNotInstalled is the 2026-08-28 failure: a
// laptop with no Docker Desktop whose helm config still named osxkeychain from
// a previous install. Apply died at component 3 of 16, five minutes in, on a
// PUBLIC chart -- helm consults the helper before it will try an anonymous
// pull, so the registry never even refused anything.
func TestFlagsACredentialHelperThatIsNotInstalled(t *testing.T) {
	got := problemFor(t, helmRegistryConfig{CredsStore: "definitely-not-installed"})

	if got == "" {
		t.Fatal("a credsStore naming a missing binary must be reported: every oci:// chart fails on it")
	}
	if !strings.Contains(got, "docker-credential-definitely-not-installed") {
		t.Errorf("problem = %q, want the full binary name an operator can install or search for", got)
	}
	// The file is the part nobody guesses -- it is NOT ~/.docker/config.json,
	// and DOCKER_CONFIG does not redirect it.
	if !strings.Contains(got, "config.json") {
		t.Errorf("problem = %q, want the path of the config that named the helper", got)
	}
}

// TestSaysNothingWhenTheHelperResolves keeps this from becoming noise. A
// warning that fires on a healthy machine is one nobody reads on a broken one.
func TestSaysNothingWhenTheHelperResolves(t *testing.T) {
	// `env` is a binary that exists everywhere this runs; naming it as the
	// helper makes the lookup succeed without installing anything.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, helmCredentialHelperPrefix+"fake"), []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // G306: an executable stub is the point.
		t.Fatalf("writing the stub helper: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if got := problemFor(t, helmRegistryConfig{CredsStore: "fake"}); got != "" {
		t.Errorf("problem = %q, want empty: the helper is on PATH", got)
	}
}

// TestSaysNothingWithoutCredentialHelpers covers the ordinary machine, and the
// podman case specifically: podman writes ~/.config/containers/auth.json with
// credentials inline and no credsStore at all, which is why pointing
// HELM_REGISTRY_CONFIG at it works.
func TestSaysNothingWithoutCredentialHelpers(t *testing.T) {
	if got := problemFor(t, helmRegistryConfig{}); got != "" {
		t.Errorf("problem = %q, want empty on a config that names no helper", got)
	}
}

// TestReportsPerRegistryHelpers covers credHelpers, which names a helper for
// one registry rather than all of them. Reporting the registry matters: the
// operator needs to know whether the broken one is a registry this run pulls
// from.
func TestReportsPerRegistryHelpers(t *testing.T) {
	got := problemFor(t, helmRegistryConfig{
		CredHelpers: map[string]string{"ghcr.io": "missing-helper"},
	})

	if !strings.Contains(got, "ghcr.io") {
		t.Errorf("problem = %q, want the registry the broken helper covers", got)
	}
}

// TestIsQuietAboutAnAbsentOrUnreadableConfig keeps this a courtesy probe
// rather than a gate. A machine that has never pulled a private chart has no
// registry config at all, and that is the normal state, not a fault.
func TestIsQuietAboutAnAbsentOrUnreadableConfig(t *testing.T) {
	if got := credentialHelperProblem(filepath.Join(t.TempDir(), "nope.json")); got != "" {
		t.Errorf("problem = %q, want empty for a config that does not exist", got)
	}

	garbage := filepath.Join(t.TempDir(), "garbage.json")
	if err := os.WriteFile(garbage, []byte("not json"), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}
	if got := credentialHelperProblem(garbage); got != "" {
		t.Errorf("problem = %q, want empty for a config that cannot be parsed", got)
	}
}

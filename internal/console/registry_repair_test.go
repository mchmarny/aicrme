package console

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// A rewrite, not a replacement. Pointing helm at an empty config would fix the
// public charts and silently break private ones -- a worse failure than the one
// being fixed, because it surfaces later and looks like a permissions problem.
func TestRepairRegistryConfigKeepsWorkingCredentials(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "config.json")
	// credsStore names a helper that certainly is not installed; auths is real
	// credential material that has to survive.
	body := `{"credsStore":"definitely-not-installed-xyz",` +
		`"auths":{"ghcr.io":{"auth":"c2VjcmV0"}},"HttpHeaders":{"User-Agent":"helm"}}`
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := repairRegistryConfig(src, dir)
	if err != nil {
		t.Fatalf("repairRegistryConfig() error = %v", err)
	}

	raw, err := os.ReadFile(out) //nolint:gosec // G304: test-owned temp path.
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("rewritten config is not JSON: %v", err)
	}

	if _, ok := cfg["credsStore"]; ok {
		t.Error("credsStore survived; it names a helper this machine does not have")
	}
	auths, ok := cfg["auths"].(map[string]any)
	if !ok || auths["ghcr.io"] == nil {
		t.Errorf("auths did not survive the rewrite: %v -- private registries would break", cfg)
	}
	// Fields aicrme has no business understanding must round-trip too.
	if _, ok := cfg["HttpHeaders"]; !ok {
		t.Error("an unrelated field was dropped; the rewrite must not be lossy")
	}
}

// A config with nothing wrong with it comes back intact. Named for what it
// actually covers: exercising the "helper IS installed" branch would need a
// docker-credential-* binary on PATH, which a unit test cannot rely on, so that
// branch is covered by helperInstalled's own lookup rather than here.
func TestRepairRegistryConfigPreservesAConfigWithNoHelpers(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "config.json")
	if err := os.WriteFile(src, []byte(`{"auths":{"example.com":{"auth":"eA=="}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := repairRegistryConfig(src, dir)
	if err != nil {
		t.Fatalf("repairRegistryConfig() error = %v", err)
	}
	raw, _ := os.ReadFile(out) //nolint:gosec // G304: test-owned temp path.
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if auths, ok := cfg["auths"].(map[string]any); !ok || auths["example.com"] == nil {
		t.Errorf("auths did not survive: %v", cfg)
	}
}

// credHelpers is per-registry. Only the entries naming missing binaries go.
func TestRepairRegistryConfigDropsOnlyTheBrokenCredHelpers(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "config.json")
	body := `{"credHelpers":{"ghcr.io":"definitely-not-installed-xyz"},` +
		`"auths":{"nvcr.io":{"auth":"eQ=="}}}`
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := repairRegistryConfig(src, dir)
	if err != nil {
		t.Fatalf("repairRegistryConfig() error = %v", err)
	}
	raw, _ := os.ReadFile(out) //nolint:gosec // G304: test-owned temp path.
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg["credHelpers"]; ok {
		t.Errorf("the broken credHelpers entry survived: %v", cfg)
	}
	if auths, ok := cfg["auths"].(map[string]any); !ok || auths["nvcr.io"] == nil {
		t.Errorf("auths did not survive: %v", cfg)
	}
}

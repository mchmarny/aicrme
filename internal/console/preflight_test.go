package console

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubTool writes an executable that prints version on stdout, so a preflight
// can resolve a tool without the machine running the test having it.
func stubTool(t *testing.T, dir, name, version string) {
	t.Helper()
	script := "#!/bin/sh\necho '" + version + "'\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", name, err)
	}
}

func TestPreflightFailsWhenBashIsMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // an empty PATH: nothing resolves

	_, err := preflight(context.Background())
	if err == nil {
		t.Fatal("preflight() succeeded with no bash on PATH")
	}
	if !strings.Contains(err.Error(), "bash") {
		t.Errorf("the error does not name bash: %v", err)
	}
}

// Each required tool is fatal on its own. Without this, a preflight that
// happened to check bash first would pass a machine missing helm.
func TestEveryRequiredToolIsIndividuallyFatal(t *testing.T) {
	for _, missing := range []string{"bash", "helm", "kubectl"} {
		t.Run(missing, func(t *testing.T) {
			dir := t.TempDir()
			for _, tool := range []string{"bash", "helm", "kubectl", "jq"} {
				if tool == missing {
					continue
				}
				stubTool(t, dir, tool, "v1.2.3")
			}
			t.Setenv("PATH", dir)

			_, err := preflight(context.Background())
			if err == nil {
				t.Fatalf("preflight() succeeded with no %s on PATH", missing)
			}
			if !strings.Contains(err.Error(), missing) {
				t.Errorf("the error does not name %s: %v", missing, err)
			}
		})
	}
}

func TestPreflightWarnsButSucceedsWithoutJq(t *testing.T) {
	dir := t.TempDir()
	for _, tool := range []string{"bash", "helm", "kubectl"} {
		stubTool(t, dir, tool, "v1.2.3")
	}
	t.Setenv("PATH", dir)

	tc, err := preflight(context.Background())
	if err != nil {
		t.Fatalf("preflight() error = %v -- a missing jq degrades, it does not block", err)
	}
	if _, ok := tc["jq"]; ok {
		t.Error("jq was recorded despite being absent")
	}
}

func TestPreflightRecordsEveryResolvedVersion(t *testing.T) {
	dir := t.TempDir()
	for _, tool := range []string{"bash", "jq", "helm", "kubectl"} {
		stubTool(t, dir, tool, "v9.9.9")
	}
	t.Setenv("PATH", dir)

	tc, err := preflight(context.Background())
	if err != nil {
		t.Fatalf("preflight() error = %v", err)
	}
	for _, tool := range []string{"bash", "jq", "helm", "kubectl"} {
		if tc[tool] == "" {
			t.Errorf("%s has no recorded version -- a run's evidence must be able to answer 'which helm installed this'", tool)
		}
	}
}

// Helm 4 is not a blocker. helmLister.List has used explicit per-status flags
// rather than --all since e36b015, "list helm releases in a way both helm
// majors accept".
func TestPreflightDoesNotBlockOnHelm4(t *testing.T) {
	dir := t.TempDir()
	for _, tool := range []string{"bash", "jq", "kubectl"} {
		stubTool(t, dir, tool, "v1.0.0")
	}
	stubTool(t, dir, "helm", "v4.0.1")
	t.Setenv("PATH", dir)

	tc, err := preflight(context.Background())
	if err != nil {
		t.Fatalf("preflight() refused helm 4: %v", err)
	}
	if !strings.Contains(tc["helm"], "4.0.1") {
		t.Errorf("helm version = %q, want the reported 4.0.1", tc["helm"])
	}
}

// A tool that resolves but will not answer --version is still a tool
// deploy.sh will use, so it is recorded rather than fatal. Refusing here
// would be exactly the blocking preflight's own comment argues against.
func TestAToolThatWillNotReportItsVersionIsRecordedAsUnknown(t *testing.T) {
	dir := t.TempDir()
	for _, tool := range []string{"bash", "jq", "kubectl"} {
		stubTool(t, dir, tool, "v1.0.0")
	}
	broken := filepath.Join(dir, "helm")
	if err := os.WriteFile(broken, []byte("#!/bin/sh\nexit 3\n"), 0o700); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	t.Setenv("PATH", dir)

	tc, err := preflight(context.Background())
	if err != nil {
		t.Fatalf("preflight() error = %v, want a recorded unknown rather than a refusal", err)
	}
	if tc["helm"] != "unknown" {
		t.Errorf("helm version = %q, want %q", tc["helm"], "unknown")
	}
}

// bash prints a paragraph, and none of it past the version is a version.
//
// This asserted the whole first line until the Connect screen showed what
// that costs: "GNU bash, version 5.2.37(1)-release (aarch64-apple-darwin…)"
// wrapped to two lines and outweighed the helm version beside it. The
// second line being dropped is still the property under test -- the first
// line is now reduced further, to the version itself.
func TestMultiLineVersionOutputIsReducedToItsFirstLine(t *testing.T) {
	dir := t.TempDir()
	for _, tool := range []string{"helm", "kubectl", "jq"} {
		stubTool(t, dir, tool, "v1.0.0")
	}
	multi := "#!/bin/sh\necho 'GNU bash, version 5.2.37(1)-release'\necho 'Copyright (C) 2022 Free Software Foundation'\n"
	if err := os.WriteFile(filepath.Join(dir, "bash"), []byte(multi), 0o700); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	t.Setenv("PATH", dir)

	tc, err := preflight(context.Background())
	if err != nil {
		t.Fatalf("preflight() error = %v", err)
	}
	if want := "5.2.37"; tc["bash"] != want {
		t.Errorf("bash version = %q, want %q", tc["bash"], want)
	}
}

func TestKubectlVersionPrefersGitVersionFromJSON(t *testing.T) {
	const payload = `{"clientVersion":{"major":"1","minor":"34","gitVersion":"v1.34.2"},"kustomizeVersion":"v5.7.1"}`
	if got := kubectlVersion(payload); got != "v1.34.2" {
		t.Errorf("kubectlVersion() = %q, want v1.34.2", got)
	}
}

// -o json is not universal across kubectl's history, and a version this
// cannot parse is still better recorded verbatim than dropped.
func TestKubectlVersionFallsBackToRawOutput(t *testing.T) {
	if got := kubectlVersion("Client Version: v1.30.0\n"); got != "Client Version: v1.30.0" {
		t.Errorf("kubectlVersion() = %q, want the raw first line", got)
	}
}

// TestBashVersionIsAVersionNotABanner is a screen-space argument with a
// correctness edge.
//
// `bash --version` prints a paragraph, and firstLine kept all of it:
// "GNU bash, version 5.3.15(1)-release (aarch64-apple-darwin25.4.0)" wrapped
// to two lines on the Connect screen and was given the same weight as
// "helm v4.2.4", which is the one an operator might actually act on. The
// other three tools are already reduced to a version; bash was the outlier.
//
// The banner is still the fallback: a bash whose output this cannot parse is
// better recorded verbatim than dropped, exactly as kubectlVersion decided
// for the same reason.
func TestBashVersionIsAVersionNotABanner(t *testing.T) {
	cases := map[string]string{
		"GNU bash, version 5.3.15(1)-release (aarch64-apple-darwin25.4.0)": "5.3.15",
		"GNU bash, version 3.2.57(1)-release (x86_64-apple-darwin23)":      "3.2.57",
		"GNU bash, version 5.2.21(1)-release":                              "5.2.21",
		// Unparseable: kept whole rather than discarded.
		"something else entirely": "something else entirely",
	}
	for in, want := range cases {
		if got := bashVersion(in); got != want {
			t.Errorf("bashVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

package applier

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// deployTemplateSHA256 pins pkg/bundler/deployer/helm/templates/deploy.sh.tmpl
// from the aicr module go.mod pins. The parser in parse.go transcribes that
// file's printf formats; nothing in the Go type system connects the two, so
// an upstream edit would otherwise silently empty the Apply timeline
// instead of failing. When this test fails on an aicr bump, re-read the
// template's output helpers against parse.go's regexes, update both this
// constant and the regexes, and re-capture
// testdata/deploy-transcript-kwok.txt.
const deployTemplateSHA256 = "df919af7e46d565d38fbf12927881ebeec1172227efac8962e4c00f035a8b519"

const deployTemplateRelPath = "pkg/bundler/deployer/helm/templates/deploy.sh.tmpl"

func TestDeployTemplateUnchanged(t *testing.T) {
	path := aicrModuleFile(t, deployTemplateRelPath)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s error = %v", path, err)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != deployTemplateSHA256 {
		t.Fatalf("deploy.sh.tmpl sha256 = %s, want %s\n"+
			"The template the marker parser transcribes has changed. Re-read its\n"+
			"output helpers against internal/applier/parse.go's regexes before\n"+
			"updating this constant.", got, deployTemplateSHA256)
	}
}

// aicrModuleFile resolves a path inside the pinned aicr module in the local
// module cache via `go list -m`. build.Default.Import (the go/build route)
// can shell out to resolve the full build list in module mode, which fails
// behind an authenticating proxy on a cold cache; `go list -m -f {{.Dir}}`
// reads only go.mod and the module cache and resolves offline once the
// cache is warm. If it can't -- e.g. a cold-cache CI box with no network --
// this test skips rather than red-failing, so the guard degrades visibly
// instead of blocking unrelated work.
func aicrModuleFile(t *testing.T, rel string) string {
	t.Helper()
	cmd := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/NVIDIA/aicr")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Skipf("skipping template pin: `go list -m` could not resolve github.com/NVIDIA/aicr (likely a cold module cache with no network access): %v: %s", err, stderr.String())
	}
	root := strings.TrimSpace(stdout.String())
	if root == "" {
		t.Skip("skipping template pin: `go list -m` returned an empty module directory")
	}
	return filepath.Join(root, filepath.FromSlash(rel))
}

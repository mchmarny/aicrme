package api_test

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/api"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
	"github.com/mchmarny/aicrme/internal/testfs"
)

// rawBundlePathStep hands a fixed bundle.path artifact to a run, so
// bundle_test.go's tests can exercise handleBundle without a real Bundle
// step, matching options_test.go's rawSnapshotStep pattern.
type rawBundlePathStep struct{ path string }

func (rawBundlePathStep) Phase() engine.Phase { return engine.PhaseBundle }
func (rawBundlePathStep) Requires() []string  { return nil }
func (s rawBundlePathStep) Run(_ context.Context, r *engine.Run, _ engine.Emit) error {
	if s.path != "" {
		r.Artifacts["bundle.path"] = []byte(s.path)
	}
	return nil
}

// newBundleTestServer builds a server whose engine runs step and returns a
// logged-in client plus the completed run's ID.
func newBundleTestServer(t *testing.T, workDir string, step engine.Step) (*httptest.Server, *http.Client, string) {
	t.Helper()
	b := bus.New(64)
	srv, err := api.New(api.Config{
		Username: "admin", Password: "correct-horse", SessionTTL: time.Hour, LoginRate: 100,
		AICR: &aicrclient.Fake{}, WorkDir: workDir,
	}, b, engine.New(b, engine.NewMemoryStore(), step), testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	ts, client := loggedInClient(t, srv.Handler())

	resp, err := client.Post(ts.URL+"/api/runs", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /api/runs error = %v", err)
	}
	var created engine.Run
	decErr := json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if decErr != nil {
		t.Fatalf("decode error = %v", decErr)
	}
	waitForRunState(t, client, ts.URL+"/api/runs/"+created.ID, engine.StateDone)

	return ts, client, created.ID
}

// A bundle.path artifact pointing outside the configured work dir must be
// refused. Nothing writes such a path today, but this handler turns an
// artifact value into a filesystem read, and "no caller does that yet" is
// not a boundary.
func TestBundleDownloadRefusesAPathOutsideWorkDir(t *testing.T) {
	workDir := t.TempDir()
	ts, client, runID := newBundleTestServer(t, workDir, rawBundlePathStep{path: "/etc"})

	resp, err := client.Get(ts.URL + "/api/runs/" + runID + "/bundle")
	if err != nil {
		t.Fatalf("GET bundle error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body error = %v", err)
	}
	if len(body) != 0 {
		t.Errorf("body = %q, want empty -- must never echo the rejected path back", body)
	}
}

func TestBundleDownloadStreamsATarball(t *testing.T) {
	workDir := t.TempDir()
	runID := "run-under-test"
	bundleDir := filepath.Join(workDir, "runs", runID, "bundle")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatalf("MkdirAll error = %v", err)
	}
	scriptContents := "#!/bin/bash\necho deploying\n"
	scriptPath := filepath.Join(bundleDir, "deploy.sh")
	if err := os.WriteFile(scriptPath, []byte(scriptContents), 0o755); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}

	ts, client, id := newBundleTestServer(t, workDir, rawBundlePathStep{path: bundleDir})

	resp, err := client.Get(ts.URL + "/api/runs/" + id + "/bundle")
	if err != nil {
		t.Fatalf("GET bundle error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/gzip" {
		t.Errorf("Content-Type = %q, want application/gzip", ct)
	}
	wantDisposition := `attachment; filename="aicrme-bundle-` + id + `.tar.gz"`
	if cd := resp.Header.Get("Content-Disposition"); cd != wantDisposition {
		t.Errorf("Content-Disposition = %q, want %q", cd, wantDisposition)
	}

	gzr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader error = %v", err)
	}
	defer gzr.Close()
	tr := tar.NewReader(gzr)

	var foundScript bool
	for {
		hdr, tarErr := tr.Next()
		if tarErr == io.EOF {
			break
		}
		if tarErr != nil {
			t.Fatalf("tar read error = %v", tarErr)
		}
		if hdr.Name != "deploy.sh" {
			continue
		}
		foundScript = true
		contents, readErr := io.ReadAll(tr)
		if readErr != nil {
			t.Fatalf("read tar entry error = %v", readErr)
		}
		if string(contents) != scriptContents {
			t.Errorf("deploy.sh contents = %q, want %q", contents, scriptContents)
		}
		if mode := hdr.FileInfo().Mode().Perm(); mode != 0o755 {
			t.Errorf("deploy.sh mode = %o, want %o", mode, 0o755)
		}
	}
	if !foundScript {
		t.Error("tarball did not contain deploy.sh")
	}
}

// A symlink planted inside an otherwise-legitimate (in-bounds) bundle
// directory must not be followed: the containment check validates the
// bundle directory's own path, but says nothing about what an entry nested
// inside it points at. os.Open follows symlinks, so without a type check a
// bundle download can be turned into an arbitrary-file read of anything the
// process can see -- and even setting content aside, tar.FileInfoHeader
// reports a symlink entry as Typeflag=TypeSymlink, Size=0, so writing the
// target's bytes under that header corrupts the archive regardless of
// intent.
func TestBundleDownloadRejectsSymlinkedContent(t *testing.T) {
	secretDir := t.TempDir()
	const secret = "TOP-SECRET-CONTENTS-not-part-of-any-bundle"
	secretPath := filepath.Join(secretDir, "secret.txt")
	if err := os.WriteFile(secretPath, []byte(secret), 0o600); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}

	workDir := t.TempDir()
	runID := "run-under-test"
	bundleDir := filepath.Join(workDir, "runs", runID, "bundle")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatalf("MkdirAll error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "deploy.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}
	if err := os.Symlink(secretPath, filepath.Join(bundleDir, "sneaky-link")); err != nil {
		t.Fatalf("Symlink error = %v", err)
	}

	ts, client, id := newBundleTestServer(t, workDir, rawBundlePathStep{path: bundleDir})

	resp, err := client.Get(ts.URL + "/api/runs/" + id + "/bundle")
	if err != nil {
		t.Fatalf("GET bundle error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusInternalServerError)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body error = %v", err)
	}
	if strings.Contains(string(body), secret) {
		t.Errorf("response leaked symlink target content: %q", body)
	}
}

func TestBundleDownloadIsNotFoundBeforeBundleRuns(t *testing.T) {
	workDir := t.TempDir()
	ts, client, runID := newBundleTestServer(t, workDir, rawBundlePathStep{})

	resp, err := client.Get(ts.URL + "/api/runs/" + runID + "/bundle")
	if err != nil {
		t.Fatalf("GET bundle error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

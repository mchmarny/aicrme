package api

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

// handleBundle streams the run's generated AICR bundle as a gzipped tarball.
// It makes the confirm gate the console shows before Apply honest: the user
// can inspect exactly what they are about to approve.
func (s *Server) handleBundle(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	// engine.Get takes no context parameter by design (internal/engine is
	// locked); see the identical justification on handleGetRun in runs.go.
	run, err := s.engine.Get(runID) //nolint:contextcheck
	if err != nil {
		writeErr(w, err)
		return
	}

	bundlePath := string(run.Artifacts["bundle.path"])
	if bundlePath == "" {
		writeErr(w, aicrerrors.New(aicrerrors.ErrCodeNotFound, "bundle not generated yet"))
		return
	}

	// The containment check is a real security boundary, not a formality:
	// this handler turns an artifact value into a filesystem read. Nothing
	// writes a malicious bundle.path today, but "no caller does that yet" is
	// not a boundary. Comparing against the cleaned work dir plus a path
	// separator (rather than a bare string prefix) stops a sibling
	// directory -- e.g. /var/lib/aicrme-evil against /var/lib/aicrme -- from
	// passing a naive prefix test.
	root := filepath.Clean(s.workDir)
	dir := filepath.Clean(bundlePath)
	if !strings.HasPrefix(dir, root+string(os.PathSeparator)) {
		// A bare status, deliberately not routed through writeErr: this
		// response body must never echo the rejected (or configured) path
		// back to the client.
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="aicrme-bundle-%s.tar.gz"`, runID))
	w.WriteHeader(http.StatusOK)

	gzw := gzip.NewWriter(w)
	tw := tar.NewWriter(gzw)

	// Headers are already sent by this point, so a walk or write failure here
	// can no longer change the response's status code -- log it instead.
	if walkErr := writeBundleTar(tw, dir); walkErr != nil {
		slog.Error("bundle download failed mid-stream", "run", runID, "error", walkErr)
	}
	if closeErr := tw.Close(); closeErr != nil {
		slog.Error("bundle tar close failed", "run", runID, "error", closeErr)
	}
	if closeErr := gzw.Close(); closeErr != nil {
		slog.Error("bundle gzip close failed", "run", runID, "error", closeErr)
	}
}

// writeBundleTar walks dir and writes each entry into tw with paths relative
// to dir, preserving each file's mode so deploy.sh stays executable.
func writeBundleTar(tw *tar.Writer, dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			header.Name += "/"
		}
		if hdrErr := tw.WriteHeader(header); hdrErr != nil {
			return hdrErr
		}
		if d.IsDir() {
			return nil
		}

		// p is produced by WalkDir under dir, which handleBundle already
		// verified is contained within s.workDir before this walk began.
		f, err := os.Open(p) //nolint:gosec // G304: p is contained within the work-dir boundary handleBundle verifies before walking.
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
}

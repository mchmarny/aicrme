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
	run, err := s.engine.Get(r.Context(), runID)
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

	// The directory being inside workDir does not make everything under it
	// safe to stream: a symlink nested in the tree can still point anywhere
	// the process can read, and os.Open follows it. The same "no caller
	// does that yet is not a boundary" reasoning above applies here --
	// nothing plants such a symlink today, but that is not a guarantee.
	// Validated before any header is written, so a bundle that fails this
	// check gets a clean 500 instead of a truncated, silently-malformed
	// archive: headers sent so far cannot un-send a 200, and
	// tar.FileInfoHeader reports Typeflag=TypeSymlink with Size=0 for an
	// unfollowed link, so writing that entry's target bytes under a
	// zero-length header would corrupt the stream from that point on
	// regardless. The tree is small (bundles run tens to low hundreds of KB
	// in practice), so a full pre-walk here is cheap.
	if err := validateBundleTree(dir); err != nil {
		slog.Error("bundle contains an unsupported entry", "run", runID, "error", err)
		writeErr(w, aicrerrors.New(aicrerrors.ErrCodeInternal, "bundle contains an unsupported file type"))
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

// bundleEntryAllowed reports whether d is a type handleBundle will stream.
// Only regular files and directories qualify -- symlinks, devices, sockets,
// and FIFOs are all rejected. d.Type() reflects the entry's own Lstat-style
// mode (WalkDir never follows a symlink to classify it), so this check is
// itself immune to the exact bug it exists to catch.
func bundleEntryAllowed(d fs.DirEntry) bool {
	return d.Type().IsRegular() || d.IsDir()
}

// validateBundleTree walks dir and fails on the first entry that is not a
// regular file or a directory, without opening or reading any file content.
// Run before any response bytes are written -- see the comment at its call
// site in handleBundle for why this has to be a separate pass rather than
// folded into writeBundleTar's own walk.
func validateBundleTree(dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !bundleEntryAllowed(d) {
			return fmt.Errorf("%s: unsupported file type %s", p, d.Type())
		}
		return nil
	})
}

// writeBundleTar walks dir and writes each entry into tw with paths relative
// to dir, preserving each file's mode so deploy.sh stays executable. dir is
// assumed already validated by validateBundleTree; the type check here is
// defense in depth against a TOCTOU change between the two walks, not the
// primary guard.
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
		if !bundleEntryAllowed(d) {
			return fmt.Errorf("%s: unsupported file type %s", p, d.Type())
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

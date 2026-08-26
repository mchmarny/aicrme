package console

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// lockFileName is the work directory's exclusion marker. It sits beside runs/
// and bundles/ rather than inside either, because what it guards is the
// directory as a whole.
const lockFileName = "lock"

// acquireLock takes exclusive ownership of workDir for this process.
//
// Two aicrme processes sharing a work directory write the same run record,
// and against the same cluster they also drive the same install. The
// in-cluster console never had this problem -- one Deployment, one replica,
// and cmstore.resolveDeploymentOwner detected a record written by a different
// deployment. That file is deleted in this restructure, so this replaces the
// guard it carried rather than merely dropping it. The local model makes the
// case likelier, not rarer: a second aicrme is one keystroke in a second
// terminal, where a second console Deployment took a deliberate helm install
// under a new release name.
//
// A stale lock is REPORTED, not cleared. A live second process and a crashed
// first one look identical from the file alone, and guessing wrong is exactly
// the case this exists to prevent.
//
// This is local exclusion, not distributed. Two operators on two laptops
// installing into the same cluster is not something a file lock can see, and
// this does not try: Apply's idempotence and AICR's own release-level
// behavior are what stand between that case and damage.
//
// O_CREATE|O_EXCL rather than flock: it is portable across the two platforms
// this ships on, it survives an NFS-mounted home directory better, and the
// file's contents carry the diagnostic -- a flock leaves nothing for an
// operator to read.
func acquireLock(workDir string) (func(), error) {
	path := filepath.Join(workDir, lockFileName)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, lockHeldError(path)
		}
		return nil, fmt.Errorf("creating the work directory lock %s: %w", path, err)
	}

	pid := os.Getpid()
	if _, err := f.WriteString(strconv.Itoa(pid)); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("writing the work directory lock %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("closing the work directory lock %s: %w", path, err)
	}

	return func() { releaseLock(path, pid) }, nil
}

// releaseLock removes this process's claim and only this process's claim. A
// release that deleted whatever it found would, after an operator cleared a
// stale lock by hand and started a second console, delete the new console's
// lock on the way out and leave the directory unguarded with nothing logged.
func releaseLock(path string, pid int) {
	held, err := readLockPID(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return
	case err != nil:
		slog.Warn("work directory lock is unreadable; leaving it in place", "lock", path, "error", err)
		return
	case held != pid:
		slog.Warn("work directory lock names another process; leaving it in place",
			"lock", path, "pid", held, "thisPid", pid)
		return
	}
	if err := os.Remove(path); err != nil {
		slog.Warn("could not remove the work directory lock", "lock", path, "error", err)
	}
}

// lockHeldError describes an existing lock as precisely as the file allows.
// Every branch refuses; they differ only in what the operator is told, and
// that difference is the whole value of the file having contents.
func lockHeldError(path string) error {
	pid, err := readLockPID(path)
	if err != nil {
		return fmt.Errorf("another aicrme may be using this work directory: %s exists but could not be read (%w). "+
			"Check for a running aicrme, then delete the file if there is none", path, err)
	}
	if processLive(pid) {
		return fmt.Errorf("another aicrme (pid %d) is using this work directory; its lock is %s. "+
			"Use that console, or start this one with --work-dir pointing somewhere else", pid, path)
	}
	return fmt.Errorf("this work directory is locked by pid %d, which is no longer running -- %s was left behind by a "+
		"crash or a kill. Confirm no aicrme is running and delete the file to continue", pid, path)
}

// readLockPID reads the PID a lock file records.
func readLockPID(path string) (int, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is filepath.Join(workDir, "lock"), not caller input.
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, fmt.Errorf("contents %q are not a pid", strings.TrimSpace(string(raw)))
	}
	return pid, nil
}

// processLive reports whether pid names a running process. Signal 0 performs
// the permission and existence check without delivering anything.
//
// Non-positive PIDs are never live, and that guard is load-bearing rather
// than defensive: POSIX gives kill(0, sig) the meaning "every process in my
// process group" and kill(-n, sig) "every process in group n", both of which
// succeed. Without this, a lock file holding 0 -- what a truncated or
// half-written file most easily decodes to -- would report live forever and
// no operator could start again without deleting a file nothing told them
// about.
//
// EPERM means the process exists and belongs to someone else, which is still
// live for this purpose: another user's aicrme is exactly as much of a second
// writer as one's own.
func processLive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

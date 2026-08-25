package console

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestSecondLockOnTheSameWorkDirIsRefused(t *testing.T) {
	dir := t.TempDir()
	release, err := acquireLock(dir)
	if err != nil {
		t.Fatalf("first acquireLock() error = %v", err)
	}
	defer release()

	if _, err := acquireLock(dir); err == nil {
		t.Fatal("a second process acquired the same work directory -- both would write the same run record")
	}
}

func TestLockIsReleasedOnClose(t *testing.T) {
	dir := t.TempDir()
	release, err := acquireLock(dir)
	if err != nil {
		t.Fatalf("acquireLock() error = %v", err)
	}
	release()

	second, err := acquireLock(dir)
	if err != nil {
		t.Fatalf("acquireLock() after release error = %v", err)
	}
	second()
}

// A lock whose PID is dead is reported, not cleared. A live second process
// and a crashed first one look identical from the file alone, and guessing
// wrong is the case this guard exists to prevent.
func TestStaleLockIsReportedWithItsPathAndPID(t *testing.T) {
	dir := t.TempDir()
	// PID 0 is never a live user process, so the liveness probe must say dead.
	if err := os.WriteFile(filepath.Join(dir, "lock"), []byte("0"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	_, err := acquireLock(dir)
	if err == nil {
		t.Fatal("acquireLock() cleared a stale lock automatically")
	}
	if !strings.Contains(err.Error(), filepath.Join(dir, "lock")) {
		t.Errorf("the error does not name the lock path: %v", err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(0)) {
		t.Errorf("the error does not name the recorded PID: %v", err)
	}
}

// kill(0, sig) means "every process in my group" to POSIX, so a liveness
// probe that hands PID 0 to Kill reports a stale lock as live and the
// operator can never start again without deleting a file nothing told them
// about. Same for the negative PIDs that name a process group.
func TestNonPositivePIDsAreNeverLive(t *testing.T) {
	for _, pid := range []int{0, -1, -os.Getpid()} {
		if processLive(pid) {
			t.Errorf("processLive(%d) = true; a group selector is not a live process", pid)
		}
	}
}

func TestThisProcessIsLive(t *testing.T) {
	if !processLive(os.Getpid()) {
		t.Error("processLive(os.Getpid()) = false -- the liveness probe cannot see the process running it")
	}
}

// A lock file this build cannot parse is still somebody's claim on the
// directory. Refusing names the file so the operator can look at it; clearing
// it would be the same guess a stale PID gets refused for.
func TestUnparseableLockIsRefusedRatherThanOverwritten(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "lock")
	if err := os.WriteFile(lock, []byte("not-a-pid"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if _, err := acquireLock(dir); err == nil {
		t.Fatal("acquireLock() took a directory whose lock file it could not read")
	} else if !strings.Contains(err.Error(), lock) {
		t.Errorf("the error does not name the lock path: %v", err)
	}

	if raw, err := os.ReadFile(lock); err != nil || string(raw) != "not-a-pid" {
		t.Errorf("lock file = %q (err %v), want it left exactly as found", raw, err)
	}
}

// Release removes this process's claim and nothing else's. A release that
// deleted whatever it found would, after an operator cleared a stale lock by
// hand and started a second console, delete the new one's lock on the way
// out and leave the directory unguarded.
func TestReleaseLeavesAnotherProcessLockAlone(t *testing.T) {
	dir := t.TempDir()
	release, err := acquireLock(dir)
	if err != nil {
		t.Fatalf("acquireLock() error = %v", err)
	}

	lock := filepath.Join(dir, "lock")
	const foreign = "999999"
	if err = os.WriteFile(lock, []byte(foreign), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	release()

	raw, err := os.ReadFile(lock)
	if err != nil {
		t.Fatalf("the lock file was removed even though it named another process: %v", err)
	}
	if string(raw) != foreign {
		t.Errorf("lock file = %q, want %q untouched", raw, foreign)
	}
}

func TestLockRecordsThisProcessPID(t *testing.T) {
	dir := t.TempDir()
	release, err := acquireLock(dir)
	if err != nil {
		t.Fatalf("acquireLock() error = %v", err)
	}
	defer release()

	raw, err := os.ReadFile(filepath.Join(dir, "lock"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if got := strings.TrimSpace(string(raw)); got != strconv.Itoa(os.Getpid()) {
		t.Errorf("lock file = %q, want this process's PID -- the diagnostic is the whole point of the contents", got)
	}
}

// prepareWorkDir is the composition Run actually calls, and the ordering
// inside it is load-bearing: a launch killed with SIGKILL leaves BOTH a
// session directory and a lock file, so a sweep that ran after the lock was
// taken would never run at all for the case that most needs it -- the
// credentials would survive every subsequent start.
func TestPrepareWorkDirSweepsEvenWhenTheLockRefuses(t *testing.T) {
	dir := t.TempDir()
	orphanSession := filepath.Join(dir, "session-0")
	if err := os.MkdirAll(orphanSession, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lock"), []byte("0"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if _, err := prepareWorkDir(dir); err == nil {
		t.Fatal("prepareWorkDir() took a directory holding a lock it could not attribute to a live process")
	}
	if _, err := os.Stat(orphanSession); !os.IsNotExist(err) {
		t.Error("the killed launch's session directory survived, so its flattened kubeconfig did too")
	}
}

func TestPrepareWorkDirCreatesTheToolDirectoriesAndTakesTheLock(t *testing.T) {
	dir := t.TempDir()
	release, err := prepareWorkDir(dir)
	if err != nil {
		t.Fatalf("prepareWorkDir() error = %v", err)
	}
	defer release()

	for _, sub := range workSubdirs {
		if _, statErr := os.Stat(filepath.Join(dir, sub)); statErr != nil {
			t.Errorf("work subdirectory %s missing: %v", sub, statErr)
		}
	}
	if _, err := prepareWorkDir(dir); err == nil {
		t.Error("a second prepareWorkDir() succeeded on a directory this process holds")
	}
}

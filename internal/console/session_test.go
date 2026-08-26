package console

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
)

func TestSessionKubeconfigIsOwnerOnlyInAnOwnerOnlyDirectory(t *testing.T) {
	work := t.TempDir()
	path, cleanup, err := writeSessionKubeconfig(work, writeKubeconfig(t), "alpha")
	if err != nil {
		t.Fatalf("writeSessionKubeconfig() error = %v", err)
	}
	defer cleanup()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("kubeconfig mode = %#o, want 0600", perm)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("Stat(dir) error = %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("session dir mode = %#o, want 0700", perm)
	}
}

func TestSessionKubeconfigBakesInTheSelectedContext(t *testing.T) {
	work := t.TempDir()
	path, cleanup, err := writeSessionKubeconfig(work, writeKubeconfig(t), "beta")
	if err != nil {
		t.Fatalf("writeSessionKubeconfig() error = %v", err)
	}
	defer cleanup()

	cfg, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatalf("LoadFromFile() error = %v", err)
	}
	if cfg.CurrentContext != "beta" {
		t.Errorf("current-context = %q, want beta -- the file alone must be a complete answer", cfg.CurrentContext)
	}
	if len(cfg.Contexts) != 1 {
		t.Errorf("minified kubeconfig has %d contexts, want 1", len(cfg.Contexts))
	}
	if _, ok := cfg.Clusters["alpha-cluster"]; ok {
		t.Error("the other context's cluster survived minification -- a stray `use-context` would still find it")
	}
}

// The session file is what every tool in the chain reads, so a context the
// source kubeconfig does not have has to fail here, by name, rather than
// producing a file that resolves to nothing three steps later inside helm.
func TestSessionKubeconfigRefusesAnUnknownContext(t *testing.T) {
	work := t.TempDir()
	_, _, err := writeSessionKubeconfig(work, writeKubeconfig(t), "gamma")
	if err == nil {
		t.Fatal("writeSessionKubeconfig() accepted a context the kubeconfig does not define")
	}
	if !strings.Contains(err.Error(), "gamma") {
		t.Errorf("error = %v, want it to name the missing context", err)
	}
}

func TestSessionDirectoryIsGoneAfterCleanup(t *testing.T) {
	work := t.TempDir()
	path, cleanup, err := writeSessionKubeconfig(work, writeKubeconfig(t), "alpha")
	if err != nil {
		t.Fatalf("writeSessionKubeconfig() error = %v", err)
	}
	cleanup()
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Error("the session directory survived cleanup -- flattened credentials would outlive the process")
	}
}

// A SIGKILL leaves the directory behind and the next launch is the only thing
// that will ever come looking. A live PID's directory is left alone: deleting
// another process's kubeconfig mid-Apply would break a running install.
func TestSweepRemovesDeadSessionsAndSparesLiveOnes(t *testing.T) {
	work := t.TempDir()
	dead := filepath.Join(work, "session-0")
	live := filepath.Join(work, "session-"+strconv.Itoa(os.Getpid()))
	for _, d := range []string{dead, live} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
	}
	sweepStaleSessions(work)
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Error("a dead session directory survived the sweep")
	}
	if _, err := os.Stat(live); err != nil {
		t.Error("the sweep removed a live process's session directory")
	}
}

// The sweep runs over the work directory the run store, bundles and the lock
// all live in, so it has to be narrow about what it deletes.
func TestSweepTouchesNothingButSessionDirectories(t *testing.T) {
	work := t.TempDir()
	keep := []string{
		filepath.Join(work, "runs"),
		filepath.Join(work, "bundles"),
		filepath.Join(work, "session-not-a-pid"),
		filepath.Join(work, "sessions-0"),
	}
	for _, d := range keep {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
	}
	lock := filepath.Join(work, "lock")
	if err := os.WriteFile(lock, []byte("0"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	sweepStaleSessions(work)

	for _, d := range append(keep, lock) {
		if _, err := os.Stat(d); err != nil {
			t.Errorf("the sweep removed %s, which is not a session directory", filepath.Base(d))
		}
	}
}

// A stale directory left by a previous launch must not survive into this one:
// it holds a flattened kubeconfig, which is to say live credentials.
func TestSweepClearsCredentialsLeftByAKilledLaunch(t *testing.T) {
	work := t.TempDir()
	orphan := filepath.Join(work, "session-0")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	secret := filepath.Join(orphan, "kubeconfig")
	if err := os.WriteFile(secret, []byte("token: super-secret"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	sweepStaleSessions(work)

	if _, err := os.Stat(secret); !os.IsNotExist(err) {
		t.Error("a killed launch's flattened kubeconfig survived the next start's sweep")
	}
}

// The steps are configured with this path at construction, before any context
// is chosen, so it must be the same path writeSessionKubeconfig later writes
// to. If those two ever diverge, Discover and Apply would each read a file
// that is never written and silently fall back to the operator's ambient
// kubeconfig -- the exact failure the pin exists to prevent.
func TestTheConfiguredSessionPathIsTheOneWritten(t *testing.T) {
	work := t.TempDir()
	predicted := sessionKubeconfigPath(work)

	written, cleanup, err := writeSessionKubeconfig(work, writeKubeconfig(t), "alpha")
	if err != nil {
		t.Fatalf("writeSessionKubeconfig() error = %v", err)
	}
	defer cleanup()

	if written != predicted {
		t.Fatalf("wrote %s, but the steps were configured with %s", written, predicted)
	}
	if _, err := os.Stat(predicted); err != nil {
		t.Errorf("nothing exists at the path the steps will read: %v", err)
	}
}

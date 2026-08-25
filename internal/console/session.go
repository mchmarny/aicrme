package console

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// sessionDirPrefix names the per-launch directories sweepStaleSessions
// recognizes. The PID after it is what makes a leftover directory
// attributable rather than merely old.
const sessionDirPrefix = "session-"

// writeSessionKubeconfig freezes the chosen context into a single-context
// kubeconfig for the life of this process.
//
// Per-launch and deleted on shutdown, not a fixed <workdir>/kubeconfig.
// Flattening inlines whatever the source context held -- a bearer token, a
// client certificate and key, a cached OIDC id_token -- so a fixed path would
// leave live credentials on disk after the process exits, indefinitely, which
// contradicts what the README tells the operator: that the binary holds their
// credentials for as long as it runs. (An exec-based context minifies to a
// stanza rather than a secret and is the benign case; a context holding a
// bearer token or a client key is not, and this cannot know which it got.)
//
// The file rather than a --context flag: it removes the question of whether
// every tool in the chain supports a context flag and spells it the same way
// (helm --kube-context, kubectl --context), and it makes the run immune to
// the operator running `kubectl config use-context` mid-Apply -- which with
// an ambient kubeconfig would silently redirect an in-flight install at the
// next helm invocation. The in-cluster console got that property free from
// its ServiceAccount.
func writeSessionKubeconfig(workDir, kubeconfig, contextName string) (string, func(), error) {
	cfg, err := loadingRules(kubeconfig).Load()
	if err != nil {
		return "", nil, fmt.Errorf("reading kubeconfig: %w", err)
	}
	// Checked here rather than left to MinifyConfig: its own error names the
	// context too, but this runs before the directory is created, so a typo
	// costs nothing and leaves nothing behind.
	if _, ok := cfg.Contexts[contextName]; !ok {
		return "", nil, fmt.Errorf("kubeconfig has no context %q", contextName)
	}
	cfg.CurrentContext = contextName

	// Minify first, then flatten: minify drops every context but the current
	// one, so flatten inlines only the credentials this session will actually
	// use rather than every certificate file the operator's kubeconfig refers
	// to.
	if err = clientcmdapi.MinifyConfig(cfg); err != nil {
		return "", nil, fmt.Errorf("reducing the kubeconfig to context %q: %w", contextName, err)
	}
	if err = clientcmdapi.FlattenConfig(cfg); err != nil {
		return "", nil, fmt.Errorf("inlining the credentials for context %q: %w", contextName, err)
	}

	dir := sessionDir(workDir, os.Getpid())
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, fmt.Errorf("creating the session directory %s: %w", dir, err)
	}
	cleanup := func() { removeSession(dir) }

	raw, err := clientcmd.Write(*cfg)
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("serializing the session kubeconfig: %w", err)
	}
	// Written by hand rather than through clientcmd.WriteToFile so the mode is
	// stated here: this file holds inlined credentials, and 0600 is the
	// property the test asserts.
	path := filepath.Join(dir, "kubeconfig")
	if err = os.WriteFile(path, raw, 0o600); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("writing the session kubeconfig %s: %w", path, err)
	}
	return path, cleanup, nil
}

// sessionState holds the session directory's cleanup between the goroutine
// that creates it and the one that shuts the console down. The connect hook
// runs on an HTTP goroutine; Run's shutdown runs on its own, after the engine
// has reaped the deploy.sh tree -- which is the last moment anything still
// reads this kubeconfig.
type sessionState struct {
	mu      sync.Mutex
	cleanup func()
}

func (s *sessionState) set(cleanup func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanup = cleanup
}

// close removes the session directory, if one was ever created. Safe to call
// on a console that never connected.
func (s *sessionState) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cleanup != nil {
		s.cleanup()
		s.cleanup = nil
	}
}

func sessionDir(workDir string, pid int) string {
	return filepath.Join(workDir, sessionDirPrefix+strconv.Itoa(pid))
}

func removeSession(dir string) {
	if err := os.RemoveAll(dir); err != nil {
		slog.Warn("could not remove the session directory; it holds a flattened kubeconfig",
			"dir", dir, "error", err)
	}
}

// sweepStaleSessions removes session directories left by launches that are no
// longer running. A SIGKILL leaves the directory behind and the next launch is
// the only thing that will ever come looking, so this runs at startup.
//
// A live PID's directory is spared: deleting another aicrme's kubeconfig
// mid-Apply would break a running install, and the work-directory lock does
// not cover this -- an operator running a second console against a different
// --work-dir is legitimate, and its session directory is not in this one, but
// the check costs nothing and the failure mode it prevents is severe.
//
// Never fatal. A directory that cannot be removed is a leftover credential
// file worth warning about, not a reason to refuse to start.
func sweepStaleSessions(workDir string) {
	entries, err := os.ReadDir(workDir)
	if err != nil {
		slog.Warn("could not scan the work directory for stale sessions", "dir", workDir, "error", err)
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), sessionDirPrefix) {
			continue
		}
		// An unparseable suffix is left alone. It was not written by this
		// code, so nothing here knows what it is or who owns it.
		pid, err := strconv.Atoi(strings.TrimPrefix(e.Name(), sessionDirPrefix))
		if err != nil || processLive(pid) {
			continue
		}
		dir := filepath.Join(workDir, e.Name())
		slog.Info("removing a session directory left by a previous launch", "dir", dir, "pid", pid)
		removeSession(dir)
	}
}

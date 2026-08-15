package applier

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// safeBuffer is a concurrency-safe io.Writer. BashExec.Run's out parameter
// must tolerate concurrent writers in general -- os/exec only collapses
// stdout and stderr onto a single copy goroutine when they are the
// identical writer value (interfaceEqual in os/exec's childStderr), which is
// BashExec's wiring today but not a contract these tests should assume.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestBashExecRunHonorsDirAndMergesEnv proves cmd.Dir is honored and that
// Spec.Env is APPENDED to the inherited environment rather than replacing
// it. That distinction is the whole point of the test: deploy.sh and every
// install.sh it invokes depend on inherited variables -- PATH to find helm
// and kubectl, plus the six work-dir variables the chart sets (TMPDIR,
// HOME, HELM_CACHE_HOME, HELM_CONFIG_HOME, HELM_DATA_HOME, KUBECACHEDIR).
// If cmd.Env ever regressed from append to replace, every one of those
// would vanish and the failure would show up on a real cluster as a
// baffling helm error, not as a test failure -- so this test plants a
// variable in the parent (t.Setenv) that Spec.Env never mentions, and
// requires the child to see BOTH it and the injected one. Asserting only
// the injected variable would pass under a replace just as well as an
// append, since Spec.Env always contains it either way.
//
// t.Setenv forbids t.Parallel(), which is fine: nothing in this file uses
// it.
func TestBashExecRunHonorsDirAndMergesEnv(t *testing.T) {
	t.Setenv("AICRME_INHERITED_MARKER", "from-parent")

	dir := t.TempDir()
	out := &safeBuffer{}
	spec := Spec{
		Dir:  dir,
		Argv: []string{"sh", "-c", `pwd; echo "$AICRME_TEST_VAR"; echo "$AICRME_INHERITED_MARKER"`},
		Env:  []string{"AICRME_TEST_VAR=merged"},
	}

	var e BashExec
	if err := e.Run(context.Background(), spec, out); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("output = %q, want 3 lines (pwd, injected var, inherited var)", out.String())
	}

	// Compare via EvalSymlinks: t.TempDir() can return a path through a
	// symlink (e.g. macOS's /var -> /private/var), and a shell's builtin
	// pwd may report the resolved physical path.
	wantDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q) error = %v", dir, err)
	}
	gotDir, err := filepath.EvalSymlinks(lines[0])
	if err != nil {
		t.Fatalf("EvalSymlinks(%q) error = %v", lines[0], err)
	}
	if gotDir != wantDir {
		t.Errorf("pwd = %q, want %q -- Spec.Dir not honored", lines[0], dir)
	}
	if lines[1] != "merged" {
		t.Errorf("$AICRME_TEST_VAR = %q, want %q -- Spec.Env was not passed to the child", lines[1], "merged")
	}
	if lines[2] != "from-parent" {
		t.Errorf("$AICRME_INHERITED_MARKER = %q, want %q -- the parent's environment was not inherited (cmd.Env replaced os.Environ() instead of appending to it)", lines[2], "from-parent")
	}
}

// TestBashExecRunMergesStdoutAndStderr proves both streams reach the same
// writer, which is the property that preserves output ordering between
// helm's stdout narration and stderr diagnostics.
func TestBashExecRunMergesStdoutAndStderr(t *testing.T) {
	out := &safeBuffer{}
	spec := Spec{Argv: []string{"sh", "-c", "echo out; echo err >&2"}}

	var e BashExec
	if err := e.Run(context.Background(), spec, out); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "out") {
		t.Errorf("output = %q, missing stdout line", got)
	}
	if !strings.Contains(got, "err") {
		t.Errorf("output = %q, missing stderr line -- Stdout and Stderr are not merged", got)
	}
}

// TestBashExecRunCancelReturnsPromptly proves cancellation ends a running
// process well short of its own sleep, rather than blocking until it exits
// on its own. The bound is deliberately loose -- promptness relative to the
// 30s sleep, not a tight wall-clock figure -- so this can't flake on a
// loaded CI box.
func TestBashExecRunCancelReturnsPromptly(t *testing.T) {
	out := &safeBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	spec := Spec{Argv: []string{"sh", "-c", "sleep 30"}}

	errCh := make(chan error, 1)
	go func() {
		var e BashExec
		errCh <- e.Run(ctx, spec, out)
	}()

	time.Sleep(100 * time.Millisecond) // give the process a moment to start
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("Run() error = nil, want an error after cancellation")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run() did not return within 20s of cancellation, want well under the 30s sleep")
	}
}

// TestBashExecRunCancelKillsGrandchild proves cancellation reaches the whole
// process tree, not just the direct child. TestBashExecRunCancelReturnsPromptly
// above uses a leaf process (sh -c 'sleep 30') and would pass even if the
// signal only reached bash while a descendant survived -- it structurally
// cannot observe that class of bug. This test spawns a child that itself
// backgrounds a grandchild and prints the grandchild's PID, then asserts
// the grandchild is gone after cancellation, not merely that Run returned.
//
// The grandchild sleeps far longer than any deadline below it deliberately:
// an orphaned grandchild inherits the same stdout/stderr pipe as its
// parent, so an unsignaled grandchild can also stall Run's own return
// (os/exec's copy goroutines block until the pipe's last writer closes it).
// If the grandchild's own sleep were short enough to land inside the
// waitForProcessExit deadline by coincidence, a still-broken build could
// pass this test by dumb luck -- it did, once, during development, against
// the very single-process form this test exists to catch. A lifetime far
// past killGrace plus polling slack makes the natural-death race
// impossible rather than merely unlikely.
func TestBashExecRunCancelKillsGrandchild(t *testing.T) {
	out := &safeBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	spec := Spec{Argv: []string{"sh", "-c", `sh -c 'sleep 120' & echo $!; wait`}}

	errCh := make(chan error, 1)
	go func() {
		var e BashExec
		errCh <- e.Run(ctx, spec, out)
	}()

	pid := waitForGrandchildPID(t, out)
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("Run() error = nil, want an error after cancellation")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run() did not return within 20s of cancellation")
	}

	waitForProcessExit(t, pid)
}

// waitForGrandchildPID polls out for the PID the spec's shell script prints
// before it blocks in `wait`, so the caller can cancel only once the
// grandchild actually exists to be killed.
func waitForGrandchildPID(t *testing.T, out *safeBuffer) int {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if line := strings.TrimSpace(out.String()); line != "" {
			pid, err := strconv.Atoi(strings.SplitN(line, "\n", 2)[0])
			if err == nil {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("did not observe grandchild PID within 5s, output so far = %q", out.String())
	return 0
}

// waitForProcessExit polls pid with the standard existence probe --
// syscall.Kill(pid, 0) delivers no signal, it only reports whether pid is
// still addressable -- until it reports ESRCH (gone) or a deadline elapses.
// The deadline is generous relative to killGrace so a loaded CI box cannot
// flake this into a false failure; the assertion is eventual disappearance,
// not exact timing.
func waitForProcessExit(t *testing.T, pid int) {
	t.Helper()

	deadline := time.Now().Add(killGrace + 10*time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("grandchild pid %d still alive %s after cancellation", pid, killGrace+10*time.Second)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestLineWriterStripsCarriageReturns covers the \r branch in Write, which
// exists because deploy.sh's terminal progress redraws (e.g. from helm)
// arrive as CRLF and must not split lines or leak \r into a parsed marker.
func TestLineWriterStripsCarriageReturns(t *testing.T) {
	var got []string
	w := newLineWriter(func(s string) { got = append(got, s) })

	if _, err := w.Write([]byte("hello\r\nworld\r\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	want := []string{"hello", "world"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("lines = %v, want %v", got, want)
	}
}

// TestLineWriterFlushesOnMaxLineBytes covers the overflow-flush branch: a
// line that never sees a newline before crossing maxLineBytes is flushed as
// its own line rather than growing the buffer without bound.
func TestLineWriterFlushesOnMaxLineBytes(t *testing.T) {
	var got []string
	w := newLineWriter(func(s string) { got = append(got, s) })

	line := strings.Repeat("a", maxLineBytes)
	if _, err := w.Write([]byte(line)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("emitted %d lines, want 1 (the overflow flush)", len(got))
	}
	if len(got[0]) != maxLineBytes {
		t.Errorf("len(line) = %d, want %d", len(got[0]), maxLineBytes)
	}

	// Writing more after the overflow flush must start a fresh buffer, not
	// silently extend the line that was just emitted.
	if _, err := w.Write([]byte("tail")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	w.Flush()
	if len(got) != 2 || got[1] != "tail" {
		t.Errorf("lines = %v, want a second line %q", got, "tail")
	}
}

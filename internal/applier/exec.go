package applier

import (
	"context"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// maxLineBytes caps how much is buffered for a single unterminated line, so
// a binary blob on stdout cannot grow the buffer without bound. Anything
// past the cap is flushed as its own line.
const maxLineBytes = 64 << 10

// killGrace is how long a canceled deploy.sh's process tree has before the
// group is SIGKILLed.
//
// What needs those ten seconds is helm, not deploy.sh's own INT/TERM trap:
// that trap is `rm -rf "${HELM_WORKDIR}"; exit 130`, an instantaneous
// directory removal that would be satisfied by a fraction of a second. Helm
// handles SIGTERM itself and marks the release failed; SIGKILLed instead it
// strands the release in pending-install, which blocks the next
// `helm upgrade --install` until someone runs `helm rollback` by hand. Do
// not shrink this on the reasoning that an `rm -rf` cannot need ten seconds.
//
// It is a var, not a const, solely so tests can shrink it to exercise the
// SIGKILL escalation path without paying its full cost on every run; no
// non-test code ever assigns to it, and internal/applier has no
// t.Parallel() anywhere, so a test that lowers it and restores it via
// t.Cleanup cannot race a sibling test.
var killGrace = 10 * time.Second

// Spec is one process invocation. Env carries only the variables to ADD to
// the inherited environment, which keeps the golden assertions in
// applier_test.go readable -- BashExec appends them to os.Environ(), and
// os/exec resolves duplicate keys in favor of the last occurrence.
type Spec struct {
	Dir  string
	Argv []string
	Env  []string
}

// Exec runs one process, streaming its merged stdout and stderr to out.
// The single seam between the applier and the operating system: tests
// substitute a fake that writes a captured transcript.
type Exec interface {
	Run(ctx context.Context, spec Spec, out io.Writer) error
}

// BashExec runs the real process.
type BashExec struct{}

// Run streams merged stdout and stderr to out. deploy.sh spawns install.sh,
// which spawns `helm upgrade --wait`, which spawns kubectl -- so on context
// cancellation Run signals that whole process group, not just the bash
// process os/exec started directly. It sends SIGTERM rather than SIGKILL so
// helm can handle it (and deploy.sh's trap can drop its temp workdir) before
// anything is force-killed, and only escalates to a group SIGKILL after
// killGrace, and only if the tree is still alive.
//
// Stdout and Stderr are set to the identical writer value, so os/exec's
// childStderr (interfaceEqual(c.Stderr, c.Stdout)) reuses Stdout's pipe for
// Stderr too: in this wiring there is exactly one pipe and one copy
// goroutine, not two. out must still be safe for concurrent use regardless
// -- that single-goroutine behavior falls out of Stdout and Stderr pointing
// at the same comparable value today, not a guarantee this method enforces,
// so a future split into two distinct writers would silently regain two
// concurrent copies. lineWriter is safe either way.
func (BashExec) Run(ctx context.Context, spec Spec, out io.Writer) error {
	//nolint:gosec // running a caller-supplied argv is this type's entire purpose -- Spec.Argv is built by Applier.Apply, not user input
	cmd := exec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = append(os.Environ(), spec.Env...)
	cmd.Stdout = out
	cmd.Stderr = out

	// Setpgid makes the child the leader of a new process group whose pgid
	// equals its own pid, so signaling -pid below reaches deploy.sh's
	// entire descendant tree instead of just the bash process os/exec
	// started. Unix-only -- this project ships a Linux container and CI is
	// Linux; local development is macOS, which has the same field, so no
	// build-tag split is needed to keep `go build ./...` honest.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// grace is read from the package var exactly once, synchronously, here
	// -- before cmd.Cancel exists for the ctx-watching goroutine to invoke
	// and before any watchdog goroutine below can exist to read it. Every
	// later use in this call (the watchdog's timer, WaitDelay) reads this
	// local copy instead of re-reading killGrace. killGrace is mutable
	// (tests shrink it to exercise escalation cheaply) and this is what
	// keeps that mutation race-free: nothing spawned by this call ever
	// touches the package var itself, only a value that was already fixed
	// before any of them existed.
	grace := killGrace

	// reaped closes once cmd.Run has returned, i.e. once the group leader
	// has been reaped and its pgid is free for the kernel to reuse.
	reaped := make(chan struct{})
	defer close(reaped)

	cmd.Cancel = func() error {
		pid := cmd.Process.Pid

		// cmd.WaitDelay escalates against cmd.Process alone, which by the
		// time it fires has already exited -- it cannot reach the group. So
		// Run runs its own escalation, racing grace against reaped rather
		// than issuing the kill after cmd.Run has already returned. The
		// alternative -- killing -pid only after Wait reaps the leader --
		// is simpler but leaves a real window in which the pgid has already
		// been recycled by an unrelated process group; racing the timer
		// against reaped instead means the escalation only fires while the
		// leader is (as far as this goroutine can tell) still unreaped,
		// which shrinks that hazard to the scheduling gap between the
		// kernel's reap and this goroutine observing it -- about as tight
		// as it gets without a pidfd.
		go func() {
			select {
			case <-time.After(grace):
				_ = syscall.Kill(-pid, syscall.SIGKILL)
			case <-reaped:
			}
		}()

		return syscall.Kill(-pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = grace

	return cmd.Run()
}

// lineWriter splits everything written to it into lines and invokes fn once
// per complete line. fn is called while holding the mutex, which lets
// callers keep unsynchronized state inside fn. Write must tolerate
// concurrent callers in general -- os/exec runs two copy goroutines
// whenever Stdout and Stderr are different writers -- so lineWriter does
// not assume single-writer use even though BashExec happens to wire Stdout
// and Stderr to the same value today.
type lineWriter struct {
	mu  sync.Mutex
	buf []byte
	fn  func(string)
}

func newLineWriter(fn func(string)) *lineWriter {
	return &lineWriter{fn: fn}
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	for _, b := range p {
		if b == '\n' {
			w.emitLocked()
			continue
		}
		if b == '\r' {
			continue
		}
		w.buf = append(w.buf, b)
		if len(w.buf) >= maxLineBytes {
			w.emitLocked()
		}
	}
	return len(p), nil
}

// Flush emits any trailing line that was never newline-terminated.
func (w *lineWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.emitLocked()
}

func (w *lineWriter) emitLocked() {
	if len(w.buf) == 0 {
		return
	}
	w.fn(string(w.buf))
	w.buf = w.buf[:0]
}

// ring retains the last n strings in O(1) per add.
type ring struct {
	items []string
	head  int
	count int
}

func newRing(n int) *ring { return &ring{items: make([]string, n)} }

func (r *ring) add(s string) {
	if r.count < len(r.items) {
		r.items[(r.head+r.count)%len(r.items)] = s
		r.count++
		return
	}
	r.items[r.head] = s
	r.head = (r.head + 1) % len(r.items)
}

func (r *ring) lines() []string {
	out := make([]string, 0, r.count)
	for i := 0; i < r.count; i++ {
		out = append(out, r.items[(r.head+i)%len(r.items)])
	}
	return out
}

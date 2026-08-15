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

// killGrace is how long a canceled deploy.sh has to run its own INT/TERM
// trap (which removes the helm temp workdir) before the process is killed.
const killGrace = 10 * time.Second

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

// Run streams merged stdout and stderr to out. On context cancellation it
// sends SIGTERM rather than SIGKILL so deploy.sh's own trap can remove the
// temp workdir it created, and only escalates after killGrace.
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
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = killGrace
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

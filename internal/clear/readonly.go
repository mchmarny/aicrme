package clear

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// ErrNotReadOnly is returned for any command outside the survey's whitelist.
var ErrNotReadOnly = errors.New("clear: command is not read-only")

// maxStderrCapture bounds the stderr buffer so a chatty or looping command
// cannot grow it without limit. Failures are reported as diagnostics, not
// logs, so this stops at the end of the message where the error is.
const maxStderrCapture = 4 << 10 // 4 KB

// readOnlyCommands is every command the survey is permitted to run, as
// "<binary> <subcommand>".
//
// A WHITELIST, NOT A BLACKLIST, and in production code rather than only in a
// test. Revision 1 of this plan asserted read-only with a test-side blacklist
// that missed `helm install` and `helm upgrade` -- the two commands a future
// edit is most likely to reach for. A whitelist cannot be defeated by
// forgetting to update it: an unlisted command fails closed.
//
// Widening this set is a design decision, not a maintenance chore. Increment 1
// deletes nothing, and every entry here is a read.
var readOnlyCommands = map[string]bool{
	"helm list":    true,
	"helm history": true,
	"kubectl get":  true,
}

type readOnly struct{ inner Exec }

// ReadOnly wraps e so that anything outside readOnlyCommands fails closed
// before reaching the operating system.
func ReadOnly(e Exec) Exec { return readOnly{inner: e} }

func (r readOnly) Run(ctx context.Context, argv []string, out io.Writer) error {
	if len(argv) < 2 {
		return fmt.Errorf("%w: %q cannot be classified", ErrNotReadOnly, strings.Join(argv, " "))
	}
	key := argv[0] + " " + argv[1]
	if !readOnlyCommands[key] {
		return fmt.Errorf("%w: %q", ErrNotReadOnly, key)
	}
	return r.inner.Run(ctx, argv, out)
}

// BashExec runs the real command, streaming stdout to out.
//
// Mirrors teardown.BashExec rather than importing it, for the reason the Exec
// seam is also duplicated: these two packages must be able to diverge on
// process handling without one silently changing the other. Stdout and stderr
// are kept separate because every caller here parses stdout as JSON and a helm
// warning on stderr would corrupt it. Stderr is captured instead so a failure
// carries a readable diagnostic: a command that exits non-zero returns an
// error that wraps its stderr, bounded to prevent unbounded growth.
type BashExec struct{}

func (BashExec) Run(ctx context.Context, argv []string, out io.Writer) error {
	if len(argv) == 0 {
		return errors.New("clear: empty argv")
	}
	//nolint:gosec // argv is built in this package from constants, never from user input
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout = out

	stderrBuf := &bytes.Buffer{}
	cmd.Stderr = &boundedWriter{buf: stderrBuf, max: maxStderrCapture}

	if err := cmd.Run(); err != nil {
		stderr := stderrBuf.String()
		if stderr != "" {
			return fmt.Errorf("%w: %s: %s", err, argv[0], stderr)
		}
		return err
	}
	return nil
}

// boundedWriter caps stderr at maxStderrCapture bytes, keeping only the tail so
// the error message is readable even if the command is chatty.
type boundedWriter struct {
	buf *bytes.Buffer
	max int
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	n, err := w.buf.Write(p)
	if w.buf.Len() > w.max {
		// Keep only the last max bytes
		content := w.buf.Bytes()
		excess := len(content) - w.max
		w.buf.Reset()
		w.buf.Write(content[excess:])
	}
	return n, err
}

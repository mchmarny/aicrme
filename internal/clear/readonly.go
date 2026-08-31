package clear

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// ErrNotReadOnly is returned for any command outside the survey's whitelist.
var ErrNotReadOnly = errors.New("clear: command is not read-only")

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
// process handling without one silently changing the other. Stderr is
// discarded because every caller here parses stdout as JSON and a helm warning
// on stderr would corrupt it.
type BashExec struct{}

func (BashExec) Run(ctx context.Context, argv []string, out io.Writer) error {
	if len(argv) == 0 {
		return errors.New("clear: empty argv")
	}
	//nolint:gosec // argv is built in this package from constants, never from user input
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout = out
	return cmd.Run()
}

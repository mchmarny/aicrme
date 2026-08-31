// Package clear surveys a cluster for AICR components that no run owns.
//
// It exists for the cluster internal/teardown cannot help with. Teardown
// removes what ONE RUN installed, decided against ownership evidence recorded
// immediately before Apply; with no run there is no evidence and every release
// is unprovable. This package answers the different question: what is here,
// and how sure is this console about who put it here.
//
// Increment 1 is read-only, and ReadOnly enforces that rather than trusting
// it. Every Exec handed to a Surveyor is wrapped.
package clear

import (
	"context"
	"io"
)

// Exec runs one command, streaming its stdout to out.
//
// Deliberately identical to teardown.Exec and deliberately not shared with it.
// The two packages have opposite ownership contracts -- teardown may only act
// on what a run proved it created, this one has no such proof -- and a shared
// helper is where those merge without anyone noticing.
type Exec interface {
	Run(ctx context.Context, argv []string, out io.Writer) error
}

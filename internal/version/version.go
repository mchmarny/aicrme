// Package version carries build-time identity injected via ldflags.
package version

import "fmt"

var (
	// Version is the semantic version, overridden at build time.
	Version = "dev"
	// Commit is the git SHA, overridden at build time.
	Commit = "none"
)

// String returns the human-readable build identity.
func String() string {
	return fmt.Sprintf("%s (%s)", Version, Commit)
}

// Package version carries build-time identity injected via ldflags.
package version

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sync"
)

var (
	// Version is the semantic version, overridden at build time.
	Version = "dev"
	// Commit is the git SHA, overridden at build time.
	Commit = "none"
	// Date is when the source was committed, NOT when the binary was built.
	//
	// Deliberate: .goreleaser.yaml sets mod_timestamp to the commit timestamp
	// so the same tag rebuilds bit-for-bit, and stamping a wall-clock build
	// time here would break that -- two builds of one tag would differ. The
	// commit date still answers "how old is this", which is the question being
	// asked.
	Date = "unknown"
)

// String returns the human-readable build identity.
func String() string {
	return fmt.Sprintf("%s (%s)", Version, Commit)
}

var (
	digestOnce sync.Once
	digest     string
)

// Digest is the sha256 of the running binary, or "" when it cannot be read.
//
// Computed at runtime rather than injected, because it cannot be injected: a
// build-time constant would have to contain the hash of the artifact being
// built, and goreleaser computes checksums only after the binary exists. Self
// hashing is the only way to answer the question an operator actually has --
// "is the process I am watching the archive I verified with gh attestation
// verify" -- which is otherwise a gap between checking a download and running
// it.
//
// Failure is not an error. A binary whose own path is unreadable still works,
// and refusing to start over a missing identity string would be a poor trade.
func Digest() string {
	digestOnce.Do(func() {
		path, err := os.Executable()
		if err != nil {
			return
		}
		f, err := os.Open(path)
		if err != nil {
			return
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return
		}
		digest = hex.EncodeToString(h.Sum(nil))
	})
	return digest
}

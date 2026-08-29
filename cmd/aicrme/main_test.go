package main

import (
	"strings"
	"testing"

	"github.com/mchmarny/aicrme/internal/version"
)

// The Homebrew formula's `test` block runs `aicrme --version` and matches on
// "aicrme". A release whose binary does not print that fails `brew test` after
// the formula is already published, which is the expensive place to find out.
func TestPrintVersionNamesTheProgramAndTheBuild(t *testing.T) {
	var out strings.Builder
	printVersion(&out)

	got := out.String()
	if !strings.HasPrefix(got, "aicrme ") {
		t.Errorf("printVersion() = %q, want it to start with the program name", got)
	}
	if !strings.Contains(got, version.String()) {
		t.Errorf("printVersion() = %q, want it to carry the build identity %q", got, version.String())
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("printVersion() = %q, want a trailing newline", got)
	}
	// One line: the value of this output is that it can be pasted whole into a
	// bug report without editing.
	if strings.Count(got, "\n") != 1 {
		t.Errorf("printVersion() = %q, want exactly one line", got)
	}
}

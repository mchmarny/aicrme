package observer

import (
	"os"
	"testing"
)

// TestMain turns the condition debounce off for this package's tests.
//
// The window exists to keep self-healing trouble off an operator's timeline,
// and it is two seconds long. Nearly every test in this package asserts that a
// handler published something; with the window on, each would have to wait out
// those two seconds, and the suite would go from under a minute to several.
// debounce_test.go builds its own debouncers with explicit delays, so the
// behavior this disables is still covered.
func TestMain(m *testing.M) {
	defaultDebounce = 0
	os.Exit(m.Run())
}

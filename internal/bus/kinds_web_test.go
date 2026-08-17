package bus_test

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Every other bus.Kind is pinned incidentally by web/src/fixtures/kwok-run.json,
// a stream recorded from a real run: rename one and the SPA's fixture-driven
// tests notice. KindRecovered has no such fixture -- no recorded stream
// contains a pod restart -- so its Go constant and useEvents.ts's 'recovered'
// literal are hand-written on both sides of a wire with nothing joining them.
// Renaming either leaves both suites green and breaks production, which is
// the same class of cross-language drift test/chart/contract.sh pins for
// AICRME_DEPLOYMENT_NAME.
//
// Neither side is enumerated here: the constants are read out of event.go and
// the union out of useEvents.ts, so adding a Kind cannot leave this test
// asserting a stale list.
const (
	kindSourceGo = "event.go"
	kindSourceTS = "../../web/src/useEvents.ts"
)

// goKindRe matches the string literal of every `KindX Kind = "x"` constant.
// The FindAllStringSubmatch call below fails the test outright on zero
// matches rather than reporting "all kinds present": a regex that has drifted
// from the source would otherwise make this whole file pass vacuously, which
// is the failure mode it exists to prevent.
var goKindRe = regexp.MustCompile(`\bKind\w+\s+Kind\s*=\s*"([a-z]+)"`)

// tsKindUnionRe captures the whole right-hand side of AicrEvent's `kind:`
// union, e.g. `'phase' | 'log' | ...`.
var tsKindUnionRe = regexp.MustCompile(`\n\s*kind:\s*([^\n]+)`)

// tsKindRe pulls each quoted member out of that union.
var tsKindRe = regexp.MustCompile(`'([a-z]+)'`)

func readOrFail(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(raw)
}

func goKinds(t *testing.T) []string {
	t.Helper()
	matches := goKindRe.FindAllStringSubmatch(readOrFail(t, kindSourceGo), -1)
	if len(matches) == 0 {
		t.Fatalf("no `KindX Kind = \"...\"` constants found in %s -- the regex has drifted from the source, so this test would pass vacuously", kindSourceGo)
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

func tsKinds(t *testing.T) []string {
	t.Helper()
	union := tsKindUnionRe.FindStringSubmatch(readOrFail(t, kindSourceTS))
	if union == nil {
		t.Fatalf("no `kind:` union found in %s -- the regex has drifted from the source", kindSourceTS)
	}
	matches := tsKindRe.FindAllStringSubmatch(union[1], -1)
	if len(matches) == 0 {
		t.Fatalf("`kind:` union in %s has no quoted members: %q", kindSourceTS, union[1])
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

func TestEveryBusKindIsInTheSPAUnion(t *testing.T) {
	got, want := tsKinds(t), goKinds(t)
	inTS := make(map[string]bool, len(got))
	for _, k := range got {
		inTS[k] = true
	}
	for _, k := range want {
		if !inTS[k] {
			t.Errorf("bus.Kind %q is not in %s's AicrEvent union (%s) -- the server publishes an event kind the SPA's type does not admit",
				k, kindSourceTS, strings.Join(got, ", "))
		}
	}
}

func TestTheSPAUnionHasNoKindTheBusDoesNotPublish(t *testing.T) {
	got, want := tsKinds(t), goKinds(t)
	inGo := make(map[string]bool, len(want))
	for _, k := range want {
		inGo[k] = true
	}
	for _, k := range got {
		if !inGo[k] {
			t.Errorf("%s's AicrEvent union admits %q but no bus.Kind produces it (%s) -- a rename left the SPA keying off a literal the server stopped sending",
				kindSourceTS, k, strings.Join(want, ", "))
		}
	}
}

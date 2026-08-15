package applier

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/mchmarny/aicrme/internal/bus"
)

// fakeExec writes a canned transcript to out, then returns err -- the seam
// that lets every applier test run with no process and no cluster.
type fakeExec struct {
	transcript string
	err        error

	mu       sync.Mutex
	lastSpec Spec
	calls    int
}

func (f *fakeExec) Run(_ context.Context, spec Spec, out io.Writer) error {
	f.mu.Lock()
	f.lastSpec = spec
	f.calls++
	f.mu.Unlock()
	if _, err := io.WriteString(out, f.transcript); err != nil {
		return err
	}
	return f.err
}

func collect() (func(bus.Event), *[]bus.Event) {
	var mu sync.Mutex
	events := []bus.Event{}
	return func(e bus.Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e)
	}, &events
}

func TestApplyInvokesDeployScriptWithTheRightSpec(t *testing.T) {
	fake := &fakeExec{transcript: "✓ Pre-flight checks passed\n"}
	emit, _ := collect()

	err := New(fake).Apply(context.Background(), Options{BundleDir: "/bundle", Retries: 5}, emit)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	if fake.lastSpec.Dir != "/bundle" {
		t.Errorf("Dir = %q, want /bundle", fake.lastSpec.Dir)
	}
	wantArgv := []string{"bash", "deploy.sh", "--retries", "5"}
	if strings.Join(fake.lastSpec.Argv, " ") != strings.Join(wantArgv, " ") {
		t.Errorf("Argv = %v, want %v", fake.lastSpec.Argv, wantArgv)
	}
	env := strings.Join(fake.lastSpec.Env, "\n")
	for _, want := range []string{"NO_COLOR=1", "DRY_RUN_FLAG=", "KUBECONFIG_FLAG=", "HELM_DEBUG_FLAG="} {
		if !strings.Contains(env, want) {
			t.Errorf("Env missing %q, got %v", want, fake.lastSpec.Env)
		}
	}
	// --best-effort is deliberately absent: a half-installed platform that
	// reports success turns an applier failure into a confusing Validate
	// failure one phase later.
	if strings.Contains(strings.Join(fake.lastSpec.Argv, " "), "--best-effort") {
		t.Error("Argv contains --best-effort, want fail-fast")
	}
}

func TestApplyDryRunSetsTheFlag(t *testing.T) {
	fake := &fakeExec{}
	emit, _ := collect()

	if err := New(fake).Apply(context.Background(), Options{BundleDir: "/b", Retries: 0, DryRun: true}, emit); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if !strings.Contains(strings.Join(fake.lastSpec.Env, "\n"), "DRY_RUN_FLAG=--dry-run") {
		t.Errorf("Env = %v, want DRY_RUN_FLAG=--dry-run", fake.lastSpec.Env)
	}
}

func TestApplyPublishesMarkersAndNothingElse(t *testing.T) {
	fake := &fakeExec{transcript: strings.Join([]string{
		"✓ Pre-flight checks passed",
		"┌─ [1/2] cert-manager  →  cert-manager",
		`Release "cert-manager" does not exist. Installing it now.`,
		"NAME: cert-manager",
		"└─ ✓ cert-manager installed",
		"",
	}, "\n")}
	emit, events := collect()

	if err := New(fake).Apply(context.Background(), Options{BundleDir: "/b"}, emit); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if len(*events) != 3 {
		for _, e := range *events {
			t.Logf("event: %+v", e)
		}
		t.Fatalf("published %d events, want 3 -- helm output must not reach the bus", len(*events))
	}
}

func TestApplyPublishesFailureWithComponentAndTail(t *testing.T) {
	fake := &fakeExec{
		transcript: strings.Join([]string{
			"┌─ [2/3] kai-scheduler  →  kai-scheduler",
			"  --- Failed hook Job kai-scheduler-crd-upgrader diagnostics ---",
			"Error: INSTALLATION FAILED: timed out waiting for the condition",
			"└─ ✗ kai-scheduler FAILED (after 2 attempts)",
			"",
		}, "\n"),
		err: errors.New("exit status 1"),
	}
	emit, events := collect()

	err := New(fake).Apply(context.Background(), Options{BundleDir: "/b"}, emit)
	if err == nil {
		t.Fatal("Apply() error = nil, want the exec failure propagated")
	}

	last := (*events)[len(*events)-1]
	if last.Kind != bus.KindError || last.Level != bus.LevelError {
		t.Fatalf("last event = %+v, want a KindError at LevelError", last)
	}
	var d FailureData
	if uerr := json.Unmarshal(last.Data, &d); uerr != nil {
		t.Fatalf("unmarshal FailureData error = %v", uerr)
	}
	if d.Component != "kai-scheduler" {
		t.Errorf("Component = %q, want kai-scheduler", d.Component)
	}
	if !strings.Contains(strings.Join(d.Tail, "\n"), "Failed hook Job") {
		t.Errorf("Tail = %v, want deploy.sh's own diagnostics captured", d.Tail)
	}
}

// The tail is a bounded ring: a 20-minute real install emits far more
// output than any failure screen should carry.
func TestApplyTailIsBounded(t *testing.T) {
	var b strings.Builder
	for i := 0; i < tailLines*3; i++ {
		b.WriteString("noise line\n")
	}
	fake := &fakeExec{transcript: b.String(), err: errors.New("exit status 1")}
	emit, events := collect()

	_ = New(fake).Apply(context.Background(), Options{BundleDir: "/b"}, emit)

	var d FailureData
	if err := json.Unmarshal((*events)[len(*events)-1].Data, &d); err != nil {
		t.Fatalf("unmarshal error = %v", err)
	}
	if len(d.Tail) != tailLines {
		t.Errorf("len(Tail) = %d, want %d", len(d.Tail), tailLines)
	}
}

// A final line with no trailing newline must still parse -- deploy.sh's
// last line is not guaranteed to be newline-terminated when it dies.
func TestApplyFlushesAnUnterminatedFinalLine(t *testing.T) {
	fake := &fakeExec{transcript: "└─ ✓ cert-manager installed"}
	emit, events := collect()

	if err := New(fake).Apply(context.Background(), Options{BundleDir: "/b"}, emit); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if len(*events) != 1 {
		t.Fatalf("published %d events, want 1", len(*events))
	}
}

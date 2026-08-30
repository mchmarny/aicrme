package steps_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/steps"
)

// record replays one AICR log line as slog delivers it.
func record(msg string, kv ...any) slog.Record {
	r := slog.NewRecord(time.Now(), slog.LevelInfo, msg, 0)
	r.Add(kv...)
	return r
}

func handle(t *testing.T, h slog.Handler, r slog.Record) {
	t.Helper()
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
}

// THE CONTRACT THIS FILE EXISTS FOR. These three messages and three attribute
// keys are AICR's, copied from its output on real hardware 2026-08-30:
//
//	msg="running validation phase" phase=deployment catalog=5 selected=5
//	msg="running validator"        name=operator-health phase=deployment
//	msg="validator completed"      name=operator-health status=passed
//
// Nothing in the compiler checks them. If an AICR upgrade rewords any of it,
// the console silently stops narrating validation and looks exactly like the
// eight minutes of silence this handler was built to end -- so the strings are
// asserted here, where a bump turns that into a test failure instead.
func TestProgressHandlerMatchesAICRsMessages(t *testing.T) {
	var got []bus.Event
	h := steps.NewProgressHandler(slog.NewTextHandler(&bytes.Buffer{}, nil))
	defer h.Attach(func(e bus.Event) { got = append(got, e) })()

	handle(t, h, record("running validation phase", "phase", "deployment", "catalog", 5, "selected", 5))
	handle(t, h, record("running validator", "name", "operator-health", "phase", "deployment"))
	handle(t, h, record("validator completed", "name", "operator-health", "status", "passed"))

	if len(got) != 3 {
		t.Fatalf("published %d events, want 3 -- AICR's message wording has changed: %+v", len(got), got)
	}
	if !strings.Contains(got[0].Message, "5") {
		t.Errorf("phase event = %q, want the check count in it", got[0].Message)
	}
	if !strings.Contains(got[1].Message, "operator-health") {
		t.Errorf("start event = %q, want the check name in it", got[1].Message)
	}
	if !strings.Contains(got[2].Message, "operator-health") || !strings.Contains(got[2].Message, "passed") {
		t.Errorf("done event = %q, want the check name and its status", got[2].Message)
	}
	if got[2].Level == bus.LevelWarn {
		t.Error("a passing check published at warn; only failures earn that")
	}
}

// A failing check is a finding. Publishing it in the same ink as routine
// narration is how it gets scrolled past.
func TestProgressHandlerPublishesAFailedCheckAtWarn(t *testing.T) {
	var got []bus.Event
	h := steps.NewProgressHandler(slog.NewTextHandler(&bytes.Buffer{}, nil))
	defer h.Attach(func(e bus.Event) { got = append(got, e) })()

	handle(t, h, record("validator completed", "name", "expected-resources", "status", "failed"))

	if len(got) != 1 {
		t.Fatalf("published %d events, want 1", len(got))
	}
	if got[0].Level != bus.LevelWarn {
		t.Errorf("Level = %q, want warn for a failed check", got[0].Level)
	}
}

// The base handler must keep receiving everything. stderr is where a failed
// validation is diagnosed today, and a tee that swallowed records would take
// that away to add a nicety.
func TestProgressHandlerStillWritesEverythingToTheBaseHandler(t *testing.T) {
	var buf bytes.Buffer
	h := steps.NewProgressHandler(slog.NewTextHandler(&buf, nil))
	defer h.Attach(func(bus.Event) {})()

	handle(t, h, record("running validator", "name", "operator-health"))
	handle(t, h, record("something else entirely", "k", "v"))

	out := buf.String()
	if !strings.Contains(out, "running validator") {
		t.Errorf("base handler lost the forwarded record: %q", out)
	}
	if !strings.Contains(out, "something else entirely") {
		t.Errorf("base handler lost an unrelated record: %q", out)
	}
}

// Unrelated records must not become events, or every log line in the process
// lands in the operator's timeline.
func TestProgressHandlerIgnoresRecordsThatAreNotValidation(t *testing.T) {
	var got []bus.Event
	h := steps.NewProgressHandler(slog.NewTextHandler(&bytes.Buffer{}, nil))
	defer h.Attach(func(e bus.Event) { got = append(got, e) })()

	handle(t, h, record("connected", "context", "gke_x"))
	handle(t, h, record("wrote local chart folder", "index", 1))
	// Shaped like AICR's but missing the name: nothing useful to say.
	handle(t, h, record("running validator", "phase", "deployment"))

	if len(got) != 0 {
		t.Errorf("published %+v, want nothing", got)
	}
}

// Detach has to actually detach. A handler left attached would narrate a later
// run's validation into a run that already finished.
func TestProgressHandlerStopsPublishingAfterDetach(t *testing.T) {
	var got []bus.Event
	h := steps.NewProgressHandler(slog.NewTextHandler(&bytes.Buffer{}, nil))
	detach := h.Attach(func(e bus.Event) { got = append(got, e) })

	handle(t, h, record("running validator", "name", "operator-health"))
	detach()
	handle(t, h, record("running validator", "name", "check-nvidia-smi"))

	if len(got) != 1 {
		t.Fatalf("published %d events, want 1 -- detach did not stop forwarding: %+v", len(got), got)
	}
	if !strings.Contains(got[0].Message, "operator-health") {
		t.Errorf("kept the wrong event: %q", got[0].Message)
	}
}

// An unattached handler is the ordinary state -- every phase except Validate --
// and must not panic on a record that would otherwise match.
func TestProgressHandlerIsAPassThroughUntilAttached(t *testing.T) {
	var buf bytes.Buffer
	h := steps.NewProgressHandler(slog.NewTextHandler(&buf, nil))

	handle(t, h, record("validator completed", "name", "operator-health", "status", "passed"))

	if !strings.Contains(buf.String(), "validator completed") {
		t.Errorf("base handler did not receive the record: %q", buf.String())
	}
}

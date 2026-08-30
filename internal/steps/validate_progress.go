package steps

import (
	"context"
	"log/slog"
	"sync"

	"github.com/mchmarny/aicrme/internal/bus"
)

// AICR's own log messages, matched verbatim. These are the only three records
// that carry validation progress, and they are the contract this file depends
// on -- if a future AICR reworks its logging, this stops forwarding and the
// screen silently goes quiet again. TestProgressHandlerMatchesAICRsMessages
// pins the strings so the breakage is a test failure rather than a demo.
const (
	aicrPhaseStartMsg     = "running validation phase"
	aicrValidatorStartMsg = "running validator"
	aicrValidatorDoneMsg  = "validator completed"
	aicrValidatorNameAttr = "name"
	aicrValidatorStatAttr = "status"
	aicrPhaseSelectedAttr = "selected"
)

// ProgressHandler forwards AICR's per-check validation logging onto the event
// bus.
//
// WHY THIS EXISTS
// aicr.Client.ValidateState is one blocking call that returns only when every
// check has finished. It takes no progress callback -- the option set is
// version, kubeconfig, namespace, runID, cleanup, tolerations, nodeSelector,
// image overrides and failFast, and none of them reports anything mid-flight.
// So a validation looked identical to a hang: measured on real H100s
// 2026-08-30, an eight-minute phase during which the console said nothing
// while the screen still showed Apply's finished component rows.
//
// But AICR is not actually silent. It logs each check through log/slog's
// DEFAULT logger, which is the one cmd/aicrme configures -- so those records
// were already arriving in this process and being written to stderr and
// nowhere else. This handler is a tee: everything still reaches the base
// handler unchanged, and the three validation records are additionally
// published as events.
//
// The alternative considered and rejected was watching Jobs in AICR's
// validation namespace. It works -- the Jobs are there, one per check, serially
// -- but it infers from cluster side effects what the SDK is already telling us
// directly, and it cannot see a check's pass/fail without reading the Job's
// pod, which AICR deletes.
type ProgressHandler struct {
	base slog.Handler

	mu   sync.Mutex
	emit func(bus.Event)
}

// NewProgressHandler wraps base. Until Attach is called it is a pass-through,
// which is every phase except Validate.
func NewProgressHandler(base slog.Handler) *ProgressHandler {
	return &ProgressHandler{base: base}
}

// Attach routes matching records to emit until Detach. Returns Detach so a
// caller can defer it on the same line and cannot forget: a handler left
// attached would publish a later run's validation onto a finished run.
func (h *ProgressHandler) Attach(emit func(bus.Event)) (detach func()) {
	h.mu.Lock()
	h.emit = emit
	h.mu.Unlock()
	return func() {
		h.mu.Lock()
		h.emit = nil
		h.mu.Unlock()
	}
}

func (h *ProgressHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.base.Enabled(ctx, l)
}

func (h *ProgressHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &ProgressHandler{base: h.base.WithAttrs(as)}
}

func (h *ProgressHandler) WithGroup(name string) slog.Handler {
	return &ProgressHandler{base: h.base.WithGroup(name)}
}

// Handle tees. The base handler runs first and unconditionally: a panic or an
// error in the forwarding path must not cost the operator their stderr log,
// which is the one place a validation failure is currently diagnosable.
func (h *ProgressHandler) Handle(ctx context.Context, r slog.Record) error {
	err := h.base.Handle(ctx, r)

	h.mu.Lock()
	emit := h.emit
	h.mu.Unlock()
	if emit == nil {
		return err
	}
	if ev, ok := progressEvent(r); ok {
		emit(ev)
	}
	return err
}

// progressEvent translates one AICR record, or reports that it is not one.
func progressEvent(r slog.Record) (bus.Event, bool) {
	switch r.Message {
	case aicrPhaseStartMsg, aicrValidatorStartMsg, aicrValidatorDoneMsg:
	default:
		return bus.Event{}, false
	}

	var name, status, selected string
	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case aicrValidatorNameAttr:
			name = a.Value.String()
		case aicrValidatorStatAttr:
			status = a.Value.String()
		case aicrPhaseSelectedAttr:
			selected = a.Value.String()
		}
		return true
	})

	ev := bus.Event{Kind: bus.KindLog}
	switch r.Message {
	case aicrPhaseStartMsg:
		if selected == "" {
			return bus.Event{}, false
		}
		ev.Message = "validating: " + selected + " checks to run"
	case aicrValidatorStartMsg:
		if name == "" {
			return bus.Event{}, false
		}
		ev.Message = "checking " + name
	case aicrValidatorDoneMsg:
		if name == "" {
			return bus.Event{}, false
		}
		ev.Message = "check " + name + ": " + status
		// A failing check is a finding, and finding it in the same ink as
		// routine narration is how it gets missed -- the same reason the
		// verdict itself is published at warn.
		if status != "" && status != "passed" {
			ev.Level = bus.LevelWarn
		}
	}
	return ev, true
}

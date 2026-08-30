package main

import (
	"context"
	"log/slog"
	"strings"
	"sync"
)

// reflectorChurn is client-go's Event watch reconnecting, which it does
// constantly and says so through klog at Info:
//
//	"watch ended with error" ... "very short watch: ... Unexpected watch close
//	- watch lasted less than a second and no items received"
//
// Fifteen of them in one twenty-minute run on real H100s 2026-08-30, printed
// straight to stderr and interleaved with the console's own output.
//
// Dropped to debug rather than deleted. A high-churn Event watch is ordinary on
// a busy cluster and this is not evidence of a problem on its own -- but if
// events ever go missing from the timeline, this is the first thread to pull,
// and a message that has been deleted outright cannot be pulled. Raise the
// handler's level to see them.
const reflectorChurn = "watch ended with error"

// quietHandler drops known churn and collapses immediate repeats.
//
// It exists because a console people watch for twenty minutes is a UI, and a
// UI that prints the same paragraph five times in one second has trained the
// operator to stop reading it -- which is expensive on the run where something
// finally goes wrong.
type quietHandler struct {
	base slog.Handler

	mu   sync.Mutex
	last string
}

func newQuietHandler(base slog.Handler) *quietHandler {
	return &quietHandler{base: base}
}

func (h *quietHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.base.Enabled(ctx, l)
}

func (h *quietHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &quietHandler{base: h.base.WithAttrs(as)}
}

func (h *quietHandler) WithGroup(name string) slog.Handler {
	return &quietHandler{base: h.base.WithGroup(name)}
}

func (h *quietHandler) Handle(ctx context.Context, r slog.Record) error {
	if strings.Contains(r.Message, reflectorChurn) && r.Level < slog.LevelWarn {
		return nil
	}

	// Consecutive identical messages only. AICR emits its gpu-operator
	// driver auto-detect warning once per resolve and resolves several times
	// in a row, so the same paragraph landed five times in one second and ten
	// times in a run. Anything interleaved between two repeats still prints
	// both, so this collapses a burst without hiding a recurrence that
	// something else happened around.
	h.mu.Lock()
	repeat := r.Message == h.last
	h.last = r.Message
	h.mu.Unlock()
	if repeat {
		return nil
	}

	return h.base.Handle(ctx, r)
}

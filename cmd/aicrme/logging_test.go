package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func rec(level slog.Level, msg string) slog.Record {
	return slog.NewRecord(time.Now(), level, msg, 0)
}

func write(t *testing.T, h slog.Handler, r slog.Record) {
	t.Helper()
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
}

// AICR emits its gpu-operator driver auto-detect warning once per resolve and
// resolves several times in a row, so the same paragraph landed five times in
// one second on real H100s 2026-08-30.
func TestQuietHandlerCollapsesImmediateRepeats(t *testing.T) {
	var buf bytes.Buffer
	h := newQuietHandler(slog.NewTextHandler(&buf, nil))

	for range 5 {
		write(t, h, rec(slog.LevelWarn, "gpu-operator driver auto-detect: topology reports non-uniform GPU labels"))
	}

	if got := strings.Count(buf.String(), "auto-detect"); got != 1 {
		t.Errorf("printed the same warning %d times, want 1", got)
	}
}

// Only IMMEDIATE repeats. A condition that recurs after something else
// happened is a different fact and has to print again.
func TestQuietHandlerPrintsARepeatThatIsNotImmediate(t *testing.T) {
	var buf bytes.Buffer
	h := newQuietHandler(slog.NewTextHandler(&buf, nil))

	write(t, h, rec(slog.LevelWarn, "same message"))
	write(t, h, rec(slog.LevelInfo, "something else happened"))
	write(t, h, rec(slog.LevelWarn, "same message"))

	if got := strings.Count(buf.String(), "same message"); got != 2 {
		t.Errorf("printed the recurrence %d times, want 2 -- it is not an immediate repeat", got)
	}
}

// client-go's Event watch reconnects constantly and says so through klog.
// Fifteen in one run, interleaved with the console's own output.
func TestQuietHandlerDropsReflectorChurn(t *testing.T) {
	var buf bytes.Buffer
	h := newQuietHandler(slog.NewTextHandler(&buf, nil))

	write(t, h, rec(slog.LevelInfo,
		`"watch ended with error" type="*v1.Event" err="very short watch: Unexpected watch close"`))

	if buf.Len() != 0 {
		t.Errorf("printed reflector churn: %q", buf.String())
	}
}

// Dropped at info, kept at warn and above. The churn is benign on a busy
// cluster; a watch failure the library considers serious is not, and silencing
// that would remove the first thread to pull if events go missing.
func TestQuietHandlerKeepsAWatchErrorThatIsNotChurn(t *testing.T) {
	var buf bytes.Buffer
	h := newQuietHandler(slog.NewTextHandler(&buf, nil))

	write(t, h, rec(slog.LevelError, `"watch ended with error" err="forbidden"`))

	if !strings.Contains(buf.String(), "forbidden") {
		t.Errorf("dropped a genuine watch error: %q", buf.String())
	}
}

// Everything else passes through untouched. A filter that ate ordinary logging
// would cost more than the noise it removed.
func TestQuietHandlerPassesOrdinaryRecordsThrough(t *testing.T) {
	var buf bytes.Buffer
	h := newQuietHandler(slog.NewTextHandler(&buf, nil))

	write(t, h, rec(slog.LevelInfo, "connected"))
	write(t, h, rec(slog.LevelInfo, "deploying agent"))

	out := buf.String()
	if !strings.Contains(out, "connected") || !strings.Contains(out, "deploying agent") {
		t.Errorf("lost ordinary records: %q", out)
	}
}

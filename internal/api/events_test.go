package api_test

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/api"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
	"github.com/mchmarny/aicrme/internal/testfs"
)

func loggedInClient(t *testing.T, h http.Handler) (*httptest.Server, *http.Client) {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	jar := &cookieJar{}
	client := &http.Client{Jar: jar}
	resp, err := client.Post(ts.URL+"/api/login", "application/json",
		strings.NewReader(`{"username":"admin","password":"correct-horse"}`))
	if err != nil {
		t.Fatalf("login error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("login status = %d", resp.StatusCode)
	}
	return ts, client
}

func TestEventStreamReplaysFromLastEventID(t *testing.T) {
	b := bus.New(64)
	srv, err := api.New(api.Config{
		Cluster:  connectedCluster(),
		Username: "admin", Password: "correct-horse", SessionTTL: time.Hour, LoginRate: 100,
		AICR: &aicrclient.Fake{}, WorkDir: t.TempDir(),
	}, b, engine.New(b, engine.NewMemoryStore()), testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	ts, client := loggedInClient(t, srv.Handler())

	b.Publish(bus.Event{Kind: bus.KindLog, Message: "one"})
	b.Publish(bus.Event{Kind: bus.KindLog, Message: "two"})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/events", nil)
	req.Header.Set("Last-Event-ID", "1")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("stream error = %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	scanner := bufio.NewScanner(resp.Body)
	var sawTwo, sawOne bool
	deadline := time.Now().Add(2 * time.Second)
	for scanner.Scan() && time.Now().Before(deadline) {
		line := scanner.Text()
		if strings.Contains(line, `"two"`) {
			sawTwo = true
			break
		}
		if strings.Contains(line, `"one"`) {
			sawOne = true
		}
	}
	if sawOne {
		t.Error("replayed an event at or before Last-Event-ID")
	}
	if !sawTwo {
		t.Error("did not replay the event after Last-Event-ID")
	}
}

// TestEventsHandlerEmitsEpochControlEventFirst proves the epoch travels
// ahead of any replay, as a named "epoch" frame with no id: field.
// EventSource cannot read response headers, so the epoch has to travel in
// the stream body; it must be named so the SPA can route it away from
// onmessage (where run data lands), and it must carry no id because
// assigning one would advance the very cursor the epoch exists to let the
// client correct.
func TestEventsHandlerEmitsEpochControlEventFirst(t *testing.T) {
	b := bus.New(64)
	srv, err := api.New(api.Config{
		Cluster:  connectedCluster(),
		Username: "admin", Password: "correct-horse", SessionTTL: time.Hour, LoginRate: 100,
		AICR: &aicrclient.Fake{}, WorkDir: t.TempDir(),
	}, b, engine.New(b, engine.NewMemoryStore()), testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	ts, client := loggedInClient(t, srv.Handler())

	b.Publish(bus.Event{Kind: bus.KindLog, Message: "data event"})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/events", nil)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("stream error = %v", err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	var lines []string
	deadline := time.Now().Add(2 * time.Second)
	for scanner.Scan() && time.Now().Before(deadline) {
		line := scanner.Text()
		lines = append(lines, line)
		if strings.Contains(line, `"data event"`) {
			break
		}
	}
	raw := strings.Join(lines, "\n")
	if !strings.Contains(raw, `"data event"`) {
		t.Fatalf("run data never arrived in the stream:\n%s", raw)
	}

	// SSE frames are separated by a blank line. Splitting on that separator
	// -- rather than locating "event: epoch" by substring and slicing the
	// stream from that offset, as an earlier version of this test did -- is
	// what catches an id: field on either side of "event: epoch": a
	// substring-anchored slice silently drops everything before the match,
	// so an id: line placed ahead of "event: epoch" in the same frame was
	// invisible to that check. Asserting the frame's full, exact contents
	// (rather than just "no id: substring in here somewhere") is the
	// strongest check available and cheap: the frame is a literal format
	// string with one variable field.
	frames := strings.Split(raw, "\n\n")
	wantEpochFrame := fmt.Sprintf("event: epoch\ndata: {\"epoch\":%q}", b.Epoch())
	if frames[0] != wantEpochFrame {
		t.Fatalf("first SSE frame = %q, want %q -- the epoch frame must be first, byte-exact, and carry no id: field on either side of \"event: epoch\"",
			frames[0], wantEpochFrame)
	}
}

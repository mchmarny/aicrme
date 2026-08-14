package api_test

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
		Username: "admin", Password: "correct-horse", SessionTTL: time.Hour, LoginRate: 100,
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

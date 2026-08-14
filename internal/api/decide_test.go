package api_test

import (
	"context"
	"encoding/json"
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

// decisionStep parks every run until "intent" is supplied, so the decide
// endpoint's success path and the engine's conflict-while-parked path are
// both reachable over HTTP without engine test doubles leaking out of
// internal/engine.
type decisionStep struct{}

func (decisionStep) Phase() engine.Phase                                       { return engine.PhaseDiscover }
func (decisionStep) Requires() []string                                        { return []string{"intent"} }
func (decisionStep) Run(_ context.Context, _ *engine.Run, _ engine.Emit) error { return nil }

func newDecideTestServer(t *testing.T) (*httptest.Server, *http.Client) {
	t.Helper()
	b := bus.New(64)
	srv, err := api.New(api.Config{
		Username: "admin", Password: "correct-horse", SessionTTL: time.Hour, LoginRate: 100,
		AICR: &aicrclient.Fake{},
	}, b, engine.New(b, engine.NewMemoryStore(), decisionStep{}), testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	return loggedInClient(t, srv.Handler())
}

func waitForRunState(t *testing.T, client *http.Client, url string, want engine.State) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last engine.Run
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err != nil {
			t.Fatalf("GET %s error = %v", url, err)
		}
		decErr := json.NewDecoder(resp.Body).Decode(&last)
		resp.Body.Close()
		if decErr != nil {
			t.Fatalf("decode error = %v", decErr)
		}
		if last.State == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("run never reached state %q, last = %q", want, last.State)
}

func TestDecideResumesRun(t *testing.T) {
	ts, client := newDecideTestServer(t)

	resp, err := client.Post(ts.URL+"/api/runs", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /api/runs error = %v", err)
	}
	var created engine.Run
	decErr := json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if decErr != nil {
		t.Fatalf("decode error = %v", decErr)
	}

	waitForRunState(t, client, ts.URL+"/api/runs/"+created.ID, engine.StateAwaitingDecision)

	decideResp, err := client.Post(ts.URL+"/api/runs/"+created.ID+"/decide", "application/json",
		strings.NewReader(`{"intent":"training"}`))
	if err != nil {
		t.Fatalf("decide error = %v", err)
	}
	defer decideResp.Body.Close()
	if decideResp.StatusCode != http.StatusOK {
		t.Fatalf("decide status = %d, want %d", decideResp.StatusCode, http.StatusOK)
	}
	var after engine.Run
	if decErr := json.NewDecoder(decideResp.Body).Decode(&after); decErr != nil {
		t.Fatalf("decode error = %v", decErr)
	}
	if after.Decisions["intent"] != "training" {
		t.Errorf("decisions = %v, want intent=training", after.Decisions)
	}
}

func TestDecideMissingRequiredKeyIs400(t *testing.T) {
	ts, client := newDecideTestServer(t)

	resp, err := client.Post(ts.URL+"/api/runs", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /api/runs error = %v", err)
	}
	var created engine.Run
	decErr := json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if decErr != nil {
		t.Fatalf("decode error = %v", decErr)
	}

	waitForRunState(t, client, ts.URL+"/api/runs/"+created.ID, engine.StateAwaitingDecision)

	decideResp, err := client.Post(ts.URL+"/api/runs/"+created.ID+"/decide", "application/json",
		strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("decide error = %v", err)
	}
	defer decideResp.Body.Close()
	if decideResp.StatusCode != http.StatusBadRequest {
		t.Errorf("decide status = %d, want %d", decideResp.StatusCode, http.StatusBadRequest)
	}
}

func TestDecideMalformedBodyIs400(t *testing.T) {
	ts, client := newDecideTestServer(t)

	resp, err := client.Post(ts.URL+"/api/runs/does-not-exist/decide", "application/json", strings.NewReader(`{`))
	if err != nil {
		t.Fatalf("decide error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestDecideUnknownRunIs404(t *testing.T) {
	ts, client := newDecideTestServer(t)

	resp, err := client.Post(ts.URL+"/api/runs/does-not-exist/decide", "application/json",
		strings.NewReader(`{"intent":"training"}`))
	if err != nil {
		t.Fatalf("decide error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestCreateRunConflictWhileAwaitingDecision(t *testing.T) {
	ts, client := newDecideTestServer(t)

	resp, err := client.Post(ts.URL+"/api/runs", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /api/runs error = %v", err)
	}
	var created engine.Run
	decErr := json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if decErr != nil {
		t.Fatalf("decode error = %v", decErr)
	}

	waitForRunState(t, client, ts.URL+"/api/runs/"+created.ID, engine.StateAwaitingDecision)

	second, err := client.Post(ts.URL+"/api/runs", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("second POST /api/runs error = %v", err)
	}
	defer second.Body.Close()
	if second.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want %d", second.StatusCode, http.StatusConflict)
	}
}

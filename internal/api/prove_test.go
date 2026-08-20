package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/api"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
	"github.com/mchmarny/aicrme/internal/prove"
	"github.com/mchmarny/aicrme/internal/testfs"
)

// activeStep is engine.Step plus engine.ActiveStep: it always succeeds and
// leaves its run at StateActive, so this file can drive a run there over
// HTTP without an internal/engine test double leaking out of that package
// -- the same reason runs_test.go's failingStep and decide_test.go's
// decisionStep exist as package-local doubles instead. It does not touch
// the cluster itself: the run ID is only known once Start has generated
// it, so each test seeds the matching Job afterward, once the ID is known
// from the POST /api/runs response.
type activeStep struct{}

func (activeStep) Phase() engine.Phase         { return engine.PhaseProve }
func (activeStep) Requires() []string          { return nil }
func (activeStep) LeavesWorkloadRunning() bool { return true }
func (activeStep) Run(_ context.Context, r *engine.Run, _ engine.Emit) error {
	r.Workload = engine.Workload{Namespace: prove.Namespace, Kind: "Job", Name: prove.WorkloadName(r.ID)}
	return nil
}

// newStopTestServer builds a server whose engine's final step is
// activeStep, with a prove.Client wrapping cs wired in as Stop's cluster
// dependency.
func newStopTestServer(t *testing.T, cs *fake.Clientset) (*httptest.Server, *http.Client, *prove.Client) {
	t.Helper()
	b := bus.New(64)
	e := engine.New(b, engine.NewMemoryStore(), activeStep{})
	client := prove.NewClient(cs)
	e.SetProveClient(client)

	srv, err := api.New(api.Config{
		Username: "admin", Password: "correct-horse", SessionTTL: time.Hour, LoginRate: 100,
		AICR: &aicrclient.Fake{}, WorkDir: t.TempDir(),
	}, b, e, testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	ts, httpClient := loggedInClient(t, srv.Handler())
	return ts, httpClient, client
}

func TestStopEndsTheRunOverHTTP(t *testing.T) {
	cs := fake.NewSimpleClientset()
	ts, client, proveClient := newStopTestServer(t, cs)

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
	waitForRunState(t, client, ts.URL+"/api/runs/"+created.ID, engine.StateActive)

	// Seeded only now that the real (randomly generated) run ID is known --
	// activeStep itself never touches the cluster, so the workload must be
	// created here for Stop's deletion to be meaningful rather than a
	// trivial no-op against an object that was never there.
	ctx := context.Background()
	if ensureErr := proveClient.EnsureNamespace(ctx); ensureErr != nil {
		t.Fatalf("EnsureNamespace() error = %v", ensureErr)
	}
	if applyErr := proveClient.Apply(ctx, created.ID); applyErr != nil {
		t.Fatalf("Apply() error = %v", applyErr)
	}

	stopResp, err := client.Post(ts.URL+"/api/runs/"+created.ID+"/stop", "application/json", nil)
	if err != nil {
		t.Fatalf("stop error = %v", err)
	}
	defer stopResp.Body.Close()
	if stopResp.StatusCode != http.StatusOK {
		t.Fatalf("stop status = %d, want %d", stopResp.StatusCode, http.StatusOK)
	}
	var stopped engine.Run
	if decErr := json.NewDecoder(stopResp.Body).Decode(&stopped); decErr != nil {
		t.Fatalf("decode error = %v", decErr)
	}
	if stopped.State != engine.StateDone {
		t.Errorf("State = %q, want %q", stopped.State, engine.StateDone)
	}

	if _, getErr := cs.BatchV1().Jobs(prove.Namespace).
		Get(ctx, prove.WorkloadName(created.ID), metav1.GetOptions{}); getErr == nil {
		t.Error("workload still present after POST .../stop")
	}
}

func TestStopOnNonActiveRunConflicts(t *testing.T) {
	b := bus.New(64)
	srv, err := api.New(api.Config{
		Username: "admin", Password: "correct-horse", SessionTTL: time.Hour, LoginRate: 100,
		AICR: &aicrclient.Fake{}, WorkDir: t.TempDir(),
	}, b, engine.New(b, engine.NewMemoryStore(), failingStep{}), testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	ts, client := loggedInClient(t, srv.Handler())

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
	waitForRunState(t, client, ts.URL+"/api/runs/"+created.ID, engine.StateFailed)

	stopResp, err := client.Post(ts.URL+"/api/runs/"+created.ID+"/stop", "application/json", nil)
	if err != nil {
		t.Fatalf("stop error = %v", err)
	}
	defer stopResp.Body.Close()
	if stopResp.StatusCode != http.StatusConflict {
		t.Errorf("stop status = %d, want %d", stopResp.StatusCode, http.StatusConflict)
	}
}

func TestStopOnUnknownRunNotFound(t *testing.T) {
	ts, client := loggedInClient(t, newTestServer(t))

	resp, err := client.Post(ts.URL+"/api/runs/does-not-exist/stop", "application/json", nil)
	if err != nil {
		t.Fatalf("stop error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

// TestStopRequiresCSRFAndAuth mirrors TestDiscardRunRequiresCSRFAndAuth:
// POST .../stop must sit behind the same three guards every other mutating
// route does.
func TestStopRequiresCSRFAndAuth(t *testing.T) {
	t.Run("requires a session", func(t *testing.T) {
		h := newTestServer(t)
		req := httptest.NewRequest(http.MethodPost, "/api/runs/does-not-exist/stop", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})

	t.Run("requires same-origin", func(t *testing.T) {
		h := newTestServer(t)
		ts, client := loggedInClient(t, h)

		req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/runs/does-not-exist/stop", nil)
		if err != nil {
			t.Fatalf("NewRequest error = %v", err)
		}
		req.Header.Set("Origin", "http://localhost:3000")

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request error = %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want %d (not 404 -- the same-origin check must run before handleStop)",
				resp.StatusCode, http.StatusForbidden)
		}
	})

	t.Run("blocked while draining", func(t *testing.T) {
		srv := newDrainableTestServer(t)
		srv.Drain()

		req := httptest.NewRequest(http.MethodPost, "/api/runs/does-not-exist/stop", nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
		}
	})
}

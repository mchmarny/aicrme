package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/api"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
	"github.com/mchmarny/aicrme/internal/testfs"
)

// TestOptionsRequiresSession pins that the newly-registered route lives on
// the protected mux like every other /api/* route -- adding a route is easy
// to accidentally do on the unauthenticated top-level mux instead.
func TestOptionsRequiresSession(t *testing.T) {
	h := newTestServer(t)
	req, _ := http.NewRequest(http.MethodGet, "/api/options", nil)
	rec := newRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// TestOptionsEndpointFiltersThroughTheCatalog proves the handler is wired to
// aicrclient.AvailableOptions rather than a static list on Config: the JSON
// response reflects exactly what the (fake) catalog reports, not a literal
// baked into the handler. Fake.ListCatalog does not itself apply the query
// filter (it is a dumb stub, matching every other Fake method in this
// package), so both candidate intents see the same scripted entries --
// that's fine here, since this test's job is the HTTP/JSON plumbing, not
// catalog semantics. The real filtering semantics are pinned against the
// embedded catalog in internal/aicrclient/options_test.go and cross-checked
// against real Recommend resolution in
// internal/steps/options_cross_test.go.
func TestOptionsEndpointFiltersThroughTheCatalog(t *testing.T) {
	fake := &aicrclient.Fake{
		Registry: recipe.NewCriteriaRegistry(),
		CatalogEntries: []aicr.CatalogEntry{
			{Name: "h100-kind-training-kubeflow", Criteria: aicr.Criteria{Service: "kind", Intent: "training", Platform: "kubeflow"}},
			{Name: "h100-kind-training", Criteria: aicr.Criteria{Service: "kind", Intent: "training"}},
		},
	}
	b := bus.New(8)
	srv, err := api.New(api.Config{
		Username: "admin", Password: "correct-horse", SessionTTL: time.Hour, LoginRate: 100,
		AICR: fake,
	}, b, engine.New(b, engine.NewMemoryStore()), testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	ts, client := loggedInClient(t, srv.Handler())

	resp, err := client.Get(ts.URL + "/api/options")
	if err != nil {
		t.Fatalf("GET /api/options error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var got aicrclient.Options
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode error = %v", err)
	}

	wantIntents := []string{"inference", "training"}
	if !equalStrings(got.Intents, wantIntents) {
		t.Errorf("intents = %v, want %v", got.Intents, wantIntents)
	}
	// "kubeflow" from the entry that names it, "any" from the entry whose
	// Platform is unset -- the "just the runtime" option (task-10-report.md).
	wantPlatforms := []string{"any", "kubeflow"}
	if !equalStrings(got.Platforms, wantPlatforms) {
		t.Errorf("platforms = %v, want %v", got.Platforms, wantPlatforms)
	}
}

// snapshotStep hands Recommend-shaped Discover output to a run so
// TestOptionsUsesCurrentRunSnapshotWhenAvailable can exercise the
// snapshot-aware branch of handleOptions.
type snapshotStep struct{}

func (snapshotStep) Phase() engine.Phase { return engine.PhaseDiscover }
func (snapshotStep) Requires() []string  { return nil }
func (snapshotStep) Run(_ context.Context, r *engine.Run, _ engine.Emit) error {
	r.Artifacts["snapshot.yaml"] = []byte("apiVersion: aicr.nvidia.com/v1\nkind: Snapshot\n")
	return nil
}

// TestOptionsUsesCurrentRunSnapshotWhenAvailable proves handleOptions
// consults the engine's current run for a snapshot rather than always
// filtering unconstrained: once a run has produced snapshot.yaml,
// /api/options still answers 200 (aicrclient.ServiceFromSnapshot degrades
// to "" on a snapshot this thin rather than erroring the whole request --
// see its own tests in internal/aicrclient for the real derivation).
func TestOptionsUsesCurrentRunSnapshotWhenAvailable(t *testing.T) {
	fake := &aicrclient.Fake{Registry: recipe.NewCriteriaRegistry()}
	b := bus.New(8)
	srv, err := api.New(api.Config{
		Username: "admin", Password: "correct-horse", SessionTTL: time.Hour, LoginRate: 100,
		AICR: fake,
	}, b, engine.New(b, engine.NewMemoryStore(), snapshotStep{}), testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	ts, client := loggedInClient(t, srv.Handler())

	createResp, err := client.Post(ts.URL+"/api/runs", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /api/runs error = %v", err)
	}
	var created engine.Run
	decErr := json.NewDecoder(createResp.Body).Decode(&created)
	createResp.Body.Close()
	if decErr != nil {
		t.Fatalf("decode error = %v", decErr)
	}
	waitForRunState(t, client, ts.URL+"/api/runs/"+created.ID, engine.StateDone)

	resp, err := client.Get(ts.URL + "/api/options")
	if err != nil {
		t.Fatalf("GET /api/options error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

// TestNewRequiresAICRClient mirrors TestEmptyPasswordRejected: a nil AICR
// client would panic the first time handleOptions runs, so api.New must
// reject it at construction like every other required Config field.
func TestNewRequiresAICRClient(t *testing.T) {
	_, err := api.New(api.Config{Username: "admin", Password: "pw", SessionTTL: time.Hour, LoginRate: 10},
		bus.New(8), engine.New(bus.New(8), engine.NewMemoryStore()), testfs.Static())
	if err == nil {
		t.Error("api.New() accepted a nil AICR client")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

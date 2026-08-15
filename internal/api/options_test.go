package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
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
		AICR: fake, WorkDir: t.TempDir(),
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
	// Platform is unset -- the "just the runtime" option.
	wantPlatforms := []string{"any", "kubeflow"}
	if !equalStrings(got.Platforms, wantPlatforms) {
		t.Errorf("platforms = %v, want %v", got.Platforms, wantPlatforms)
	}
	for _, intent := range wantIntents {
		if !equalStrings(got.PlatformsByIntent[intent], wantPlatforms) {
			t.Errorf("platformsByIntent[%q] = %v, want %v", intent, got.PlatformsByIntent[intent], wantPlatforms)
		}
	}
	// No run has started, so handleOptions has no snapshot-derived service
	// to filter by yet: the response must say so.
	if !got.Provisional {
		t.Error("provisional = false, want true with no run started")
	}
}

// rawSnapshotStep hands a fixed byte slice to a run as its Discover output,
// so options_test.go's tests can exercise handleOptions's snapshot-aware
// branch with content of their choosing.
type rawSnapshotStep struct{ raw []byte }

func (rawSnapshotStep) Phase() engine.Phase { return engine.PhaseDiscover }
func (rawSnapshotStep) Requires() []string  { return nil }
func (s rawSnapshotStep) Run(_ context.Context, r *engine.Run, _ engine.Emit) error {
	r.Artifacts["snapshot.yaml"] = s.raw
	return nil
}

func startAndAwaitDone(t *testing.T, ts *httptest.Server, client *http.Client) aicrclient.Options {
	t.Helper()
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
	var got aicrclient.Options
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode error = %v", err)
	}
	return got
}

// TestOptionsUsesCurrentRunSnapshotWhenAvailable proves handleOptions
// consults the engine's current run for a snapshot rather than always
// filtering unconstrained. It also pins the sharp edge Options.Provisional's
// doc comment calls out: a snapshot this thin (no measurements) makes
// aicrclient.ServiceFromSnapshot degrade to "" rather than error, so
// Provisional stays true even though a run now exists and completed --
// "a run exists" is not proof the options are cluster-accurate.
func TestOptionsUsesCurrentRunSnapshotWhenAvailable(t *testing.T) {
	fake := &aicrclient.Fake{Registry: recipe.NewCriteriaRegistry()}
	b := bus.New(8)
	step := rawSnapshotStep{raw: []byte("apiVersion: aicr.nvidia.com/v1\nkind: Snapshot\n")}
	srv, err := api.New(api.Config{
		Username: "admin", Password: "correct-horse", SessionTTL: time.Hour, LoginRate: 100,
		AICR: fake, WorkDir: t.TempDir(),
	}, b, engine.New(b, engine.NewMemoryStore(), step), testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	ts, client := loggedInClient(t, srv.Handler())

	got := startAndAwaitDone(t, ts, client)
	if !got.Provisional {
		t.Error("provisional = false, want true: this snapshot has no measurements to derive a service from")
	}
}

// TestOptionsProvisionalClearsOnceASnapshotYieldsARealService is the "clear
// after" half of the contract Options.Provisional documents: reusing the
// real simulated-H100 KWOK fixture already pinned in
// internal/steps/recommend_test.go (TestRecommendResolvesAgainstSimulatedH100Fixture),
// a run whose snapshot fingerprints to a real, non-empty service must
// report Provisional=false -- not just "some run exists", which
// TestOptionsUsesCurrentRunSnapshotWhenAvailable already proves is not
// enough on its own.
func TestOptionsProvisionalClearsOnceASnapshotYieldsARealService(t *testing.T) {
	raw, err := os.ReadFile("../steps/testdata/snapshot-kwok-h100.yaml")
	if err != nil {
		t.Fatalf("fixture read error = %v", err)
	}

	fake := &aicrclient.Fake{Registry: recipe.NewCriteriaRegistry()}
	b := bus.New(8)
	step := rawSnapshotStep{raw: raw}
	srv, err := api.New(api.Config{
		Username: "admin", Password: "correct-horse", SessionTTL: time.Hour, LoginRate: 100,
		AICR: fake, WorkDir: t.TempDir(),
	}, b, engine.New(b, engine.NewMemoryStore(), step), testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	ts, client := loggedInClient(t, srv.Handler())

	got := startAndAwaitDone(t, ts, client)
	if got.Provisional {
		t.Error("provisional = true, want false once the snapshot fingerprints to a real service (kind)")
	}
}

// TestOptionsDegradesToProvisionalOnCorruptSnapshot pins that a corrupt
// snapshot.yaml does not take the endpoint down. /api/options supplies the
// only two questions this console ever asks, so a 400 here would brick the
// wizard with no way forward; the contract is a 200 carrying the widened
// provisional set, with steps.Recommend left as the fail-loud backstop on the
// same bytes.
func TestOptionsDegradesToProvisionalOnCorruptSnapshot(t *testing.T) {
	fake := &aicrclient.Fake{
		Registry: recipe.NewCriteriaRegistry(),
		CatalogEntries: []aicr.CatalogEntry{
			{Name: "h100-kind-training-kubeflow", Criteria: aicr.Criteria{Platform: "kubeflow"}},
		},
	}
	b := bus.New(8)
	step := rawSnapshotStep{raw: []byte("- this\n- is\n- a list, not a Snapshot\n")}
	srv, err := api.New(api.Config{
		Username: "admin", Password: "correct-horse", SessionTTL: time.Hour, LoginRate: 100,
		AICR: fake, WorkDir: t.TempDir(),
	}, b, engine.New(b, engine.NewMemoryStore(), step), testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	ts, client := loggedInClient(t, srv.Handler())

	got := startAndAwaitDone(t, ts, client)
	if !got.Provisional {
		t.Error("provisional = false, want true -- the snapshot could not be parsed")
	}
	if !equalStrings(got.Platforms, []string{"kubeflow"}) {
		t.Errorf("platforms = %v, want [kubeflow] -- the widened candidate set, not an empty answer",
			got.Platforms)
	}
}

// TestNewRequiresAICRClient mirrors TestEmptyPasswordRejected: a nil AICR
// client would panic the first time handleOptions runs, so api.New must
// reject it at construction like every other required Config field.
func TestNewRequiresAICRClient(t *testing.T) {
	_, err := api.New(api.Config{Username: "admin", Password: "pw", SessionTTL: time.Hour, LoginRate: 10, WorkDir: t.TempDir()},
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

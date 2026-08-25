package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/api"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
	"github.com/mchmarny/aicrme/internal/prove"
	"github.com/mchmarny/aicrme/internal/testfs"
)

// installingStep records a component so the run Reset is offered has
// something it could remove -- engine.Reset refuses a run that installed
// nothing, which would otherwise mask every assertion in this file behind
// the same 409.
type installingStep struct{}

func (installingStep) Phase() engine.Phase { return engine.PhaseApply }
func (installingStep) Requires() []string  { return nil }
func (installingStep) Run(_ context.Context, r *engine.Run, _ engine.Emit) error {
	r.Components = []engine.ComponentState{
		{Name: "gpu-operator", Namespace: "gpu-operator", Index: 1, Total: 1, Status: "installed"},
	}
	r.Ownership = engine.Ownership{Namespaces: []engine.NamespaceRef{
		{Name: "gpu-operator"},
	}}
	return nil
}

// countingTeardown records whether the engine ever reached the cluster.
// Atomic because the teardown runs on engine.Reset's own goroutine while
// the test goroutine reads it.
type countingTeardown struct{ calls atomic.Int64 }

func (c *countingTeardown) Releases(_, _ context.Context, comps []engine.ComponentState,
	_ engine.Ownership, emit func(engine.ResidueItem)) []engine.ResidueItem {

	c.calls.Add(1)
	items := make([]engine.ResidueItem, 0, len(comps))
	for _, comp := range comps {
		it := engine.ResidueItem{
			Kind: engine.KindRelease, Name: comp.Name, Namespace: comp.Namespace, Removed: true,
		}
		emit(it)
		items = append(items, it)
	}
	return items
}

func (c *countingTeardown) Namespaces(_ context.Context, names []string, _ engine.Ownership,
	emit func(engine.ResidueItem)) []engine.ResidueItem {

	items := make([]engine.ResidueItem, 0, len(names))
	for _, n := range names {
		it := engine.ResidueItem{Kind: engine.KindNamespace, Name: n, Removed: true}
		emit(it)
		items = append(items, it)
	}
	return items
}

// newResetTestServer builds a console whose run finishes with one component
// recorded, with both of Reset's cluster dependencies wired.
func newResetTestServer(t *testing.T) (*httptest.Server, *http.Client, *countingTeardown) {
	t.Helper()
	b := bus.New(64)
	e := engine.New(b, engine.NewMemoryStore(), installingStep{})
	e.SetProveClient(prove.NewClient(fake.NewSimpleClientset()))
	td := &countingTeardown{}
	e.SetTeardown(td)

	srv, err := api.New(api.Config{
		Cluster:  connectedCluster(),
		Username: "admin", Password: "correct-horse", SessionTTL: time.Hour, LoginRate: 100,
		AICR: &aicrclient.Fake{}, WorkDir: t.TempDir(),
	}, b, e, testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	ts, httpClient := loggedInClient(t, srv.Handler())
	return ts, httpClient, td
}

// startFinishedRun drives a run to completion and returns its ID.
func startFinishedRun(t *testing.T, client *http.Client, base string) string {
	t.Helper()
	resp, err := client.Post(base+"/api/runs", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /api/runs error = %v", err)
	}
	var created engine.Run
	decErr := json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if decErr != nil {
		t.Fatalf("decode error = %v", decErr)
	}
	waitForRunState(t, client, base+"/api/runs/"+created.ID, engine.StateDone)
	return created.ID
}

func postReset(t *testing.T, client *http.Client, base, id, body string) *http.Response {
	t.Helper()
	resp, err := client.Post(base+"/api/runs/"+id+"/reset", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST .../reset error = %v", err)
	}
	return resp
}

// Reset is the one operation in this console that destroys rather than
// creates. A bare POST -- which any stray retry or a URL pasted into a
// browser could produce -- must not start a teardown.
func TestResetRequiresTheConfirmationBody(t *testing.T) {
	for name, body := range map[string]string{
		"empty body":        "",
		"empty object":      "{}",
		"wrong word":        `{"confirm":"yes"}`,
		"case mismatch":     `{"confirm":"RESET"}`,
		"confirm elsewhere": `{"other":"reset"}`,
	} {
		t.Run(name, func(t *testing.T) {
			ts, client, td := newResetTestServer(t)
			id := startFinishedRun(t, client, ts.URL)

			resp := postReset(t, client, ts.URL, id, body)
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
			}
			if td.calls.Load() != 0 {
				t.Error("the teardown ran without a confirmation -- the guard is decorative")
			}
		})
	}
}

func TestResetAcceptsTheConfirmationBody(t *testing.T) {
	ts, client, td := newResetTestServer(t)
	id := startFinishedRun(t, client, ts.URL)

	resp := postReset(t, client, ts.URL, id, `{"confirm":"reset"}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	// The handler answers as soon as the teardown is accepted and its state
	// persisted, not when it finishes -- so the run comes back resetting,
	// and the operator follows the rest on the event stream.
	var got engine.Run
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode error = %v", err)
	}
	if got.State != engine.StateResetting && got.State != engine.StateDone {
		t.Errorf("State = %q, want %q (or %q if it had already finished)",
			got.State, engine.StateResetting, engine.StateDone)
	}
	waitForRunState(t, client, ts.URL+"/api/runs/"+id, engine.StateDone)
	if td.calls.Load() != 1 {
		t.Errorf("teardown ran %d times, want 1", td.calls.Load())
	}
}

// The engine's own guards have to reach the operator as HTTP status codes,
// not be flattened into a 500 -- the console offers different actions for a
// conflict than for a server fault. A run parked at a decision gate is
// live: a teardown would race the step that is about to install things.
func TestResetSurfacesTheEnginesConflict(t *testing.T) {
	b := bus.New(64)
	e := engine.New(b, engine.NewMemoryStore(), decisionStep{})
	e.SetProveClient(prove.NewClient(fake.NewSimpleClientset()))
	e.SetTeardown(&countingTeardown{})

	srv, err := api.New(api.Config{
		Cluster:  connectedCluster(),
		Username: "admin", Password: "correct-horse", SessionTTL: time.Hour, LoginRate: 100,
		AICR: &aicrclient.Fake{}, WorkDir: t.TempDir(),
	}, b, e, testfs.Static())
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
	waitForRunState(t, client, ts.URL+"/api/runs/"+created.ID, engine.StateAwaitingDecision)

	got := postReset(t, client, ts.URL, created.ID, `{"confirm":"reset"}`)
	defer got.Body.Close()
	if got.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want %d", got.StatusCode, http.StatusConflict)
	}
}

// A second Reset must succeed rather than conflict. It is the normal
// recovery path after a partial one, and it is what helm's
// --ignore-not-found is there for -- a repeat teardown that answered "run
// not found" or "already reset" would leave an operator with a cluster they
// could see residue on and no button that would act on it.
func TestResetIsRepeatable(t *testing.T) {
	ts, client, td := newResetTestServer(t)
	id := startFinishedRun(t, client, ts.URL)

	first := postReset(t, client, ts.URL, id, `{"confirm":"reset"}`)
	first.Body.Close()
	waitForRunState(t, client, ts.URL+"/api/runs/"+id, engine.StateDone)

	second := postReset(t, client, ts.URL, id, `{"confirm":"reset"}`)
	defer second.Body.Close()
	if second.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d -- a repeat reset is the recovery path, not an error",
			second.StatusCode, http.StatusOK)
	}
	waitForRunState(t, client, ts.URL+"/api/runs/"+id, engine.StateDone)
	if td.calls.Load() != 2 {
		t.Errorf("teardown ran %d times, want 2 -- the second reset must actually re-run it", td.calls.Load())
	}
}

// A run that installed nothing is engine.Reset's own 409, and it must
// arrive as one rather than as a generic failure.
func TestResetOnARunThatInstalledNothingConflicts(t *testing.T) {
	b := bus.New(64)
	e := engine.New(b, engine.NewMemoryStore(), failingStep{})
	e.SetProveClient(prove.NewClient(fake.NewSimpleClientset()))
	e.SetTeardown(&countingTeardown{})

	srv, err := api.New(api.Config{
		Cluster:  connectedCluster(),
		Username: "admin", Password: "correct-horse", SessionTTL: time.Hour, LoginRate: 100,
		AICR: &aicrclient.Fake{}, WorkDir: t.TempDir(),
	}, b, e, testfs.Static())
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

	got := postReset(t, client, ts.URL, created.ID, `{"confirm":"reset"}`)
	defer got.Body.Close()
	if got.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want %d", got.StatusCode, http.StatusConflict)
	}
}

// The same-origin check must run before the handler, exactly as it does for
// stop: a cross-origin POST that reached handleReset would be a
// one-request teardown of someone's cluster.
func TestResetRejectsACrossOriginPost(t *testing.T) {
	ts, client, td := newResetTestServer(t)
	id := startFinishedRun(t, client, ts.URL)

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/runs/"+id+"/reset",
		strings.NewReader(`{"confirm":"reset"}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
	if td.calls.Load() != 0 {
		t.Error("a cross-origin request reached the teardown")
	}
}

// Draining is shutdown: accepting a teardown the process cannot finish
// would leave the cluster half-removed with no goroutine to report it.
func TestResetIsBlockedWhileDraining(t *testing.T) {
	srv := newDrainableTestServer(t)
	srv.Drain()

	req := httptest.NewRequest(http.MethodPost, "/api/runs/does-not-exist/reset",
		strings.NewReader(`{"confirm":"reset"}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

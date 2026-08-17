package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/api"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
	"github.com/mchmarny/aicrme/internal/testfs"
)

func TestCreateAndGetRun(t *testing.T) {
	b := bus.New(64)
	srv, err := api.New(api.Config{
		Username: "admin", Password: "correct-horse", SessionTTL: time.Hour, LoginRate: 100,
		AICR: &aicrclient.Fake{}, WorkDir: t.TempDir(),
	}, b, engine.New(b, engine.NewMemoryStore()), testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	ts, client := loggedInClient(t, srv.Handler())

	resp, err := client.Post(ts.URL+"/api/runs", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /api/runs error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusAccepted)
	}

	var created engine.Run
	if decErr := json.NewDecoder(resp.Body).Decode(&created); decErr != nil {
		t.Fatalf("decode error = %v", decErr)
	}
	if created.ID == "" {
		t.Fatal("created run has no ID")
	}

	got, err := client.Get(ts.URL + "/api/runs/" + created.ID)
	if err != nil {
		t.Fatalf("GET run error = %v", err)
	}
	defer got.Body.Close()
	if got.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", got.StatusCode, http.StatusOK)
	}
}

func TestGetUnknownRunIs404(t *testing.T) {
	b := bus.New(8)
	srv, _ := api.New(api.Config{
		Username: "admin", Password: "correct-horse", SessionTTL: time.Hour, LoginRate: 100,
		AICR: &aicrclient.Fake{}, WorkDir: t.TempDir(),
	}, b, engine.New(b, engine.NewMemoryStore()), testfs.Static())
	ts, client := loggedInClient(t, srv.Handler())

	resp, err := client.Get(ts.URL + "/api/runs/does-not-exist")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

// failingStep always fails, so runs_test.go's Retry tests can drive a run to
// engine.StateFailed over HTTP without engine test doubles leaking out of
// internal/engine, matching decide_test.go's decisionStep pattern.
type failingStep struct{}

func (failingStep) Phase() engine.Phase { return engine.PhaseDiscover }
func (failingStep) Requires() []string  { return nil }
func (failingStep) Run(_ context.Context, _ *engine.Run, _ engine.Emit) error {
	return errors.New("boom")
}

func TestRetryReturnsTheRun(t *testing.T) {
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

	retryResp, err := client.Post(ts.URL+"/api/runs/"+created.ID+"/retry", "application/json", nil)
	if err != nil {
		t.Fatalf("retry error = %v", err)
	}
	defer retryResp.Body.Close()
	if retryResp.StatusCode != http.StatusOK {
		t.Fatalf("retry status = %d, want %d", retryResp.StatusCode, http.StatusOK)
	}
	var retried engine.Run
	if decErr := json.NewDecoder(retryResp.Body).Decode(&retried); decErr != nil {
		t.Fatalf("decode error = %v", decErr)
	}
	if retried.ID != created.ID {
		t.Errorf("retried run ID = %q, want %q", retried.ID, created.ID)
	}
}

func TestRetryOnRunningRunConflicts(t *testing.T) {
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

	retryResp, err := client.Post(ts.URL+"/api/runs/"+created.ID+"/retry", "application/json", nil)
	if err != nil {
		t.Fatalf("retry error = %v", err)
	}
	defer retryResp.Body.Close()
	if retryResp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want %d", retryResp.StatusCode, http.StatusConflict)
	}
}

func TestRetryOnUnknownRunNotFound(t *testing.T) {
	ts, client := loggedInClient(t, newTestServer(t))

	resp, err := client.Post(ts.URL+"/api/runs/does-not-exist/retry", "application/json", nil)
	if err != nil {
		t.Fatalf("retry error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestSessionProbeReturns204WhenAuthed(t *testing.T) {
	ts, client := loggedInClient(t, newTestServer(t))

	resp, err := client.Get(ts.URL + "/api/session")
	if err != nil {
		t.Fatalf("GET /api/session error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
}

func TestSessionProbeReturns401WhenNot(t *testing.T) {
	h := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// bundleStep reports PhaseBundle and does nothing else. Recover's
// bundleStepIndex requires exactly one such step in the engine's slice
// before it will install a recovered run at all, so every test in this file
// that seeds a store and calls Recover needs one.
type bundleStep struct{}

func (bundleStep) Phase() engine.Phase                                       { return engine.PhaseBundle }
func (bundleStep) Requires() []string                                        { return nil }
func (bundleStep) Run(_ context.Context, _ *engine.Run, _ engine.Emit) error { return nil }

// recoveredRunID matches validRunID's format (16 hex characters) so the
// seeded record survives Recover's own validation, mirroring
// internal/engine/recover_test.go's testRunID.
const recoveredRunID = "0123456789abcdef"

// seedRecoveredRun saves a StateFailed run directly into store and recovers
// it, landing the engine in the same recoveredPending state a pod restart
// with an in-flight run would produce -- the setup TestRetryClearsRecoveryPending
// and friends use in internal/engine, reproduced here because
// internal/api must exercise it over HTTP, not by calling engine internals.
func seedRecoveredRun(t *testing.T, b *bus.Bus, store engine.Store) *engine.Engine {
	t.Helper()
	seed := &engine.Run{
		ID:        recoveredRunID,
		State:     engine.StateFailed,
		Phase:     engine.PhaseDiscover,
		Decisions: map[string]string{},
		Artifacts: map[string][]byte{},
		StartedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := store.Save(context.Background(), seed); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	e := engine.New(b, store, bundleStep{})
	if err := e.Recover(context.Background()); err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	return e
}

// TestCreateRunReturns409WhenRecoveryPending pins the SPA's silent-409
// contract (POST /api/runs on load must not fight a recovered run) across
// this task's context-threading refactor. The status alone is not proof: a
// live current run also answers 409, via isLive's separate conflict branch,
// so the assertion also checks the body names the recovered-run gate
// specifically (Start's recoveredPending branch) rather than that other
// source -- this setup never triggers isLive's branch anyway (the recovered
// run is StateFailed, not live), but the check keeps that true by
// construction instead of by coincidence.
func TestCreateRunReturns409WhenRecoveryPending(t *testing.T) {
	b := bus.New(64)
	store := engine.NewMemoryStore()
	e := seedRecoveredRun(t, b, store)

	srv, err := api.New(api.Config{
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
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusConflict)
	}
	var body map[string]string
	if decErr := json.NewDecoder(resp.Body).Decode(&body); decErr != nil {
		t.Fatalf("decode error = %v", decErr)
	}
	if !strings.Contains(body["error"], "recovered run is waiting") {
		t.Errorf("error = %q, want the recovered-run gate's message, not a different 409 source", body["error"])
	}
}

// TestDiscardRunDeletesAndAllowsRestart exercises DELETE end to end: a
// recovered run blocks Start (see TestCreateRunReturns409WhenRecoveryPending)
// until it is discarded, at which point both the in-memory gate and the
// persisted record must be gone.
func TestDiscardRunDeletesAndAllowsRestart(t *testing.T) {
	b := bus.New(64)
	store := engine.NewMemoryStore()
	e := seedRecoveredRun(t, b, store)

	srv, err := api.New(api.Config{
		Username: "admin", Password: "correct-horse", SessionTTL: time.Hour, LoginRate: 100,
		AICR: &aicrclient.Fake{}, WorkDir: t.TempDir(),
	}, b, e, testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	ts, client := loggedInClient(t, srv.Handler())

	delReq, err := http.NewRequest(http.MethodDelete, ts.URL+"/api/runs/"+recoveredRunID, nil)
	if err != nil {
		t.Fatalf("NewRequest error = %v", err)
	}
	delResp, err := client.Do(delReq)
	if err != nil {
		t.Fatalf("DELETE error = %v", err)
	}
	defer delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want %d", delResp.StatusCode, http.StatusNoContent)
	}

	if _, loadErr := store.LoadCurrent(context.Background()); loadErr == nil {
		t.Error("LoadCurrent() succeeded after discard, want the persisted record gone")
	}

	createResp, err := client.Post(ts.URL+"/api/runs", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /api/runs error = %v", err)
	}
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusAccepted {
		t.Errorf("post-discard create status = %d, want %d", createResp.StatusCode, http.StatusAccepted)
	}
}

// TestDiscardRunRequiresCSRFAndAuth pins DELETE /api/runs/{id} behind the
// same three guards every other mutating route sits behind: the auth
// wrapper, the same-origin (CSRF) check, and Drain. Each subtest isolates
// one guard by leaving the other two satisfied, so a regression in any single
// guard is attributable rather than lost in a single aggregate assertion.
func TestDiscardRunRequiresCSRFAndAuth(t *testing.T) {
	t.Run("requires a session", func(t *testing.T) {
		h := newTestServer(t)
		req := httptest.NewRequest(http.MethodDelete, "/api/runs/does-not-exist", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})

	// Mirrors csrf_test.go's TestCrossOriginDecideRejected: a run ID that
	// does not exist distinguishes "the same-origin check ran first" (403)
	// from "the same-origin check was bypassed and the handler ran" (404).
	t.Run("requires same-origin", func(t *testing.T) {
		h := newTestServer(t)
		ts, client := loggedInClient(t, h)

		req, err := http.NewRequest(http.MethodDelete, ts.URL+"/api/runs/does-not-exist", nil)
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
			t.Errorf("status = %d, want %d (not 404 -- the same-origin check must run before handleDiscardRun)",
				resp.StatusCode, http.StatusForbidden)
		}
	})

	t.Run("blocked while draining", func(t *testing.T) {
		srv := newDrainableTestServer(t)
		srv.Drain()

		req := httptest.NewRequest(http.MethodDelete, "/api/runs/does-not-exist", nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
		}
	})
}

// blockingLoadStore's Load blocks until released, then reports whether the
// context it was handed had already been canceled -- the only way to prove
// from outside internal/engine that a handler threaded the real request
// context into the store call rather than a background one that can never
// observe the request's cancellation.
type blockingLoadStore struct {
	entered  chan struct{}
	release  chan struct{}
	canceled bool
}

func (s *blockingLoadStore) Save(context.Context, *engine.Run) error { return nil }

func (s *blockingLoadStore) Load(ctx context.Context, _ string) (*engine.Run, error) {
	close(s.entered)
	<-s.release
	s.canceled = errors.Is(ctx.Err(), context.Canceled)
	return nil, ctx.Err()
}

func (s *blockingLoadStore) LoadCurrent(context.Context) (*engine.Run, error) {
	return nil, aicrerrors.New(aicrerrors.ErrCodeNotFound, "no current run")
}

func (s *blockingLoadStore) Delete(context.Context) error { return nil }

// directLogin logs in via a direct ServeHTTP call (no real network) and
// returns the session cookie, so a subsequent request built the same way can
// attach it -- needed here because the test below drives the handler with a
// caller-controlled, cancelable context, which a real *http.Client request
// cannot do (the Transport refuses to even send a request whose context is
// already canceled, so cancellation could never race the handler).
func directLogin(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(
		`{"username":"admin","password":"correct-horse"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("login status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("login set no session cookie")
	}
	return cookies[0]
}

// TestRequestCancellationReachesTheStore is the regression test for this
// task's actual subject: that Get threads the real request context into
// store.Load instead of a context.Background() the request's cancellation
// can never reach. Without real threading, blockingLoadStore.Load would
// still observe an un-canceled context after cancel() runs, and the test
// would hang until it times out rather than observing ctx.Err().
func TestRequestCancellationReachesTheStore(t *testing.T) {
	b := bus.New(8)
	store := &blockingLoadStore{entered: make(chan struct{}), release: make(chan struct{})}
	e := engine.New(b, store)
	srv, err := api.New(api.Config{
		Username: "admin", Password: "correct-horse", SessionTTL: time.Hour, LoginRate: 100,
		AICR: &aicrclient.Fake{}, WorkDir: t.TempDir(),
	}, b, e, testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	h := srv.Handler()
	cookie := directLogin(t, h)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/runs/not-current", nil).WithContext(ctx)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-store.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("store.Load was never called")
	}
	cancel()
	close(store.release)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never returned after the store call unblocked")
	}

	if !store.canceled {
		t.Error("store.Load's context was not canceled -- the request context was not threaded through to it")
	}
}

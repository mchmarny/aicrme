package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
		Cluster: connectedCluster(),
		Token:   testToken,
		AICR:    &aicrclient.Fake{}, WorkDir: t.TempDir(),
	}, b, engine.New(b, engine.NewMemoryStore()), testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	ts, client := authedClient(t, srv.Handler())

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
		Cluster: connectedCluster(),
		Token:   testToken,
		AICR:    &aicrclient.Fake{}, WorkDir: t.TempDir(),
	}, b, engine.New(b, engine.NewMemoryStore()), testfs.Static())
	ts, client := authedClient(t, srv.Handler())

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
		Cluster: connectedCluster(),
		Token:   testToken,
		AICR:    &aicrclient.Fake{}, WorkDir: t.TempDir(),
	}, b, engine.New(b, engine.NewMemoryStore(), failingStep{}), testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	ts, client := authedClient(t, srv.Handler())

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
	ts, client := authedClient(t, newTestServer(t))

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
	ts, client := authedClient(t, newTestServer(t))

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
		Cluster: connectedCluster(),
		Token:   testToken,
		AICR:    &aicrclient.Fake{}, WorkDir: t.TempDir(),
	}, b, e, testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	ts, client := authedClient(t, srv.Handler())

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
		Cluster: connectedCluster(),
		Token:   testToken,
		AICR:    &aicrclient.Fake{}, WorkDir: t.TempDir(),
	}, b, e, testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	ts, client := authedClient(t, srv.Handler())

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
		ts, client := authedClient(t, h)

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

// directSession exchanges the launch token via a direct ServeHTTP call (no
// real network) and returns the session cookie, so a subsequent request built
// the same way can attach it -- needed here because the test below drives the
// handler with a caller-controlled, cancelable context, which a real
// *http.Client request cannot do (the Transport refuses to even send a
// request whose context is already canceled, so cancellation could never race
// the handler).
func directSession(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/session", strings.NewReader(
		`{"token":"`+testToken+`"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST /api/session status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("the exchange set no session cookie")
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
		Cluster: connectedCluster(),
		Token:   testToken,
		AICR:    &aicrclient.Fake{}, WorkDir: t.TempDir(),
	}, b, e, testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	h := srv.Handler()
	cookie := directSession(t, h)

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

// retryProbeStep fails its first Run (driving the run to StateFailed so
// Retry is legal), then on every later Run blocks until released and
// records whether its own context was ever canceled while blocked. It is
// the regression test's only way to observe the execution context
// handleRetry hands to the relaunched step -- there is no legal way to read
// it from outside internal/engine.
type retryProbeStep struct {
	mu       sync.Mutex
	attempt  int
	entered  chan struct{}
	release  chan struct{}
	canceled bool
}

func (*retryProbeStep) Phase() engine.Phase { return engine.PhaseApply }
func (*retryProbeStep) Requires() []string  { return nil }

func (s *retryProbeStep) Run(ctx context.Context, _ *engine.Run, _ engine.Emit) error {
	s.mu.Lock()
	s.attempt++
	first := s.attempt == 1
	s.mu.Unlock()
	if first {
		return errors.New("boom")
	}
	close(s.entered)
	<-s.release
	s.mu.Lock()
	s.canceled = ctx.Err() != nil
	s.mu.Unlock()
	return nil
}

// Canceled reports whether the step's context was ever observed canceled.
// Locked, not a raw field read: the write happens on the engine's own
// execute goroutine, synchronized with a caller here only through
// pollDirectRunState's e.mu-mediated happens-before chain (see that
// function's comment) -- reading through the same mutex the write used
// keeps that property robust rather than relying on the chain alone.
func (s *retryProbeStep) Canceled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.canceled
}

// pollDirectRunState drives GET /api/runs/{id} directly through h (no
// network) until it reports want or 2 seconds pass. Both this function's
// callers in TestRetryExecutionSurvivesRequestCancellation rely on it for
// more than convenience: engine.Get takes e.mu, and finish (which sets the
// terminal state this polls for) takes e.mu too, so observing the state
// change here is also what makes reading retryProbeStep.Canceled()
// afterwards race-free -- Retry itself returns as soon as the run is
// registered, well before the relaunched step's Run (and its write to
// canceled) has necessarily completed, so the HTTP response alone is not a
// safe synchronization point for that read.
func pollDirectRunState(t *testing.T, h http.Handler, cookie *http.Cookie, id string, want engine.State) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last engine.Run
	for time.Now().Before(deadline) {
		req := httptest.NewRequest(http.MethodGet, "/api/runs/"+id, nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if decErr := json.NewDecoder(rec.Body).Decode(&last); decErr != nil {
			t.Fatalf("decode error = %v", decErr)
		}
		if last.State == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("run never reached state %q, last = %q", want, last.State)
}

// saveCtxProbeStore blocks one chosen Save call until released, then records
// the ctx.Err() that call was handed. That context is the only thing
// handleCreateRun's and handleRetry's context.WithoutCancel actually governs:
// engine.Start and engine.Retry each pass their caller's context straight to
// store.Save (internal/engine/engine.go), and each rolls the run back and
// launches no goroutine at all if that Save fails.
//
// Probing the STORE, not the step, is the point. A step's context comes from
// engine.Retry's own context.WithoutCancel a few lines further down, so a
// test that watches the step passes whether or not the handler detached
// anything -- which is exactly how TestRetryExecutionSurvivesRequestCancellation
// (below) went green against the bug its comment names. Nothing but the
// checkpoint context distinguishes the two.
//
// It embeds a real store rather than stubbing every method, so Load,
// LoadCurrent, and Delete behave normally and only Save is instrumented.
type saveCtxProbeStore struct {
	engine.Store

	entered chan struct{}
	release chan struct{}

	mu       sync.Mutex
	armed    bool
	probed   bool
	observed error
}

// Arm makes the next Save the probed one. Called at the point in each test
// where the interesting checkpoint is about to be issued, rather than
// counting Save calls from process start -- the number of checkpoints a run
// takes before that point is an engine implementation detail no test in this
// package should have to track.
func (s *saveCtxProbeStore) Arm() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armed = true
}

func (s *saveCtxProbeStore) Save(ctx context.Context, r *engine.Run) error {
	s.mu.Lock()
	probe := s.armed && !s.probed
	if probe {
		s.probed = true
	}
	s.mu.Unlock()
	if !probe {
		return s.Store.Save(ctx, r)
	}

	close(s.entered)
	<-s.release
	err := ctx.Err()
	s.mu.Lock()
	s.observed = err
	s.mu.Unlock()
	if err != nil {
		// A real store fails a write under a canceled context; returning nil
		// here would hide the rollback that failure triggers and leave only
		// the recorded error to fail on.
		return err
	}
	return s.Store.Save(ctx, r)
}

// Observed reports the ctx.Err() the probed Save saw, or nil if it was never
// reached.
func (s *saveCtxProbeStore) Observed() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.observed
}

// awaitProbe blocks until the probed Save is in flight, cancels the request,
// gives an incorrectly-threaded cancellation a generous window to reach the
// store's context, and releases the call.
func awaitProbe(t *testing.T, store *saveCtxProbeStore, cancel context.CancelFunc) {
	t.Helper()
	select {
	case <-store.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the probed checkpoint Save was never reached")
	}
	cancel()
	time.Sleep(50 * time.Millisecond)
	close(store.release)
}

func newProbeStore() *saveCtxProbeStore {
	return &saveCtxProbeStore{
		Store:   engine.NewMemoryStore(),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

// TestCreateRunCheckpointSurvivesRequestCancellation is handleCreateRun's
// half of the contract, which had no test at all. engine.Start writes its
// first checkpoint before launching the run's goroutine and rolls the whole
// run back if that write fails, so a request context reaching it means a
// browser tab closing during the round trip leaves no run started and a 500
// where the SPA expects a 202.
func TestCreateRunCheckpointSurvivesRequestCancellation(t *testing.T) {
	b := bus.New(64)
	store := newProbeStore()
	store.Arm() // Start's own checkpoint is the very first Save this store sees.
	e := engine.New(b, store, failingStep{})
	srv, err := api.New(api.Config{
		Cluster: connectedCluster(),
		Token:   testToken,
		AICR:    &aicrclient.Fake{}, WorkDir: t.TempDir(),
	}, b, e, testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	h := srv.Handler()
	cookie := directSession(t, h)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/api/runs", strings.NewReader("{}")).WithContext(ctx)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	awaitProbe(t, store, cancel)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("create request never returned after the checkpoint unblocked")
	}

	if got := store.Observed(); got != nil {
		t.Errorf("Start's checkpoint context was canceled (%v) by the request's own cancellation -- handleCreateRun must detach the execution context, or a closed tab rolls the run back before its goroutine is ever launched", got)
	}
	if rec.Code != http.StatusAccepted {
		t.Errorf("create status = %d, want %d", rec.Code, http.StatusAccepted)
	}
}

// TestRetryCheckpointSurvivesRequestCancellation is the assertion
// TestRetryExecutionSurvivesRequestCancellation below was supposed to make.
// That test is satisfied by engine.Retry's own context.WithoutCancel on the
// execution context, so reverting handleRetry to r.Context() leaves it -- and
// the rest of internal/api -- green. This one watches the retry checkpoint
// instead, which is governed by nothing but the handler's choice.
func TestRetryCheckpointSurvivesRequestCancellation(t *testing.T) {
	b := bus.New(64)
	store := newProbeStore()
	e := engine.New(b, store, failingStep{})
	srv, err := api.New(api.Config{
		Cluster: connectedCluster(),
		Token:   testToken,
		AICR:    &aicrclient.Fake{}, WorkDir: t.TempDir(),
	}, b, e, testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	h := srv.Handler()
	cookie := directSession(t, h)

	createReq := httptest.NewRequest(http.MethodPost, "/api/runs", strings.NewReader("{}"))
	createReq.AddCookie(cookie)
	createRec := httptest.NewRecorder()
	h.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusAccepted {
		t.Fatalf("create status = %d, want %d", createRec.Code, http.StatusAccepted)
	}
	var created engine.Run
	if decErr := json.NewDecoder(createRec.Body).Decode(&created); decErr != nil {
		t.Fatalf("decode error = %v", decErr)
	}
	pollDirectRunState(t, h, cookie, created.ID, engine.StateFailed)

	// Armed only now: everything before this is the failing run's own
	// checkpoints, and the next Save is Retry's.
	store.Arm()

	ctx, cancel := context.WithCancel(context.Background())
	retryReq := httptest.NewRequest(http.MethodPost, "/api/runs/"+created.ID+"/retry", nil).WithContext(ctx)
	retryReq.AddCookie(cookie)
	retryRec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(retryRec, retryReq)
		close(done)
	}()

	awaitProbe(t, store, cancel)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("retry request never returned after the checkpoint unblocked")
	}

	if got := store.Observed(); got != nil {
		t.Errorf("Retry's checkpoint context was canceled (%v) by the request's own cancellation -- handleRetry must detach the execution context, or a closed tab rolls the retry back to StateFailed and no goroutine is ever launched", got)
	}
	if retryRec.Code != http.StatusOK {
		t.Errorf("retry status = %d, want %d", retryRec.Code, http.StatusOK)
	}
}

// TestRetryExecutionSurvivesRequestCancellation is the regression test for
// handleRetry's context.WithoutCancel(r.Context()) call: reverting it to
// r.Context() reintroduces "a closed browser tab cancels a 10-20 minute
// Apply retry mid-flight" while every other test in this package and
// internal/engine stays green, which is exactly why this needed its own
// test rather than relying on the suite to catch it incidentally.
//
// Kept, but it is not the assertion its name promises: the step's context
// comes from engine.Retry's own context.WithoutCancel (internal/engine/
// engine.go), so this passes with handleRetry reverted. What it does still
// pin is that the engine's detachment holds, which is worth keeping.
// TestRetryCheckpointSurvivesRequestCancellation above is what bites on the
// handler.
func TestRetryExecutionSurvivesRequestCancellation(t *testing.T) {
	b := bus.New(64)
	step := &retryProbeStep{entered: make(chan struct{}), release: make(chan struct{})}
	e := engine.New(b, engine.NewMemoryStore(), step)
	srv, err := api.New(api.Config{
		Cluster: connectedCluster(),
		Token:   testToken,
		AICR:    &aicrclient.Fake{}, WorkDir: t.TempDir(),
	}, b, e, testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	h := srv.Handler()
	cookie := directSession(t, h)

	createReq := httptest.NewRequest(http.MethodPost, "/api/runs", strings.NewReader("{}"))
	createReq.AddCookie(cookie)
	createRec := httptest.NewRecorder()
	h.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusAccepted {
		t.Fatalf("create status = %d, want %d", createRec.Code, http.StatusAccepted)
	}
	var created engine.Run
	if decErr := json.NewDecoder(createRec.Body).Decode(&created); decErr != nil {
		t.Fatalf("decode error = %v", decErr)
	}

	pollDirectRunState(t, h, cookie, created.ID, engine.StateFailed)

	// Retry with a cancelable context, canceled only once the relaunched
	// step is confirmed mid-flight -- a context canceled before the request
	// is even sent would never reach the handler at all with a real
	// net/http.Client (the Transport refuses to send an already-canceled
	// request), which is why this drives the handler directly rather than
	// through httptest.Server.
	ctx, cancel := context.WithCancel(context.Background())
	retryReq := httptest.NewRequest(http.MethodPost, "/api/runs/"+created.ID+"/retry", nil).WithContext(ctx)
	retryReq.AddCookie(cookie)
	retryRec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(retryRec, retryReq)
		close(done)
	}()

	select {
	case <-step.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("retried step was never entered")
	}
	cancel()
	// A generous window for an incorrectly-threaded cancellation to
	// propagate to the step's context before it is inspected below.
	time.Sleep(50 * time.Millisecond)
	close(step.release)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("retry request never returned")
	}
	if retryRec.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want %d", retryRec.Code, http.StatusOK)
	}

	// Retry (and so the HTTP response above) returns as soon as the run is
	// registered, not once the relaunched step finishes -- wait for the
	// run's own terminal state before reading Canceled(), see
	// pollDirectRunState's comment for why that also makes the read safe.
	pollDirectRunState(t, h, cookie, created.ID, engine.StateDone)

	if step.Canceled() {
		t.Error("the retried step's context was canceled by the request's own cancellation -- handleRetry must detach the execution context")
	}
}

package api_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/api"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
	"github.com/mchmarny/aicrme/internal/testfs"
)

// fakeCluster stands in for internal/console's connector. connected is what
// the gate reads; the rest records what the two connect routes were asked.
type fakeCluster struct {
	connected    bool
	contexts     any
	contextsErr  error
	connectErr   error
	connectedTo  string
	connectCalls int
}

func (c *fakeCluster) Contexts() (any, error) {
	if c.contextsErr != nil {
		return nil, c.contextsErr
	}
	return c.contexts, nil
}

func (c *fakeCluster) Connect(_ context.Context, contextName string) (any, error) {
	c.connectCalls++
	c.connectedTo = contextName
	if c.connectErr != nil {
		return nil, c.connectErr
	}
	c.connected = true
	return map[string]string{"context": contextName}, nil
}

func (c *fakeCluster) Connected() bool { return c.connected }

// connectedCluster is the state every test that exercises a cluster-touching
// route needs: a console that has already connected.
func connectedCluster() *fakeCluster { return &fakeCluster{connected: true} }

// newTestServer is the default server every test that does not care about
// connect state uses: a launch token, a fake AICR client, and a cluster
// already connected. It lived in auth_test.go until the password auth was
// deleted.
func newTestServer(t *testing.T) http.Handler {
	t.Helper()
	return serverWithCluster(t, connectedCluster())
}

func serverWithCluster(t *testing.T, cluster api.Cluster) http.Handler {
	t.Helper()
	srv, err := api.New(api.Config{
		Token:   testToken,
		AICR:    &aicrclient.Fake{},
		WorkDir: t.TempDir(),
		Cluster: cluster,
	}, bus.New(64), engine.New(bus.New(64), engine.NewMemoryStore()), testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	return srv.Handler()
}

// A nil Cluster is refused rather than defaulted: a permissive default would
// leave every cluster-touching route ungated, and the routes would answer --
// just about no cluster in particular.
func TestNewRefusesANilCluster(t *testing.T) {
	_, err := api.New(api.Config{
		Token: testToken,
		AICR:  &aicrclient.Fake{}, WorkDir: t.TempDir(),
	}, bus.New(64), engine.New(bus.New(64), engine.NewMemoryStore()), testfs.Static())
	if err == nil {
		t.Fatal("api.New() accepted a nil Cluster -- every gated route would answer without a connection")
	}
}

// The gate is what makes a run impossible before a cluster is chosen. Without
// it, POST /api/runs starts a run against whatever ambient config the
// process happens to resolve.
func TestGatedRoutesConflictUntilConnected(t *testing.T) {
	h := serverWithCluster(t, &fakeCluster{connected: false})
	jar := sessionJar(t, h)

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/runs"},
		{http.MethodGet, "/api/options"},
		{http.MethodGet, "/api/runs/abcdef0123456789"},
		{http.MethodDelete, "/api/runs/abcdef0123456789"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := do(t, h, jar, tc.method, tc.path, "")
			if rec.Code != http.StatusConflict {
				t.Errorf("%s %s = %d, want %d before a cluster is connected", tc.method, tc.path, rec.Code, http.StatusConflict)
			}
		})
	}
}

// Choosing a cluster cannot be gated on having chosen one, and the session
// probe answers a question about the session rather than the cluster.
func TestConnectRoutesAnswerBeforeAnyConnection(t *testing.T) {
	cluster := &fakeCluster{contexts: []map[string]any{{"name": "kind-kind", "current": true}}}
	h := serverWithCluster(t, cluster)
	jar := sessionJar(t, h)

	if rec := do(t, h, jar, http.MethodGet, "/api/contexts", ""); rec.Code != http.StatusOK {
		t.Errorf("GET /api/contexts = %d before connecting, want 200 -- it is how a cluster gets chosen", rec.Code)
	}
	if rec := do(t, h, jar, http.MethodGet, "/api/session", ""); rec.Code != http.StatusNoContent {
		t.Errorf("GET /api/session = %d before connecting, want 204 -- the session outlives the cluster choice", rec.Code)
	}

	rec := do(t, h, jar, http.MethodPost, "/api/connect", `{"context":"kind-kind"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/connect = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if cluster.connectedTo != "kind-kind" {
		t.Errorf("Connect() got context %q, want the one the request named", cluster.connectedTo)
	}

	// And the gate opens behind it.
	if rec := do(t, h, jar, http.MethodGet, "/api/options", ""); rec.Code == http.StatusConflict {
		t.Error("GET /api/options still conflicts after a successful connect")
	}
}

func TestConnectRejectsAnEmptyContext(t *testing.T) {
	cluster := connectedCluster()
	cluster.connected = false
	h := serverWithCluster(t, cluster)
	jar := sessionJar(t, h)

	rec := do(t, h, jar, http.MethodPost, "/api/connect", `{"context":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("POST /api/connect with an empty context = %d, want 400", rec.Code)
	}
	if cluster.connectCalls != 0 {
		t.Error("an empty context reached the connector")
	}
}

// A second connect is a conflict, not a switch: the connection is
// single-assignment, and the error has to survive the trip to the SPA as a
// 409 so it can say so rather than showing a generic failure.
func TestSecondConnectSurfacesAsConflict(t *testing.T) {
	cluster := connectedCluster()
	cluster.connectErr = aicrerrors.New(aicrerrors.ErrCodeConflict, "already connected")
	h := serverWithCluster(t, cluster)
	jar := sessionJar(t, h)

	rec := do(t, h, jar, http.MethodPost, "/api/connect", `{"context":"other"}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("POST /api/connect after connecting = %d, want %d", rec.Code, http.StatusConflict)
	}
}

func TestContextsSurfacesAnUnreadableKubeconfig(t *testing.T) {
	cluster := &fakeCluster{contextsErr: errors.New("reading kubeconfig: permission denied")}
	h := serverWithCluster(t, cluster)
	jar := sessionJar(t, h)

	rec := do(t, h, jar, http.MethodGet, "/api/contexts", "")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("GET /api/contexts with an unreadable kubeconfig = %d, want 500", rec.Code)
	}
}

// sessionJar exchanges the launch token and returns the session cookie every
// request below carries. The connect gate runs inside the token gate, so a
// test of the gate has to authenticate first.
func sessionJar(t *testing.T, h http.Handler) []*http.Cookie {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/session", strings.NewReader(`{"token":"`+testToken+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST /api/session = %d, want 204", rec.Code)
	}
	return rec.Result().Cookies()
}

func do(t *testing.T, h http.Handler, jar []*http.Cookie, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	for _, c := range jar {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

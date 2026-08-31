package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/api"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/clear"
	"github.com/mchmarny/aicrme/internal/engine"
	"github.com/mchmarny/aicrme/internal/testfs"
)

type fakeSurveyor struct {
	survey *clear.Survey
	err    error
	uid    string
	calls  int
}

func (f *fakeSurveyor) Survey(_ context.Context, clusterUID string) (*clear.Survey, error) {
	f.calls++
	f.uid = clusterUID
	return f.survey, f.err
}

// serverWithSurveyor mirrors serverWithCluster, which takes no Surveyor. A
// nil sv is a real case: it is how the 503 branch is exercised.
func serverWithSurveyor(t *testing.T, cluster api.Cluster, sv api.Surveyor) http.Handler {
	t.Helper()
	srv, err := api.New(api.Config{
		Token:    testToken,
		AICR:     &aicrclient.Fake{},
		WorkDir:  t.TempDir(),
		Cluster:  cluster,
		Surveyor: sv,
	}, bus.New(64), engine.New(bus.New(64), engine.NewMemoryStore()), testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	return srv.Handler()
}

func getSurvey(t *testing.T, h http.Handler) *http.Response {
	t.Helper()
	ts, client := authedClient(t, h)
	res, err := client.Get(ts.URL + "/api/cluster/survey")
	if err != nil {
		t.Fatalf("GET /api/cluster/survey error = %v", err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

// The survey is offered at Connect, before any run exists, so it must live in
// the pre-connect route group beside GET /api/cluster. A gated route would 409
// on exactly the screen this feature is for.
func TestSurveyIsServedBeforeARunExists(t *testing.T) {
	f := &fakeSurveyor{survey: &clear.Survey{
		ClusterUID: "cluster-uid-1",
		Complete:   true,
		DriverMode: clear.DriverHost,
		Releases:   []clear.Release{{Name: "gpu-operator", Namespace: "gpu-operator"}},
	}}
	res := getSurvey(t, serverWithSurveyor(t, connectedCluster(), f)) //nolint:bodyclose // getSurvey registers the close via t.Cleanup

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var got clear.Survey
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decoding survey: %v", err)
	}
	if len(got.Releases) != 1 {
		t.Errorf("got %d releases, want 1", len(got.Releases))
	}
	if got.DriverMode != clear.DriverHost {
		t.Errorf("DriverMode = %q, want it carried through the wire", got.DriverMode)
	}
}

// The UID must come from the connection this process holds, never from the
// request: a client-supplied UID would defeat the guard it exists for.
func TestSurveyUsesTheConnectedClusterUID(t *testing.T) {
	f := &fakeSurveyor{survey: &clear.Survey{}}
	getSurvey(t, serverWithSurveyor(t, connectedCluster(), f)) //nolint:bodyclose // getSurvey registers the close via t.Cleanup

	if f.uid != "cluster-uid-1" {
		t.Errorf("Survey called with uid %q, want the connected cluster's", f.uid)
	}
}

func TestSurveyRefusesWhenNotConnected(t *testing.T) {
	f := &fakeSurveyor{survey: &clear.Survey{}}
	res := getSurvey(t, serverWithSurveyor(t, &fakeCluster{}, f)) //nolint:bodyclose // getSurvey registers the close via t.Cleanup

	if res.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409 when no cluster is connected", res.StatusCode)
	}
	if f.calls != 0 {
		t.Error("the surveyor ran against no cluster")
	}
}

// 503 rather than 404, so the SPA can tell "this console cannot do that" from
// "that route does not exist".
func TestSurveyReports503WithNoSurveyorConfigured(t *testing.T) {
	res := getSurvey(t, serverWithSurveyor(t, connectedCluster(), nil)) //nolint:bodyclose // getSurvey registers the close via t.Cleanup

	if res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", res.StatusCode)
	}
}

// A failure must surface as one. Smoothing it into an empty survey would tell
// an operator their cluster is clean when this console could not look.
func TestSurveySurfacesAFailureRatherThanAnEmptyResult(t *testing.T) {
	f := &fakeSurveyor{err: errors.New("helm: not found")}
	res := getSurvey(t, serverWithSurveyor(t, connectedCluster(), f)) //nolint:bodyclose // getSurvey registers the close via t.Cleanup

	if res.StatusCode == http.StatusOK {
		t.Fatal("status = 200 on a failed survey; an empty result reads as a clean cluster")
	}
}

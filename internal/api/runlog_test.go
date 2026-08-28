package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/mchmarny/aicrme/internal/api"
	"github.com/mchmarny/aicrme/internal/bus"
)

// runLogOf fetches and decodes the export for one run.
func runLogOf(t *testing.T, client *http.Client, base, runID string) api.RunLog {
	t.Helper()
	resp, err := client.Get(base + "/api/runs/" + runID + "/log")
	if err != nil {
		t.Fatalf("GET log error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var got api.RunLog
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode error = %v", err)
	}
	return got
}

// The whole point: everything the timeline shows lives in an in-memory ring
// and dies with the process, so on real hardware the log from the run that
// just failed is the one an operator cannot keep. The events were reachable
// only as SSE, which never ends and therefore cannot be saved by a browser.
func TestRunLogExportsTheRecordAndTheEvents(t *testing.T) {
	ts, client, runID := newBundleTestServer(t, t.TempDir(), rawBundlePathStep{})

	got := runLogOf(t, client, ts.URL, runID)

	if got.Run == nil || got.Run.ID != runID {
		t.Fatalf("Run = %+v, want the record for %s", got.Run, runID)
	}
	if len(got.Events) == 0 {
		t.Fatal("Events is empty -- the export carries the record without the story")
	}
	// The epoch identifies the process whose ring these came from: two logs
	// from either side of a restart both count ids from 1, and pasted
	// together would look like one stream with an inexplicable jump.
	if got.Epoch == "" {
		t.Error("Epoch is empty -- nothing identifies which console produced this")
	}
	if got.ExportedAt.IsZero() {
		t.Error("ExportedAt is zero -- a log pulled mid-run must be distinguishable from a final one")
	}
}

// The ring is shared by every run this process has served. A file labeled
// with one run id carrying another run's events would be worse than no
// export at all -- it is evidence, and it would be evidence about the wrong
// thing.
func TestRunLogCarriesOnlyTheRequestedRun(t *testing.T) {
	ts, client, runID := newBundleTestServer(t, t.TempDir(), rawBundlePathStep{})

	got := runLogOf(t, client, ts.URL, runID)

	for _, e := range got.Events {
		if e.RunID != runID {
			t.Fatalf("event %d belongs to run %q, want only %q", e.ID, e.RunID, runID)
		}
	}
}

// An export names a file someone attaches to a bug report. "download.json"
// in a downloads folder is unidentifiable a day later.
func TestRunLogDownloadsAsAFileNamedForTheRun(t *testing.T) {
	ts, client, runID := newBundleTestServer(t, t.TempDir(), rawBundlePathStep{})

	resp, err := client.Get(ts.URL + "/api/runs/" + runID + "/log")
	if err != nil {
		t.Fatalf("GET log error = %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	want := `attachment; filename="aicrme-` + runID + `.json"`
	if cd := resp.Header.Get("Content-Disposition"); cd != want {
		t.Errorf("Content-Disposition = %q, want %q", cd, want)
	}
}

// A log for a run this console never had is a 404, not an empty export: an
// empty file would read as "the run produced nothing".
func TestRunLogIsNotFoundForAnUnknownRun(t *testing.T) {
	ts, client, _ := newBundleTestServer(t, t.TempDir(), rawBundlePathStep{})

	resp, err := client.Get(ts.URL + "/api/runs/does-not-exist/log")
	if err != nil {
		t.Fatalf("GET log error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

// Truncated is reported rather than left to be inferred. A reader chasing a
// missing event needs to know whether it was never published or merely aged
// out of the ring, and comparing a replay's length against a constant the
// caller declared itself compares against an assumption, not against the Bus.
func TestBusReportsItsOwnCapacity(t *testing.T) {
	if got := bus.New(64).Capacity(); got != 64 {
		t.Errorf("Capacity() = %d, want 64", got)
	}
}

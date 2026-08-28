package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
)

// RunLog is one run's whole story in one file: the record as the store holds
// it, and every event this process published for it.
//
// The two halves answer different questions and neither substitutes for the
// other. The record carries state, phase, ownership, the resolved toolchain
// and the component list -- the things a reader needs to know what the run
// WAS. The events carry what happened, in order, including the diagnostic
// tail of a failure, which exists nowhere else.
type RunLog struct {
	// ExportedAt dates the file rather than the run: a log pulled while a run
	// is still going is a legitimate and common thing to want, and the reader
	// has to be able to tell that from a final one.
	ExportedAt time.Time `json:"exportedAt"`
	// Epoch identifies the console process whose ring these events came from.
	// A log from a restarted console is a different epoch with ids counting
	// from 1 again, and two files pasted together would otherwise look like
	// one stream with an inexplicable jump.
	Epoch string `json:"epoch"`
	// Truncated marks a run whose events may have aged out of the replay
	// ring. Stated rather than inferred: a reader debugging a missing event
	// needs to know whether it was never published or merely evicted.
	Truncated bool        `json:"truncated"`
	Run       *engine.Run `json:"run"`
	Events    []bus.Event `json:"events"`
}

// handleRunLog exports one run as a single JSON document.
//
// WHY THIS EXISTS: everything the timeline shows lives only in an in-memory
// ring (internal/console, replayCapacity), and stopping the console discards
// it. On real hardware that means the log an operator most needs -- the one
// from the run that just failed -- is the one they cannot keep. The event
// stream was already reachable at GET /api/events?since=0, but only as SSE:
// it never ends, so a browser cannot save it and a human ends up piping curl
// through sed. This is the same data as a file.
//
// It does NOT make the events durable. A console that is killed mid-Apply
// still loses them, and fixing that means writing the stream to disk as it is
// published -- a real design decision about retention and disk use that this
// route deliberately does not pre-empt. What it removes is the case where the
// events still exist and there is no way to get them out.
func (s *Server) handleRunLog(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	run, err := s.engine.Get(r.Context(), runID)
	if err != nil {
		writeErr(w, err)
		return
	}

	// Replay(0) asks for the whole ring, then filtered to this run: the ring
	// is shared by every run this process has served, and a log labeled with
	// one run id must not carry another's events.
	all := s.bus.Replay(0)
	events := make([]bus.Event, 0, len(all))
	for _, e := range all {
		if e.RunID == runID {
			events = append(events, e)
		}
	}

	payload := RunLog{
		ExportedAt: time.Now().UTC(),
		Epoch:      s.bus.Epoch(),
		// The ring evicts oldest-first, so a full ring is the signal that
		// something may already be gone. It is a may, not a did -- this run's
		// own events may all still be present -- which is why the field is
		// named for the risk rather than asserting a loss.
		Truncated: len(all) >= s.bus.Capacity(),
		Run:       run,
		Events:    events,
	}

	// An attachment with the run id in the name: this is a file someone
	// attaches to a bug report, and "download.json" in a downloads folder is
	// unidentifiable a day later.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", "aicrme-"+runID+".json"))
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(payload); err != nil {
		// The header is already written, so there is no status left to send.
		// Logged by the server's own middleware; nothing useful can be added
		// to a half-written body.
		return
	}
}

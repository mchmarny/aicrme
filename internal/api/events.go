package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// sseKeepalive bounds proxy idle timeouts on a quiet run.
const sseKeepalive = 20 * time.Second

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// The epoch identifies this process's Bus. It travels ahead of any
	// replay so the SPA can detect a restart: nextID resets to 1 on every
	// process start, so a client's lastId from a previous process looks
	// like a valid-but-stale cursor rather than an obviously wrong one, and
	// the stream would otherwise deliver nothing at or below it -- silently.
	// It is named ("event: epoch") so EventSource routes it to a dedicated
	// listener instead of onmessage, where run data lands, and it carries no
	// id: field -- assigning one would advance the very cursor the epoch
	// exists to let the client correct.
	fmt.Fprintf(w, "event: epoch\ndata: {\"epoch\":%q}\n\n", s.bus.Epoch())
	flusher.Flush()

	ch, cancel := s.bus.Subscribe(lastEventID(r))
	defer cancel()

	ticker := time.NewTicker(sseKeepalive)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case ev, open := <-ch:
			if !open {
				return
			}
			payload, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			// id + data only, no event: type -- unlike the epoch frame above,
			// run data must land in the browser's onmessage handler, which is
			// where the SPA listens; Kind already travels inside the JSON
			// payload, so there's nothing an event: name would add here.
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ev.ID, payload)
			flusher.Flush()
		}
	}
}

// lastEventID reads the reconnect cursor from the SSE header, falling back to
// the ?since= query param used by the test harness and by manual curl.
func lastEventID(r *http.Request) uint64 {
	for _, raw := range []string{r.Header.Get("Last-Event-ID"), r.URL.Query().Get("since")} {
		if raw == "" {
			continue
		}
		if n, err := strconv.ParseUint(raw, 10, 64); err == nil {
			return n
		}
	}
	return 0
}

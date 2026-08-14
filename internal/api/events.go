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
			// id + data only. Naming an `event:` type would route frames away
			// from the browser's onmessage handler, which is where the SPA
			// listens; Kind already travels inside the JSON payload.
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

package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mchmarny/aicrme/internal/api"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
	"github.com/mchmarny/aicrme/internal/testfs"
)

func TestCreateAndGetRun(t *testing.T) {
	b := bus.New(64)
	srv, err := api.New(api.Config{
		Username: "admin", Password: "correct-horse", SessionTTL: time.Hour, LoginRate: 100,
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

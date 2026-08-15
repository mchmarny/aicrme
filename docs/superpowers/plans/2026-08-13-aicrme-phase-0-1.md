# aicrme Phases 0-1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the aicrme skeleton (repo, CI, chart, auth, SSE bus, embedded SPA) and the first two wizard phases (Discover, Recommend) running for real against a Kind/KWOK cluster.

**Architecture:** One Go binary serving an embedded React SPA. A `bus` fans typed events to SSE subscribers with ring-buffer replay; an `engine` drives a linear `Step` state machine that parks for user decisions; `steps` wrap the pinned `github.com/NVIDIA/aicr` module through narrow interfaces so every step is testable with a fake. `api` is thin HTTP over `engine` + `bus`.

**Tech Stack:** Go 1.26.5, `github.com/NVIDIA/aicr v0.19.0`, `client-go`, Vite + React + TypeScript + Tailwind, Vitest, Helm, Kind/KWOK, GitHub Actions.

**Spec:** `approach.md` (repo root)

---

## Spec corrections applied by this plan

These were verified against the AICR checkout at `/Users/mchmarny/dev/aicr` (HEAD `a3390836`, latest tag `v0.19.0`). They override the spec where they conflict.

1. **Risk 1 is resolved upstream.** `pkg/client/v1` already exports
   `CollectSnapshot(ctx, *AgentConfig) (*Snapshot, error)` and
   `ValidateState(ctx, *RecipeResult, *Snapshot, ...ValidateOption) ([]*PhaseResult, error)` —
   both present at `v0.19.0`. `pkg/validator` is **not** a dependency. Open Question 2
   (upstreaming `Snapshot()`/`Validate()`) is moot; the project's upstream-PR budget is
   reallocated to the deploy.sh event stream (item 3).
2. **The recipe catalog and checks are embedded in the module.** `recipes/data.go` embeds
   `overlays/*.yaml mixins/*.yaml registry.yaml validators/catalog.yaml components/*/*.yaml
   components/*/manifests/*.yaml checks/*/*.yaml`. `aicr.EmbeddedSource()` gives the console
   the full catalog with no `recipes/` tree in the image. Spec Risk 5 (pinned catalog
   snapshot) stands and is the correct framing.
3. **The applier drives `deploy.sh`, not per-component `install.sh`.** Per-component
   `NNN-<name>/install.sh` files do exist, but orchestration lives in a generated 480-line
   `deploy.sh` carrying correctness logic a naive per-component loop drops: preflight
   (terminating namespaces, stale webhooks, orphaned CRD groups), per-component wait
   derivation (`kai-scheduler` 20m/1 retry, `*-readiness` gates 1h35m, `ASYNC_COMPONENTS`
   skip `--wait`), quadratic-backoff retry with helm hook-Job cleanup, and a post-install
   block that waits for `nvidia.com/gpu-driver-upgrade-state=upgrade-done` on every managed
   node before restarting the DRA kubelet plugin (AICR issue #973 — skipping it strands DRA
   pods in `ContainerCreating`). Decision: exec `deploy.sh` as one subprocess with
   `NO_COLOR=1` and parse its stable step markers into typed events. Phase 2 work; the marker
   grammar is recorded in the roadmap below so this analysis is not lost.
4. **`demos/workloads/training/` contains only `gke-nccl-test-tcpxo.yaml`.** The spec's
   "Training + Kubeflow → TrainJob NCCL all-reduce" close has no existing source material for
   EKS. Inference is well covered (`vllm-agg.yaml`, `nimservice-llama-3-2-1b.yaml`,
   `chat-server.sh`, `chat.html`). The training TrainJob must be authored in Phase 3.
5. **AICR's version single-source-of-truth is `.go-version` + `.settings.yaml`**, not
   `.versions.yaml`. This plan mirrors that convention.

---

## Global Constraints

- Go toolchain `1.26.5`, pinned in `.go-version`, read by Makefile and CI. Never hardcode elsewhere.
- `github.com/NVIDIA/aicr v0.19.0` — exact pin. Bumps are deliberate, never `@latest`.
- Import **only** `github.com/NVIDIA/aicr/pkg/client/v1`, `.../pkg/errors`, `.../pkg/snapshotter`, `.../pkg/measurement`, `.../pkg/fingerprint`, `.../pkg/recipe` (for `Criteria` construction). Anything else needs a recorded justification.
- All errors constructed with `github.com/NVIDIA/aicr/pkg/errors` (`errors.New(code, msg)`, `errors.Wrap(code, msg, err)`).
- Tests are table-driven, run with `-race`, coverage floor **80%** enforced from `.settings.yaml` `quality.coverage_threshold`.
- Never skip or disable an existing test.
- MIT license header not required (repo is MIT via root `LICENSE`); do not copy AICR's Apache headers.
- Commit to `main`, sign commits with `-S`, no `Co-Authored-By`, no sign-off.
- Single replica. No database. No CRDs. No sidecars.
- SSE only for server→client streaming. No WebSocket.
- Product boundary: demo and eval clusters only. `cluster-admin` is stated plainly in NOTES.txt and README.

---

## File Structure

```
cmd/aicrme/main.go            flag parsing, wiring, graceful shutdown
internal/bus/event.go         Event type + Kind/Level constants (the cross-unit contract)
internal/bus/bus.go           fan-out + ring buffer + replay
internal/engine/run.go        Run, State, Phase types
internal/engine/engine.go     linear Step state machine, decision parking
internal/engine/store.go      Store interface + memory impl
internal/steps/step.go        Step interface, shared Emit helper
internal/steps/discover.go    CollectSnapshot -> Run.Snapshot + criteria
internal/steps/recommend.go   ResolveRecipeFromSnapshot -> Run.Recipe
internal/aicrclient/client.go narrow interfaces over aicr.Client (Snapshotter, Resolver)
internal/gap/gap.go           snapshot -> capability statement + gap list
internal/api/server.go        router, middleware
internal/api/auth.go          session cookie, constant-time compare, rate limit
internal/api/events.go        SSE handler with Last-Event-ID replay
internal/api/runs.go          POST /api/runs, POST /api/runs/{id}/decide, GET /api/runs/{id}
web/                          Vite + React + TS + Tailwind, built to web/dist
internal/web/embed.go         //go:embed dist -> http.FS
charts/aicrme/                the single chart
.go-version .settings.yaml Makefile Dockerfile .github/workflows/ci.yaml
```

Boundary rules: `bus` imports nothing from the project. `engine` imports `bus`. `steps` imports `bus`, `engine`, `aicrclient`, `gap`. `api` imports `bus`, `engine`, and `pkg/errors` only — no other AICR imports, no business logic. `pkg/errors` is permitted because mapping structured error codes onto HTTP status is presentation work, and AICR's own `pkg/server` does exactly this.

---

## Phase 0 — Skeleton

### Task 1: Project scaffold, version pinning, CI

**Files:**
- Create: `.go-version`, `.settings.yaml`, `go.mod`, `Makefile`, `.github/workflows/ci.yaml`, `.golangci.yaml`, `internal/version/version.go`
- Test: `internal/version/version_test.go`

**Interfaces:**
- Produces: `version.Version` (string, ldflags-injected), `version.String() string`

- [ ] **Step 1: Create the version pins**

`.go-version`:
```
1.26.5
```

`.settings.yaml`:
```yaml
# Go toolchain version is owned by .go-version (single source of truth).
quality:
  coverage_threshold: 80

linting:
  golangci_lint: 'v2.12.2'

build_tools:
  ko: 'v0.19.1'

test_tools:
  kind: 'v0.31.0'
  kwok: 'v0.8.0'
```

- [ ] **Step 2: Initialize the module and pin AICR**

```bash
go mod init github.com/mchmarny/aicrme
go get github.com/NVIDIA/aicr@v0.19.0
go mod tidy
```

Confirm `go.mod` shows `go 1.26.5` and `github.com/NVIDIA/aicr v0.19.0`. If the toolchain line disagrees with `.go-version`, fix `go.mod`, not `.go-version`.

- [ ] **Step 3: Write the failing test**

`internal/version/version_test.go`:
```go
package version_test

import (
	"testing"

	"github.com/mchmarny/aicrme/internal/version"
)

func TestString(t *testing.T) {
	tests := []struct {
		name    string
		version string
		commit  string
		want    string
	}{
		{name: "dev default", version: "dev", commit: "none", want: "dev (none)"},
		{name: "release", version: "v0.1.0", commit: "abc1234", want: "v0.1.0 (abc1234)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			version.Version = tc.version
			version.Commit = tc.commit
			if got := version.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 4: Run test to verify it fails**

Run: `go test ./internal/version/ -run TestString -v`
Expected: FAIL — `no required module provides package github.com/mchmarny/aicrme/internal/version`

- [ ] **Step 5: Write minimal implementation**

`internal/version/version.go`:
```go
// Package version carries build-time identity injected via ldflags.
package version

import "fmt"

var (
	// Version is the semantic version, overridden at build time.
	Version = "dev"
	// Commit is the git SHA, overridden at build time.
	Commit = "none"
)

// String returns the human-readable build identity.
func String() string {
	return fmt.Sprintf("%s (%s)", Version, Commit)
}
```

- [ ] **Step 6: Run test to verify it passes**

Run: `go test ./internal/version/ -race -v`
Expected: PASS

- [ ] **Step 7: Write the Makefile**

`Makefile`:
```makefile
GO_VERSION          := $(shell cat .go-version)
COVERAGE_THRESHOLD  ?= $(shell yq -r '.quality.coverage_threshold' .settings.yaml)
VERSION             ?= $(shell git describe --tags --always --dirty)
COMMIT              ?= $(shell git rev-parse --short HEAD)
LDFLAGS             := -X github.com/mchmarny/aicrme/internal/version.Version=$(VERSION) \
                       -X github.com/mchmarny/aicrme/internal/version.Commit=$(COMMIT)

.PHONY: all
all: help

.PHONY: help
help: ## Prints available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-22s\033[0m %s\n", $$1, $$2}'

.PHONY: tidy
tidy: ## Formats code and tidies modules
	go fmt ./...
	go mod tidy

.PHONY: lint
lint: ## Lints Go sources
	golangci-lint run --timeout 5m ./...

.PHONY: test
test: ## Runs unit tests with race detector and coverage
	go test -race -covermode=atomic -coverprofile=coverage.out ./...

.PHONY: test-coverage
test-coverage: test ## Runs tests and enforces the coverage floor
	@coverage=$$(go tool cover -func=coverage.out | grep total: | awk '{print substr($$3, 1, length($$3)-1)}'); \
	echo "Coverage: $$coverage% (threshold: $(COVERAGE_THRESHOLD)%)"; \
	if [ $$(echo "$$coverage < $(COVERAGE_THRESHOLD)" | bc) -eq 1 ]; then \
		echo "ERROR: coverage $$coverage% below threshold $(COVERAGE_THRESHOLD)%"; exit 1; \
	fi

.PHONY: web
web: ## Builds the SPA into web/dist
	cd web && npm ci && npm run build

.PHONY: build
build: web ## Builds the aicrme binary with the SPA embedded
	go build -ldflags "$(LDFLAGS)" -o bin/aicrme ./cmd/aicrme

.PHONY: qualify
qualify: lint test-coverage ## Full local gate — must match CI exactly
```

- [ ] **Step 8: Write the CI workflow**

`.github/workflows/ci.yaml`:
```yaml
name: ci
on:
  push:
    branches: [main]
  pull_request:

permissions:
  contents: read

jobs:
  qualify:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - id: go
        run: echo "version=$(cat .go-version)" >> "$GITHUB_OUTPUT"
      - uses: actions/setup-go@v6
        with:
          go-version: ${{ steps.go.outputs.version }}
      - uses: actions/setup-node@v5
        with:
          node-version: '24'
          cache: npm
          cache-dependency-path: web/package-lock.json
      - run: sudo apt-get update && sudo apt-get install -y bc
      - uses: mikefarah/yq@master
      - uses: golangci/golangci-lint-action@v8
        with:
          version: v2.12.2
      - run: make test-coverage
```

Note: `make web` is not in the CI gate until Task 5 creates `web/`. Add it to the `qualify` job in Task 5.

- [ ] **Step 9: Verify the whole gate passes**

Run: `make lint test-coverage`
Expected: PASS, coverage 100% for the single package.

- [ ] **Step 10: Commit**

```bash
git add .go-version .settings.yaml go.mod go.sum Makefile .golangci.yaml \
        .github/workflows/ci.yaml internal/version/
git commit -S -m "feat: project scaffold with pinned toolchain and CI gate"
```

---

### Task 2: Event bus with replay

**Files:**
- Create: `internal/bus/event.go`, `internal/bus/bus.go`
- Test: `internal/bus/bus_test.go`

**Interfaces:**
- Produces:
  - `bus.Event{ID uint64, RunID string, At time.Time, Kind Kind, Phase string, Level Level, Component string, Message string, Data json.RawMessage}`
  - `bus.New(capacity int) *Bus`
  - `(*Bus).Publish(e Event) Event` — assigns `ID`, returns the stamped copy
  - `(*Bus).Subscribe(since uint64) (<-chan Event, func())` — channel replays events with `ID > since`, then streams live; `func()` unsubscribes
  - `(*Bus).Replay(since uint64) []Event`
  - Kinds: `KindPhase`, `KindLog`, `KindComponent`, `KindCluster`, `KindDecision`, `KindError`
  - Levels: `LevelInfo`, `LevelWarn`, `LevelError`

- [ ] **Step 1: Write the failing test**

`internal/bus/bus_test.go`:
```go
package bus_test

import (
	"sync"
	"testing"
	"time"

	"github.com/mchmarny/aicrme/internal/bus"
)

func TestPublishAssignsMonotonicIDs(t *testing.T) {
	b := bus.New(8)
	first := b.Publish(bus.Event{Message: "a"})
	second := b.Publish(bus.Event{Message: "b"})

	if first.ID != 1 {
		t.Errorf("first ID = %d, want 1", first.ID)
	}
	if second.ID != 2 {
		t.Errorf("second ID = %d, want 2", second.ID)
	}
	if first.At.IsZero() {
		t.Error("Publish did not stamp At")
	}
}

func TestReplay(t *testing.T) {
	tests := []struct {
		name     string
		capacity int
		publish  int
		since    uint64
		wantIDs  []uint64
	}{
		{name: "from start", capacity: 8, publish: 3, since: 0, wantIDs: []uint64{1, 2, 3}},
		{name: "after id 2", capacity: 8, publish: 3, since: 2, wantIDs: []uint64{3}},
		{name: "caught up", capacity: 8, publish: 3, since: 3, wantIDs: nil},
		{name: "ring evicts oldest", capacity: 2, publish: 4, since: 0, wantIDs: []uint64{3, 4}},
		{name: "since beyond head", capacity: 8, publish: 2, since: 99, wantIDs: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := bus.New(tc.capacity)
			for i := 0; i < tc.publish; i++ {
				b.Publish(bus.Event{Message: "x"})
			}
			got := b.Replay(tc.since)
			if len(got) != len(tc.wantIDs) {
				t.Fatalf("Replay(%d) returned %d events, want %d", tc.since, len(got), len(tc.wantIDs))
			}
			for i, want := range tc.wantIDs {
				if got[i].ID != want {
					t.Errorf("event[%d].ID = %d, want %d", i, got[i].ID, want)
				}
			}
		})
	}
}

func TestSubscribeReplaysThenStreams(t *testing.T) {
	b := bus.New(8)
	b.Publish(bus.Event{Message: "before"})

	ch, cancel := b.Subscribe(0)
	defer cancel()

	if got := <-ch; got.Message != "before" {
		t.Fatalf("first event = %q, want %q", got.Message, "before")
	}

	b.Publish(bus.Event{Message: "after"})
	select {
	case got := <-ch:
		if got.Message != "after" {
			t.Errorf("second event = %q, want %q", got.Message, "after")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for live event")
	}
}

func TestSlowSubscriberDoesNotBlockPublish(t *testing.T) {
	b := bus.New(4)
	_, cancel := b.Subscribe(0) // never drained
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			b.Publish(bus.Event{Message: "flood"})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a slow subscriber")
	}
}

func TestConcurrentSubscribeUnsubscribe(t *testing.T) {
	b := bus.New(16)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, cancel := b.Subscribe(0)
			b.Publish(bus.Event{Message: "churn"})
			<-ch
			cancel()
		}()
	}
	wg.Wait()
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/bus/ -race -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write the Event contract**

`internal/bus/event.go`:
```go
// Package bus fans typed console events out to SSE subscribers and retains a
// bounded replay buffer so a reconnecting browser sees the whole run.
package bus

import (
	"encoding/json"
	"time"
)

// Kind classifies an event for UI routing.
type Kind string

const (
	// KindPhase marks a run-phase transition.
	KindPhase Kind = "phase"
	// KindLog is free-form narration.
	KindLog Kind = "log"
	// KindComponent is per-component install progress.
	KindComponent Kind = "component"
	// KindCluster is observer-sourced cluster telemetry.
	KindCluster Kind = "cluster"
	// KindDecision signals the run is parked awaiting user input.
	KindDecision Kind = "decision"
	// KindError is a terminal or retryable failure.
	KindError Kind = "error"
)

// Level is the severity used for UI emphasis.
type Level string

const (
	// LevelInfo is normal progress.
	LevelInfo Level = "info"
	// LevelWarn is surfaced, not buried; may be annotated as benign.
	LevelWarn Level = "warn"
	// LevelError is a failure.
	LevelError Level = "error"
)

// Event is the single wire shape every producer publishes and the SPA consumes.
// ID is assigned by the Bus and is the SSE Last-Event-ID cursor.
type Event struct {
	ID        uint64          `json:"id"`
	RunID     string          `json:"runId,omitempty"`
	At        time.Time       `json:"at"`
	Kind      Kind            `json:"kind"`
	Phase     string          `json:"phase,omitempty"`
	Level     Level           `json:"level"`
	Component string          `json:"component,omitempty"`
	Message   string          `json:"message"`
	Data      json.RawMessage `json:"data,omitempty"`
}
```

- [ ] **Step 4: Write the Bus implementation**

`internal/bus/bus.go`:
```go
package bus

import (
	"sync"
	"time"
)

// subscriberBuffer is the per-subscriber queue depth. A subscriber that falls
// this far behind is dropped rather than allowed to block Publish; the browser
// reconnects with Last-Event-ID and replays from the ring.
const subscriberBuffer = 256

// Bus is a fan-out hub with a bounded replay ring. Safe for concurrent use.
type Bus struct {
	mu       sync.RWMutex
	nextID   uint64
	ring     []Event
	capacity int
	subs     map[int]chan Event
	nextSub  int
	now      func() time.Time
}

// New returns a Bus retaining the most recent capacity events for replay.
func New(capacity int) *Bus {
	if capacity < 1 {
		capacity = 1
	}
	return &Bus{
		ring:     make([]Event, 0, capacity),
		capacity: capacity,
		subs:     make(map[int]chan Event),
		now:      time.Now,
	}
}

// Publish stamps e with the next ID and current time, retains it for replay,
// and delivers it to every live subscriber. A subscriber whose buffer is full
// is skipped, never waited on. Returns the stamped event.
func (b *Bus) Publish(e Event) Event {
	b.mu.Lock()
	b.nextID++
	e.ID = b.nextID
	if e.At.IsZero() {
		e.At = b.now().UTC()
	}
	if e.Level == "" {
		e.Level = LevelInfo
	}
	if len(b.ring) == b.capacity {
		b.ring = append(b.ring[:0], b.ring[1:]...)
	}
	b.ring = append(b.ring, e)

	targets := make([]chan Event, 0, len(b.subs))
	for _, ch := range b.subs {
		targets = append(targets, ch)
	}
	b.mu.Unlock()

	for _, ch := range targets {
		select {
		case ch <- e:
		default: // slow subscriber: drop, it will replay on reconnect
		}
	}
	return e
}

// Replay returns retained events with ID greater than since, oldest first.
func (b *Bus) Replay(since uint64) []Event {
	b.mu.RLock()
	defer b.mu.RUnlock()

	var out []Event
	for _, e := range b.ring {
		if e.ID > since {
			out = append(out, e)
		}
	}
	return out
}

// Subscribe returns a channel that first yields retained events newer than
// since, then streams live events. The returned func unsubscribes and closes
// the channel; it is safe to call more than once.
func (b *Bus) Subscribe(since uint64) (<-chan Event, func()) {
	b.mu.Lock()
	id := b.nextSub
	b.nextSub++
	ch := make(chan Event, subscriberBuffer)
	backlog := make([]Event, 0, len(b.ring))
	for _, e := range b.ring {
		if e.ID > since {
			backlog = append(backlog, e)
		}
	}
	b.subs[id] = ch
	b.mu.Unlock()

	for _, e := range backlog {
		select {
		case ch <- e:
		default:
		}
	}

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, id)
			b.mu.Unlock()
			close(ch)
		})
	}
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/bus/ -race -v`
Expected: PASS, all five tests.

- [ ] **Step 6: Commit**

```bash
git add internal/bus/
git commit -S -m "feat(bus): typed event fan-out with bounded replay ring"
```

---

### Task 3: Run state machine

**Files:**
- Create: `internal/engine/run.go`, `internal/engine/store.go`, `internal/engine/engine.go`
- Test: `internal/engine/engine_test.go`

**Interfaces:**
- Consumes: `bus.Event`, `bus.Kind*`, `bus.Level*`
- Produces:
  - `engine.Phase` string type with `PhaseDiscover`, `PhaseRecommend`, `PhaseBundle`, `PhaseApply`, `PhaseValidate`, `PhaseProve`
  - `engine.State` with `StateIdle`, `StateRunning`, `StateAwaitingDecision`, `StateFailed`, `StateActive`, `StateDone`
  - `engine.Run{ID, State, Phase, Decisions map[string]string, Artifacts map[string][]byte, Err string, StartedAt, UpdatedAt}`
  - `engine.Step` interface: `Phase() Phase`, `Requires() []string`, `Run(ctx, *Run, Emit) error`
  - `engine.Emit func(bus.Event)`
  - `engine.Store` interface: `Save(ctx, *Run) error`, `Load(ctx, id string) (*Run, error)`
  - `engine.NewMemoryStore() Store`
  - `engine.New(b *bus.Bus, st Store, steps ...Step) *Engine`
  - `(*Engine).Start(ctx) (*Run, error)` — starts a run, returns immediately
  - `(*Engine).Decide(runID string, decisions map[string]string) error`
  - `(*Engine).Get(runID string) (*Run, error)` — returns a deep copy
  - `engine.StateActive` is the terminal-but-active state Prove parks in (spec §6)

- [ ] **Step 1: Write the failing test**

`internal/engine/engine_test.go`:
```go
package engine_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
)

type fakeStep struct {
	phase    engine.Phase
	requires []string
	err      error
	ran      chan struct{}
}

func newFakeStep(p engine.Phase, requires ...string) *fakeStep {
	return &fakeStep{phase: p, requires: requires, ran: make(chan struct{}, 4)}
}

func (f *fakeStep) Phase() engine.Phase  { return f.phase }
func (f *fakeStep) Requires() []string   { return f.requires }
func (f *fakeStep) Run(_ context.Context, r *engine.Run, emit engine.Emit) error {
	f.ran <- struct{}{}
	emit(bus.Event{Kind: bus.KindLog, Message: string(f.phase) + " ran"})
	r.Artifacts[string(f.phase)] = []byte("done")
	return f.err
}

func waitState(t *testing.T, e *engine.Engine, id string, want engine.State) *engine.Run {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			got, _ := e.Get(id)
			t.Fatalf("timed out waiting for state %q, last state %q", want, got.State)
		default:
		}
		r, err := e.Get(id)
		if err == nil && r.State == want {
			return r
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRunCompletesAllSteps(t *testing.T) {
	b := bus.New(64)
	a := newFakeStep(engine.PhaseDiscover)
	c := newFakeStep(engine.PhaseRecommend)
	e := engine.New(b, engine.NewMemoryStore(), a, c)

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	final := waitState(t, e, run.ID, engine.StateDone)
	if final.Phase != engine.PhaseRecommend {
		t.Errorf("final phase = %q, want %q", final.Phase, engine.PhaseRecommend)
	}
	if len(a.ran) != 1 || len(c.ran) != 1 {
		t.Errorf("step run counts = (%d, %d), want (1, 1)", len(a.ran), len(c.ran))
	}
}

func TestRunParksForDecisions(t *testing.T) {
	b := bus.New(64)
	a := newFakeStep(engine.PhaseDiscover)
	c := newFakeStep(engine.PhaseRecommend, "intent", "platform")
	e := engine.New(b, engine.NewMemoryStore(), a, c)

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	waitState(t, e, run.ID, engine.StateAwaitingDecision)
	if len(c.ran) != 0 {
		t.Fatal("gated step ran before decisions were supplied")
	}

	if err := e.Decide(run.ID, map[string]string{"intent": "training"}); err == nil {
		t.Error("Decide() with a missing key should error")
	}

	if err := e.Decide(run.ID, map[string]string{"intent": "training", "platform": "kubeflow"}); err != nil {
		t.Fatalf("Decide() error = %v", err)
	}

	final := waitState(t, e, run.ID, engine.StateDone)
	if final.Decisions["platform"] != "kubeflow" {
		t.Errorf("decisions not recorded: %v", final.Decisions)
	}
}

func TestStepFailureStopsRun(t *testing.T) {
	b := bus.New(64)
	boom := errors.New("boom")
	a := newFakeStep(engine.PhaseDiscover)
	a.err = boom
	c := newFakeStep(engine.PhaseRecommend)
	e := engine.New(b, engine.NewMemoryStore(), a, c)

	run, _ := e.Start(context.Background())
	final := waitState(t, e, run.ID, engine.StateFailed)

	if final.Err == "" {
		t.Error("failed run carries no error message")
	}
	if len(c.ran) != 0 {
		t.Error("step after a failure ran anyway")
	}
}

func TestGetReturnsCopy(t *testing.T) {
	b := bus.New(64)
	e := engine.New(b, engine.NewMemoryStore(), newFakeStep(engine.PhaseDiscover))
	run, _ := e.Start(context.Background())
	waitState(t, e, run.ID, engine.StateDone)

	got, _ := e.Get(run.ID)
	got.Decisions["tamper"] = "yes"
	again, _ := e.Get(run.ID)
	if _, ok := again.Decisions["tamper"]; ok {
		t.Error("Get() returned a live reference, not a copy")
	}
}

// blockingStep writes to the Run while the test hammers Get concurrently. Under
// -race this fails if the engine hands steps the live *Run instead of a copy.
type blockingStep struct {
	release chan struct{}
	entered chan struct{}
}

func (b *blockingStep) Phase() engine.Phase { return engine.PhaseDiscover }
func (b *blockingStep) Requires() []string  { return nil }
func (b *blockingStep) Run(_ context.Context, r *engine.Run, _ engine.Emit) error {
	close(b.entered)
	for i := 0; i < 200; i++ {
		r.Artifacts["k"] = []byte{byte(i)}
	}
	<-b.release
	r.Artifacts["final"] = []byte("done")
	return nil
}

func TestGetDuringStepIsRaceFree(t *testing.T) {
	b := bus.New(64)
	step := &blockingStep{release: make(chan struct{}), entered: make(chan struct{})}
	e := engine.New(b, engine.NewMemoryStore(), step)

	run, _ := e.Start(context.Background())
	<-step.entered

	for i := 0; i < 200; i++ {
		if _, err := e.Get(run.ID); err != nil {
			t.Fatalf("Get() error = %v", err)
		}
	}
	close(step.release)

	final := waitState(t, e, run.ID, engine.StateDone)
	if string(final.Artifacts["final"]) != "done" {
		t.Error("step writes were not merged back into the run")
	}
}

func TestPhaseEventsPublished(t *testing.T) {
	b := bus.New(64)
	e := engine.New(b, engine.NewMemoryStore(), newFakeStep(engine.PhaseDiscover))
	run, _ := e.Start(context.Background())
	waitState(t, e, run.ID, engine.StateDone)

	var phases int
	for _, ev := range b.Replay(0) {
		if ev.Kind == bus.KindPhase {
			phases++
		}
		if ev.RunID != run.ID {
			t.Errorf("event %d has RunID %q, want %q", ev.ID, ev.RunID, run.ID)
		}
	}
	if phases == 0 {
		t.Error("no phase events published")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/engine/ -race -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write the Run and Store types**

`internal/engine/run.go`:
```go
// Package engine drives the linear run state machine over a slice of Steps.
package engine

import (
	"maps"
	"time"
)

// Phase identifies one stage of the six-phase arc.
type Phase string

const (
	// PhaseDiscover captures the cluster snapshot.
	PhaseDiscover Phase = "discover"
	// PhaseRecommend resolves the AICR recipe.
	PhaseRecommend Phase = "recommend"
	// PhaseBundle generates the deployable bundle.
	PhaseBundle Phase = "bundle"
	// PhaseApply installs the bundle.
	PhaseApply Phase = "apply"
	// PhaseValidate runs the recipe's validation phases.
	PhaseValidate Phase = "validate"
	// PhaseProve runs the reference workload.
	PhaseProve Phase = "prove"
)

// State is the run's lifecycle position.
type State string

const (
	// StateIdle is a created but unstarted run.
	StateIdle State = "idle"
	// StateRunning means a step is executing.
	StateRunning State = "running"
	// StateAwaitingDecision means the next step needs user input.
	StateAwaitingDecision State = "awaiting_decision"
	// StateFailed is terminal with an error.
	StateFailed State = "failed"
	// StateActive is terminal-but-active: every step finished and the Prove
	// workload is deliberately still running. Reset must tear the workload
	// down before uninstalling components beneath it (spec §6).
	StateActive State = "active"
	// StateDone is terminal and quiescent.
	StateDone State = "done"
)

// Run is the full state of one console run.
type Run struct {
	ID        string            `json:"id"`
	State     State             `json:"state"`
	Phase     Phase             `json:"phase"`
	Decisions map[string]string `json:"decisions"`
	Artifacts map[string][]byte `json:"-"`
	Pending   []string          `json:"pending,omitempty"`
	Err       string            `json:"error,omitempty"`
	StartedAt time.Time         `json:"startedAt"`
	UpdatedAt time.Time         `json:"updatedAt"`
}

// Clone returns a deep copy safe to hand to callers outside the engine lock.
func (r *Run) Clone() *Run {
	out := *r
	out.Decisions = maps.Clone(r.Decisions)
	if out.Decisions == nil {
		out.Decisions = map[string]string{}
	}
	out.Artifacts = make(map[string][]byte, len(r.Artifacts))
	for k, v := range r.Artifacts {
		out.Artifacts[k] = append([]byte(nil), v...)
	}
	out.Pending = append([]string(nil), r.Pending...)
	return &out
}
```

`internal/engine/store.go`:
```go
package engine

import (
	"context"
	"sync"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

// Store persists run state so a pod restart mid-demo does not wipe the
// timeline. The memory implementation is the development and test default;
// the ConfigMap-backed implementation lands with Phase 2, when Apply's
// 10-to-20-minute duration makes restart survival worth the complexity.
type Store interface {
	Save(ctx context.Context, r *Run) error
	Load(ctx context.Context, id string) (*Run, error)
}

type memoryStore struct {
	mu   sync.RWMutex
	runs map[string]*Run
}

// NewMemoryStore returns an in-process Store.
func NewMemoryStore() Store {
	return &memoryStore{runs: make(map[string]*Run)}
}

func (m *memoryStore) Save(_ context.Context, r *Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs[r.ID] = r.Clone()
	return nil
}

func (m *memoryStore) Load(_ context.Context, id string) (*Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.runs[id]
	if !ok {
		return nil, aicrerrors.New(aicrerrors.ErrCodeNotFound, "run not found: "+id)
	}
	return r.Clone(), nil
}
```

- [ ] **Step 4: Write the Engine**

`internal/engine/engine.go`:
```go
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/mchmarny/aicrme/internal/bus"
)

// Emit publishes a console event. The engine stamps RunID and Phase, so steps
// only supply Kind, Level, Message, Component, and Data.
type Emit func(bus.Event)

// Step is one phase of the run. Requires lists decision keys that must be
// present in Run.Decisions before Run is called; the engine parks in
// StateAwaitingDecision until they are supplied.
type Step interface {
	Phase() Phase
	Requires() []string
	Run(ctx context.Context, r *Run, emit Emit) error
}

// Engine executes steps in order for a single run. One run at a time: this is
// a single-replica demo console, not a scheduler.
type Engine struct {
	bus   *bus.Bus
	store Store
	steps []Step

	mu      sync.Mutex
	current *Run
	resume  chan struct{}
	newID   func() string
}

// New returns an Engine that will execute steps in the order given.
func New(b *bus.Bus, st Store, steps ...Step) *Engine {
	return &Engine{
		bus:   b,
		store: st,
		steps: steps,
		newID: randomID,
	}
}

func randomID() string {
	var buf [8]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

// Start creates a run and executes it in the background. It returns as soon as
// the run is registered; callers observe progress over the bus.
func (e *Engine) Start(ctx context.Context) (*Run, error) {
	e.mu.Lock()
	if e.current != nil && isLive(e.current.State) {
		e.mu.Unlock()
		return nil, aicrerrors.New(aicrerrors.ErrCodeConflict, "a run is already in progress")
	}
	now := time.Now().UTC()
	r := &Run{
		ID:        e.newID(),
		State:     StateRunning,
		Decisions: map[string]string{},
		Artifacts: map[string][]byte{},
		StartedAt: now,
		UpdatedAt: now,
	}
	e.current = r
	e.resume = make(chan struct{}, 1)
	snapshot := r.Clone()
	e.mu.Unlock()

	if err := e.store.Save(ctx, snapshot); err != nil {
		return nil, err
	}
	go e.execute(context.WithoutCancel(ctx), r.ID)
	return snapshot, nil
}

func isLive(s State) bool {
	return s == StateRunning || s == StateAwaitingDecision
}

// Decide supplies user decisions and unparks a run waiting on them.
func (e *Engine) Decide(runID string, decisions map[string]string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.current == nil || e.current.ID != runID {
		return aicrerrors.New(aicrerrors.ErrCodeNotFound, "run not found: "+runID)
	}
	if e.current.State != StateAwaitingDecision {
		return aicrerrors.New(aicrerrors.ErrCodeConflict, "run is not awaiting a decision")
	}
	for _, key := range e.current.Pending {
		if _, ok := decisions[key]; !ok {
			return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "missing required decision: "+key)
		}
	}
	for k, v := range decisions {
		e.current.Decisions[k] = v
	}
	e.current.Pending = nil
	e.current.State = StateRunning
	e.current.UpdatedAt = time.Now().UTC()

	select {
	case e.resume <- struct{}{}:
	default:
	}
	return nil
}

// Get returns a copy of the run's current state.
func (e *Engine) Get(runID string) (*Run, error) {
	e.mu.Lock()
	if e.current != nil && e.current.ID == runID {
		out := e.current.Clone()
		e.mu.Unlock()
		return out, nil
	}
	e.mu.Unlock()
	return e.store.Load(context.Background(), runID)
}

func (e *Engine) execute(ctx context.Context, runID string) {
	for _, step := range e.steps {
		if !e.awaitDecisions(ctx, step) {
			return
		}
		if err := e.runStep(ctx, step); err != nil {
			return
		}
	}
	e.finish(ctx, StateDone, "")
}

// awaitDecisions parks the run until every key in step.Requires() is present.
// Returns false if the context ended while parked.
func (e *Engine) awaitDecisions(ctx context.Context, step Step) bool {
	for {
		e.mu.Lock()
		var missing []string
		for _, key := range step.Requires() {
			if _, ok := e.current.Decisions[key]; !ok {
				missing = append(missing, key)
			}
		}
		if len(missing) == 0 {
			e.mu.Unlock()
			return true
		}
		e.current.Pending = missing
		e.current.State = StateAwaitingDecision
		e.current.Phase = step.Phase()
		e.current.UpdatedAt = time.Now().UTC()
		runID := e.current.ID
		resume := e.resume
		snapshot := e.current.Clone()
		e.mu.Unlock()

		_ = e.store.Save(ctx, snapshot)
		e.bus.Publish(bus.Event{
			RunID: runID, Kind: bus.KindDecision, Phase: string(step.Phase()),
			Message: "awaiting decision",
		})

		select {
		case <-resume:
		case <-ctx.Done():
			return false
		}
	}
}

func (e *Engine) runStep(ctx context.Context, step Step) error {
	e.mu.Lock()
	e.current.Phase = step.Phase()
	e.current.State = StateRunning
	e.current.UpdatedAt = time.Now().UTC()
	runID := e.current.ID
	// The step gets a private copy, not e.current. A step writing
	// r.Artifacts while a concurrent Get() clones e.current is a data race
	// that -race reports; the copy's writes are merged back under the lock
	// after Run returns.
	scratch := e.current.Clone()
	snapshot := e.current.Clone()
	e.mu.Unlock()

	_ = e.store.Save(ctx, snapshot)
	e.bus.Publish(bus.Event{
		RunID: runID, Kind: bus.KindPhase, Phase: string(step.Phase()),
		Message: "phase started",
	})

	emit := func(ev bus.Event) {
		ev.RunID = runID
		if ev.Phase == "" {
			ev.Phase = string(step.Phase())
		}
		e.bus.Publish(ev)
	}

	if err := step.Run(ctx, scratch, emit); err != nil {
		e.bus.Publish(bus.Event{
			RunID: runID, Kind: bus.KindError, Phase: string(step.Phase()),
			Level: bus.LevelError, Message: err.Error(),
		})
		e.finish(ctx, StateFailed, err.Error())
		return err
	}

	// Merge the step's writes back under the lock. Artifacts and Decisions are
	// the only fields a step may add to; the engine owns everything else.
	e.mu.Lock()
	for k, v := range scratch.Artifacts {
		e.current.Artifacts[k] = v
	}
	for k, v := range scratch.Decisions {
		e.current.Decisions[k] = v
	}
	e.current.UpdatedAt = time.Now().UTC()
	merged := e.current.Clone()
	e.mu.Unlock()
	_ = e.store.Save(ctx, merged)

	e.bus.Publish(bus.Event{
		RunID: runID, Kind: bus.KindPhase, Phase: string(step.Phase()),
		Message: "phase complete",
	})
	return nil
}

func (e *Engine) finish(ctx context.Context, state State, errMsg string) {
	e.mu.Lock()
	e.current.State = state
	e.current.Err = errMsg
	e.current.UpdatedAt = time.Now().UTC()
	snapshot := e.current.Clone()
	e.mu.Unlock()

	_ = e.store.Save(ctx, snapshot)
	e.bus.Publish(bus.Event{
		RunID: snapshot.ID, Kind: bus.KindPhase,
		Level: levelFor(state), Message: "run " + string(state),
	})
}

func levelFor(s State) bus.Level {
	if s == StateFailed {
		return bus.LevelError
	}
	return bus.LevelInfo
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/engine/ -race -v`
Expected: PASS, all six tests. `TestGetDuringStepIsRaceFree` is the one that proves steps receive a copy — if it reports a data race, `runStep` is handing out `e.current`.

- [ ] **Step 6: Commit**

```bash
git add internal/engine/
git commit -S -m "feat(engine): run state machine with decision parking"
```

---

### Task 4: HTTP API — auth, SSE, run endpoints

**Files:**
- Create: `internal/api/auth.go`, `internal/api/events.go`, `internal/api/runs.go`, `internal/api/server.go`
- Test: `internal/api/auth_test.go`, `internal/api/events_test.go`, `internal/api/runs_test.go`

**Interfaces:**
- Consumes: `bus.Bus`, `bus.Event`, `engine.Engine`, `engine.Run`
- Produces:
  - `api.Config{Username, Password string, SessionTTL time.Duration, LoginRate int}`
  - `api.New(cfg Config, b *bus.Bus, e *engine.Engine, static fs.FS) (*Server, error)`
  - `(*Server).Handler() http.Handler`
  - Routes: `POST /api/login`, `POST /api/logout`, `GET /api/events`, `POST /api/runs`, `GET /api/runs/{id}`, `POST /api/runs/{id}/decide`, `GET /healthz`, `GET /*` (SPA)

- [ ] **Step 1: Write the failing auth test**

`internal/api/auth_test.go`:
```go
package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mchmarny/aicrme/internal/api"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
	"github.com/mchmarny/aicrme/internal/testfs"
)

func newTestServer(t *testing.T) http.Handler {
	t.Helper()
	srv, err := api.New(api.Config{
		Username:   "admin",
		Password:   "correct-horse",
		SessionTTL: time.Hour,
		LoginRate:  100,
	}, bus.New(64), engine.New(bus.New(64), engine.NewMemoryStore()), testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	return srv.Handler()
}

func TestLogin(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantCode int
		wantSet  bool
	}{
		{name: "valid", body: `{"username":"admin","password":"correct-horse"}`, wantCode: http.StatusNoContent, wantSet: true},
		{name: "wrong password", body: `{"username":"admin","password":"nope"}`, wantCode: http.StatusUnauthorized},
		{name: "wrong username", body: `{"username":"root","password":"correct-horse"}`, wantCode: http.StatusUnauthorized},
		{name: "malformed", body: `{`, wantCode: http.StatusBadRequest},
	}
	h := newTestServer(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			cookie := rec.Header().Get("Set-Cookie")
			if tc.wantSet {
				if cookie == "" {
					t.Fatal("no session cookie set")
				}
				for _, attr := range []string{"HttpOnly", "SameSite=Strict"} {
					if !strings.Contains(cookie, attr) {
						t.Errorf("cookie missing %s: %s", attr, cookie)
					}
				}
			} else if cookie != "" {
				t.Errorf("unexpected cookie on failure: %s", cookie)
			}
		})
	}
}

func TestProtectedRoutesRequireSession(t *testing.T) {
	h := newTestServer(t)
	for _, path := range []string{"/api/events", "/api/runs"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
		})
	}
}

func TestLoginRateLimited(t *testing.T) {
	srv, err := api.New(api.Config{
		Username: "admin", Password: "pw", SessionTTL: time.Hour, LoginRate: 2,
	}, bus.New(8), engine.New(bus.New(8), engine.NewMemoryStore()), testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	h := srv.Handler()

	var got429 bool
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"username":"admin","password":"bad"}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Error("login was never rate limited")
	}
}

func TestHealthzIsPublic(t *testing.T) {
	h := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestEmptyPasswordRejected(t *testing.T) {
	_, err := api.New(api.Config{Username: "admin", Password: ""}, bus.New(8),
		engine.New(bus.New(8), engine.NewMemoryStore()), testfs.Static())
	if err == nil {
		t.Error("api.New() accepted an empty password")
	}
}
```

Create the tiny helper the tests share, `internal/testfs/testfs.go`:
```go
// Package testfs provides a minimal static filesystem for API tests.
package testfs

import (
	"io/fs"
	"testing/fstest"
)

// Static returns a one-file SPA stand-in.
func Static() fs.FS {
	return fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<!doctype html><title>aicrme</title>")}}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/ -race -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write auth**

`internal/api/auth.go`:
```go
package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const sessionCookie = "aicrme_session"

type session struct {
	expires time.Time
}

type authenticator struct {
	username string
	password string
	ttl      time.Duration
	secure   bool

	mu       sync.RWMutex
	sessions map[string]session

	limiter *rate.Limiter
}

func newAuthenticator(cfg Config) *authenticator {
	perSecond := float64(cfg.LoginRate) / 60.0
	return &authenticator{
		username: cfg.Username,
		password: cfg.Password,
		ttl:      cfg.SessionTTL,
		secure:   cfg.TLS,
		sessions: make(map[string]session),
		limiter:  rate.NewLimiter(rate.Limit(perSecond), cfg.LoginRate),
	}
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (a *authenticator) login(w http.ResponseWriter, r *http.Request) {
	if !a.limiter.Allow() {
		http.Error(w, "too many login attempts", http.StatusTooManyRequests)
		return
	}
	var req loginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "malformed request", http.StatusBadRequest)
		return
	}
	// Compare both fields unconditionally so the response time does not leak
	// which one was wrong.
	userOK := subtle.ConstantTimeCompare([]byte(req.Username), []byte(a.username))
	passOK := subtle.ConstantTimeCompare([]byte(req.Password), []byte(a.password))
	if userOK&passOK != 1 {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}

	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		http.Error(w, "session creation failed", http.StatusInternalServerError)
		return
	}
	token := hex.EncodeToString(raw[:])

	a.mu.Lock()
	a.sessions[token] = session{expires: time.Now().Add(a.ttl)}
	a.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteStrictMode,
		Expires:  time.Now().Add(a.ttl),
	})
	w.WriteHeader(http.StatusNoContent)
}

func (a *authenticator) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.mu.Lock()
		delete(a.sessions, c.Value)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (a *authenticator) valid(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	a.mu.RLock()
	s, ok := a.sessions[c.Value]
	a.mu.RUnlock()
	if !ok {
		return false
	}
	if time.Now().After(s.expires) {
		a.mu.Lock()
		delete(a.sessions, c.Value)
		a.mu.Unlock()
		return false
	}
	return true
}

// require wraps h so only requests carrying a live session reach it.
func (a *authenticator) require(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.valid(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}
```

- [ ] **Step 4: Write the SSE handler**

`internal/api/events.go`:
```go
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/mchmarny/aicrme/internal/bus"
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

var _ = bus.Event{} // keep the import explicit for readers of the wire shape
```

- [ ] **Step 5: Write the run endpoints and server**

`internal/api/runs.go`:
```go
package api

import (
	"encoding/json"
	"errors"
	"net/http"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.engine.Start(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.engine.Get(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleDecide(w http.ResponseWriter, r *http.Request) {
	var decisions map[string]string
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&decisions); err != nil {
		http.Error(w, "malformed request", http.StatusBadRequest)
		return
	}
	if err := s.engine.Decide(r.PathValue("id"), decisions); err != nil {
		writeErr(w, err)
		return
	}
	run, err := s.engine.Get(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr maps AICR structured error codes onto HTTP status codes so the
// console's error contract matches AICR's.
func writeErr(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	var se *aicrerrors.StructuredError
	if errors.As(err, &se) {
		switch se.Code {
		case aicrerrors.ErrCodeNotFound:
			code = http.StatusNotFound
		case aicrerrors.ErrCodeInvalidRequest:
			code = http.StatusBadRequest
		case aicrerrors.ErrCodeConflict:
			code = http.StatusConflict
		case aicrerrors.ErrCodeTimeout:
			code = http.StatusGatewayTimeout
		case aicrerrors.ErrCodeUnavailable:
			code = http.StatusServiceUnavailable
		}
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
```

Verified surface: the concrete type is `*aicrerrors.StructuredError{Code ErrorCode, Message string, Cause error, Context map[string]any}` with `New(code, msg)`, `Wrap(code, msg, cause)`, and an `Unwrap()` — so stdlib `errors.As` is the correct extraction. The package exports no `As` of its own.

`internal/api/server.go`:
```go
// Package api serves the console HTTP surface. It carries no business logic:
// every handler is a thin adapter over engine and bus.
package api

import (
	"io/fs"
	"net/http"
	"time"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
)

// Config is the server's runtime configuration.
type Config struct {
	Username   string
	Password   string
	SessionTTL time.Duration
	// LoginRate is the burst and per-minute ceiling on login attempts.
	LoginRate int
	// TLS marks the session cookie Secure.
	TLS bool
}

// Server wires the HTTP routes.
type Server struct {
	auth   *authenticator
	bus    *bus.Bus
	engine *engine.Engine
	static fs.FS
}

// New validates cfg and returns a Server.
func New(cfg Config, b *bus.Bus, e *engine.Engine, static fs.FS) (*Server, error) {
	if cfg.Password == "" {
		return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "password must not be empty")
	}
	if cfg.Username == "" {
		cfg.Username = "admin"
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 8 * time.Hour
	}
	if cfg.LoginRate <= 0 {
		cfg.LoginRate = 10
	}
	return &Server{auth: newAuthenticator(cfg), bus: b, engine: e, static: static}, nil
}

// Handler returns the fully routed http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("POST /api/login", s.auth.login)
	mux.HandleFunc("POST /api/logout", s.auth.logout)

	protected := http.NewServeMux()
	protected.HandleFunc("GET /api/events", s.handleEvents)
	protected.HandleFunc("POST /api/runs", s.handleCreateRun)
	protected.HandleFunc("GET /api/runs/{id}", s.handleGetRun)
	protected.HandleFunc("POST /api/runs/{id}/decide", s.handleDecide)
	mux.Handle("/api/", s.auth.require(protected))

	mux.Handle("GET /", spaHandler(s.static))
	return securityHeaders(mux)
}

// spaHandler serves static assets, falling back to index.html so client-side
// routes resolve on a hard refresh.
func spaHandler(static fs.FS) http.Handler {
	files := http.FileServer(http.FS(static))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := fs.Stat(static, cleanPath(r.URL.Path)); err != nil {
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}
		files.ServeHTTP(w, r)
	})
}

func cleanPath(p string) string {
	if p == "" || p == "/" {
		return "index.html"
	}
	return p[1:]
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		h.ServeHTTP(w, r)
	})
}
```

- [ ] **Step 6: Write the SSE and run endpoint tests**

`internal/api/events_test.go`:
```go
package api_test

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mchmarny/aicrme/internal/api"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
	"github.com/mchmarny/aicrme/internal/testfs"
)

func loggedInClient(t *testing.T, h http.Handler) (*httptest.Server, *http.Client) {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	jar := &cookieJar{}
	client := &http.Client{Jar: jar}
	resp, err := client.Post(ts.URL+"/api/login", "application/json",
		strings.NewReader(`{"username":"admin","password":"correct-horse"}`))
	if err != nil {
		t.Fatalf("login error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("login status = %d", resp.StatusCode)
	}
	return ts, client
}

func TestEventStreamReplaysFromLastEventID(t *testing.T) {
	b := bus.New(64)
	srv, err := api.New(api.Config{
		Username: "admin", Password: "correct-horse", SessionTTL: time.Hour, LoginRate: 100,
	}, b, engine.New(b, engine.NewMemoryStore()), testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	ts, client := loggedInClient(t, srv.Handler())

	b.Publish(bus.Event{Kind: bus.KindLog, Message: "one"})
	b.Publish(bus.Event{Kind: bus.KindLog, Message: "two"})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/events", nil)
	req.Header.Set("Last-Event-ID", "1")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("stream error = %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	scanner := bufio.NewScanner(resp.Body)
	var sawTwo, sawOne bool
	deadline := time.Now().Add(2 * time.Second)
	for scanner.Scan() && time.Now().Before(deadline) {
		line := scanner.Text()
		if strings.Contains(line, `"two"`) {
			sawTwo = true
			break
		}
		if strings.Contains(line, `"one"`) {
			sawOne = true
		}
	}
	if sawOne {
		t.Error("replayed an event at or before Last-Event-ID")
	}
	if !sawTwo {
		t.Error("did not replay the event after Last-Event-ID")
	}
}
```

Add the minimal cookie jar used above, `internal/api/jar_test.go`:
```go
package api_test

import (
	"net/http"
	"net/url"
	"sync"
)

type cookieJar struct {
	mu      sync.Mutex
	cookies []*http.Cookie
}

func (j *cookieJar) SetCookies(_ *url.URL, cs []*http.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.cookies = cs
}

func (j *cookieJar) Cookies(_ *url.URL) []*http.Cookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.cookies
}
```

`internal/api/runs_test.go`:
```go
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
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode error = %v", err)
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
```

- [ ] **Step 7: Run tests to verify they pass**

Run: `go test ./internal/api/ -race -v`
Expected: PASS. If `writeErr` fails to compile, fix it against the real `pkg/errors` shape per the note in Step 5.

- [ ] **Step 8: Commit**

```bash
git add internal/api/ internal/testfs/ go.mod go.sum
git commit -S -m "feat(api): session auth, SSE stream with replay, run endpoints"
```

---

### Task 5: Embedded SPA shell

**Files:**
- Create: `web/package.json`, `web/vite.config.ts`, `web/tsconfig.json`, `web/tailwind.config.js`, `web/index.html`, `web/src/main.tsx`, `web/src/App.tsx`, `web/src/api.ts`, `web/src/useEvents.ts`, `web/src/components/Login.tsx`, `web/src/components/Timeline.tsx`, `internal/web/embed.go`
- Test: `web/src/useEvents.test.ts`, `web/src/components/Timeline.test.tsx`, `internal/web/embed_test.go`
- Modify: `.github/workflows/ci.yaml` (add `make web` before `make test-coverage`), `.gitignore` (add `web/dist/`, `web/node_modules/`, `bin/`)

**Interfaces:**
- Consumes: the SSE wire shape from `bus.Event`
- Produces:
  - `web.Static() (fs.FS, error)` — the built SPA rooted at `dist`
  - TS `AicrEvent` type mirroring `bus.Event` field-for-field
  - `useEvents(): { events: AicrEvent[], connected: boolean }`

- [ ] **Step 1: Scaffold the web project**

```bash
cd web
npm create vite@latest . -- --template react-ts
npm install
npm install -D tailwindcss @tailwindcss/vite vitest @testing-library/react @testing-library/jest-dom jsdom
```

Set `web/package.json` scripts to:
```json
{
  "scripts": {
    "dev": "vite",
    "build": "tsc -b && vite build",
    "test": "vitest run"
  }
}
```

`web/vite.config.ts`:
```ts
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: { outDir: 'dist', emptyOutDir: true },
  server: { proxy: { '/api': 'http://localhost:8080' } },
  test: { environment: 'jsdom', globals: true },
})
```

- [ ] **Step 2: Write the failing event-hook test**

`web/src/useEvents.test.ts`:
```ts
import { describe, expect, it } from 'vitest'
import { mergeEvents, type AicrEvent } from './useEvents'

const ev = (id: number, message: string): AicrEvent => ({
  id, at: '2026-08-13T00:00:00Z', kind: 'log', level: 'info', message,
})

describe('mergeEvents', () => {
  it('appends new events in id order', () => {
    expect(mergeEvents([ev(1, 'a')], ev(2, 'b')).map(e => e.id)).toEqual([1, 2])
  })

  it('drops duplicates delivered by a reconnect replay', () => {
    expect(mergeEvents([ev(1, 'a'), ev(2, 'b')], ev(2, 'b')).map(e => e.id)).toEqual([1, 2])
  })

  it('reorders an out-of-order delivery', () => {
    expect(mergeEvents([ev(2, 'b')], ev(1, 'a')).map(e => e.id)).toEqual([1, 2])
  })
})
```

- [ ] **Step 3: Run test to verify it fails**

Run: `cd web && npm test`
Expected: FAIL — `Cannot find module './useEvents'`

- [ ] **Step 4: Write the event hook**

`web/src/useEvents.ts`:
```ts
import { useEffect, useRef, useState } from 'react'

/** AicrEvent mirrors Go's bus.Event field-for-field. */
export interface AicrEvent {
  id: number
  runId?: string
  at: string
  kind: 'phase' | 'log' | 'component' | 'cluster' | 'decision' | 'error'
  phase?: string
  level: 'info' | 'warn' | 'error'
  component?: string
  message: string
  data?: unknown
}

/**
 * mergeEvents inserts e into the ordered list, ignoring duplicates. The SSE
 * replay-on-reconnect contract means the same id can arrive twice.
 */
export function mergeEvents(existing: AicrEvent[], e: AicrEvent): AicrEvent[] {
  if (existing.some(x => x.id === e.id)) return existing
  const next = [...existing, e]
  next.sort((a, b) => a.id - b.id)
  return next
}

/** useEvents subscribes to /api/events and accumulates the ordered timeline. */
export function useEvents() {
  const [events, setEvents] = useState<AicrEvent[]>([])
  const [connected, setConnected] = useState(false)
  const lastId = useRef(0)

  useEffect(() => {
    // EventSource sends Last-Event-ID automatically on reconnect; ?since seeds
    // the very first connection after a full page reload.
    const source = new EventSource(`/api/events?since=${lastId.current}`)
    source.onopen = () => setConnected(true)
    source.onerror = () => setConnected(false)
    source.onmessage = (msg: MessageEvent<string>) => {
      const parsed = JSON.parse(msg.data) as AicrEvent
      lastId.current = Math.max(lastId.current, parsed.id)
      setEvents(prev => mergeEvents(prev, parsed))
    }
    return () => source.close()
  }, [])

  return { events, connected }
}
```

Note: Task 4 already emits `id:` + `data:` only, with no `event:` field, so every frame lands on `onmessage`. Do not add an `event:` type to the server.

- [ ] **Step 5: Run test to verify it passes**

Run: `cd web && npm test`
Expected: PASS, 3 tests.

- [ ] **Step 6: Write the Timeline component and its test**

`web/src/components/Timeline.test.tsx`:
```tsx
import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { Timeline } from './Timeline'
import type { AicrEvent } from '../useEvents'

const events: AicrEvent[] = [
  { id: 1, at: '2026-08-13T00:00:00Z', kind: 'phase', level: 'info', message: 'phase started', phase: 'discover' },
  { id: 2, at: '2026-08-13T00:00:01Z', kind: 'log', level: 'warn', message: 'FailedScheduling' },
]

describe('Timeline', () => {
  it('renders every event message', () => {
    render(<Timeline events={events} />)
    expect(screen.getByText('phase started')).toBeDefined()
    expect(screen.getByText('FailedScheduling')).toBeDefined()
  })

  it('marks warnings so they are surfaced, not buried', () => {
    render(<Timeline events={events} />)
    expect(screen.getByTestId('event-2').className).toContain('text-amber')
  })

  it('renders an empty state with no events', () => {
    render(<Timeline events={[]} />)
    expect(screen.getByText(/waiting for events/i)).toBeDefined()
  })
})
```

`web/src/components/Timeline.tsx`:
```tsx
import type { AicrEvent } from '../useEvents'

const levelClass: Record<AicrEvent['level'], string> = {
  info: 'text-slate-300',
  warn: 'text-amber-400',
  error: 'text-red-400',
}

export function Timeline({ events }: { events: AicrEvent[] }) {
  if (events.length === 0) {
    return <p className="text-slate-500 text-sm">Waiting for events…</p>
  }
  return (
    <ol className="font-mono text-sm space-y-1">
      {events.map(e => (
        <li key={e.id} data-testid={`event-${e.id}`} className={levelClass[e.level]}>
          <span className="text-slate-600 mr-2">{new Date(e.at).toLocaleTimeString()}</span>
          {e.component && <span className="text-slate-400 mr-2">[{e.component}]</span>}
          {e.message}
        </li>
      ))}
    </ol>
  )
}
```

- [ ] **Step 7: Write Login, App, and api.ts**

`web/src/api.ts`:
```ts
export async function login(username: string, password: string): Promise<void> {
  const res = await fetch('/api/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username, password }),
  })
  if (!res.ok) throw new Error(res.status === 429 ? 'Too many attempts' : 'Invalid credentials')
}

export async function startRun(): Promise<{ id: string }> {
  const res = await fetch('/api/runs', { method: 'POST' })
  if (!res.ok) throw new Error('Failed to start run')
  return res.json()
}
```

`web/src/components/Login.tsx`:
```tsx
import { useState } from 'react'
import { login } from '../api'

export function Login({ onSuccess }: { onSuccess: () => void }) {
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    try {
      await login('admin', password)
      onSuccess()
    } catch (err) {
      setError((err as Error).message)
    }
  }

  return (
    <form onSubmit={submit} className="mx-auto mt-32 w-80 space-y-4">
      <h1 className="text-2xl font-semibold text-slate-100">aicrme</h1>
      <input
        type="password" value={password} onChange={e => setPassword(e.target.value)}
        placeholder="Password" aria-label="Password"
        className="w-full rounded border border-slate-700 bg-slate-900 px-3 py-2 text-slate-100"
      />
      {error && <p className="text-red-400 text-sm">{error}</p>}
      <button type="submit" className="w-full rounded bg-emerald-600 py-2 text-white">Sign in</button>
    </form>
  )
}
```

`web/src/App.tsx`:
```tsx
import { useState } from 'react'
import { Login } from './components/Login'
import { Timeline } from './components/Timeline'
import { useEvents } from './useEvents'

export default function App() {
  const [authed, setAuthed] = useState(false)
  if (!authed) return <Login onSuccess={() => setAuthed(true)} />
  return <Console />
}

function Console() {
  const { events, connected } = useEvents()
  return (
    <main className="min-h-screen bg-slate-950 p-8 text-slate-100">
      <header className="mb-6 flex items-center gap-3">
        <h1 className="text-xl font-semibold">aicrme</h1>
        <span className={connected ? 'text-emerald-400 text-xs' : 'text-slate-500 text-xs'}>
          {connected ? 'connected' : 'reconnecting…'}
        </span>
      </header>
      <Timeline events={events} />
    </main>
  )
}
```

- [ ] **Step 8: Write the embed package and its test**

`internal/web/embed_test.go`:
```go
package web_test

import (
	"io/fs"
	"testing"

	"github.com/mchmarny/aicrme/internal/web"
)

func TestStaticServesIndex(t *testing.T) {
	static, err := web.Static()
	if err != nil {
		t.Fatalf("Static() error = %v", err)
	}
	if _, err := fs.Stat(static, "index.html"); err != nil {
		t.Errorf("index.html not embedded: %v", err)
	}
}
```

`internal/web/embed.go`:
```go
// Package web embeds the built SPA. `make web` must run before `go build`;
// CI enforces the ordering.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var assets embed.FS

// Static returns the built SPA rooted at dist.
func Static() (fs.FS, error) {
	return fs.Sub(assets, "dist")
}
```

Commit a placeholder `web/dist/.gitkeep` so `go:embed` resolves on a clean checkout before the first `make web`, and add `web/dist/*` (except `.gitkeep`) to `.gitignore`.

- [ ] **Step 9: Write main.go**

`cmd/aicrme/main.go`:
```go
// Command aicrme serves the AI Cluster Runtime demo console.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mchmarny/aicrme/internal/api"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
	"github.com/mchmarny/aicrme/internal/version"
	"github.com/mchmarny/aicrme/internal/web"
)

// replayCapacity bounds the event ring. A full real-hardware run emits a few
// thousand events; this keeps the whole timeline replayable to a late tab.
const replayCapacity = 20000

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	slog.Info("starting aicrme", "version", version.String())

	static, err := web.Static()
	if err != nil {
		slog.Error("embedded SPA unavailable", "error", err)
		os.Exit(1)
	}

	b := bus.New(replayCapacity)
	eng := engine.New(b, engine.NewMemoryStore())

	srv, err := api.New(api.Config{
		Username:   envOr("AICRME_USERNAME", "admin"),
		Password:   os.Getenv("AICRME_PASSWORD"),
		SessionTTL: 8 * time.Hour,
		LoginRate:  10,
		TLS:        os.Getenv("AICRME_TLS") == "true",
	}, b, eng, static)
	if err != nil {
		slog.Error("server configuration invalid", "error", err)
		os.Exit(1)
	}

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: SSE streams are long-lived by design.
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("listening", "addr", *addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server failed", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
```

- [ ] **Step 10: Verify the full build and both test suites**

```bash
make web
make test-coverage
cd web && npm test
```
Expected: SPA builds to `web/dist`, Go coverage ≥ 80%, 6 web tests pass.

- [ ] **Step 11: Add `make web` to CI and commit**

In `.github/workflows/ci.yaml`, insert `- run: make web` and `- run: cd web && npm test` before `- run: make test-coverage`.

```bash
git add web/ internal/web/ cmd/ .github/workflows/ci.yaml .gitignore
git commit -S -m "feat(web): SPA shell with SSE timeline, embedded via embed.FS"
```

---

### Task 6: Helm chart, image, and Kind smoke test

**Files:**
- Create: `charts/aicrme/Chart.yaml`, `charts/aicrme/values.yaml`, `charts/aicrme/templates/{deployment,service,serviceaccount,clusterrolebinding,secret,configmap,NOTES.txt}.yaml`, `charts/aicrme/templates/_helpers.tpl`, `Dockerfile`, `.github/workflows/e2e.yaml`, `test/e2e/smoke.sh`
- Modify: `.github/workflows/ci.yaml`

**Interfaces:**
- Consumes: `cmd/aicrme` binary, `AICRME_PASSWORD` env
- Produces: chart installable as `helm install aicrme charts/aicrme -n aicrme --create-namespace`

- [ ] **Step 1: Write the Dockerfile**

`Dockerfile`:
```dockerfile
# syntax=docker/dockerfile:1
ARG GO_VERSION=1.26.5
ARG HELM_VERSION=3.19.0
ARG KUBECTL_VERSION=1.34.1

FROM node:24-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
ARG VERSION=dev
ARG COMMIT=none
RUN CGO_ENABLED=0 go build \
      -ldflags "-s -w -X github.com/mchmarny/aicrme/internal/version.Version=${VERSION} -X github.com/mchmarny/aicrme/internal/version.Commit=${COMMIT}" \
      -o /out/aicrme ./cmd/aicrme

# The console shells out to the bundle's deploy.sh, which needs bash, helm,
# kubectl, and jq (the webhook preflight degrades without jq).
FROM alpine:3.22
ARG HELM_VERSION
ARG KUBECTL_VERSION
RUN apk add --no-cache bash ca-certificates curl jq tar && \
    curl -fsSL "https://get.helm.sh/helm-v${HELM_VERSION}-linux-amd64.tar.gz" | tar -xz -C /tmp && \
    mv /tmp/linux-amd64/helm /usr/local/bin/helm && \
    curl -fsSLo /usr/local/bin/kubectl "https://dl.k8s.io/release/v${KUBECTL_VERSION}/bin/linux/amd64/kubectl" && \
    chmod +x /usr/local/bin/kubectl && \
    rm -rf /tmp/linux-amd64 && \
    adduser -D -u 10001 aicrme
COPY --from=build /out/aicrme /usr/local/bin/aicrme
USER 10001
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/aicrme"]
```

Spec Risk 6 (image size) is on the critical path of the first impression. Record the compressed size in the commit message and treat growth as a regression.

- [ ] **Step 2: Write the chart**

`charts/aicrme/Chart.yaml`:
```yaml
apiVersion: v2
name: aicrme
description: Turns a vanilla GPU cluster into a working AI platform. Demo and eval clusters only.
type: application
version: 0.1.0
appVersion: "0.1.0"
```

`charts/aicrme/values.yaml`:
```yaml
image:
  repository: ghcr.io/mchmarny/aicrme/aicrme
  tag: ""          # defaults to .Chart.AppVersion
  pullPolicy: IfNotPresent

service:
  type: ClusterIP  # LoadBalancer exposes a cluster-admin console; see NOTES.txt
  port: 8080

auth:
  username: admin
  password: ""     # generated when empty, preserved across upgrade

resources:
  requests: { cpu: 100m, memory: 128Mi }
  limits:   { memory: 512Mi }
```

`charts/aicrme/templates/secret.yaml` — the `lookup` guard is what stops `helm upgrade` rotating the password mid-demo:
```yaml
{{- $name := printf "%s-auth" (include "aicrme.fullname" .) -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace $name -}}
{{- $password := .Values.auth.password -}}
{{- if not $password -}}
  {{- if $existing -}}
    {{- $password = index $existing.data "password" | b64dec -}}
  {{- else -}}
    {{- $password = randAlphaNum 24 -}}
  {{- end -}}
{{- end -}}
apiVersion: v1
kind: Secret
metadata:
  name: {{ $name }}
  namespace: {{ .Release.Namespace }}
  labels: {{- include "aicrme.labels" . | nindent 4 }}
type: Opaque
data:
  username: {{ .Values.auth.username | b64enc | quote }}
  password: {{ $password | b64enc | quote }}
```

`charts/aicrme/templates/clusterrolebinding.yaml` — cluster-admin, stated plainly:
```yaml
# The console installs gpu-operator, cert-manager, DRA drivers, CRDs, and
# privileged DaemonSets, and creates namespaces. That is cluster-admin. Any
# hand-enumerated role breaks the first time a recipe gains a component.
# Demo and eval clusters only.
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: {{ include "aicrme.fullname" . }}
  labels: {{- include "aicrme.labels" . | nindent 4 }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
subjects:
  - kind: ServiceAccount
    name: {{ include "aicrme.serviceAccountName" . }}
    namespace: {{ .Release.Namespace }}
```

`charts/aicrme/templates/NOTES.txt`:
```
aicrme is installed.

  kubectl -n {{ .Release.Namespace }} port-forward svc/{{ include "aicrme.fullname" . }} 8080:{{ .Values.service.port }}

Then open:  http://localhost:8080

  Username: {{ .Values.auth.username }}
  Password: kubectl -n {{ .Release.Namespace }} get secret {{ include "aicrme.fullname" . }}-auth -o jsonpath='{.data.password}' | base64 -d

SECURITY: this console runs with cluster-admin. It installs privileged
DaemonSets, CRDs, and cluster-scoped resources, and the AICR snapshot agent
runs privileged pods on GPU nodes. It is a DEMO AND EVAL TOOL. Do not install
it on a production cluster, and do not expose it with service.type=LoadBalancer
on an untrusted network — a cluster-admin console behind one password is a
cluster-takeover surface.

This console requires direct internet access to ghcr.io, nvcr.io, and the
upstream Helm repositories. Air-gapped clusters are not supported.
```

Write `deployment.yaml`, `service.yaml`, `serviceaccount.yaml`, `configmap.yaml`, and `_helpers.tpl` to match. The Deployment is 1 replica, mounts the auth Secret as `AICRME_USERNAME`/`AICRME_PASSWORD`, sets `readinessProbe` and `livenessProbe` on `/healthz`, and runs with `runAsNonRoot: true`, `allowPrivilegeEscalation: false`, `seccompProfile: RuntimeDefault`, `capabilities.drop: [ALL]`.

- [ ] **Step 3: Write the Kind smoke test**

`test/e2e/smoke.sh`:
```bash
#!/usr/bin/env bash
# Installs the chart on a Kind cluster and asserts the console serves a login
# page and rejects unauthenticated API access.
set -euo pipefail

CLUSTER="${CLUSTER:-aicrme-e2e}"
NS="${NS:-aicrme}"
IMAGE="${IMAGE:-aicrme:e2e}"

cleanup() { kind delete cluster --name "${CLUSTER}" >/dev/null 2>&1 || true; }
trap cleanup EXIT

kind create cluster --name "${CLUSTER}" --wait 120s
docker build -t "${IMAGE}" .
kind load docker-image "${IMAGE}" --name "${CLUSTER}"

helm install aicrme charts/aicrme -n "${NS}" --create-namespace \
  --set image.repository="${IMAGE%:*}" --set image.tag="${IMAGE#*:}" \
  --set image.pullPolicy=Never --wait --timeout 5m

kubectl -n "${NS}" rollout status deploy/aicrme --timeout=120s

kubectl -n "${NS}" port-forward svc/aicrme 18080:8080 >/dev/null 2>&1 &
PF_PID=$!
trap 'kill "${PF_PID}" 2>/dev/null || true; cleanup' EXIT
sleep 3

echo "--- GET / serves the SPA"
curl -fsS http://localhost:18080/ | grep -q "aicrme"

echo "--- GET /healthz is public"
[[ "$(curl -s -o /dev/null -w '%{http_code}' http://localhost:18080/healthz)" == "200" ]]

echo "--- GET /api/events is 401 without a session"
[[ "$(curl -s -o /dev/null -w '%{http_code}' http://localhost:18080/api/events)" == "401" ]]

echo "--- login then POST /api/runs succeeds"
PASSWORD="$(kubectl -n "${NS}" get secret aicrme-auth -o jsonpath='{.data.password}' | base64 -d)"
curl -fsS -c /tmp/aicrme.jar -X POST http://localhost:18080/api/login \
  -H 'Content-Type: application/json' \
  -d "{\"username\":\"admin\",\"password\":\"${PASSWORD}\"}"
curl -fsS -b /tmp/aicrme.jar -X POST http://localhost:18080/api/runs | grep -q '"id"'

echo "PASS: smoke test green"
```

- [ ] **Step 4: Run the smoke test locally**

Run: `chmod +x test/e2e/smoke.sh && ./test/e2e/smoke.sh`
Expected: `PASS: smoke test green`

- [ ] **Step 5: Wire the e2e workflow**

`.github/workflows/e2e.yaml` runs `test/e2e/smoke.sh` on `pull_request` using `helm/kind-action@v1`, with the same `.go-version` read as `ci.yaml`.

- [ ] **Step 6: Lint the chart**

Run: `helm lint charts/aicrme && helm template aicrme charts/aicrme | kubectl apply --dry-run=client -f -`
Expected: no errors.

- [ ] **Step 7: Commit**

```bash
git add charts/ Dockerfile test/ .github/workflows/e2e.yaml
git commit -S -m "feat(chart): single-chart install with generated credentials and Kind smoke test"
```

**Phase 0 is now demoable:** `helm install` on Kind, port-forward, log in, see an empty live timeline, start a run that does nothing.

---

## Phase 1 — Discover and Recommend

### Task 7: AICR client ports

**Files:**
- Create: `internal/aicrclient/client.go`, `internal/aicrclient/fake.go`
- Test: `internal/aicrclient/client_test.go`

**Interfaces:**
- Consumes: `github.com/NVIDIA/aicr/pkg/client/v1`
- Produces:
  - `aicrclient.Snapshotter` interface: `CollectSnapshot(ctx context.Context, cfg *aicr.AgentConfig) (*aicr.Snapshot, error)`
  - `aicrclient.Resolver` interface: `ResolveRecipeFromSnapshot(ctx context.Context, c *aicr.Criteria, s *aicr.Snapshot) (*aicr.RecipeResult, error)`
  - `aicrclient.CriteriaRegistrar` interface: `CriteriaRegistry() *aicr.CriteriaRegistry`
  - `aicrclient.API` interface embedding all three plus `Close() error`
  - `aicrclient.New() (API, error)` — wraps `aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()), aicr.WithVersion(version.Version))`
  - `aicrclient.Fake{Snapshot *aicr.Snapshot, Recipe *aicr.RecipeResult, SnapshotErr, ResolveErr error, Calls int}` implementing `API`

- [ ] **Step 1: Write the failing test**

`internal/aicrclient/client_test.go`:
```go
package aicrclient_test

import (
	"context"
	"testing"

	"github.com/mchmarny/aicrme/internal/aicrclient"
)

func TestNewUsesEmbeddedCatalog(t *testing.T) {
	client, err := aicrclient.New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if client.CriteriaRegistry() == nil {
		t.Error("CriteriaRegistry() returned nil — embedded catalog did not load")
	}
}

func TestFakeSatisfiesAPI(t *testing.T) {
	var api aicrclient.API = &aicrclient.Fake{}
	if _, err := api.CollectSnapshot(context.Background(), nil); err != nil {
		t.Errorf("Fake.CollectSnapshot() error = %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/aicrclient/ -race -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write the ports and the real client**

`internal/aicrclient/client.go`:
```go
// Package aicrclient narrows the AICR facade to the operations the console
// uses, so every step is testable with a fake and the console's dependency on
// the pinned aicr module is visible in one file.
package aicrclient

import (
	"context"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/mchmarny/aicrme/internal/version"
)

// Snapshotter deploys the AICR snapshot agent Job and returns the result.
type Snapshotter interface {
	CollectSnapshot(ctx context.Context, cfg *aicr.AgentConfig) (*aicr.Snapshot, error)
}

// Resolver turns a snapshot plus user criteria into a pinned recipe.
type Resolver interface {
	ResolveRecipeFromSnapshot(ctx context.Context, c *aicr.Criteria, s *aicr.Snapshot) (*aicr.RecipeResult, error)
}

// CriteriaRegistrar exposes the recipe catalog's criteria registry, used to
// filter the platform options offered to the user.
type CriteriaRegistrar interface {
	CriteriaRegistry() *aicr.CriteriaRegistry
}

// API is the whole console-facing AICR surface.
type API interface {
	Snapshotter
	Resolver
	CriteriaRegistrar
	Close() error
}

// New returns a client backed by the recipe catalog embedded in the pinned
// aicr module — no recipes/ tree ships in the console image.
func New() (API, error) {
	return aicr.NewClient(
		aicr.WithRecipeSource(aicr.EmbeddedSource()),
		aicr.WithVersion(version.Version),
	)
}
```

If `*aicr.Client` does not satisfy `API` verbatim, add a thin adapter struct in this file rather than widening the interfaces — the point is that the console's AICR surface stays one screen long.

`internal/aicrclient/fake.go`:
```go
package aicrclient

import (
	"context"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
)

// Fake is a scripted API for step tests. It records call counts so tests can
// assert a step did not re-collect a snapshot it already had.
type Fake struct {
	Snapshot    *aicr.Snapshot
	Recipe      *aicr.RecipeResult
	Registry    *aicr.CriteriaRegistry
	SnapshotErr error
	ResolveErr  error

	SnapshotCalls int
	ResolveCalls  int
	LastCriteria  *aicr.Criteria
}

func (f *Fake) CollectSnapshot(_ context.Context, _ *aicr.AgentConfig) (*aicr.Snapshot, error) {
	f.SnapshotCalls++
	if f.SnapshotErr != nil {
		return nil, f.SnapshotErr
	}
	if f.Snapshot == nil {
		return &aicr.Snapshot{}, nil
	}
	return f.Snapshot, nil
}

func (f *Fake) ResolveRecipeFromSnapshot(_ context.Context, c *aicr.Criteria, _ *aicr.Snapshot) (*aicr.RecipeResult, error) {
	f.ResolveCalls++
	f.LastCriteria = c
	if f.ResolveErr != nil {
		return nil, f.ResolveErr
	}
	return f.Recipe, nil
}

func (f *Fake) CriteriaRegistry() *aicr.CriteriaRegistry { return f.Registry }
func (f *Fake) Close() error                             { return nil }
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/aicrclient/ -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/aicrclient/ go.mod go.sum
git commit -S -m "feat(aicrclient): narrow ports over the pinned AICR facade"
```

---

### Task 8: Discover step

**Files:**
- Create: `internal/steps/discover.go`
- Test: `internal/steps/discover_test.go`, `internal/steps/testdata/snapshot-kwok.yaml`

**Interfaces:**
- Consumes: `aicrclient.Snapshotter`, `engine.Step`, `bus.Event`
- Produces:
  - `steps.NewDiscover(c aicrclient.API, cfg DiscoverConfig) engine.Step`
  - `steps.DiscoverConfig{Namespace, Image string, Timeout time.Duration, Privileged, RequireGPU bool}`
  - Writes `Run.Artifacts["snapshot.yaml"]` = `Snapshot.Raw`
  - Writes `Run.Artifacts["capability.json"]` = marshaled `gap.Report` (Task 9)

- [ ] **Step 1: Capture a real snapshot fixture**

Stand up KWOK and capture a snapshot to `internal/steps/testdata/snapshot-kwok.yaml`:

```bash
cd /Users/mchmarny/dev/aicr
make kwok-up   # or: kwokctl create cluster --name aicrme-fixture
./aicr snapshot -o /Users/mchmarny/dev/aicrme/internal/steps/testdata/snapshot-kwok.yaml
```

If `make kwok-up` does not exist, run `kwokctl create cluster` directly and point `aicr snapshot` at its kubeconfig. This fixture is the ground truth for Task 9's gap rules — do not hand-write it.

- [ ] **Step 2: Write the failing test**

`internal/steps/discover_test.go`:
```go
package steps_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
	"github.com/mchmarny/aicrme/internal/steps"
)

func newRun() *engine.Run {
	return &engine.Run{
		ID:        "test",
		Decisions: map[string]string{},
		Artifacts: map[string][]byte{},
	}
}

func TestDiscoverStoresRawSnapshot(t *testing.T) {
	raw, err := os.ReadFile("testdata/snapshot-kwok.yaml")
	if err != nil {
		t.Fatalf("fixture read error = %v", err)
	}
	fake := &aicrclient.Fake{Snapshot: &aicr.Snapshot{Raw: raw}}
	step := steps.NewDiscover(fake, steps.DiscoverConfig{Namespace: "aicrme", Timeout: time.Minute})

	run := newRun()
	var events []bus.Event
	if err := step.Run(context.Background(), run, func(e bus.Event) { events = append(events, e) }); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if string(run.Artifacts["snapshot.yaml"]) != string(raw) {
		t.Error("stored snapshot is not the agent's raw bytes")
	}
	if fake.SnapshotCalls != 1 {
		t.Errorf("CollectSnapshot called %d times, want 1", fake.SnapshotCalls)
	}
	if len(events) == 0 {
		t.Error("Discover emitted no events")
	}
}

func TestDiscoverPropagatesFailure(t *testing.T) {
	boom := errors.New("agent job timed out")
	fake := &aicrclient.Fake{SnapshotErr: boom}
	step := steps.NewDiscover(fake, steps.DiscoverConfig{Namespace: "aicrme"})

	if err := step.Run(context.Background(), newRun(), func(bus.Event) {}); err == nil {
		t.Fatal("Run() returned nil on a collection failure")
	}
}

func TestDiscoverMetadata(t *testing.T) {
	step := steps.NewDiscover(&aicrclient.Fake{}, steps.DiscoverConfig{})
	if step.Phase() != engine.PhaseDiscover {
		t.Errorf("Phase() = %q, want %q", step.Phase(), engine.PhaseDiscover)
	}
	if len(step.Requires()) != 0 {
		t.Errorf("Requires() = %v, want empty — Discover runs automatically on first load", step.Requires())
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/steps/ -race -v`
Expected: FAIL — package does not exist.

- [ ] **Step 4: Write the implementation**

`internal/steps/discover.go`:
```go
// Package steps implements one engine.Step per phase of the run.
package steps

import (
	"context"
	"encoding/json"
	"time"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
	"github.com/mchmarny/aicrme/internal/gap"
)

// DiscoverConfig configures the AICR snapshot agent Job.
type DiscoverConfig struct {
	Namespace  string
	Image      string
	Timeout    time.Duration
	Privileged bool
	RequireGPU bool
}

type discover struct {
	client aicrclient.Snapshotter
	cfg    DiscoverConfig
}

// NewDiscover returns the Discover step. It runs automatically on first load —
// no decisions gate it.
func NewDiscover(c aicrclient.Snapshotter, cfg DiscoverConfig) engine.Step {
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Minute
	}
	return &discover{client: c, cfg: cfg}
}

func (d *discover) Phase() engine.Phase { return engine.PhaseDiscover }
func (d *discover) Requires() []string  { return nil }

func (d *discover) Run(ctx context.Context, r *engine.Run, emit engine.Emit) error {
	emit(bus.Event{Kind: bus.KindLog, Message: "deploying cluster snapshot agent"})

	snap, err := d.client.CollectSnapshot(ctx, &aicr.AgentConfig{
		Namespace:  d.cfg.Namespace,
		Image:      d.cfg.Image,
		Timeout:    d.cfg.Timeout,
		Privileged: d.cfg.Privileged,
		RequireGPU: d.cfg.RequireGPU,
		Cleanup:    true,
	})
	if err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeUnavailable, "cluster snapshot failed", err)
	}

	// Persist the RAW agent bytes, not a re-serialization: a newer agent image
	// can emit fields this binary's Snapshot type does not model, and a typed
	// round trip drops them silently.
	r.Artifacts["snapshot.yaml"] = snap.Raw

	report := gap.Analyze(snap)
	encoded, err := json.Marshal(report)
	if err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "encoding capability report failed", err)
	}
	r.Artifacts["capability.json"] = encoded

	emit(bus.Event{Kind: bus.KindLog, Message: report.Headline, Data: encoded})
	for _, g := range report.Gaps {
		emit(bus.Event{Kind: bus.KindCluster, Level: bus.LevelWarn, Message: g.Title})
	}
	emit(bus.Event{Kind: bus.KindLog, Message: report.Punchline})
	return nil
}
```

- [ ] **Step 5: Run tests (they will fail on the missing gap package)**

Run: `go test ./internal/steps/ -race -v`
Expected: FAIL — `internal/gap` does not exist. Task 9 supplies it; implement Task 9 next, then return here.

- [ ] **Step 6: Commit after Task 9 makes this green**

```bash
git add internal/steps/discover.go internal/steps/discover_test.go internal/steps/testdata/
git commit -S -m "feat(steps): discover step collecting the AICR cluster snapshot"
```

---

### Task 9: Capability and gap analysis

**Files:**
- Create: `internal/gap/gap.go`, `internal/gap/rules.go`
- Test: `internal/gap/gap_test.go`, `internal/gap/testdata/snapshot-kwok.yaml` (symlink or copy of Task 8's fixture)

**Interfaces:**
- Consumes: `aicr.Snapshot` (via `Unwrap()`), `snapshotter.Snapshot`, `measurement.Measurement`
- Produces:
  - `gap.Report{Headline, Detail, Punchline string, Gaps []Gap, UsableGPUs, TotalGPUs int}`
  - `gap.Gap{ID, Title, Detail, Component string}` — `Component` is the AICR component that closes it, so the Discover screen pre-explains the Apply screen
  - `gap.Analyze(s *aicr.Snapshot) Report`
  - `gap.Rule{ID, Title, Detail, Component string, Applies func(probe) bool}` in `rules.go`

- [ ] **Step 1: Inspect the fixture and write the rule table from what is actually there**

```bash
grep -n "type:\|subtype:" internal/gap/testdata/snapshot-kwok.yaml | head -40
```

Verified measurement keys available from `github.com/NVIDIA/aicr/pkg/measurement`:
`measurement.TypeK8s`, `TypeGPU`, `TypeOS`, `TypeSystemD`, `TypeNodeTopology`, `TypeNetworkTopology`;
keys `measurement.KeyVersion`, `KeyGPUDriver`, `KeyGPUModel`, `KeyGPUCount`, `KeyGPUPresent`, `KeyGPUDriverLoaded`, `KeyGPUDetectionSource`.
Accessors: `(*Measurement).GetSubtype(name) *Subtype`, `(*Subtype).Get/GetString/GetInt64/Has`.

Write only rules the fixture proves. Do not invent keys. A rule that cannot be evaluated from the fixture is deferred to Task 8's EKS fixture in Phase 4.

- [ ] **Step 2: Write the failing test**

`internal/gap/gap_test.go`:
```go
package gap_test

import (
	"os"
	"testing"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/mchmarny/aicrme/internal/gap"
	"gopkg.in/yaml.v3"
)

func loadFixture(t *testing.T, name string) *aicr.Snapshot {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var s snapshotter.Snapshot
	if err := yaml.Unmarshal(raw, &s); err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	wrapped := aicr.WrapSnapshot(&s)
	wrapped.Raw = raw
	return wrapped
}

func TestAnalyzeKWOK(t *testing.T) {
	report := gap.Analyze(loadFixture(t, "snapshot-kwok.yaml"))

	if report.Headline == "" {
		t.Error("Analyze produced no headline — Discover opens with a capability statement")
	}
	if report.TotalGPUs != 0 {
		t.Errorf("TotalGPUs = %d, want 0 on a KWOK cluster", report.TotalGPUs)
	}
	if report.Punchline == "" {
		t.Error("Analyze produced no punchline — the finale calls back to this number")
	}
}

func TestAnalyzeNilSnapshotIsSafe(t *testing.T) {
	report := gap.Analyze(nil)
	if report.Headline == "" {
		t.Error("Analyze(nil) must still produce a renderable report")
	}
	if len(report.Gaps) != 0 {
		t.Errorf("Analyze(nil) produced %d gaps, want 0", len(report.Gaps))
	}
}

func TestEveryGapNamesItsClosingComponent(t *testing.T) {
	report := gap.Analyze(loadFixture(t, "snapshot-kwok.yaml"))
	for _, g := range report.Gaps {
		if g.Component == "" {
			t.Errorf("gap %q names no closing component — the Discover screen must pre-explain Apply", g.ID)
		}
		if g.Title == "" {
			t.Errorf("gap %q has no title", g.ID)
		}
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/gap/ -race -v`
Expected: FAIL — package does not exist.

- [ ] **Step 4: Write the implementation**

`internal/gap/gap.go`:
```go
// Package gap turns an AICR cluster snapshot into the capability statement and
// gap list that open the console. Each gap names the component that closes it,
// so the Discover screen pre-explains the Apply screen.
package gap

import (
	"fmt"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/measurement"
)

// Gap is one missing capability.
type Gap struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Detail    string `json:"detail,omitempty"`
	Component string `json:"component"`
}

// Report is the full Discover payload.
type Report struct {
	Headline   string `json:"headline"`
	Detail     string `json:"detail,omitempty"`
	Punchline  string `json:"punchline"`
	Gaps       []Gap  `json:"gaps"`
	UsableGPUs int    `json:"usableGpus"`
	TotalGPUs  int    `json:"totalGpus"`
}

// probe is the read-only view the rules evaluate against.
type probe struct {
	measurements []*measurement.Measurement
}

func (p probe) measurement(t measurement.Type) *measurement.Measurement {
	for _, m := range p.measurements {
		if m.Type == t {
			return m
		}
	}
	return nil
}

// Analyze produces the capability statement and gap list. A nil or empty
// snapshot yields a renderable report rather than a panic — the UI must always
// have something to show.
func Analyze(s *aicr.Snapshot) Report {
	if s == nil {
		return Report{
			Headline:  "No cluster snapshot available.",
			Punchline: "Run Discover to capture the cluster's current state.",
		}
	}
	inner := s.Unwrap()
	if inner == nil {
		return Report{
			Headline:  "No cluster snapshot available.",
			Punchline: "Run Discover to capture the cluster's current state.",
		}
	}

	p := probe{measurements: inner.Measurements}
	report := Report{
		Headline:  headline(p),
		Detail:    detail(p),
		TotalGPUs: totalGPUs(p),
	}
	for _, rule := range rules {
		if rule.Applies(p) {
			report.Gaps = append(report.Gaps, Gap{
				ID: rule.ID, Title: rule.Title, Detail: rule.Detail, Component: rule.Component,
			})
		}
	}
	report.UsableGPUs = usableGPUs(p)
	report.Punchline = punchline(report)
	return report
}

func punchline(r Report) string {
	if r.TotalGPUs == 0 {
		return "No GPU hardware detected — this is a simulated cluster."
	}
	return fmt.Sprintf("%d of %d GPUs are usable by a workload today.", r.UsableGPUs, r.TotalGPUs)
}
```

`internal/gap/rules.go` holds `headline`, `detail`, `totalGPUs`, `usableGPUs`, and the `rules` table. Implement each helper against the keys verified in Step 1. Start with the two the fixture proves:

```go
package gap

import "github.com/NVIDIA/aicr/pkg/measurement"

// rule is one gap detector. Applies returns true when the capability is absent.
type rule struct {
	ID        string
	Title     string
	Detail    string
	Component string
	Applies   func(probe) bool
}

var rules = []rule{
	{
		ID:        "gpu-driver",
		Title:     "No GPU driver installed, the kernel does not see the devices",
		Component: "gpu-operator",
		Applies: func(p probe) bool {
			m := p.measurement(measurement.TypeGPU)
			if m == nil {
				return false
			}
			st := m.GetSubtype(measurement.KeyGPUPresent)
			if st == nil || !st.Has(measurement.KeyGPUDriverLoaded) {
				return false
			}
			loaded, err := st.GetString(measurement.KeyGPUDriverLoaded)
			return err == nil && loaded != "true"
		},
	},
	// Remaining rules (device plugin, scheduler, EFA plugin, GPU metrics) are
	// added in Step 5 once the fixture's exact subtype names are known.
}
```

- [ ] **Step 5: Calibrate the remaining rules against the fixture**

For each of the spec's five gaps (driver, device plugin, GPU-aware scheduler, EFA plugin, GPU metrics), find the measurement subtype in the fixture that proves presence or absence, add the rule, and add a table-driven case to `gap_test.go` asserting it fires. If a gap cannot be detected from the snapshot at all, delete it from the rule table and record it in the plan's unresolved questions — do not ship a rule that guesses.

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/gap/ ./internal/steps/ -race -v`
Expected: PASS. Task 8's Discover tests now compile and pass too.

- [ ] **Step 7: Commit**

```bash
git add internal/gap/
git commit -S -m "feat(gap): snapshot-to-capability analysis with fixture-calibrated rules"
```

---

### Task 10: Recommend step

**Files:**
- Create: `internal/steps/recommend.go`
- Test: `internal/steps/recommend_test.go`

**Interfaces:**
- Consumes: `aicrclient.Resolver`, `aicrclient.CriteriaRegistrar`, `engine.Run.Decisions`
- Produces:
  - `steps.NewRecommend(c aicrclient.API) engine.Step`
  - `Requires()` returns `[]string{"intent", "platform"}` — exactly two user decisions (spec §2)
  - Writes `Run.Artifacts["recipe.json"]` = marshaled `steps.RecipeSummary`
  - `steps.RecipeSummary{Name, Version string, ComponentCount int, Components []ComponentSummary}`
  - `steps.ComponentSummary{Name, Version, Namespace, Chart, Source, Kind string}`

- [ ] **Step 1: Write the failing test**

`internal/steps/recommend_test.go`:
```go
package steps_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
	"github.com/mchmarny/aicrme/internal/steps"
)

func recipeFixture() *aicr.RecipeResult {
	return &aicr.RecipeResult{
		Name:    "h100-eks-ubuntu-training",
		Version: "0.19.0",
		Components: []aicr.ComponentRef{
			{Name: "gpu-operator", Kind: "Helm", Version: "v26.3.3", Namespace: "gpu-operator",
				Chart: "gpu-operator", Source: "https://helm.ngc.nvidia.com/nvidia"},
			{Name: "kai-scheduler", Kind: "Helm", Version: "v0.14.1", Namespace: "kai-scheduler",
				Chart: "kai-scheduler", Source: "oci://ghcr.io/kai-scheduler/kai-scheduler"},
		},
	}
}

func TestRecommendRequiresExactlyTwoDecisions(t *testing.T) {
	step := steps.NewRecommend(&aicrclient.Fake{})
	got := step.Requires()
	want := []string{"intent", "platform"}
	if len(got) != len(want) {
		t.Fatalf("Requires() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Requires()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRecommendMapsDecisionsToCriteria(t *testing.T) {
	fake := &aicrclient.Fake{Recipe: recipeFixture()}
	step := steps.NewRecommend(fake)

	run := newRun()
	run.Decisions["intent"] = "training"
	run.Decisions["platform"] = "kubeflow"
	run.Artifacts["snapshot.yaml"] = []byte("apiVersion: aicr.nvidia.com/v1\nkind: Snapshot\n")

	if err := step.Run(context.Background(), run, func(bus.Event) {}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if fake.LastCriteria == nil {
		t.Fatal("resolver was called without criteria")
	}
	if fake.LastCriteria.Intent != "training" {
		t.Errorf("Criteria.Intent = %q, want %q", fake.LastCriteria.Intent, "training")
	}
	if fake.LastCriteria.Platform != "kubeflow" {
		t.Errorf("Criteria.Platform = %q, want %q", fake.LastCriteria.Platform, "kubeflow")
	}
}

func TestRecommendStoresComponentSummary(t *testing.T) {
	fake := &aicrclient.Fake{Recipe: recipeFixture()}
	step := steps.NewRecommend(fake)

	run := newRun()
	run.Decisions["intent"] = "training"
	run.Decisions["platform"] = "kubeflow"

	if err := step.Run(context.Background(), run, func(bus.Event) {}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	var summary steps.RecipeSummary
	if err := json.Unmarshal(run.Artifacts["recipe.json"], &summary); err != nil {
		t.Fatalf("recipe.json decode error = %v", err)
	}
	if summary.ComponentCount != 2 {
		t.Errorf("ComponentCount = %d, want 2", summary.ComponentCount)
	}
	if summary.Components[0].Version == "" {
		t.Error("component version not carried — every version must be shown pinned")
	}
}

func TestRecommendPropagatesResolveFailure(t *testing.T) {
	fake := &aicrclient.Fake{ResolveErr: errors.New("no recipe for these coordinates")}
	step := steps.NewRecommend(fake)

	run := newRun()
	run.Decisions["intent"] = "training"
	run.Decisions["platform"] = "kubeflow"

	if err := step.Run(context.Background(), run, func(bus.Event) {}); err == nil {
		t.Fatal("Run() returned nil on a resolve failure")
	}
}

func TestRecommendPhase(t *testing.T) {
	if got := steps.NewRecommend(&aicrclient.Fake{}).Phase(); got != engine.PhaseRecommend {
		t.Errorf("Phase() = %q, want %q", got, engine.PhaseRecommend)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/steps/ -race -run TestRecommend -v`
Expected: FAIL — `undefined: steps.NewRecommend`

- [ ] **Step 3: Write the implementation**

`internal/steps/recommend.go`:
```go
package steps

import (
	"context"
	"encoding/json"
	"fmt"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/mchmarny/aicrme/internal/aicrclient"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
	"gopkg.in/yaml.v3"
)

// ComponentSummary is one reviewable component in the resolved recipe.
type ComponentSummary struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Version   string `json:"version"`
	Namespace string `json:"namespace"`
	Chart     string `json:"chart,omitempty"`
	Source    string `json:"source,omitempty"`
}

// RecipeSummary is the folded component list shown on the Recommend screen.
type RecipeSummary struct {
	Name           string             `json:"name"`
	Version        string             `json:"version"`
	ComponentCount int                `json:"componentCount"`
	Components     []ComponentSummary `json:"components"`
}

type recommend struct {
	client aicrclient.Resolver
}

// NewRecommend returns the Recommend step. It gates on the only two decisions
// the console asks for: intent and platform. Service, accelerator, OS,
// component set, versions, and values are all derived by AICR.
func NewRecommend(c aicrclient.Resolver) engine.Step {
	return &recommend{client: c}
}

func (r *recommend) Phase() engine.Phase { return engine.PhaseRecommend }
func (r *recommend) Requires() []string  { return []string{"intent", "platform"} }

func (r *recommend) Run(ctx context.Context, run *engine.Run, emit engine.Emit) error {
	snap, err := decodeSnapshot(run.Artifacts["snapshot.yaml"])
	if err != nil {
		return err
	}

	// Only intent and platform come from the user. Every other dimension is
	// derived by AICR from the snapshot during resolution.
	criteria := &aicr.Criteria{
		Intent:   run.Decisions["intent"],
		Platform: run.Decisions["platform"],
	}

	emit(bus.Event{Kind: bus.KindLog, Message: fmt.Sprintf(
		"resolving recipe for intent=%s platform=%s", criteria.Intent, criteria.Platform)})

	result, err := r.client.ResolveRecipeFromSnapshot(ctx, criteria, snap)
	if err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, "recipe resolution failed", err)
	}
	if result == nil {
		return aicrerrors.New(aicrerrors.ErrCodeInternal, "recipe resolution returned no result")
	}

	summary := RecipeSummary{
		Name:           result.Name,
		Version:        result.Version,
		ComponentCount: len(result.Components),
	}
	for _, c := range result.Components {
		summary.Components = append(summary.Components, ComponentSummary{
			Name: c.Name, Kind: c.Kind, Version: c.Version,
			Namespace: c.Namespace, Chart: c.Chart, Source: c.Source,
		})
	}

	encoded, err := json.Marshal(summary)
	if err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "encoding recipe summary failed", err)
	}
	run.Artifacts["recipe.json"] = encoded

	emit(bus.Event{
		Kind: bus.KindLog, Data: encoded,
		Message: fmt.Sprintf("%d components, every version pinned", summary.ComponentCount),
	})
	return nil
}

// decodeSnapshot rebuilds the facade Snapshot from the raw agent bytes stored
// by Discover. A missing artifact means Discover did not run.
func decodeSnapshot(raw []byte) (*aicr.Snapshot, error) {
	if len(raw) == 0 {
		return nil, nil // resolution falls back to criteria-only when no snapshot exists
	}
	var inner snapshotter.Snapshot
	if err := yaml.Unmarshal(raw, &inner); err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, "stored snapshot is unparseable", err)
	}
	wrapped := aicr.WrapSnapshot(&inner)
	wrapped.Raw = raw
	return wrapped, nil
}
```

Note: `ResolveRecipeFromSnapshot` rejects criteria whose `Specificity()` is 0 (AICR issue #1888). If the KWOK fixture plus intent+platform yields no recognizable dimension, the error surfaces as `ErrCodeInvalidRequest` and the UI must show it rather than silently emitting a generic recipe. Add a test case asserting that path once the fixture is in hand.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/steps/ -race -v`
Expected: PASS, all Discover and Recommend tests.

- [ ] **Step 5: Commit**

```bash
git add internal/steps/recommend.go internal/steps/recommend_test.go
git commit -S -m "feat(steps): recommend step resolving the recipe from snapshot plus two decisions"
```

---

### Task 11: Wire the real steps into main and expose platform options

**Files:**
- Modify: `cmd/aicrme/main.go`, `internal/api/server.go`, `internal/api/runs.go`
- Create: `internal/api/options.go`
- Test: `internal/api/options_test.go`

**Interfaces:**
- Produces: `GET /api/options` returning `{"intents":[...],"platforms":[...]}` filtered to what the catalog can actually resolve for this cluster

- [ ] **Step 1: Write the failing test**

`internal/api/options_test.go`:
```go
package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/mchmarny/aicrme/internal/api"
	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
	"github.com/mchmarny/aicrme/internal/testfs"
)

func TestOptionsEndpoint(t *testing.T) {
	b := bus.New(8)
	srv, err := api.New(api.Config{
		Username: "admin", Password: "correct-horse", SessionTTL: time.Hour, LoginRate: 100,
		Intents:   []string{"training", "inference"},
		Platforms: []string{"kubeflow", "slurm", "runai", "none"},
	}, b, engine.New(b, engine.NewMemoryStore()), testfs.Static())
	if err != nil {
		t.Fatalf("api.New() error = %v", err)
	}
	ts, client := loggedInClient(t, srv.Handler())

	resp, err := client.Get(ts.URL + "/api/options")
	if err != nil {
		t.Fatalf("GET /api/options error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var got struct {
		Intents   []string `json:"intents"`
		Platforms []string `json:"platforms"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode error = %v", err)
	}
	if len(got.Intents) != 2 {
		t.Errorf("intents = %v, want 2 entries", got.Intents)
	}
	if len(got.Platforms) != 4 {
		t.Errorf("platforms = %v, want 4 entries", got.Platforms)
	}
}

func TestOptionsRequiresSession(t *testing.T) {
	h := newTestServer(t)
	req, _ := http.NewRequest(http.MethodGet, "/api/options", nil)
	rec := newRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}
```

Add `newRecorder()` as a one-line helper returning `httptest.NewRecorder()` in `jar_test.go`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/ -race -run TestOptions -v`
Expected: FAIL — `unknown field Intents in api.Config`

- [ ] **Step 3: Add Intents/Platforms to Config and the handler**

In `internal/api/server.go`, add to `Config`:
```go
	// Intents and Platforms are the two decision option sets, filtered by the
	// caller to what this cluster's coordinates can actually resolve.
	Intents   []string
	Platforms []string
```
Store them on `Server` and register `protected.HandleFunc("GET /api/options", s.handleOptions)`.

`internal/api/options.go`:
```go
package api

import "net/http"

type optionsResponse struct {
	Intents   []string `json:"intents"`
	Platforms []string `json:"platforms"`
}

// handleOptions returns the two decisions the console asks for. Everything
// else — service, accelerator, OS, component set, versions, values — is
// derived by the AICR recipe engine and never offered as a choice.
func (s *Server) handleOptions(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, optionsResponse{
		Intents:   s.intents,
		Platforms: s.platforms,
	})
}
```

- [ ] **Step 4: Wire the real steps in main.go**

Replace the bare `engine.New(b, engine.NewMemoryStore())` in `cmd/aicrme/main.go` with:
```go
	client, err := aicrclient.New()
	if err != nil {
		slog.Error("AICR client init failed", "error", err)
		os.Exit(1)
	}
	defer func() { _ = client.Close() }()

	eng := engine.New(b, engine.NewMemoryStore(),
		steps.NewDiscover(client, steps.DiscoverConfig{
			Namespace:  envOr("AICRME_NAMESPACE", "aicrme"),
			Image:      os.Getenv("AICRME_SNAPSHOT_IMAGE"),
			Privileged: true,
			Timeout:    10 * time.Minute,
		}),
		steps.NewRecommend(client),
	)
```
and pass `Intents: []string{"training", "inference"}`, `Platforms: []string{"kubeflow", "slurm", "runai", "none"}` into `api.Config`.

Platform filtering against the catalog's real overlay coverage (spec §2, "filtered to those with an overlay matching this cluster's coordinates") needs `client.CriteriaRegistry()`. Implement it here if the registry exposes a per-dimension value list; if it does not, ship the static list, log the limitation, and record it as an unresolved question rather than faking the filter.

- [ ] **Step 5: Run the full Go gate**

Run: `make test-coverage`
Expected: PASS, coverage ≥ 80%.

- [ ] **Step 6: Commit**

```bash
git add internal/api/ cmd/aicrme/main.go
git commit -S -m "feat(api): options endpoint and real Discover/Recommend wiring"
```

---

### Task 12: Discover and Recommend screens

**Files:**
- Create: `web/src/components/Discover.tsx`, `web/src/components/Recommend.tsx`, `web/src/components/Wizard.tsx`, `web/src/fixtures/kwok-run.json`
- Test: `web/src/components/Discover.test.tsx`, `web/src/components/Recommend.test.tsx`
- Modify: `web/src/App.tsx`, `web/src/api.ts`

**Interfaces:**
- Consumes: `AicrEvent`, `GET /api/options`, `POST /api/runs/{id}/decide`
- Produces: `Discover({ report })`, `Recommend({ options, onDecide })`, `Wizard({ events })`

- [ ] **Step 1: Record the fixture event stream**

Run the console against KWOK, capture the SSE stream, and save it as `web/src/fixtures/kwok-run.json` (a JSON array of `AicrEvent`):

```bash
kubectl -n aicrme port-forward svc/aicrme 8080:8080 &
curl -s -c /tmp/j -X POST localhost:8080/api/login -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"'"$PASSWORD"'"}'
curl -s -b /tmp/j -X POST localhost:8080/api/runs
curl -sN -b /tmp/j localhost:8080/api/events \
  | grep '^data: ' | sed 's/^data: //' | jq -s '.' > web/src/fixtures/kwok-run.json
```

This recorded stream is what makes the entire UI — including failure states and, later, the cockpit's slow-step callouts — developable and testable without touching GPU hardware. Every UI test from here on reads from a fixture like this one.

- [ ] **Step 2: Write the failing Discover test**

`web/src/components/Discover.test.tsx`:
```tsx
import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { Discover, type CapabilityReport } from './Discover'

const report: CapabilityReport = {
  headline: 'This is an EKS cluster with 64 H100 GPUs.',
  detail: '8 x p5.48xlarge, H100 SXM 80GB, EFA fabric, Kubernetes 1.33, Ubuntu 22.04',
  punchline: '0 of 64 GPUs are usable by a workload today.',
  usableGpus: 0,
  totalGpus: 64,
  gaps: [
    { id: 'gpu-driver', title: 'No GPU driver installed, the kernel does not see the devices', component: 'gpu-operator' },
    { id: 'device-plugin', title: 'No device plugin, Kubernetes cannot schedule nvidia.com/gpu', component: 'gpu-operator' },
  ],
}

describe('Discover', () => {
  it('opens with the capability statement, not an inventory', () => {
    render(<Discover report={report} />)
    expect(screen.getByRole('heading', { name: /EKS cluster with 64 H100 GPUs/ })).toBeDefined()
  })

  it('lists every gap', () => {
    render(<Discover report={report} />)
    expect(screen.getAllByTestId(/^gap-/)).toHaveLength(2)
  })

  it('names the component that closes each gap so this screen pre-explains the next', () => {
    render(<Discover report={report} />)
    expect(screen.getByTestId('gap-gpu-driver').textContent).toContain('gpu-operator')
  })

  it('lands on the number the finale pays off', () => {
    render(<Discover report={report} />)
    expect(screen.getByTestId('punchline').textContent).toBe('0 of 64 GPUs are usable by a workload today.')
  })
})
```

- [ ] **Step 3: Run test to verify it fails**

Run: `cd web && npm test`
Expected: FAIL — `Cannot find module './Discover'`

- [ ] **Step 4: Write Discover**

`web/src/components/Discover.tsx`:
```tsx
export interface CapabilityGap {
  id: string
  title: string
  detail?: string
  component: string
}

export interface CapabilityReport {
  headline: string
  detail?: string
  punchline: string
  usableGpus: number
  totalGpus: number
  gaps: CapabilityGap[]
}

export function Discover({ report }: { report: CapabilityReport }) {
  return (
    <section className="mx-auto max-w-2xl space-y-6">
      <div>
        <h2 className="text-2xl font-semibold text-slate-100">{report.headline}</h2>
        {report.detail && <p className="mt-1 text-sm text-slate-400">{report.detail}</p>}
      </div>

      <ul className="space-y-2">
        {report.gaps.map(g => (
          <li key={g.id} data-testid={`gap-${g.id}`} className="rounded border border-slate-800 bg-slate-900 p-3">
            <p className="text-slate-200">{g.title}</p>
            <p className="mt-1 text-xs text-slate-500">Closed by {g.component}</p>
          </li>
        ))}
      </ul>

      <p data-testid="punchline" className="text-xl font-semibold text-amber-400">
        {report.punchline}
      </p>

      <details className="text-sm text-slate-500">
        <summary className="cursor-pointer">Node detail, driver versions, taints, labels, raw snapshot</summary>
        <pre className="mt-2 overflow-auto text-xs">{JSON.stringify(report, null, 2)}</pre>
      </details>
    </section>
  )
}
```

- [ ] **Step 5: Write the failing Recommend test**

`web/src/components/Recommend.test.tsx`:
```tsx
import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { Recommend } from './Recommend'

const options = { intents: ['training', 'inference'], platforms: ['kubeflow', 'slurm', 'runai', 'none'] }
const recipe = {
  name: 'h100-eks-ubuntu-training',
  version: '0.19.0',
  componentCount: 16,
  components: [
    { name: 'gpu-operator', kind: 'Helm', version: 'v26.3.3', namespace: 'gpu-operator' },
    { name: 'kai-scheduler', kind: 'Helm', version: 'v0.14.1', namespace: 'kai-scheduler' },
  ],
}

describe('Recommend', () => {
  it('asks exactly two questions', () => {
    render(<Recommend options={options} recipe={null} onDecide={vi.fn()} />)
    expect(screen.getAllByRole('radiogroup')).toHaveLength(2)
  })

  it('submits both decisions together', () => {
    const onDecide = vi.fn()
    render(<Recommend options={options} recipe={null} onDecide={onDecide} />)
    fireEvent.click(screen.getByLabelText('training'))
    fireEvent.click(screen.getByLabelText('kubeflow'))
    fireEvent.click(screen.getByRole('button', { name: /continue/i }))
    expect(onDecide).toHaveBeenCalledWith({ intent: 'training', platform: 'kubeflow' })
  })

  it('does not submit until both are chosen', () => {
    const onDecide = vi.fn()
    render(<Recommend options={options} recipe={null} onDecide={onDecide} />)
    fireEvent.click(screen.getByLabelText('training'))
    fireEvent.click(screen.getByRole('button', { name: /continue/i }))
    expect(onDecide).not.toHaveBeenCalled()
  })

  it('folds the resolved component list behind a summary', () => {
    render(<Recommend options={options} recipe={recipe} onDecide={vi.fn()} />)
    expect(screen.getByText(/16 components/)).toBeDefined()
    expect(screen.getByText(/gpu-operator v26.3.3/)).toBeDefined()
  })
})
```

- [ ] **Step 6: Write Recommend**

`web/src/components/Recommend.tsx`:
```tsx
import { useState } from 'react'

export interface Options { intents: string[]; platforms: string[] }
export interface ComponentSummary { name: string; kind: string; version: string; namespace: string }
export interface RecipeSummary { name: string; version: string; componentCount: number; components: ComponentSummary[] }

// Platform labels describe what the user types to use it, not what it is.
const platformLabel: Record<string, string> = {
  kubeflow: 'kubectl apply -f trainjob.yaml',
  slurm: 'sbatch train.sh',
  runai: 'runai submit',
  none: 'just the runtime',
}

function Choice({ name, legend, values, value, onChange, describe }: {
  name: string; legend: string; values: string[]; value: string
  onChange: (v: string) => void; describe?: (v: string) => string
}) {
  return (
    <fieldset role="radiogroup" aria-label={legend} className="space-y-2">
      <legend className="text-sm font-medium text-slate-300">{legend}</legend>
      {values.map(v => (
        <label key={v} className="flex cursor-pointer items-center gap-3 rounded border border-slate-800 bg-slate-900 p-3">
          <input type="radio" name={name} value={v} aria-label={v}
            checked={value === v} onChange={() => onChange(v)} />
          <span className="text-slate-200">{v}</span>
          {describe && <code className="ml-auto text-xs text-slate-500">{describe(v)}</code>}
        </label>
      ))}
    </fieldset>
  )
}

export function Recommend({ options, recipe, onDecide }: {
  options: Options
  recipe: RecipeSummary | null
  onDecide: (d: { intent: string; platform: string }) => void
}) {
  const [intent, setIntent] = useState('')
  const [platform, setPlatform] = useState('')

  return (
    <section className="mx-auto max-w-2xl space-y-6">
      <Choice name="intent" legend="What is this cluster for?" values={options.intents}
        value={intent} onChange={setIntent} />
      <Choice name="platform" legend="How do you want to submit work?" values={options.platforms}
        value={platform} onChange={setPlatform} describe={v => platformLabel[v] ?? ''} />

      <button
        onClick={() => { if (intent && platform) onDecide({ intent, platform }) }}
        className="w-full rounded bg-emerald-600 py-2 text-white disabled:opacity-40"
        disabled={!intent || !platform}
      >
        Continue
      </button>

      {recipe && (
        <details className="rounded border border-slate-800 bg-slate-900 p-3">
          <summary className="cursor-pointer text-slate-200">
            <strong>{recipe.componentCount} components</strong>, every version pinned and signed
          </summary>
          <ul className="mt-3 space-y-1 font-mono text-xs text-slate-400">
            {recipe.components.map(c => (
              <li key={c.name}>{c.name} {c.version} → {c.namespace}</li>
            ))}
          </ul>
        </details>
      )}
    </section>
  )
}
```

Note: the fold test asserts `gpu-operator v26.3.3`; the component renders `gpu-operator v26.3.3 → gpu-operator`. `getByText` with a substring matcher handles this, but if it fails, use `screen.getByText(/gpu-operator v26\.3\.3/)` rather than loosening the component.

- [ ] **Step 7: Wire the Wizard into App**

`web/src/components/Wizard.tsx` selects the screen from the latest `phase` event and the run's `state`: `discover` → `<Discover>`, `awaiting_decision` on `recommend` → `<Recommend>`. Add `fetchOptions()` and `decide(runId, decisions)` to `api.ts`. Keep the timeline visible as a right rail so the calm wizard already shows the stream that becomes the cockpit in Phase 2.

- [ ] **Step 8: Run all tests**

```bash
cd web && npm test
cd .. && make test-coverage
```
Expected: PASS on both.

- [ ] **Step 9: Commit**

```bash
git add web/
git commit -S -m "feat(web): Discover and Recommend wizard screens with recorded-stream tests"
```

---

### Task 13: End-to-end through Recommend on KWOK

**Files:**
- Create: `test/e2e/discover-recommend.sh`
- Modify: `.github/workflows/e2e.yaml`

- [ ] **Step 1: Write the e2e script**

`test/e2e/discover-recommend.sh` extends the Task 6 smoke test: create a KWOK cluster with fake GPU nodes, install the chart, log in, `POST /api/runs`, poll `GET /api/runs/{id}` until `state == "awaiting_decision"` with `pending == ["intent","platform"]`, `POST /api/runs/{id}/decide` with both, then poll until `state == "done"` and assert `recipe.json` resolved a non-zero component count. Fail the script if the run reaches `failed`, printing the run's `error` and the last 50 SSE events.

- [ ] **Step 2: Run it locally**

Run: `./test/e2e/discover-recommend.sh`
Expected: `PASS`. If recipe resolution fails with `ErrCodeInvalidRequest` because the KWOK snapshot has zero specificity, that is a real finding — record it and decide whether the console supplies fallback criteria for simulated clusters, rather than weakening the test.

- [ ] **Step 3: Add to the e2e workflow and commit**

```bash
git add test/e2e/ .github/workflows/e2e.yaml
git commit -S -m "test(e2e): full Discover-to-Recommend arc on KWOK"
```

**Phase 1 is now demoable:** install on KWOK, open the console, watch the snapshot run, read the capability statement and gap list, answer two questions, see 16 pinned components.

---

## Roadmap for Phases 2-5

Not planned in detail here. Re-run `superpowers:writing-plans` per phase, informed by what Phase 1 produced.

**Phase 2 — Applier, cockpit, observer (the bulk of the work).** Preserve this analysis:

The applier execs `bash deploy.sh` once, from the bundle directory, with `NO_COLOR=1` and `KUBECONFIG_FLAG` / `DRY_RUN_FLAG` / `HELM_DEBUG_FLAG` exported. Flags: `[--no-wait] [--best-effort] [--retries N]`. It parses these stable markers from stdout into `bus.Event`s:

| Marker (NO_COLOR=1) | Event |
|---|---|
| `┌─ [N/M] <name>  →  <namespace>` | `KindComponent`, component started, index N of M |
| `└─ ✓ <name> installed` | `KindComponent`, succeeded |
| `└─ ✗ <name> FAILED (after N attempts)` | `KindComponent`, `LevelError` |
| `  ↺ <name>: attempt N/M failed, retrying in Ss...` | `KindComponent`, `LevelWarn` |
| `✓ Pre-flight checks passed` | `KindPhase` |
| `⚠ <msg>` / `✗ <msg>` | `KindLog` at warn / error |

Tests: golden files asserting the marker-to-event mapping against a captured `deploy.sh` transcript, plus `TestDeployTemplateUnchanged` pinning the sha256 of `pkg/bundler/deployer/helm/templates/deploy.sh.tmpl` from the pinned aicr module so a CI failure forces a parser review on every upstream edit.

Then upstream a PR adding an opt-in machine-readable stream (`AICR_DEPLOY_EVENTS=jsonl`) to that template, and retire the parser. This is the project's one upstream contribution — reallocated from the now-moot `Snapshot()`/`Validate()` PR.

Also in Phase 2: the `observer` (shared informers over Pods, Events, Nodes, DaemonSets, Deployments; `client-go` fake clientset in tests), the ConfigMap-backed `engine.Store`, the cockpit layout expansion, the static slow-step callout map keyed by component, and the failure state — which the spec correctly insists is designed in, not retrofitted.

**Phase 3 — Validate and Prove, simulated on Kind.** Validate via `client.ValidateState(...)` with `WithValidationNoCluster(true)` in unit tests and per-component `pkg/chainsaw.Run` against `recipes/checks/<name>/health-check.yaml`. Prove needs the terminal-but-active `StateActive`, a Stop-workload control, and Reset ordering workload-teardown before component uninstall. **The training TrainJob must be authored** — `demos/workloads/training/` has only `gke-nccl-test-tcpxo.yaml`. Inference reuses `vllm-agg.yaml` / `nimservice-llama-3-2-1b.yaml` / `chat.html`.

**Phase 4 — Real hardware: EKS, then GKE.** Capture a real EKS snapshot fixture and a real `deploy.sh` transcript on day one; both retro-feed Tasks 9 and Phase 2's golden files. Calibrate the slow-step map against real timings. Investigate image pre-pull.

**Phase 5 — AKS, reset, GitOps export, verification screen.** AKS needs the `az aks nodepool list` paste-in feeding `--aks-gpu-pools` (ADR-015). Export regenerates the bundle through the `argocd` / `argocd-helm` / `flux` deployers via `MakeBundle` with a different `BundleConfig`, and downloads it — never installing Argo or Flux.

---

## Self-review

**Spec coverage (Phases 0-1 scope only).** §Architecture units `bus`/`engine`/`api`/`web` → Tasks 2-5. §Packaging install/URL/credentials/auth/security posture → Tasks 4, 6. §UX phase 1 Discover → Tasks 8, 9, 12. §UX phase 2 Recommend → Tasks 10, 11, 12. §Testing strategy rows for `engine`, `bus`, `api`, `web`, end-to-end → Tasks 2-6, 12, 13; `applier` and `observer` rows are Phase 2. §Data flow SSE + ring replay → Tasks 2, 4, 5. §State ConfigMap checkpoint → interface in Task 3, implementation deferred to Phase 2 with the reason recorded. §AICR integration Discover/Recommend rows → Tasks 7-10; Bundle/Apply/Validate/Prove rows are Phases 2-3.

**Gaps deliberately left open:** platform filtering against real overlay coverage (Task 11 Step 4 — ships static if the registry cannot express it, and says so); gap rules beyond the driver rule (Task 9 Step 5 — fixture-calibrated, not guessed).

**Type consistency:** `bus.Event` fields match the TS `AicrEvent` one-for-one. `engine.Emit` is `func(bus.Event)` at every call site. `engine.Step` is `Phase()/Requires()/Run()` in the interface, the fake, `discover`, and `recommend`. `Run.Artifacts` keys are `snapshot.yaml`, `capability.json`, `recipe.json` consistently across Tasks 8, 9, 10, 12. `gap.Report` JSON tags match `CapabilityReport` in `Discover.tsx`. `steps.RecipeSummary` JSON tags match `RecipeSummary` in `Recommend.tsx`.

---

## Unresolved questions

1. **Platform filtering.** Whether `aicr.CriteriaRegistry` can enumerate the platform values that have an overlay for a given service+accelerator+OS. If not, the spec's "filtered to those with an overlay matching this cluster's coordinates" cannot be honored without resolving speculatively per platform — which is four extra resolves on a screen that should feel instant.
2. **KWOK specificity.** `ResolveRecipeFromSnapshot` fails closed when snapshot-derived criteria have zero specificity (AICR issue #1888). Whether a KWOK snapshot plus intent+platform clears that bar decides whether Phase 1's e2e needs fallback criteria for simulated clusters.
3. **GHCR package visibility.** Spec §Packaging flags this as the first thing that breaks for anyone but the author. Nothing in Phases 0-1 depends on it (Kind loads the image locally), but it blocks the first external demo, so set it when the first image is pushed.
4. **Snapshot agent image.** `AgentConfig.Image` is left empty, taking the module default. Confirm the default tag matches the pinned aicr `v0.19.0` and is publicly pullable, or the very first thing the console does on a customer cluster fails.
5. **Ownership and budget.** Spec Open Question 1, untouched by this plan. It does not block Phases 0-1 but does decide whether Phase 4's real-hardware EKS/GKE time exists.

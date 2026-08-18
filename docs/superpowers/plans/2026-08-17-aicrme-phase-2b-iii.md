# Phase 2b-iii Implementation Plan — the observer's visible half

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The cockpit shows, against the deployment action currently installing, what the cluster is doing while it installs — and surfaces `ImagePullBackOff`, crash loops and `FailedScheduling`.

**Architecture:** The engine keeps a small attribution snapshot (run, namespaces, phase, active action, generation) updated on marker transitions. The observer reads it once per watch event and stamps `Event.Component`. Pod and Event informers are namespace-scoped and started lazily once a recipe resolves. Cluster events carry typed `ClusterData` instead of formatted strings, and the cockpit renders the highest-severity unresolved condition per row.

**Tech Stack:** Go 1.26, `k8s.io/client-go` (fake clientset for tests), React/TypeScript SPA, `golangci-lint`, Kind + KWOK for e2e.

**Spec:** `docs/superpowers/specs/2026-08-17-aicrme-phase-2b-iii-design.md` — read it before Task 1. `docs/phase-2-handoff.md` carries the inherited constraints.

## Global Constraints

- **`github.com/NVIDIA/aicr` is pinned at `v0.19.0`.** No version may change; `make check-aicr-pin` enforces it.
- **Coverage floor 80%** aggregate. All Go tests run under `-race`. Web tests via `make test-web` — currently 87, must not drop.
- **`make qualify` must pass before every commit.**
- **Commits signed (`-S`).** No `Co-Authored-By`, no sign-off (`-s`), no "Generated with" trailer. Branch `phase-2b-iii`, never `main`.
- **Never delete, skip, or weaken an existing test.**
- **Correlation is temporal, never ownership.** Every message, comment and label says "while `<action>` installs", never "belongs to". `deploy.sh.tmpl:488` documents that convergence continues after the script exits — Nodewright for 10-20 minutes.
- **13 components, 14 deployment actions.** Rows are deployment actions; `[N/14]` counts actions. Do not collapse them.
- **The observer aggregates, it never relays.** `internal/bus` drops live events for any subscriber more than 256 behind. A relay-shaped path starves the browser of the `deploy.sh` marker stream, which is the product.
- **No store I/O and no artifact clone on the per-watch-event path.** `Engine.Current()` deep-copies every artifact.
- **Nothing new on a blocking startup path.** 2b-i crashlooped the console on an unbounded `WaitForCacheSync`; 2b-ii caught an unbounded Deployment lookup before it shipped.
- **Prefer self-documenting code.** Comment *why*, never *what*. `misspell` locale US. `goimports` grouping is enforced and differs from `gofmt`.
- **Twelve tests shipped in Phase 2b-ii that passed while the property they named was broken** — across ten tasks, two fix waves and a final round; four caught by their own author. For every test you write: state what would have to break for it to fail. If the answer is "nothing I can name", it is decoration. **Print `git diff --numstat` and confirm non-zero before drawing any conclusion from a mutation** — one that silently matches nothing gives a green run that reads as evidence.

---

## File Structure

**Create:**
- `internal/bus/cluster.go` — `ClusterData`, severity, and condition precedence.
- `internal/bus/cluster_test.go`
- `internal/engine/attribution.go` — the attribution snapshot and its accessor.
- `internal/engine/attribution_test.go`
- `internal/observer/pods.go` — Pod handler and actionable-state filter.
- `internal/observer/events.go` — Event handler, Warning filter, dedupe.
- `internal/observer/scoped.go` — lazy per-namespace informer lifecycle.
- `web/src/components/ComponentConditions.tsx` — per-row condition rendering.

**Modify:**
- `internal/bus/event.go` — no schema change; document `Component` on cluster events.
- `internal/observer/handlers.go` — emit `ClusterData`; read the snapshot.
- `internal/observer/observer.go` — accept the attribution accessor; wire scoped informers.
- `internal/steps/apply.go` — update the snapshot on marker transitions.
- `internal/engine/engine.go` — own the snapshot; clear it on leaving Apply.
- `web/src/pipeline.ts` — fold attributed cluster events into `ComponentState`.
- `web/src/components/Cockpit.tsx` — render conditions per row.
- `test/e2e/apply-real.sh` — assert correlation and volume.
- `docs/phase-2-handoff.md` — Task 9.

---

## Task 1: Typed cluster data

**Files:**
- Create: `internal/bus/cluster.go`, `internal/bus/cluster_test.go`
- Modify: `internal/observer/handlers.go`

**Interfaces:**
- Produces: `bus.ClusterData`, `bus.Severity`, `ClusterData.Supersedes`.
- Consumes: nothing.

- [ ] **Step 1: Write the failing tests**

`internal/bus/cluster_test.go` must cover, each as its own test:

1. `TestSeverityOrdering` — `SeverityInfo < SeverityWarn < SeverityError`.
2. `TestSupersedesPrefersHigherSeverity` — an `error` condition supersedes a `warn` on the same resource.
3. `TestSupersedesPrefersNewerAtEqualSeverity` — same severity, later `At` wins.
4. `TestResolvedConditionSupersedesUnresolved` — a `Resolved` condition for the same `(UID, Reason)` supersedes the unresolved one **regardless of severity**. This is what stops a cleared `ImagePullBackOff` sitting on a row forever.
5. `TestSupersedesIsNotTransitiveAcrossResources` — conditions on different UIDs never supersede each other; each resource holds its own.
6. `TestClusterDataRoundTripsThroughEventData` — marshal into `bus.Event.Data`, unmarshal, all fields intact.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/bus/ -race -run 'TestSeverity|TestSupersedes|TestResolved|TestClusterData' -v`
Expected: FAIL — `undefined: ClusterData`.

- [ ] **Step 3: Implement**

```go
package bus

import "time"

// Severity orders cluster conditions so a row can show the one that matters
// most. It is deliberately separate from Level: Level is how loudly to render
// an event in the timeline, Severity is which condition wins a row.
type Severity int

const (
	SeverityInfo Severity = iota
	SeverityWarn
	SeverityError
)

// ClusterData is the typed payload of a KindCluster event. It exists instead
// of a formatted message string because the cockpit needs to compare and
// supersede conditions per row, and parsing prose to do that is how a stale
// ImagePullBackOff ends up pinned to a row forever.
//
// Kind/Namespace/Name/UID identify the resource. UID is the identity that
// matters: a Deployment deleted and recreated under the same name is a
// different resource, and its old conditions must not survive.
type ClusterData struct {
	Kind      string    `json:"kind"`
	Namespace string    `json:"namespace,omitempty"`
	Name      string    `json:"name"`
	UID       string    `json:"uid"`
	Container string    `json:"container,omitempty"`
	Reason    string    `json:"reason"`
	Ready     int32     `json:"ready,omitempty"`
	Desired   int32     `json:"desired,omitempty"`
	Severity  Severity  `json:"severity"`
	// Resolved marks a condition clearing rather than arising. A resolved
	// condition always supersedes the unresolved one it clears, whatever the
	// severity -- otherwise a row keeps showing a failure that has gone away.
	Resolved bool      `json:"resolved,omitempty"`
	At       time.Time `json:"at"`
}

// Supersedes reports whether d should replace prev on a component row.
func (d ClusterData) Supersedes(prev ClusterData) bool {
	if d.UID != prev.UID || d.Reason != prev.Reason {
		return false
	}
	if d.Resolved != prev.Resolved {
		return d.Resolved
	}
	if d.Severity != prev.Severity {
		return d.Severity > prev.Severity
	}
	return d.At.After(prev.At)
}
```

- [ ] **Step 4: Migrate the three existing kinds**

`internal/observer/handlers.go`'s DaemonSet, Deployment and Node handlers currently publish a formatted string. They now also attach `ClusterData` (`Ready`/`Desired` for the workloads, the allocatable transition for Nodes) while keeping the message for the timeline. **Do not change the message text** — `web/src/pipeline.ts` and the e2e scripts read it, and this task's scope is adding structure, not reformatting output.

- [ ] **Step 5: Run to verify they pass**

Run: `go test ./internal/bus/ ./internal/observer/ -race -count=1 -v`
Expected: PASS, including every pre-existing observer test unchanged.

- [ ] **Step 6: Bite-proof the resolved rule**

Invert `return d.Resolved` to `return !d.Resolved`. Confirm `TestResolvedConditionSupersedesUnresolved` fails **alone**. Restore exactly; `git status --short` empty.

- [ ] **Step 7: Qualify and commit**

```bash
make qualify
git add internal/bus/ internal/observer/
git commit -S -m "feat(bus): typed cluster data with condition precedence

A row must show the highest-severity unresolved condition, which means
comparing conditions -- and comparing them by parsing the prose we render is
how a cleared ImagePullBackOff stays pinned to a row forever.

Identity is the UID, not the name: a Deployment deleted and recreated under
the same name is a different resource and must not inherit conditions.

A resolved condition supersedes its unresolved twin regardless of severity,
which is the rule that lets a row recover."
```

---

## Task 2: The attribution snapshot

**Files:**
- Create: `internal/engine/attribution.go`, `internal/engine/attribution_test.go`
- Modify: `internal/engine/engine.go`, `internal/steps/apply.go`

**Interfaces:**
- Produces: `engine.Attribution`, `Engine.Attribution() Attribution`, `Engine.setActiveAction(...)`.
- Consumes: nothing from Task 1.

- [ ] **Step 1: Understand why the obvious approach does not work**

Read `internal/steps/apply.go`'s `trackComponents` and `internal/engine/engine.go:603` and `:632` before writing anything.

`trackComponents` writes into the **scratch** run the step was handed. `e.current.Components` is assigned only *after* the step returns. **An accessor over `e.current` therefore reads stale state for the whole of Apply** — the spike observed exactly this, with the progress line printing once at the end with all 14 actions already `installed`. The snapshot below exists because that path does not work.

- [ ] **Step 2: Write the failing tests**

`internal/engine/attribution_test.go`:

1. `TestAttributionIsEmptyBeforeAnyRun` — zero value, no active action.
2. `TestAttributionCarriesTheActiveAction` — after a marker transition, name and index are current.
3. `TestAttributionGenerationAdvancesOnEveryTransition` — three transitions yield three distinct generations.
4. `TestAttributionClearsActiveActionOnLeavingApply` — a terminal state leaves `RunID` and `Namespaces` but **no** active action.
5. `TestAttributionIsReadAtomically` — under `-race`, a reader never observes a half-updated snapshot (name from one action, index from another). Drive concurrent transitions and reads.
6. `TestAttributionDoesNotCloneArtifacts` — a fake whose artifact map is instrumented shows zero reads. This is the 2b-i constraint; the accessor runs per watch event.

- [ ] **Step 3: Run to verify they fail**

Run: `go test ./internal/engine/ -race -run TestAttribution -v`
Expected: FAIL — `undefined: Attribution`.

- [ ] **Step 4: Implement**

```go
package engine

// Attribution is the small, self-consistent view the observer needs to label a
// cluster event. It is read once per watch event, so it must be cheap: no
// artifact clone, no store I/O, one lock acquisition.
//
// It exists as its own value rather than as an accessor over e.current because
// e.current.Components is not updated until a step RETURNS (see engine.go's
// merge-back) -- an accessor over it would read stale state for the entire
// duration of Apply, which is exactly the window this feature exists to
// narrate.
type Attribution struct {
	RunID      string
	Namespaces map[string]struct{}
	Phase      Phase
	// ActiveAction is the deployment action deploy.sh is currently installing,
	// or empty between actions and outside Apply. It is a TEMPORAL cursor, not
	// a claim of ownership: deploy.sh's own note warns that convergence
	// continues after it exits.
	ActiveAction string
	ActiveIndex  int
	ActiveTotal  int
	// Generation advances on every transition so a consumer can tell a stale
	// read from a current one without comparing every field.
	Generation uint64
}

// Attribution returns a consistent snapshot.
func (e *Engine) Attribution() Attribution {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.attribution
}
```

`setActiveAction(name string, index, total int)` and `clearActiveAction()` both take `e.mu`, mutate, and bump `Generation`.

- [ ] **Step 5: Wire it to marker transitions, in the required order**

`trackComponents` calls `setActiveAction` when it sees a component's `started` marker — **after** forwarding the event to `emit`, never before.

That ordering is the contract: a cluster event must not cite a row whose header has not yet reached the bus, or the SPA receives an event referencing an action it has never heard of. Write the reason at the call site.

`clearActiveAction` is called when the run leaves Apply — on the step returning, on failure, and on a terminal state.

- [ ] **Step 6: Run to verify they pass**

Run: `go test ./internal/engine/ ./internal/steps/ -race -count=1 -v`
Expected: PASS.

- [ ] **Step 7: Two bite-proofs**

**(a)** Move the `setActiveAction` call to *before* `emit`. Confirm the ordering test fails alone.
**(b)** Remove the `Generation` bump. Confirm `TestAttributionGenerationAdvancesOnEveryTransition` fails alone.

Print `git diff --numstat` before each conclusion. Restore exactly.

- [ ] **Step 8: Qualify and commit**

```bash
make qualify
git add internal/engine/ internal/steps/
git commit -S -m "feat(engine): an attribution snapshot the observer can read cheaply

trackComponents writes the scratch run the step was handed, and
e.current.Components is assigned only after the step returns -- so an
accessor over e.current reads stale state for the entire duration of Apply,
which is precisely the window this feature narrates. The spike saw this and
the first draft of the design proposed that accessor anyway.

The snapshot updates AFTER the marker reaches the bus, so a cluster event can
never cite a row the SPA has not heard of yet.

ActiveAction is a temporal cursor, not a claim of ownership."
```

---

## Task 3: The observer stamps attribution

**Files:**
- Modify: `internal/observer/observer.go`, `internal/observer/handlers.go`
- Modify: `cmd/aicrme/main.go` (wire the accessor)

- [ ] **Step 1: Write the failing tests**

1. `TestPublishStampsTheActiveAction` — a cluster event published while action 7 is active carries `Component == "gpu-operator"`.
2. `TestPublishLeavesComponentEmptyWhenNoActiveAction` — outside Apply, `Component` is empty and the event still publishes.
3. `TestPublishReadsAttributionOncePerEvent` — a counting fake proves one read per event, not one per field.
4. `TestPublishDoesNotStampAcrossARunTransition` — an event racing a transition carries either the old or the new action, never a mix. Use the generation counter.

- [ ] **Step 2: Run to verify they fail, then implement**

`observer.New` takes an attribution accessor alongside the existing scope function — or replaces it, if the snapshot subsumes `RunScope` (it carries `RunID` and `Namespaces`). **Prefer replacing it**: two accessors returning overlapping state is how they drift. Say which you chose and why in the report.

`publish` reads the snapshot once, filters on `Namespaces`, and sets `RunID` and `Component`.

- [ ] **Step 3: Bite-proof**

Read the snapshot twice inside `publish` (once for the filter, once for the stamp). Confirm `TestPublishDoesNotStampAcrossARunTransition` fails. This is the mutation that matters: two reads is the natural way to write it and the wrong one.

- [ ] **Step 4: Qualify and commit**

---

## Task 4: Lazy, namespace-scoped informers

**Files:**
- Create: `internal/observer/scoped.go`
- Modify: `internal/observer/observer.go`

**This is the largest task in the phase.** Read the spec's Section 3 in full first.

- [ ] **Step 1: Understand the constraint**

The observer starts at pod start, **before any run exists**, so the recipe's namespaces are unknown then. `informers.WithNamespace` takes a *single* namespace, so following the run scope means **one factory per namespace** — roughly ten for the current recipe.

Nodes stay cluster-scoped and keep their existing factory. Only Pods and Events are scoped.

- [ ] **Step 2: Write the failing tests**

1. `TestScopedInformersDoNotStartBeforeAScopeExists` — no watches until a recipe resolves.
2. `TestScopedInformersStartOncePerNamespace` — a scope of three namespaces yields three factories.
3. `TestScopedInformersStopWhenTheRunEnds` — factories are stopped and released.
4. `TestScopedInformersAreIdempotent` — a repeated scope does not double-start.
5. `TestScopedInformerStartDoesNotBlock` — a client whose List never returns must not block the caller. This is the 2b-i crashloop, one door over: assert the call returns within a bounded time.
6. `TestScopedInformersSurviveAScopeChange` — a new run with different namespaces stops the old set and starts the new.

- [ ] **Step 3: Implement**

A `scopedInformers` type owning a `map[string]*factoryEntry`, a `sync.Mutex`, and start/stop lifecycle. Starting happens on a goroutine — **never on the caller's path** — with the same reasoning as 2b-ii's async `observer.Start`.

- [ ] **Step 4: Bite-proof**

Make the start synchronous. Confirm `TestScopedInformerStartDoesNotBlock` fails alone.

- [ ] **Step 5: Qualify and commit**

---

## Task 5: The Pod informer

**Files:** Create `internal/observer/pods.go`; modify `internal/observer/scoped.go`.

- [ ] **Step 1: Write the failing tests**

1. `TestPodNarratesImagePullBackOff` — and `ErrImagePull`, `CrashLoopBackOff`, unschedulable.
2. `TestPodDoesNotNarrateHealthyTransitions` — `Pending`→`Running` emits nothing; the workload ready counts already cover it.
3. `TestPodConditionResolves` — a pod leaving `ImagePullBackOff` emits a `Resolved` condition so the row can clear.
4. `TestPodInitialListDoesNotNarrate` — pre-existing broken pods at startup are not narrated as new.
5. `TestPodEmitsTypedClusterData` — `Kind`, `UID`, `Container`, `Reason`, `Severity` all populated.

- [ ] **Step 2: Register with `ResourceEventHandlerDetailedFuncs`**

Present in the pinned client-go at `tools/cache/controller.go:319`. Its `AddFunc(obj any, isInInitialList bool)` is what distinguishes an initial-list Add from a later one — suppress the former, process the latter.

- [ ] **Step 3: Bite-proof**

Suppress **all** Adds rather than initial-list Adds. Confirm a later-Add test fails while `TestPodInitialListDoesNotNarrate` still passes. That asymmetry is the assertion.

- [ ] **Step 4: Qualify and commit**

---

## Task 6: The Event informer

**Files:** Create `internal/observer/events.go`; modify `internal/observer/scoped.go`.

- [ ] **Step 1: Write the failing tests**

1. `TestEventNarratesWarnings` — `FailedScheduling` reaches the bus.
2. `TestEventIgnoresNormalType` — `Normal` never narrates.
3. `TestEventDedupesOnUIDAndReason` — the same `(UID, reason)` twice narrates once.
4. `TestEventDoesNotReEmitOnCountIncrease` — Kubernetes coalesces repeats into `count` precisely so consumers need not re-narrate; a rising count is not a new event.
5. `TestEventDedupeIsClearedOnResourceDeletion` — the map must not grow for the process lifetime.
6. `TestEventDedupeIsBoundedByGeneration` — a new run does not inherit the previous run's dedupe state.
7. `TestEventArrivingOnlyAsAddIsNarrated` — **the case initial-list suppression would swallow.** Events are created once and never updated, so a `Warning` arrives as an Add. Without `DetailedFuncs` this feature emits nothing at all.

- [ ] **Step 2: Apply a server-side Warning selector**

`informers.WithTweakListOptions` sets `FieldSelector: "type=Warning"` where the API server supports it, so filtering happens before the bytes are sent rather than after.

- [ ] **Step 3: Bite-proof**

Remove the dedupe; assert the emitted count explodes for a repeated event. Then remove `DetailedFuncs` and confirm `TestEventArrivingOnlyAsAddIsNarrated` fails — that one is the whole point of the task.

- [ ] **Step 4: Qualify and commit**

---

## Task 7: Per-row rendering

**Files:** Create `web/src/components/ComponentConditions.tsx`; modify `web/src/pipeline.ts`, `web/src/components/Cockpit.tsx`.

- [ ] **Step 1: Write the failing tests**

1. Attributed cluster events fold into the matching `ComponentState`, keyed on `Component`.
2. Unattributed events do **not** attach to any row and remain in the timeline.
3. A row shows the highest-severity unresolved condition.
4. A resolved condition clears the row.
5. Conditions on different UIDs coexist; same UID supersedes.
6. The label reads as temporal — "while installing" — and **not** as ownership. Assert the copy, because this is the one property a reviewer cannot check by reading behavior.

- [ ] **Step 2: Implement, then bite-proof**

Key the fold on `Component` alone (dropping the UID check) and confirm the coexistence test fails.

- [ ] **Step 3: Qualify and commit**

---

## Task 8: e2e assertions against the real install

**Files:** Modify `test/e2e/apply-real.sh`.

- [ ] **Step 1: Add three assertions**

1. **Cluster events appeared and were attributed** — at least one `kind=cluster` event carrying a non-empty `component` that matches an action in the run's projection.
2. **At most one event per normalized state transition** — a property, not a count. Group cluster events by `(uid, reason, resolved)` and assert no group exceeds one.
3. **Zero bus gaps and zero drops** — event IDs across the run are contiguous, which is what the absolute ceiling was only ever a proxy for.

**Do not add an events-per-run ceiling.** It encodes today's action count and breaks on an AICR bump; the handoff's standing rule is that assertions follow the pinned version, never a number someone wrote down once.

- [ ] **Step 2: Verify locally before committing**

`./test/e2e/apply-real.sh` takes ~16 min and needs Docker plus a writable `~/.kube` — it cannot run inside the sandbox. Run it, paste the output, and confirm the new assertions fire on real data rather than only on fixtures.

- [ ] **Step 3: Commit**

---

## Task 9: Update the handoff

**Files:** Modify `docs/phase-2-handoff.md`.

- [ ] **Step 1** Move to "Resolved in 2b-iii": per-component live sub-status, the Pod and Event informers, and the volume open question — which this phase answers with a property assertion rather than a measurement.

- [ ] **Step 2** Record what 2b-iii deliberately did not do: exact ownership attribution (with the `helm get manifest` route stated), and the recovered-run screen.

- [ ] **Step 3** Record the new constraints: correlation is temporal and the copy must stay honest; the attribution snapshot must be updated after the marker publishes; scoped informers are one factory per namespace and lazily started.

- [ ] **Step 4** Sweep for stale references, including outside `docs/` — Phase 2a's lesson is that a docs-only scope orphans code comments.

- [ ] **Step 5** `make qualify`, commit.

---

## Self-Review

**Spec coverage.** Section 1 (temporal correlation, unattributed as first-class, deferred ownership) → Tasks 2, 3, 7. Section 2 (the snapshot, staleness, ordering) → Task 2. Section 3 (scoping, lifecycle, initial-list, volume) → Tasks 4, 5, 6. Section 4 (typed data, precedence, clearing) → Task 1. Section 5 (real-install assertions, adversarial cases) → Tasks 5, 6, 8.

**Placeholder scan.** Tasks 3–8 describe some tests by required assertion rather than pasting bodies. That is deliberate where the test must adopt an existing harness — `internal/observer`'s fake-clientset shape, the SPA's `useEvents`/`MockEventSource` idiom, `apply-real.sh`'s existing `jq` filters — and inventing a parallel one would be the defect. Literal code appears wherever a new type or non-obvious control flow is introduced.

**Type consistency.** `bus.ClusterData` and `Severity` are defined in Task 1 and consumed unchanged in 5, 6, 7. `engine.Attribution` is defined in Task 2 and consumed in 3. `scopedInformers` is defined in Task 4 and extended, not redefined, in 5 and 6.

**Two hazards worth naming.** Task 3 may replace `RunScope` with `Attribution`; if it does, Tasks 4–6 must consume the replacement, not the original — the dispatch for each must say which exists by then. And Task 2 changes `trackComponents`, which Task 1 Step 4 also touches via `handlers.go`; Task 1 lands first, so Task 2's implementer must read the migrated version.

**One thing deliberately left to the implementer.** Whether `Attribution` subsumes `RunScope` or sits alongside it. Subsuming is cleaner and is the recommendation; the decision and its reasoning must appear in Task 3's report.

## Unresolved questions

1. **What does a row show while its action installs but nothing has happened?** Blank is indistinguishable from "no telemetry"; "waiting" is noisy across 14 rows. Best answered against the real install.
2. **Should pre-existing workloads in a recipe namespace be narrated?** Namespace scoping cannot distinguish "this run installed it" from "it was already there". Suppressing needs an ownership signal this phase defers.
3. ~~How aggressive should lazy teardown be?~~ **Decided 2026-08-17: as aggressive as possible.**
   Pod and Event informers stop as soon as the run reaches a terminal state — no
   grace window, no lingering watches. Task 4 implements that directly rather
   than making it configurable.

   **The cost, stated so nobody rediscovers it as a bug:** the console will not
   narrate the post-Apply convergence window `deploy.sh` warns about — up to
   10-20 minutes of node tuning on fresh GPU nodes. A driver DaemonSet that
   fails to come up *after* the run reports `done` will be invisible to the
   console. That is a deliberate trade of telemetry for a bounded, simple
   lifecycle, and it is the right default for a demo console whose runs are
   watched live. Revisit only with evidence that the missed window matters.

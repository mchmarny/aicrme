# Phase 2b-ii design — restart recovery and the SSE cursor

**Status:** design, revised after external review. Not yet planned.
**Spec:** `approach.md`. **Prior phase:** `docs/phase-2-handoff.md` (read "Constraints 2b-ii inherits" and Finding 1's required-contract list first).
**Predecessor design:** `docs/superpowers/specs/2026-08-16-aicrme-phase-2b-i-design.md`.

## Goal

A console pod that restarts mid-Apply comes back with the run's state, decisions, artifacts,
and per-component progress intact; lands the interrupted run in a state that requires an
explicit operator action; refuses to silently replace it; and reconnects the browser to a
working event stream instead of a live connection that receives nothing.

## Why now

Apply takes 10 to 20 minutes on real hardware. That is the window in which a pod restart stops
being a curiosity and starts costing a demo. Phase 4 runs on real clusters, so this is its hard
prerequisite.

Phase 2b-i built the half that runs at shutdown: `Engine.CancelAndWait` guarantees an in-flight
run reaches a terminal state and that the terminal write is detached from the canceled context.
That work is inert against `memoryStore`, which never fails and never survives anything. This
phase supplies the store that makes it matter.

## Scope

**In:** the ConfigMap-backed `engine.Store`, a recovery bootstrap contract, a bounded
per-component state projection, context threading for real store I/O, and the SSE cursor fix.

**Out, deferred to 2b-iii:** per-component live sub-status wired into the cockpit's component
rows, and the Pod and Event informers (`internal/observer` ships 3 of the 5 informer kinds
`approach.md` names). This phase *persists* component state; rendering it per-row against live
observer events is 2b-iii's job.

**Out, owned by Phase 3:** `StateActive` reachability, and Reset. Both are noted below where
they constrain this phase's code, but neither is built here.

---

## Section 1 — The ConfigMap store

### Shape: one ConfigMap, whole-run snapshot per Save

A single ConfigMap in the console's own namespace (`AICRME_NAMESPACE`, default `aicrme`) holds
the current run. Each `Save` marshals the entire `Run` and issues one `Update`.

`Save` fires at every state transition, never per-event, so write volume is small and a
whole-run write buys atomicity for free: there is no window in which persisted state and
persisted artifacts disagree.

The rejected alternative was splitting state into `data` and artifacts into `binaryData`,
rewriting artifacts only when they change. More efficient, more moving parts, and it optimizes
nothing worth the extra consistency reasoning at this volume.

The layout keeps exactly one run. No per-run ConfigMap, no label-selector discovery, no
retention policy or garbage collection to get wrong.

### It must not be the chart's ConfigMap, and must not be templated at all

`charts/aicrme/templates/configmap.yaml` already ships a ConfigMap named
`{{ include "aicrme.fullname" . }}` — plain `aicrme` by default — holding `AICRME_TLS` and
`AICRME_NAMESPACE`. The run store must not reuse it, and must not be added to the chart as a
second template either. Two reasons, both producing silent data loss:

- **Helm would revert the console's writes.** A templated object is reset to the chart's
  rendered content on every `helm upgrade`. Upgrading the console mid-Apply would wipe the run
  state it is actively checkpointing — the exact scenario this phase exists to survive.
- **`helm uninstall` must still remove it.** If the console creates the object at runtime and
  nothing owns it, it outlives the release. Install, run, uninstall, reinstall, and the console
  recovers a run from a previous life and presents it as interrupted. Worse than no
  persistence, because it is wrong rather than absent.

So: a distinct name (`<fullname>-run`, following the `-auth` Secret's convention), created and
updated by the console at runtime, carrying an `ownerReference` to the **Deployment**.

**The owner must be the Deployment, never the ReplicaSet.** A ReplicaSet is transient — a
rollout creates a new one and reaps the old — and `ownerReference` garbage collection would
then delete the run state on the next upgrade. The Deployment is the stable object whose
lifetime actually matches the release's.

### Single writer requires `strategy: Recreate`

`charts/aicrme/templates/deployment.yaml` sets `replicas: 1` and specifies **no `strategy`
key**, so it defaults to `RollingUpdate` with `maxSurge` rounding up to 1. During any upgrade
the old and new pods overlap, and both would recover from and write to the same ConfigMap
concurrently — two writers against a design that assumes one.

The chart sets `strategy: Recreate`. This is the proportionate fix: the console is a
single-operator demo tool where a few seconds of downtime during an upgrade costs nothing, and
leader election would be machinery for a problem the product does not have.

Optimistic concurrency is still enforced (below), because `Recreate` narrows the window rather
than proving it closed.

### Artifacts need an explicit, versioned envelope

`Run.Artifacts` is tagged `json:"-"` (`internal/engine/run.go`). The store therefore **cannot**
`json.Marshal(run)` and get a usable record — artifacts would be silently dropped, and
artifacts are most of what recovery needs.

That tag is load-bearing and stays: it is what keeps `snapshot.yaml` from leaking to the
browser through the HTTP API's `Run` responses. The store defines its own envelope type that
carries artifacts deliberately. Two consumers want two projections of one struct, and writing
that down is cheaper than a tag that has to satisfy both.

The envelope is versioned from the first commit. A schema field costs nothing now and is the
only thing that makes a future format change safe against a ConfigMap written by a previous
image.

### gzip, a fail-closed size guard, and a bounded decompressor

Artifacts are gzipped before encoding. `snapshot.yaml` is 66–73 KB on the KWOK fixtures
(`internal/gap/testdata/`) and YAML compresses roughly ten to one.

A `Save` whose encoded payload would exceed 800 KiB — below Kubernetes' ~1 MiB ConfigMap cap —
fails with a named, testable error rather than letting the API server reject an oversized
object with something opaque.

**Decompression is bounded on read.** A small stored payload can expand without limit, and the
pod has a 512Mi cap. The reader is wrapped in an `io.LimitReader` sized to the same budget the
write path enforces, so a malformed or hostile record fails as a decode error instead of an
OOM kill. The record is attacker-influenced only in unusual circumstances, but an unbounded
decompressor guarding a memory-capped process is not a risk worth carrying for the two lines
it costs to remove.

### `bundle.path` is dropped, never restored

`bundle.path` holds a path under `<WorkDir>/runs/<runID>/bundle`, inside the emptyDir the chart
mounts. That directory does not survive a restart. Persisting the key would hand Apply a path
to a directory that no longer exists (`internal/steps/apply.go` fails legibly on an empty
value, but the Retry is dead either way). The store drops it on write; Section 2's rewind is
where this constraint's real work lives.

### Per-component state is persisted; the event stream is not

Finding 1's contract list requires enough persisted state to redraw the pipeline after a
restart. This phase satisfies it with a **bounded projection**, not by persisting events: one
row per component carrying its name and latest status. Fourteen components at a few dozen
bytes each is a few KB — irrelevant against the cap, and it is what makes a recovered run
render as a pipeline rather than a bare failure.

The raw event stream stays unpersisted. That is what would actually strain the cap, and it is
history rather than state — after a restart the timeline starts empty and shows where the run
*is*, not how it got there.

### Save-failure policy, stated because today it reads as an oversight

Six call sites in `internal/engine/engine.go` call `store.Save`. Two check the error, four
discard it with `_ =`. Against `memoryStore` that is harmless; against an API server it is the
"failed checkpoint writes silently wedge the run" item from Finding 1's contract list.

- **`Start` and `Retry` stay checked, with their existing rollback.** Both already undo their
  state transition when the save fails, guarded by an identity check and an epoch-aliveness
  check (Phase 2a, Ruling 13). A run that cannot be recorded must not be left live with no
  goroutine.
- **`Decide` becomes checked, with rollback** — see Section 3.
- **The successful-step checkpoint becomes ordered.** The save that follows a step's success
  must carry the advanced `StepIndex` and must complete before the next step begins. Otherwise
  a crash between step success and checkpoint replays a completed step on Retry.
- **Mid-step checkpoints stay best-effort, but never silent.** Losing one degrades recovery
  granularity; failing a running Apply over it would be worse. They stay unchecked for control
  flow but **every failure logs at warn level**. Six to thirty warnings across a run is not
  noise — it is the only signal that recovery has quietly stopped working.
- **The terminal save in `finish` becomes visible, but stays unrecoverable.** It cannot roll
  back — the run is already terminal. A failure logs at error level and publishes a bus event.
  Note the actual consequence precisely: the persisted record is not *absent*, it is a **stale
  earlier checkpoint**. The next startup will find that older record and recover it as an
  interrupted run, which is a different and more confusing outcome than finding nothing.

### Writes are serialized, with bounded conflict retries

All store writes go through a single serialization point, and a `Conflict` from optimistic
concurrency triggers a bounded re-read-and-retry rather than a blind overwrite. Each retry
re-validates that the object still carries the expected owner UID, so the console cannot
clobber a ConfigMap that a different install recreated under the same name.

**No store I/O happens while holding `e.mu`.** This is not a style preference: after Phase
2b-i, the observer's scope accessor calls `Engine.CurrentID` and `Engine.Artifact` on a
per-watch-event path, and both take `e.mu`. Holding that lock across a ConfigMap round trip
would stall every observer publish for the duration of an API call. The existing pattern —
mutate under the lock, snapshot, unlock, then do I/O, then re-acquire to roll back on failure —
is what every new call site follows.

### Degradation

`main` already constructs a `kubernetes.Interface` that is nil outside a cluster. When it is
nil, the engine keeps `NewMemoryStore()` and logs a warning. `make build && ./bin/aicrme`
outside a cluster stays a supported development path, and the console never fails to start
because persistence is unavailable — the same posture, for the same reason, as the observer.

### Store interface changes

`Load(ctx, id)` is unusable at startup, which has no ID to ask for. The interface gains:

- `LoadCurrent(ctx) (*Run, error)` — recovery's entry point.
- `Delete(ctx) error` — backing the discard action in Section 2.

`memoryStore` implements both, so every existing test keeps working unchanged.

---

## Section 2 — Recovery semantics and the bootstrap contract

### Recovery runs before the server serves

Recovery completes before `httpSrv.ListenAndServe`. A window in which the console answers
requests while the recovered run is not yet installed is a window in which the SPA's automatic
`POST /api/runs` wins the race.

This does **not** reintroduce the 2b-i startup-hang class: every ConfigMap call is bounded by
an explicit timeout, and any failure falls through to the degraded path below rather than
blocking. Bounded-and-fails-open is the rule; the observer's unbounded `WaitForCacheSync` was
the counterexample.

### The recovered run must not be silently replaced

`web/src/App.tsx` posts `/api/runs` automatically on load — by design, because Discover needs
no decisions to begin — and treats a 409 as expected and silent. `Engine.Start` rejects only
`isLive` states (`internal/engine/engine.go`), and recovery produces `StateFailed`, which is
not live.

So without a change, the SPA destroys the recovered run automatically, on the normal path,
before the operator ever sees it. This is the single most important correction in this revision.

The engine gains a `recoveredPending` flag, set when recovery installs a run and cleared only
by an explicit operator action:

- **`Start` returns `ErrCodeConflict` (409) while the flag is set.** The SPA's existing 409
  handling then does exactly the right thing with no frontend change: it stays quiet and
  renders from the stream.
- **`Retry` clears the flag** and resumes, which is the intended path.
- **A new discard action clears the flag** and deletes the persisted record, freeing the
  console to start fresh. Without this the console would be permanently unable to begin a new
  run, which is a worse wedge than the one being fixed.

### The UI learns about it from the bus

On installing a recovered run, recovery publishes a small bootstrap set of events describing
it: the run's identity and phase, its component rows from the persisted projection, and the
interruption notice. The SPA's normal replay path then renders it with no new fetch path,
which is why bootstrap events are preferred to a `GET /api/runs/current` endpoint — the stream
is already the SPA's source of truth, and a second source would need reconciling against it.

### One resume path: land failed, let `Retry` work

A recovered non-terminal run lands in `StateFailed`. `Retry` is then the only way forward.

This is reuse, not preference. `Retry` already exists, is gated on `StateFailed`, and resumes
from `StepIndex`, which advances only after a step succeeds. It is tested and reviewed; a
second resume path would duplicate it.

It also disposes of `StateAwaitingDecision` honestly. Restoring a parked run to that state is
tempting and wrong: the state implies a goroutine blocked in `awaitDecisions`, and after a
restart there is none, so `Decide` would signal a channel nobody reads. Landing it
failed-and-retryable costs one click and loses nothing — `run.Decisions` persists, so `Retry`
re-parks only for decisions still missing.

`StateDone` and `StateActive` restore as-is. **`StateFailed` is explicitly not read-only** —
it is the retryable state, and describing terminal states as read-only was wrong in the prior
revision. `StateActive` is declared but unreachable; Phase 3 owns that, and recovery treats it
as terminal meanwhile.

### `StepIndex` rewinds for any retryable run at or beyond Bundle

The rewind rule keys on **retryability, not on how the run reached its state**. Any loaded run
in `StateFailed` — whether it failed before the crash or was marked failed by recovery — that
sits at or beyond the Bundle step rewinds to Bundle.

The prior revision rewound only runs that were non-terminal when interrupted, which left a run
that had *already* failed during Apply restoring as-is, with `bundle.path` dropped and its
Retry dead on arrival. The bundle directory is gone after a restart regardless of what state
the run was persisted in; the rewind must follow the missing directory, not the run's history.

Bundle takes seconds, is deterministic given the same recipe and snapshot, and writes only into
the emptyDir, so re-running it is cheap and has no side effects outside the pod.

The clamp identifies Bundle by `Phase() == PhaseBundle` rather than a hardcoded index, because
the step slice is assembled in `main` and an index would silently rot. **Startup asserts
exactly one step reports `PhaseBundle`** and fails fast otherwise — a rewind target that is
ambiguous or absent is a programming error, and discovering it during recovery on a real
cluster is the worst possible time.

### Loaded records are validated before they are trusted

A record that decodes is not yet a record worth installing. Recovery validates the run's ID
format, that `State` and `Phase` are values the engine defines, that timestamps are sane, and
that `StepIndex` is within the bounds of the configured step slice. A record failing validation
takes the unreadable path below — it is not partially installed.

### Unreadable is not the same as absent

The prior revision treated missing, corrupt, and unknown-version records identically, which
permits a real data-loss sequence: a transient API error yields "no current run", the SPA
starts a new one, and the new run's first `Save` overwrites a record that was valid and merely
unreadable at that moment — or was written by a newer image.

Recovery therefore distinguishes:

- **`NotFound`** — genuinely no prior run. Normal cold start. The console proceeds and will
  write when a run begins.
- **Any other failure** — API error, decode failure, failed validation, or an unrecognized
  schema version. Bounded retries first, since the common case is a transient API blip. If it
  still fails, the console starts with the **memory store for the remainder of this process**
  and **never writes the record it could not read.**

Refusing to overwrite what it could not understand is the conservative direction: an operator
can recover a preserved record by rolling back the image or reading it by hand, and can recover
from nothing at all only by re-running.

The console still starts in every case. Persistence is a convenience; starting is not.

### The error text is load-bearing

A recovered run carries a distinguishable error — "interrupted by a console restart" — so the
cockpit can say that rather than rendering a generic failure. A restart is not a failed
`helm install`, and an operator staring at a red pipeline needs to tell them apart before
deciding whether `Retry` is safe.

---

## Section 3 — Context threading and `Decide`

### Real I/O needs real contexts

`internal/api/runs.go` deliberately detaches `Start` and uses background contexts for `Retry`
and `Get`. The file's own comment already names this phase as the point it stops being safe:
"That stops being true once 2b-ii's store rewrite makes `Load`/`Save` real ConfigMap API calls
— at that point `context.Background()` here starts ignoring genuine caller cancellation
instead of hitting an in-memory map."

So: request contexts thread through `Start`, `Retry`, `Get`, and `Decide`, and every ConfigMap
call is bounded by an explicit timeout.

The one deliberate detachment stays: the **run's execution context** must outlive the request
that started it, because Apply takes 10–20 minutes and a closed browser tab must not cancel an
install. The distinction this phase draws is between the *execution* context (detached, as
today) and the *store I/O* context for a specific API call (the caller's, bounded). `Engine`
already separates these; the API layer has not been.

### `Decide` must persist before it acknowledges

`Engine.Decide` mutates `Decisions`, clears `Pending`, sets `StateRunning`, signals `resume`,
and returns — with no `Save` anywhere. A pod that dies immediately after a 200 loses the
operator's choice, and recovery re-parks for a decision they already made and were told was
accepted.

`Decide` persists before acknowledging. On a save failure it rolls back the in-memory mutation
and returns the error, so the click fails loudly instead of being silently lost — the same
shape `Start` and `Retry` already use.

This requires restructuring `Decide`, which currently holds `e.mu` for its whole body under a
`defer`. Per Section 1, no store I/O may happen under that lock: mutate, snapshot, unlock,
save, and re-acquire only to roll back. The `resume` signal must not be sent until the save
succeeds, or the step proceeds on a decision that was never recorded.

---

## Section 4 — The bus epoch

### The bug is silence, not breakage

`internal/bus` assigns event IDs from `nextID`, which resets to 1 on restart. The SPA
(`web/src/useEvents.ts`) keeps `lastId` in a ref and reconnects with `?since=lastId`.

After a restart the server filters everything at or below a cursor it never issued, so the
browser holds a live, healthy-looking connection and receives nothing. `detectGap` cannot fire,
because detecting a gap requires events to arrive. It presents as a console that stopped moving.

### An epoch announced on connect, as a named control event

`EventSource` cannot read response headers, so the epoch travels in the stream: on connect the
server emits a **named, ID-less** SSE control event carrying an epoch generated at process
start. It is ID-less deliberately — assigning it an ID would advance the client's cursor and
make the control channel indistinguishable from run data.

Persisting `nextID` was rejected. Restoring a stale counter risks *reusing* IDs, and a reused
ID is worse than a reset one: the SPA drops it silently, which is the failure this section
exists to remove.

### Reset alone is insufficient — the client must reconnect

Clearing `lastId` in place does not work. The server already selected its backlog using the
original cursor when the connection opened; mutating client state afterwards cannot retroactively
change what the server chose to send. If the stale cursor sat below the new process's `nextID`,
every lower-ID event is missed permanently.

So on an epoch change the SPA **clears its state and reconnects from zero**, and **ignores any
frames still queued from the stale `EventSource`** — those were selected under the old cursor
and would interleave a partial backlog into a freshly cleared timeline.

### Plus a server-side guard

`since > nextID` is an impossible cursor — a client cannot be ahead of the process issuing the
IDs — and replays from 0. The epoch fixes correctness; the guard covers the case where the
control event is missed or ignored.

`detectGap` stays. It catches ring eviction *within* one process, a different failure from a
restart, and still worth surfacing.

---

## Testing

| Property | Test |
|---|---|
| Artifacts survive a round trip | Envelope round trip with a `snapshot.yaml`-sized blob; fails if `json:"-"` silently drops them |
| `bundle.path` does not survive | Save a run carrying it, load, assert absent |
| Oversized payload fails closed | Artifacts over the threshold return the named error, not an API error |
| gzip actually shrinks the payload | Encoded size of a real KWOK snapshot is a fraction of raw |
| Decompression is bounded | A record expanding past the limit fails as a decode error, not an OOM |
| Unknown schema version is refused *and not overwritten* | Load takes the degraded path; assert no subsequent write touches the record |
| `NotFound` differs from unreadable | Table over both; only `NotFound` permits writing |
| Recovery lands non-terminal runs failed | Table over every `State`; `StateDone`/`StateActive` restore as-is |
| Rewind covers already-failed runs | A run persisted `StateFailed` at Apply recovers pointing at Bundle |
| `StateDone` does not rewind | A done run past Bundle keeps its `StepIndex` |
| Exactly one Bundle step | Startup fails fast with zero or two |
| Loaded records are validated | Bad ID, unknown state, out-of-range `StepIndex` each take the degraded path |
| `Start` is refused while recovery is pending | 409, matching the SPA's existing silent-409 handling |
| Discard frees the console | After discard, `Start` succeeds and the record is deleted |
| `Retry` clears the pending flag | A second `Start` after `Retry` behaves normally |
| Bootstrap events render the run | Recovery publishes identity, phase, and component rows |
| Component projection round-trips | Persisted rows reload with names and statuses intact |
| `Decide` persists before acknowledging | A store failing `Save` makes `Decide` return an error, leaves decisions unchanged, and does not signal `resume` |
| Step cursor is checkpointed in order | A crash after step success and before checkpoint does not replay the step on Retry |
| No store I/O under `e.mu` | A store whose `Save` blocks does not block `CurrentID`/`Artifact` |
| Conflict retries are bounded and owner-checked | A conflicting update retries, then fails; a foreign owner UID aborts |
| Best-effort failures are logged | A failing checkpoint emits a warning |
| Terminal-save failure is visible | Error log plus bus event |
| Two writers cannot overlap | Chart contract asserts `strategy: Recreate` |
| Owner is the Deployment | Fake-clientset assertion on the created object's `ownerReferences` kind |
| The store never touches the chart's ConfigMap | Name differs from `aicrme.fullname`; no template renders it |
| Stale cursor cases | `since` <, ==, and > the new `nextID`, each asserted separately |
| Epoch triggers reconnect, not just reset | The hook reconnects from zero and discards stale-source frames |

The ConfigMap store's tests use `client-go`'s fake clientset, matching `internal/observer`'s
pattern. Fake-clientset tests verify wiring, not API-server behavior, so the size guard is
tested against the encoder's own output rather than a real rejection.

## What this phase does not do

- **The raw event stream is not persisted.** The timeline starts empty after a restart. Per-component
  state *is* persisted, so the pipeline redraws; the narration that produced it does not.
- **No auto-resume.** An interrupted Apply never silently continues — Finding 1's fail-closed
  requirement, and correct regardless: the `deploy.sh` process tree died with the pod's PID
  namespace.
- **No multi-run history**, no run-history UI, no ConfigMap garbage collection beyond the
  `ownerReference`.
- **No leader election.** `strategy: Recreate` plus optimistic concurrency is the proportionate
  answer for a single-operator demo console.

## Constraint carried for Phase 3

**Reset must route through `finish` before bumping `epoch`.** `Engine.execute`'s epoch-guard
early returns are dead code today: `epoch` advances only via `Retry`, gated on `StateFailed`,
a state only the same goroutine's own `finish` sets. Reset is the feature that makes them
reachable. If Reset bumps `epoch` without first driving the superseded run through `finish`,
that goroutine's next `aliveLocked` check fails, it returns without calling `finish`, and `done`
closes with no terminal state persisted — reopening the completion-contract gap `CancelAndWait`
was built to close, through a different caller.

This phase makes that worse in one specific way: with a real store, "no terminal state
persisted" now means the ConfigMap keeps a stale non-terminal record, which the next startup
recovers and marks failed. The bug becomes durable rather than merely in-memory.

## Open questions

1. **Does a real cluster's snapshot fit?** 66–73 KB is the KWOK figure. Compression should make
   a substantially larger real snapshot comfortable, but the first honest measurement is Phase 4
   on EKS. The size guard turns a wrong guess into a legible error rather than a corrupted run.
2. **Should recovery distinguish "restarted" from "crashed"?** A pod gracefully drained by
   `CancelAndWait` should already have persisted a terminal state, so a recovered non-terminal
   run implies an *ungraceful* exit. Saying so in the error text may beat the generic wording;
   deferred until there is a real instance to look at.
3. **Is the discard action enough of a UI?** This phase gives it an endpoint and the engine
   contract. Whether the cockpit needs a distinct "discard and start over" control beside
   Retry, versus surfacing it only when Retry is inapplicable, is a 2b-iii question once the
   recovered-run rendering exists.

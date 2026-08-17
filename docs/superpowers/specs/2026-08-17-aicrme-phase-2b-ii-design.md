# Phase 2b-ii design — restart recovery and the SSE cursor

**Status:** design, not yet planned.
**Spec:** `approach.md`. **Prior phase:** `docs/phase-2-handoff.md` (read "Constraints 2b-ii inherits" and Finding 1's required-contract list first).
**Predecessor design:** `docs/superpowers/specs/2026-08-16-aicrme-phase-2b-i-design.md`.

## Goal

A console pod that restarts mid-Apply comes back with the run's state, decisions, and
artifacts intact, lands the interrupted run in a state that requires an explicit operator
`Retry`, and reconnects the browser to a working event stream instead of a silently dead one.

## Why now

Apply takes 10 to 20 minutes on real hardware. That is the window in which a pod restart
stops being a curiosity and starts costing a demo. Phase 4 runs on real clusters, so this is
its hard prerequisite, not a nicety.

Phase 2b-i built the half of this that runs at shutdown: `Engine.CancelAndWait` guarantees an
in-flight run reaches a terminal state and that the terminal write is detached from the
canceled context. That work is inert against `memoryStore`, which never fails and never
survives anything. This phase supplies the store that makes it matter.

## Scope

**In:** the ConfigMap-backed `engine.Store`, recovery on startup, and the SSE cursor fix.

**Out, deferred to 2b-iii:** per-component live sub-status in the cockpit, and the Pod and
Event informers (`internal/observer` ships 3 of the 5 informer kinds `approach.md` names).
These are the visible half of the observer and belong together in their own phase; mixing UI
and informer work into a persistence phase would make one review surface out of two unrelated
risk profiles.

**Out, owned by Phase 3:** `StateActive` reachability, and Reset. Both are noted below where
they constrain this phase's code, but neither is built here.

---

## Section 1 — The ConfigMap store

### Shape: one ConfigMap, whole-run snapshot per Save

A single ConfigMap with a well-known name in the console's own namespace
(`AICRME_NAMESPACE`, default `aicrme`) holds the current run. Each `Save` marshals the entire
`Run` and issues one `Update`.

`Save` fires 6 to 15 times per run — every call site is a state transition, none is per-event
— so write volume is a non-issue, and a whole-run write buys atomicity for free: there is no
window in which persisted state and persisted artifacts disagree with each other.

The rejected alternative was splitting run state into `data` and artifacts into `binaryData`,
rewriting artifacts only when they change. It is more efficient and has more moving parts, and
at this write volume it optimizes nothing worth the extra consistency reasoning.

The layout keeps exactly one run, per the "current run only" scope decision. There is no
per-run ConfigMap, no label selector discovery, and therefore no retention policy or garbage
collection to get wrong. Past runs are gone once a new one starts.

### It must not be the chart's ConfigMap, and must not be templated at all

`charts/aicrme/templates/configmap.yaml` already ships a ConfigMap named
`{{ include "aicrme.fullname" . }}` — plain `aicrme` by default — holding `AICRME_TLS` and
`AICRME_NAMESPACE`. The run store must not reuse it, and must not be added to the chart as a
second template either. Two distinct reasons, both of which produce silent data loss:

- **Helm would revert the console's writes.** A templated object is reset to the chart's
  rendered content on every `helm upgrade`. Upgrading the console mid-Apply would wipe the run
  state it is actively checkpointing — the exact scenario this phase exists to survive.
- **`helm uninstall` must still remove it.** If instead the console creates the object at
  runtime and nothing owns it, it outlives the release. Install, run, uninstall, reinstall, and
  the console recovers a run from a previous life and presents it as interrupted. That is worse
  than having no persistence at all, because it is wrong rather than absent.

So: a distinct name (`<fullname>-run`, following the `-auth` Secret's convention), created and
updated by the console at runtime, carrying an `ownerReference` to a Helm-owned object in the
release so that Kubernetes garbage-collects it on `helm uninstall`. The owner discovery
mechanism — downward-API pod name walked up its `ownerReferences` chain, versus an explicit
env var carrying the release's fullname — is left to the plan; the requirement is that the
object dies with the release and is never rendered by Helm.

### Artifacts need an explicit envelope

`Run.Artifacts` is tagged `json:"-"` (`internal/engine/run.go`). The store therefore **cannot**
`json.Marshal(run)` and get a usable record — artifacts would be silently dropped, and
artifacts are most of what recovery needs.

That tag is load-bearing and must stay: it is what keeps `snapshot.yaml` and friends from
leaking to the browser through the HTTP API's `Run` responses. So the store defines its own
envelope type that carries artifacts deliberately, rather than reusing the API's serialization.
Two different consumers want two different projections of the same struct, and writing that
down is cheaper than a tag that has to satisfy both.

The envelope is versioned from the first commit. A schema field costs nothing now and is the
only thing that makes a future format change safe to roll out against a ConfigMap written by
a previous image.

### gzip, and a fail-closed size guard

Artifacts are gzipped before encoding. `snapshot.yaml` is 66–73 KB on the KWOK fixtures
(`internal/gap/testdata/`) and YAML compresses roughly ten to one, which turns the dominant
cost into noise and buys an order of magnitude of headroom against the cap.

Kubernetes caps a ConfigMap at approximately 1 MiB. A `Save` whose encoded payload would
exceed a threshold below that ceiling (800 KiB) fails with a named, testable error rather than
letting the API server reject an oversized object with something opaque. Fail-closed and
legible beats fail-late and mysterious — the operator needs to know that the run is too large
to checkpoint, not that an `Update` returned a 413.

### `bundle.path` is dropped, never restored

`bundle.path` holds a filesystem path under `<WorkDir>/runs/<runID>/bundle`, inside the
emptyDir the chart mounts. That directory does not survive a pod restart.

Persisting the key would be actively worse than omitting it: recovery would hand Apply a path
to a directory that no longer exists. The store drops it on write. After recovery the key is
simply absent, and Bundle regenerates it — see Section 2, which is where the real work of
this constraint lives.

### Save-failure policy, stated because today it reads as an oversight

Six call sites in `internal/engine/engine.go` call `store.Save`. Two check the error, four
discard it with `_ =`. Against `memoryStore` that is harmless, because `memoryStore.Save`
cannot fail. Against an API server it is the "failed checkpoint writes silently wedge the run"
item from Finding 1's contract list.

The policy this phase establishes, documented at each call site so a future reader does not
"fix" one of them in the wrong direction:

- **`Start` and `Retry` stay checked, with their existing rollback.** Both already undo their
  state transition when the save fails, guarded by an identity check and an epoch-aliveness
  check (Phase 2a, Ruling 13). A run that cannot be recorded must not be left live with no
  goroutine — that is the 409 latch this rollback exists to prevent.
- **Mid-step checkpoints stay best-effort.** Losing a checkpoint degrades recovery granularity;
  failing a running Apply over it would be a worse outcome than the problem. These stay `_ =`,
  with a comment saying so.
- **The terminal save in `finish` becomes visible, but stays unrecoverable.** It cannot roll
  back — the run is already terminal, and there is no earlier state to return to. What it must
  not do is fail in silence: a failure logs at error level and publishes a bus event, so the
  operator learns that the run finished but its outcome was not persisted. That distinction
  matters because the next startup will not find the run, and the operator otherwise has no way
  to know why.

### Degradation

`main` already constructs a `kubernetes.Interface` that is nil outside a cluster. When it is
nil, the engine keeps `NewMemoryStore()` and logs a warning. `make build && ./bin/aicrme`
outside a cluster stays a supported development path, and the console never fails to start
because persistence is unavailable — the same posture, for the same reason, as the observer.

---

## Section 2 — Recovery semantics

### One resume path: land failed, let `Retry` work

A recovered non-terminal run lands in `StateFailed`. `Retry` is then the only way forward.

This is reuse, not a policy preference. `Retry` already exists, is gated on `StateFailed`, and
resumes from `StepIndex` — which advances only after a step succeeds, so it already points at
the step that needs redoing. It is tested and reviewed. A second resume path would duplicate
machinery that already does exactly this.

It also disposes of `StateAwaitingDecision` honestly. Restoring a parked run directly to that
state is tempting and wrong: the state implies a goroutine blocked in `awaitDecisions`, and
after a restart there is no goroutine, so `Decide` would signal a channel nobody is reading.
Landing it failed-and-retryable costs the operator one click and loses nothing — `run.Decisions`
persists, so `Retry` re-parks only for decisions still missing.

Terminal runs (`StateDone`, `StateFailed`, `StateActive`) restore as-is and are read-only.
`StateActive` is declared but currently unreachable; Phase 3 owns making it reachable, and
recovery treats it as terminal in the meantime.

### `StepIndex` must rewind past Bundle

This is the one place recovery is more than a restore, and the sharp edge of the whole phase.

If the crash happened during Apply, `StepIndex` points at Apply — but the bundle directory
died with the emptyDir. Retrying Apply would read an absent `bundle.path` and fail against a
directory that is not there.

So recovery clamps `StepIndex` back to the Bundle step whenever the persisted run had advanced
beyond it. Bundle takes seconds, is deterministic given the same recipe and snapshot, and
writes only into the emptyDir — re-running it is cheap and has no side effects outside the pod.
This is Finding 1's "bundle regeneration" contract item.

The clamp identifies the Bundle step by `Phase() == PhaseBundle` rather than by a hardcoded
index, because the step slice is assembled in `main` and an index would silently rot if the
arc changes. Exactly one step today has outputs that live on ephemeral disk, so this stays a
concrete rule rather than a general "ephemeral step" interface — YAGNI until a second one
exists.

### Failure to load is fail-open

A missing, unreadable, or corrupt ConfigMap logs a warning and starts the console with no
current run. Recovery is a convenience; the console starting is not. A corrupt persisted record
must never be able to brick the console — the same invariant Phase 2b-i established for the
observer, where an unreachable API server could otherwise stop `:8080` from binding.

"Corrupt" explicitly includes an envelope with an unrecognized schema version. Refusing to
guess at an unknown format is the fail-open behavior, not an exception to it.

### The error text is load-bearing

A recovered run carries a distinguishable error — "interrupted by a console restart" — so the
cockpit can say that rather than rendering a generic failure. A restart is not the same event
as a failed `helm install`, and an operator staring at a red pipeline needs to tell those apart
before deciding whether `Retry` is safe.

---

## Section 3 — The bus epoch

### The bug is silence, not breakage

`internal/bus` assigns event IDs from `nextID`, which resets to 1 when the process restarts.
The SPA (`web/src/useEvents.ts`) keeps `lastId` in a ref and reconnects with `?since=lastId`.

After a restart the server filters everything at or below a cursor it never issued, so the
browser holds a live, healthy-looking connection and receives nothing. `detectGap` cannot fire,
because detecting a gap requires events to arrive. The failure presents as a console that has
simply stopped moving.

### A per-process epoch, announced on connect

`EventSource` cannot read response headers, so the epoch has to travel in the stream itself:
on connect, the server emits an opening event carrying an epoch generated at process start.
The SPA compares it against the epoch it holds; on a change it clears its timeline and resets
`lastId` to 0.

Persisting `nextID` alongside the run was rejected. Restoring a stale counter risks *reusing*
IDs after a crash, and a reused ID is strictly worse than a reset one: the SPA drops it
silently, which is the same invisible failure this section exists to remove.

### Plus a server-side guard, because the epoch alone leaves a gap

If the new process has already issued more events than the client's old cursor, `since` is
*below* `nextID`, the epoch message has already been consumed, and the client appends
genuinely-new events onto a stale timeline — mixed history, no error anywhere.

So `since > nextID` is treated as an impossible cursor and replays from 0. A client cannot be
ahead of the process that issues the IDs, so the condition is unambiguous. The epoch fixes
correctness; the guard covers the case where the epoch message is missed or ignored.

`detectGap` stays as it is. It catches ring eviction *within* one process, which is a different
failure from a restart and still worth surfacing.

---

## Testing

The load-bearing properties, and how each is pinned:

| Property | Test |
|---|---|
| Artifacts survive a save/load round trip | Envelope round trip including a `snapshot.yaml`-sized blob; fails if `json:"-"` silently drops them |
| `bundle.path` does not survive | Save a run carrying it, load, assert the key is absent |
| Oversized payload fails closed | A run whose artifacts exceed the threshold returns the named error, not an API error |
| gzip actually shrinks the payload | Encoded size of a real KWOK snapshot is a fraction of raw; guards against silently disabling compression |
| Unknown schema version is refused | Load returns the fail-open path, not a partially-decoded run |
| Recovery lands non-terminal runs failed | Table over every `State`, asserting terminal states restore as-is |
| `StepIndex` rewinds past Bundle | A run persisted at the Apply step recovers pointing at Bundle |
| Terminal runs do not rewind | A `StateDone` run past Bundle keeps its `StepIndex` |
| Load failure does not stop startup | A store whose `Load` errors yields a running console with no current run |
| Terminal-save failure is visible | A store failing only the terminal save produces an error log and a bus event |
| Stale cursor replays from 0 | `since > nextID` returns the full ring |
| Epoch changes across processes | Two buses in one test yield different epochs; the SPA hook resets on change |
| The store never touches the chart's ConfigMap | Assert the object name differs from `aicrme.fullname`; a chart-contract assertion that no template renders the run ConfigMap |
| The run ConfigMap carries an ownerReference | Fake-clientset assertion on the created object, so an orphan on `helm uninstall` fails the build rather than surfacing as a stale recovered run |

The ConfigMap store's own tests use `client-go`'s fake clientset, matching `internal/observer`'s
established pattern. Fake-clientset tests verify wiring, not API-server behavior, so the size
guard is tested against the encoder's own output rather than against a real rejection.

## What this phase does not do

- **Event history is not persisted.** After a restart the timeline starts empty and shows
  current state rather than the history that produced it. This was a deliberate scope choice:
  persisting the event stream is what would actually strain the 1 MiB cap, and the run's state
  is what the operator needs to act.
- **No auto-resume.** An interrupted Apply never silently continues. This is Finding 1's
  fail-closed requirement, and it is also just correct: the `deploy.sh` process tree died with
  the pod's PID namespace, so there is nothing to continue.
- **No multi-run history**, no run-history UI, no ConfigMap garbage collection.

## Constraint carried for Phase 3

**Reset must route through `finish` before bumping `epoch`.** `Engine.execute`'s epoch-guard
early returns are dead code today: `epoch` advances only via `Retry`, which is gated on
`StateFailed`, a state only the same goroutine's own `finish` sets. Reset is the feature that
makes them reachable. If Reset bumps `epoch` without first driving the superseded run through
`finish`, that goroutine's next `aliveLocked` check fails, it returns without calling `finish`,
and `done` closes with no terminal state persisted — reopening the exact completion-contract
gap `CancelAndWait` was built to close, through a different caller.

This phase makes that worse in one specific way worth naming: with a real store, "no terminal
state persisted" now means the ConfigMap keeps a stale non-terminal record, which the next
startup will recover and mark failed. The bug becomes durable rather than merely in-memory.

## Open questions

1. **Does a real cluster's snapshot fit?** 66–73 KB is the KWOK figure. Compression should make
   even a substantially larger real snapshot comfortable, but the first honest measurement is
   Phase 4 on EKS. The size guard is what turns a wrong guess into a legible error instead of
   a corrupted run.
2. **Is one ConfigMap per console the right multi-tenancy story?** It is correct for the
   single-operator demo this console is scoped to, and it would need revisiting if the console
   ever served concurrent operators — which is an explicit non-goal today.
3. **Should recovery distinguish "restarted" from "crashed"?** Both land in the same state via
   the same path. A pod that was gracefully drained by `CancelAndWait` should already have
   persisted a terminal state, so a recovered non-terminal run implies an *ungraceful* exit.
   Saying so in the error text may be more useful than the generic wording above; deferred
   until there is a real instance to look at.

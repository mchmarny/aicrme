# Phase 2b-iii design — the observer's visible half

**Status:** design, revised after external review. Not yet planned.
**Spec:** `approach.md`. **Prior phase:** `docs/phase-2-handoff.md` — read "Constraints 2b-iii inherits" first.
**Predecessor designs:** `docs/superpowers/specs/2026-08-16-aicrme-phase-2b-i-design.md` (the observer), `docs/superpowers/specs/2026-08-17-aicrme-phase-2b-ii-design.md` (persistence).

## Goal

The cockpit shows, against the deployment action currently installing, what the
cluster is doing while it installs — `nvidia-driver-daemonset 3/8 ready` — and
surfaces the failure signals an operator needs: `ImagePullBackOff`, crash
loops, `FailedScheduling`.

## Terminology, fixed here because the previous draft got it wrong

**13 components, 14 deployment actions.** The resolved recipe has 13 components;
the bundle emits 14 numbered directories, because `kubeflow-trainer` contributes
both an upstream chart and a `-post` local chart. Phase 2a made this distinction
a finding and the handoff carries it; the first draft of this spec collapsed
them into "14 components" four times. Rows in the cockpit are **deployment
actions**, and `deploy.sh`'s `[N/14]` header counts actions, not components.

## Why now

2b-i built the observer and 2b-ii persisted its output, but nothing renders it
per row. `approach.md` §Apply(cockpit) promises this, and it is the difference
between a timeline that scrolls and a pipeline that explains itself.

`test/e2e/apply-real.sh` now drives a real install in CI on every push to
`main`, so for the first time there is genuine workload churn to observe and
assert against.

## Scope

**In:** temporal correlation of observer events to deployment-action rows,
computed server-side; Pod and Event informers with real volume control; typed
cluster event data; per-row rendering; e2e assertions against the real install.

**Out:** a designed recovered-run screen. 2b-ii's C1 fix made a recovered run
usable under time pressure; it works and is tested but has not had design
attention. Pairing UI design with informer work would put two unrelated risk
profiles behind one review surface.

---

## Section 1 — Correlation, and what it does *not* claim

### It is temporal, not ownership. The previous draft claimed otherwise and was wrong.

The first draft asserted that because 13 of 14 actions install with `--wait`,
the active action "genuinely is the one whose workloads are changing —
attribution is correct by construction." **That is false**, and `deploy.sh`
says so itself, in a note the draft read past
(`deploy.sh.tmpl:488-492`):

> NOTE: The above status reflects Helm install and manifest apply results, not
> whether the cluster is ready for GPU workloads. On fresh GPU nodes, cluster
> convergence may continue asynchronously after this script exits. Full
> workload readiness can take additional time for: node tuning (e.g.,
> Nodewright, ~10-20 min on fresh nodes)

`--wait` is a point-in-time readiness barrier. Controllers keep reconciling,
restarting and creating resources long after it returns — Nodewright for ten to
twenty minutes by the script's own estimate. Shared namespaces compound it:
`monitoring` holds three components' workloads, and pre-existing workloads in
those namespaces were never installed by this run at all.

So the console shows **"cluster activity while `<action>` installs"** and says
exactly that. It does not claim the activity belongs to the action.

### The honest framing dissolves the async problem rather than solving it

The previous draft promised that async actions' events would attribute to no
row. That is **not implementable from a cursor**: once the script advances,
delayed activity from an async action is indistinguishable from the new active
action's activity. The draft promised a property its own mechanism could not
deliver.

Under temporal correlation the question disappears. A late event during action
N+1 *is* cluster activity observed while N+1 installs, which is what the label
says. No async special case, no positional dependency on the async marker, and
one fewer thing pinned to the marker grammar.

### Unattributed is a first-class outcome

Events attribute to no row when there is no active action: outside Apply,
between actions, and after the run reaches a terminal state. Those stay in the
timeline. The UI renders that case deliberately rather than treating a missing
correlation as a defect.

### Exact ownership, deliberately deferred

Exact attribution is achievable — `helm get manifest <release>` returns
precisely the resources a release owns, and Pod owner traversal (Pod →
ReplicaSet → Deployment) closes the gap for pods. It needs no change to AICR's
output. It is deferred because temporal correlation is honest, far cheaper, and
we have no evidence yet of how misleading it actually is on a real install —
evidence `apply-real.sh` will now produce.

---

## Section 2 — The attribution snapshot

### `e.current` is stale during Apply, so there is nothing to read

The previous draft proposed a cheap accessor over `e.current`, in the shape of
`Engine.CurrentID`. **That accessor would read stale state for the entire
duration of Apply.**

`internal/steps/apply.go`'s `trackComponents` writes into the **scratch** run
the step was handed, and `e.current.Components` is assigned only at
`internal/engine/engine.go:603` and `:632` — both *after* the step returns.

This was observable and was observed: the spike's progress line printed exactly
once, at the end, with all 14 actions already `installed`. The draft recorded
that as a finding and then designed against `e.current` anyway.

### One small snapshot, updated on marker transitions

The engine keeps a single attribution snapshot, guarded by its own lock,
containing:

- `RunID`
- `Namespaces` — the resolved recipe's namespaces
- `Phase`
- `ActiveAction` — index, total, and name, or empty when none
- `Generation` — a counter bumped on every transition, so a consumer can tell
  a stale read from a current one

`trackComponents` updates it on each `KindComponent` marker and **clears
`ActiveAction` explicitly** when the run leaves Apply or reaches a terminal
state. It is read **once per observer event**, not per field, so a single event
cannot straddle two transitions.

This replaces, and must not be built on, 2b-ii's `newRunScopeFn` cache. That
cache exists to avoid `Engine.Current()`'s artifact deep-copy on a
per-watch-event path, and keys on run ID — which does not change when the
active action does.

### Marker ordering is part of the contract

A cluster event must not reference a row before that row's header has reached
the bus, or the SPA will receive an event citing an action it has never heard
of. The snapshot is therefore updated **after** the corresponding
`KindComponent` marker is published, not before. This ordering is a stated
contract with a test, not an incidental consequence of statement order.

---

## Section 3 — Pod and Event informers

### Scope the informers, not just the narration

The current factory is cluster-wide (`internal/observer/observer.go:85`).
Publish-time filtering bounds *narration* but not ListWatch traffic, cache
memory, or CPU — the previous draft put its filters at the wrong layer.

Pods and Events are watched **namespace-scoped, following the run scope**.
Nodes stay cluster-scoped, as they must. Where the API server supports it, a
field selector restricts Events to `type=Warning` server-side, so the filtering
happens before the bytes are sent.

**The lifecycle tension this creates, stated rather than glossed:** the observer
starts at pod start, before any run exists, so the recipe's namespaces are not
known at startup. Namespace-scoped informers cannot be built then. The
resolution — start Pod/Event informers lazily once Recommend resolves a scope,
and stop them when the run ends — is a genuine change to the observer's
lifecycle and the largest single piece of work in this phase. It must not
reintroduce a blocking startup path: 2b-i crashlooped the console on an
unbounded `WaitForCacheSync`, and 2b-ii caught an unbounded Deployment lookup
before it shipped.

### Initial-list suppression currently swallows the signal

`internal/observer/handlers.go:12`'s `onAdd` records without emitting,
deliberately: an informer's initial list delivers every existing object as an
Add, and emitting there would narrate the cluster's entire pre-existing state at
pod start.

**Events are created once and never updated.** A `Warning` /
`FailedScheduling` therefore arrives as an Add and is silently dropped by that
rule — the informer would deliver exactly the signal this phase exists to
surface, and the observer would discard it.

`ResourceEventHandlerDetailedFuncs` (present in the pinned client-go at
`tools/cache/controller.go:319`) distinguishes an initial-list Add from a later
one. Both new kinds register with it: suppress initial-list Adds, process later
Adds.

### Volume control

- **Pods:** narrate only transitions into states an operator must act on —
  `ImagePullBackOff`, `CrashLoopBackOff`, `ErrImagePull`, unschedulable. A Pod
  going `Pending`→`Running` is already summarized by the Deployment and
  DaemonSet ready counts.
- **Events:** `Warning` only, deduped on `(resource UID, reason)`, with the
  dedupe map **bounded by run generation and cleaned on resource deletion** so
  it cannot grow for the process's lifetime. Whether a rising Event `count`
  re-emits is decided explicitly: it does **not**, because Kubernetes coalesces
  repeats precisely so consumers need not re-narrate them.

---

## Section 4 — Typed cluster data

Cluster events carry typed data, not a formatted message string the SPA has to
parse:

- resource kind, namespace, name, UID
- container (where applicable)
- reason
- current and desired counts (where applicable)
- severity
- resolved state — whether this is the condition arising or clearing

**Precedence and clearing are specified**, because otherwise a stale
`ImagePullBackOff` sits on a row forever or is masked by a later informational
update. A row shows the highest-severity unresolved condition; a condition
clears when its resource reaches a healthy state or is deleted.

The message string becomes a rendering concern, which is where it belongs.

---

## Section 5 — Testing

### Real-install assertions

`apply-real.sh` gives this phase genuine workload churn in CI. The e2e asserts
cluster events appear, correlate to rows, and do not swamp the stream.

**On the volume assertion specifically:** an absolute events-per-run ceiling
encodes today's action count and breaks on an AICR bump — the handoff's
standing rule is that assertions follow the pinned version, never a number
someone wrote down once. So the assertions are:

- **at most one event per normalized state transition** — a property, not a
  count, that holds at any scale
- **zero bus gaps and zero subscriber drops** across the run — which is the
  actual thing the ceiling was a proxy for

This closes the handoff's open question — *"does change-detection alone bound
observer volume on a real rollout?"* — which was deferred to Phase 4 only
because no real rollout existed to measure.

### Adversarial cases, each of which broke a previous draft's assumption

| Case | What it catches |
|---|---|
| An earlier action's workload changes *after* the next header | The temporal label is honest; no ownership is claimed |
| Async activity arrives after the script advances | No async special case is needed or attempted |
| A pre-existing Pod in a shared namespace churns | Namespace scoping does not imply this run installed it |
| A `Warning` arrives only as an Add | Initial-list suppression does not swallow it |
| A cluster event races a run transition | The generation counter catches the stale read |
| A cluster event arrives before its row's header | The ordering contract holds |

### Bite-proofs

Each property must have a mutation that breaks it: route the snapshot read
through `Engine.Current()`; drop the generation check; suppress all Adds rather
than initial-list Adds; remove the Event dedupe; widen the informers back to
cluster scope; publish the snapshot update before the marker.

### The standing instruction

**Twelve tests shipped in Phase 2b-ii that passed while the property they named
was broken** — across ten tasks, two fix waves and a final round; four caught by
their own author. That number goes in the plan's global constraints verbatim.
Naming it concretely moved the catch rate; a general caution did not.

For every test: state what would have to break for it to fail. If the answer is
"nothing I can name", it is decoration.

---

## What this phase does not do

- **No recovered-run screen redesign.** Carried forward.
- **No exact ownership attribution.** Deferred with a stated route (Section 1).
- **No change to AICR's bundle output.**
- **No new persistence.** `Run.Components` is a post-hoc projection written once
  per step; live correlation is a render-time concern.

## Constraints carried forward

- **`StateActive` is declared but unreachable**; Phase 3 owns the hook.
- **Phase 3's Reset must route through `finish` before bumping `epoch`**, and
  with 2b-ii's store the consequence is durable rather than in-memory.
- **The marker grammar is pinned only by `TestDeployTemplateUnchanged`.** This
  phase's dependency on it is *lighter* than the first draft's, since dropping
  the async special case removes one positional rule.

## Open questions

1. **What does a row show while its action is installing but nothing has
   happened yet?** Blank is clean but indistinguishable from "no telemetry";
   "waiting" is honest but noisy across 14 rows. Best answered against the real
   install rather than in the abstract.
2. **Should pre-existing workloads in a recipe namespace be narrated at all?**
   Namespace scoping cannot tell "this run installed it" from "it was already
   there". Suppressing them needs an ownership signal this phase deliberately
   defers; narrating them is honest but may be noise.
3. **How aggressive should lazy informer teardown be?** Stopping Pod/Event
   informers when a run ends reclaims memory but loses the post-Apply
   convergence window — the ten-to-twenty minutes `deploy.sh` warns about, which
   is arguably the most interesting telemetry the console could show.

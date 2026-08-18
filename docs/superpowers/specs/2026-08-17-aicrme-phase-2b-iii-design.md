# Phase 2b-iii design — the observer's visible half

**Status:** design, not yet planned.
**Spec:** `approach.md`. **Prior phase:** `docs/phase-2-handoff.md` — read "Constraints 2b-iii inherits" first.
**Predecessor designs:** `docs/superpowers/specs/2026-08-16-aicrme-phase-2b-i-design.md` (the observer), `docs/superpowers/specs/2026-08-17-aicrme-phase-2b-ii-design.md` (persistence).

## Goal

The cockpit shows, on the row of the component currently installing, what that
component's workloads are actually doing — `waiting on rollout:
nvidia-driver-daemonset 3/8 ready` — and surfaces the failure signals an
operator needs: `ImagePullBackOff`, crash loops, `FailedScheduling`.

## Why now

Phase 2b-i built the observer and 2b-ii persisted its output, but nothing
renders it per row. `approach.md` §Apply(cockpit) promises exactly this, and it
is the difference between a timeline that scrolls and a pipeline that explains
itself.

Two things changed that make this the right moment. `test/e2e/apply-real.sh`
now drives a real 14-component install in CI on every push to `main`, so for
the first time there is genuine multi-minute workload churn to observe and
assert against. And 2b-ii's component projection gives every row a stable
identity to attach live state to.

## Scope

**In:** positional attribution of observer events to component rows, computed
server-side; Pod and Event informers with volume control; per-row rendering in
the cockpit; an e2e assertion against the real install.

**Out, deliberately:** a designed recovered-run screen. Phase 2b-ii's C1 fix
made a recovered run *usable* — an affordance keyed on run state, reachable in
every phase — under time pressure at the end of a phase. It works and is
tested. It has not had design attention, and pairing UI design with observer
and informer work would put two unrelated risk profiles behind one review
surface. It carries forward.

---

## Section 1 — Attribution

### Positional, because `deploy.sh` makes it true

`deploy.sh` installs components in strict recipe order and emits a
`┌─ [N/14] name  →  namespace` header for each. **13 of 14 components are
installed with `--wait`**, so helm blocks until the workload is ready before
the script moves on. The component the script says is active therefore *is* the
component whose workloads are changing — attribution is correct by
construction, not by coincidence.

This was verified against the pinned module rather than assumed:
`deploy.sh.tmpl:376-383` derives `COMPONENT_WAIT_ARGS`, using `--wait` unless
the component appears in `ASYNC_COMPONENTS`.

The rejected alternatives, and why:

- **Labelling the bundle's manifests** at generation time would be exact and
  durable, but means modifying AICR's rendered output and coupling this console
  to the bundle format. Larger blast radius for a demo console.
- **Namespace plus name matching** fails silently wherever a workload's name
  diverges from its component's — `monitoring` alone holds three components'
  workloads. Silent divergence is the failure mode this project has been
  punished for repeatedly.

### Computed server-side, at publish time

The observer stamps each `KindCluster` event with the active component before
publishing. Not in the browser: a late-joining tab and a replayed stream then
both get correct attribution for free, rather than re-deriving an ordering the
bus already guarantees.

`internal/steps/apply.go`'s `trackComponents` already intercepts every
`KindComponent` event as it streams — it is what maintains `run.Components` —
so the active component is known without new parsing.

### A cheap accessor, not an extension of the cached scope

2b-ii's `newRunScopeFn` caches by run ID **specifically** to avoid
`Engine.Current()`'s deep copy of every artifact on a per-watch-event path. The
active component changes many times *within* a run, so it cannot ride that
cache without defeating its purpose.

It gets a separate accessor that takes the engine lock, reads one small field,
and returns — the same shape as `Engine.CurrentID`, which exists for exactly
this reason. **No store I/O and no artifact clone on this path**, which remains
the standing constraint from 2b-i.

### The async exception is discovered, never hardcoded

Some components install **without** `--wait`, so `deploy.sh` moves on
immediately and their rollout events can land while a later component is
active. Attributing those to the active row would be wrong.

Two facts govern how this is handled, both verified against the pinned module:

- The marker (`deploy.sh.tmpl:382`) carries **no component name**. It is
  `(async component — skipping --wait, keeping --timeout for hooks)`, emitted
  inside the active component's block, and `internal/applier/parse.go` turns it
  into an untyped `bus.KindLog`. So which component is async is knowable only
  **positionally** — it is whichever component's header preceded the marker.
- Which components are async comes from `ASYNC_COMPONENTS`, a variable the
  bundler sets. **It is not permanently `kai-scheduler`.** Today's recipe has
  exactly one; a different recipe or a later AICR release may have more or
  none. Any implementation that hardcodes a component name is wrong the first
  time that set changes, and will fail silently.

An async component's subsequent workload events attribute to **no row** rather
than to whatever component happens to be active. Showing nothing is correct;
showing the wrong row is worse than showing nothing, and indistinguishable from
working.

This does add a second positional dependency on the marker grammar, which the
handoff already records as a maintenance liability pinned only by
`TestDeployTemplateUnchanged`. That test remains the tripwire, and this design
deepens the surface it protects — an argument for the upstream
machine-readable event stream the handoff proposes, not against this feature.

---

## Section 2 — Pod and Event informers

The observer ships 3 of the 5 informer kinds `approach.md` names. Pods and
Events carry the signals its own live-feedback section uses as examples:
`ImagePullBackOff`, crash loops, `FailedScheduling`.

### Volume is the risk, and the failure is silent

The governing property from 2b-i is unchanged: **the observer aggregates, it
never relays.** `internal/bus` drops live events for any subscriber more than
`subscriberBuffer` (256) behind.

Change-detection alone sufficed for three low-churn kinds. Pods and Events are
a different order of magnitude — a 14-component install produces hundreds of Pod
transitions and thousands of Events. If the filters under-perform, the symptom
is not an error: it is the bus dropping the `deploy.sh` marker stream, and the
operator watching the pipeline stop moving. That is the same silent shape as
the SSE cursor bug 2b-ii fixed.

So both kinds get a relevance filter *in addition to* change detection:

- **Pods:** narrate only transitions into states an operator must act on —
  `ImagePullBackOff`, `CrashLoopBackOff`, `ErrImagePull`, and unschedulable —
  not every phase change. A Pod going `Pending`→`Running` is what the
  DaemonSet/Deployment ready counts already summarize.
- **Events:** `Warning` type only, deduped on `(involvedObject, reason)`.
  Kubernetes already coalesces repeats into a `count` field; re-narrating the
  same `FailedScheduling` forty times is precisely what the ring drop exists to
  protect against.

---

## Section 3 — Rendering

Per-row sub-status in the cockpit, driven by attributed events.
`web/src/pipeline.ts`'s `deriveComponents` already replays `KindComponent`
events into one `ComponentState` per component, keyed by name. Attributed
cluster events attach to the matching row rather than forming a parallel
structure the two would have to be reconciled across.

Unattributed events remain in the timeline, which is where they are today.

---

## Section 4 — Degradation

**Unattributed is a first-class state, not an error.** Outside Apply — during
Discover, Recommend and Bundle — there is no active component, so cluster
events attribute to no row. Same for async components. The UI renders that
deliberately rather than treating a missing attribution as a defect.

**Informer failures keep 2b-i's posture.** A Pod or Event informer that fails
to sync warns; the console keeps working. Telemetry is optional, the console is
not. That constraint has now bitten twice — `WaitForCacheSync` crashlooping the
console in 2b-i, and an unbounded Deployment lookup on the startup path caught
during 2b-ii — so **nothing added here goes on a blocking startup path**, and
every new API call is bounded.

---

## Section 5 — Testing

### The e2e can now assert this against a real install

`test/e2e/apply-real.sh` drives a real 14-component install in CI on every push
to `main`. So this phase's e2e assertions run against genuine workload churn
rather than a fake clientset: cluster events appear, attribute to the right
rows, and do not swamp the stream.

**This closes an open question the handoff deferred to Phase 4** — *"does
change-detection alone bound observer volume on a real rollout?"* It was
deferred because there was no real rollout to measure. There is now. Event
volume gets an explicit ceiling assertion, derived from the run rather than
hardcoded, so the answer is enforced on every push instead of observed once.

### Unit-level properties, each needing a bite-proof

| Property | The mutation that must fail it |
|---|---|
| Attribution assigns to the **active** component | Assign to the previous or next component; an off-by-one here is invisible in a screenshot |
| Async components attribute to **no** row | Attribute them to the active component |
| Async detection is **positional**, not name-based | Change which component `ASYNC_COMPONENTS` contains; a name-keyed implementation still passes |
| Pod filter narrates only actionable states | Narrate every phase transition; assert the volume delta |
| Event filter drops non-`Warning` and dedupes | Remove the dedupe; the count explodes |
| The observer still never relays | Remove change-detection from either new kind |
| No artifact clone on the attribution path | Route the accessor through `Engine.Current()` |

### The standing instruction this phase inherits

**Twelve tests shipped in Phase 2b-ii that passed while the property they named
was broken** — across ten tasks, two fix waves and a final round. Four were
caught by their own author, which is the direction the trend should keep
moving. That number belongs in the plan's global constraints verbatim, because
naming it concretely is what moved the catch rate; a general caution did not.

For every test: state what would have to break for it to fail. If the answer is
"nothing I can name", it is decoration.

---

## What this phase does not do

- **No recovered-run screen redesign.** Carried forward; the current fix works.
- **No change to AICR's bundle output.** Manifest labelling was considered and
  rejected in Section 1.
- **No new persistence.** `Run.Components` is a post-hoc projection — verified
  during the spike: `runStep`'s merge-back copies `scratch.Components` only
  when the step *returns*, so the projection is written once per step, not
  live. Live sub-status is a render-time correlation of the event stream, and
  nothing about that needs storing.

## Constraints carried forward

- **`StateActive` is declared but unreachable**, and Phase 3 owns the hook.
- **Phase 3's Reset must route through `finish` before bumping `epoch`**, and
  with 2b-ii's real store the consequence is durable rather than in-memory.
- **The marker grammar is pinned only by `TestDeployTemplateUnchanged`.** This
  phase deepens the dependency on it (Section 1's async positional rule), which
  strengthens the case for the upstream machine-readable event stream.

## Open questions

1. **Is a ceiling on events-per-run the right volume assertion**, or does it
   encode today's component count in a way that breaks on an AICR bump? A ratio
   against workload-transition count may travel better. Decide during planning,
   with the handoff's standing rule in mind: assertions follow the pinned
   version, never a number someone wrote down once.
2. **What does a row show when its component is async?** "Installed, not
   waited" is honest but wordy; blank is clean but indistinguishable from
   "nothing happening". A UI question, best answered against the real install
   rather than in the abstract.
3. **Do Pod events need run-scoping beyond namespace?** The recipe's namespaces
   contain workloads this run did not install — pre-existing pods in
   `monitoring`, for instance. Namespace filtering alone will narrate them.
   Whether that is noise or useful context is worth deciding deliberately.

# UX feedback

A running list of UX observations from **actually running the demo**, kept so they are not lost
between sessions. Nothing here is scheduled — capture first, decide later.

Each entry records what was observed, where in the code it lives, and anything that constrains
the fix. Entries are removed when they ship, not when they are agreed with.

---

## 1. The event stream appends, so the newest event is off-screen — SHIPPED 2026-08-23

**Observed:** 2026-08-19, Mark, first local `make demo` run, during Apply.

The right-hand event stream appends newest-last. During a 14-action install it grows past the
viewport, so the operator has to scroll to see what is happening *now* — during precisely the
five minutes the demo is meant to be watched.

**Proposed:** prepend, so the newest event is always at the top and no scrolling is required to
follow a live run.

**Where:** `web/src/components/Timeline.tsx:15` (`events.map(...)`), and its test at
`web/src/components/Timeline.test.tsx`.

**Worth thinking about before changing it:**
- The bus replays from a ring buffer on reconnect, so ordering has to hold for a late-joining
  tab too, not just a live one.
- Reversing display order affects how a *multi-line* event reads — several events in the
  screenshot wrap to two or three lines, and newest-first means a wrapped event's continuation
  sits below its own timestamp while the previous event sits below that. Worth checking it still
  scans.
- The timeline currently reads chronologically, which matches how the install narrates itself
  ("phase started" → "phase complete"). Newest-first inverts that for a reader catching up.
  Both orderings are defensible; the live-watching case is the one the demo optimises for.

---

## 2. A stale, already-converged warning sits on a healthy row on the success screen — SHIPPED 2026-08-21

**Fixed.** A Warning about a controller (ReplicaSet, DaemonSet) now resolves when a pod that
controller owns becomes healthy: the pod's controller `ownerReference` carries the same
Kind/Name/UID the Warning's `InvolvedObject` does, so the Pod informer this package already runs
supplies the recovery signal — no new informer, no name matching. See
`internal/observer/events.go`'s `controllerEventKey` and `onPodChange`'s owner resolve.

**One residual, stated rather than left to be rediscovered:** a pod that is created but
*unschedulable* does not clear its controller's `FailedCreate`, because `onPodChange` only takes
the full-recovery branch when the pod has no trouble of its own. Creation did succeed in that
case, so the `FailedCreate` is stale — but the row is legitimately red anyway (Unschedulable is
live and true), and both clear together once the pod schedules. That errs toward showing a
condition rather than hiding one, which is the direction `internal/bus/cluster.go` prefers.

A Warning involving a **Deployment** still has no resolution path: a Deployment's pods are owned
by its ReplicaSet, not by it, so this mechanism does not reach it. Deployment-involved Warnings
are rare (its own events are almost all Normal `ScalingReplicaSet`), and inventing a signal for
them was not worth a second mechanism.

The entry below is kept verbatim as the record of what was observed and why it mattered.

---

**Observed:** 2026-08-19, Mark, first local `make demo` run, on the "Bundle installed" screen.

**This is a correctness bug, not a preference.** It is the deferred 2b-iii finding, sighted in
a real run within minutes of a human first using the product.

The Done screen reads "Every component in the bundle installed successfully", every row says
INSTALLED — and the `kubeflow-trainer` row carries:

```
kai-scheduler/kai-scheduler-default-b5c69699f: FailedCreate -- Error creating: pods ...
is forbidden: error looking up service account kai-scheduler/scheduler:
serviceaccount "scheduler" not found (last observed while kubeflow-trainer installed)
```

Two separate things are happening, and only one of them is intended:

**Intended:** the condition appears on `kubeflow-trainer`'s row although it concerns
`kai-scheduler`, because attribution is *temporal* — that action was installing when the
condition was observed. The copy says so and claims nothing more. Working as designed.

**Not intended:** the condition is **stale**. `test/e2e/apply-real.sh`'s own header documents
this exact warning as transient and self-converging ("kai-scheduler's ReplicaSet transiently
reports FailedCreate ... and converges shortly after" — it is why `SETTLE_SECONDS` exists). It
resolved, and the row never cleared, because **resolution is keyed to Pod recovery and this is
a ReplicaSet-involved warning**.

That is exactly the open item recorded in `docs/phase-2-handoff.md` — *"resolution covers
Pod-involved Warnings only; a DaemonSet `FailedCreate` survives the DaemonSet's own deletion"* —
deferred during 2b-iii as narrower than the Pod path. It is not narrower in practice: it is the
first thing a human saw.

**Net effect:** a success screen that shows a red failure for a problem that fixed itself. This
is the "amber row on a green run" outcome the Pod path was explicitly changed to prevent
(Ruling 23), still open for non-Pod involved objects.

**Where:** `internal/observer/events.go` — `resolveEventsLocked` and its call sites are keyed on
`podKey`. A ReplicaSet/DaemonSet-involved Event has no recovery signal.

**Worth thinking about before changing it:**
- The obvious fix — resolve a workload-involved warning when that workload reaches its desired
  replica count — means the Event handler needs a second recovery source alongside the Pod
  cache. 2b-iii already added one such cross-informer read and it was judged sound, so there is
  a precedent to follow rather than invent.
- The cheaper alternative is to expire conditions at terminal state, which was **explicitly
  rejected** in 2b-iii: it hides real conditions to paper over unresolved ones, and at Done a
  genuinely broken component is what an operator most needs to see. Do not reach for it.

---

## Observed in the same screenshot, not raised as feedback

Recorded only so they are not re-discovered as surprises. Neither is a request.

- The Discover gap list renders exactly as intended on a simulated cluster — "No GPU driver
  installed", "No device plugin", "No GPU-aware scheduler", and an explicit "this is a simulated
  cluster" line. That is the copy honesty the design asked for, working.
- 2b-iii's per-row condition renders correctly on a real install: `RolloutProgress
  node-feature-discovery/nfd-node-feature-discovery-worker 0/3 nodes ready (cluster activity
  while nfd installs)`. First confirmation from a human-driven run that the attributed condition
  lands on the right row with the temporal copy intact.

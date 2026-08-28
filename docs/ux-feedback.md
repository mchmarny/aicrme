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

## 3. The first full UI run on real GPU hardware — 2026-08-28

**Observed:** 2026-08-28, Mark, driving the whole arc against a GKE cluster (2x a3-megagpu-8g,
144 contexts in the kubeconfig) in a browser, screenshot by screenshot. Thirty observations. All
but four shipped the same day; what follows records them so the reasoning is not re-derived.

### Shipped

| # | Observation | Fix |
|---|---|---|
| 25 | **The success screen read as a failure.** `StateActive` is Prove's terminal success state, and the only coloured things on it were a solid red Stop and a red Reset. Read as an error by the person who built it. | An explicit success line that also rules out the wrong conclusion the state invites — the run *ends* here. Stop demoted to the outlined style ResetGate already used. |
| 28 | **Stop looked like a dead click.** It is synchronous over a wait that is minutes long (delete the workload, wait for pods to actually be gone), and a disabled button was the entire feedback. The cluster had already done it. | "Stopping…", plus a caption naming what it waits on. |
| 10 | **The cluster stopped being named after Connect.** The header said only "connected" — including on the gate where cluster-admin is granted. | Context name and GPU count in the header, on the fresh-connect and reload paths. |
| 17, 18 | **The app's green was not the brand green** (emerald hue 162 vs the logo's 91–124), and there was no mark or favicon. | The palette from https://validation.aicr.run verbatim, as semantic tokens; NVIDIA green #76b900; mark and favicon generated small from the source PNGs. |
| 19, 20, 21, 23 | **No aggregate progress, no time, no way to find the in-flight row.** Two denominators and no numerator; `INSTALLED` eleven times in one column. | "11 of 16 installed", a bar, per-row durations, elapsed clock, a pulse on the working row, a glyph for the rest. The timing data was already on `ComponentState` and unrendered. |
| 27 | **No run summary at the moment of success** — the numbers were spread over forty timeline lines. | One line: actions, wall clock, gang size, GPUs (the last only when the cluster reported any). |
| 1, 2, 3, 4, 5, 6, 11 | **Connect was unusable on a real kubeconfig.** 144 contexts, the preselected one at row 89, no filter, a 448px column against full EKS ARNs. | Current context pinned first and badged, a filter past six contexts, name over server, `max-w-3xl` on both screens. |
| 7, 8, 9 | Confirm had no hierarchy, bash printed a two-line banner, the GPU row read like the "no GPUs" rows. | Section headings, `bashVersion` reduces to `5.3.15`, GPU row accented. |
| 12, 14, 15, 16 | **The gate did not justify itself.** Flat alphabetical list; "every version pinned" contradicted two rows later by AICR-generated local charts; nothing linked Discover's findings to the components that close them. | Grouped by namespace, each component naming the gap it closes, and a claim that counts rather than asserts. Discover's gap list gains a framing heading. |
| 26, 30 | Reset sat alone in the left margin; the disabled Continue gave no reason. | Same column as Prove; the button says what it is waiting for. |
| 24 | **No way to export the run log**, and the events die with the process. | `GET /api/runs/{id}/log` — record plus events, as a named attachment, with a download link on the timeline. |

### Not fixed, deliberately

**13. The timeline reads newest-first.** Raised as "the narrative reads backwards". It does — and
that is item 1 of this file, shipped 2026-08-23 from a real demo, trading chronological reading
for never scrolling during a live install. Reversing it would undo a decision already made with
the evidence in hand. Left as is.

**22. "The timeline disappears during Apply."** Does not reproduce: `Wizard` renders the timeline
aside unconditionally for every phase. The screenshot reporting it is 1723px wide where the
others are 2000px, so it was almost certainly cropped. Recorded rather than closed, in case it
turns out to be real on a narrow window.

**29. The screen did not advance after Stop.** It did eventually, so the run state was never
stuck. Whether it lagged because of an SSE drop or because nothing renders during teardown is
still open; the refresh test that would distinguish them was not run.

**Related bug, still open:** `GET /api/runs/{id}/bundle` 404s for a *recovered* run —
`bundle.path` lives in `ephemeralArtifacts` and is dropped on encode. The existing download is
broken on exactly the path where debugging matters most, and the log export above does not fix
it.

---

## 4. A run over an existing install reports success and does not work — SHIPPED 2026-08-28

**Observed:** 2026-08-28, Mark, immediately after the UI pass above. Apply reported 16/16 and
Prove placed 0/2 on the cluster it had just succeeded on an hour earlier.

**Not a regression.** A second run had installed over the first with no Reset between them.
kai-scheduler's `SchedulingShard` survives by design and owns the `kai-scheduler-default`
Deployment, so the cluster ran the *first* install's scheduler against a control plane replaced
underneath it. The shard and that Deployment were two hours older than the five kai Deployments
beside them.

**The UX defect caused it.** Item 28 above — Stop and Reset both run silently for minutes — meant
the operator could not tell which had happened. A new run appears the same second Stop closes the
old one, which reads exactly like a completed Reset. The run records show no Reset ever ran. That
is the strongest argument in this file for treating silent long operations as correctness bugs
rather than polish.

**Fixed:** `internal/steps`' `alreadyInstalled` refuses Apply when the recipe's own releases are
already installed, naming them and both ways out. Two traps it had to avoid: Retry re-runs Apply
over the run's own partial install (keyed on first attempt), and the ownership snapshot was being
re-taken on retry, which recorded a run's own releases as pre-existing and would have left them
behind at Reset. `prove.sh` assert 5 drove exactly this path and now resets first.

**Also corrected:** the Prove timeout message had blamed the pod-grouper and said the failure
follows a Reset. Both wrong — the pod-grouper chatter appears on healthy installs, and this
cluster was never reset. It now names the mechanism and a five-second check.

**Still open:** the three residue classes that blocked the manual cleanup afterwards (stale
APIServices, orphaned webhooks, orphaned finalizers). See `STATE.md`.

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

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

That is exactly the open item recorded during 2b-iii — *"resolution covers
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

---

## 5. The console never says which AICR version it is running — SHIPPED 2026-08-29

**Observed:** 2026-08-29, Mark, after the repo went public.

Every recipe decision, component version and validation verdict the console shows comes from
AICR, and the console pins one exact version of it — but nothing on screen says which. An
operator watching a run cannot tell whether they are looking at the output of v0.20.0 or
something else, and two runs from two binaries are not comparable without that.

It matters more now than it did: the binary is installable from a Homebrew tap and an install
script, so the person running it is no longer the person who built it and cannot check `go.mod`.

**Proposed:** show the AICR version in the console header, linked to that version's release page
upstream — `https://github.com/NVIDIA/aicr/releases/tag/v<version>` — so the operator can read
what changed in the version they are actually running.

**What the fix can rely on:** the version is already pinned in three places that `make
check-aicr-pin` keeps equal — `.settings.yaml` (`dependencies.aicr`), `go.mod`, and
`defaultSnapshotAgentImage` in `internal/console/console.go`. Any of them is a source; none of
them currently reaches the SPA. `internal/version` already carries aicrme's own version and
commit through ldflags and is the obvious place to carry AICR's alongside them, which would also
put it in the run record and the evidence bundle rather than only on screen.

**Constraint worth stating before someone builds it:** the header should show the version the
binary is *pinned to*, not one discovered from the cluster. A cluster can have been configured by
a different aicrme build, and a header that quietly reported the cluster's version would answer a
different question than the one being asked.

---

## 6. The first real-hardware validation run — 2026-08-29

**Observed:** Mark, GKE H100s, driving aicrme v0.1.0 installed from the Homebrew tap.
The run itself succeeded: 16 of 16 installed in 11m 8s, gang of 2 placed, 16 of 16 GPUs usable.
Five findings came out of it, all now shipped.

### Shipped

- **AICR pulled validator images tagged with aicrme's version.** AICR rewrites its validator
  catalog's `:latest` images using whatever `WithVersion` receives, and it was receiving
  aicrme's. Every deployment validator tried to pull
  `ghcr.io/nvidia/aicr-validators/deployment:v0.1.0`, which does not exist; each burned its full
  per-check deadline, and validation reported 0 of 5 passed after 24 minutes of nothing. Latent
  from day one and impossible to see before the first tagged release, because AICR only rewrites
  the tag when the version looks like a release — every dev build passed `dev` or a git SHA.
  Fixed by `aicrclient.AICRVersion`, guarded by `make check-aicr-pin`.
- **A stopped run could not be reset.** Stop started a new run, the new run took the engine's
  `current` slot, and `engine.Reset` refuses any run that is not current — so a cluster with 16
  releases installed had no teardown path in the console at all. The cleanup had to be done by
  hand. The auto-start is gone; starting over is an explicit control on the stopped screen.
- **The Validate phase was invisible.** It ran under Apply's heading, Apply's full green progress
  bar and Apply's still-counting elapsed timer for 24 minutes. The only signal was one line in
  the timeline rail — and the phase events themselves said a bare `phase started` without naming
  the phase.
- **No AICR version anywhere** — entry 5 above.
- **`aicr-validation` survived and nothing mentioned it.** Reset still will not remove it, which
  is the standing rule for a deployer's own namespaces, but the residue inventory now names it.

### Measured, not inferred

| | |
|---|---|
| Apply, 16 of 16 | 11m 8s |
| Validate (all five checks hitting their deadlines) | 24m 11s |
| `kube-prometheus-stack` | 3m 14s |
| `gpu-operator` | 36s, driver pre-installed |

The 24m 11s is the serial sum of the five per-check ceilings, and it landed under the 30-minute
facade cap. At the 15 minutes originally specified, AICR would have discarded every partial
result and reported only a deadline — no per-check messages, no CTRF, and no way to diagnose any
of this. The cap being right is why the run produced a diagnosis instead of a shrug.

### Still not observed

A validation run whose checks actually execute. Every check failed on the image pull, so a
**passing** verdict on real hardware remains unproven.

---

## 7. Second real-hardware run, with the validation fixes in — 2026-08-30

**Observed:** Mark, new GKE H100 cluster, `make build` from `419b424`. The run succeeded:
16 of 16 installed in 11m 6s, gang of 2 placed, and **AICR validation executed for the first
time on real GPUs** — 4 of 5 checks passed.

### Confirmed working

All five fixes from the 2026-08-29 run, verified on hardware: validator images resolve
(`:v0.20.0`, containers Running); Stop no longer starts a new run and leaves Reset reachable;
the Validate phase has its own heading and phase events name themselves; the AICR version shows
in the header; and the residue names `aicr-validation` verbatim. Also confirmed: the current
kubeconfig context is preselected and badged, and the kai purge removed all four keep-policy
objects.

### Measured

| | |
|---|---|
| Apply, 16 of 16 | 11m 6s (within 2s of the previous run) |
| Validate, wall clock | 8m 39s |
| **A healthy deployment validation** | **~20s** — the four passing checks total about that |
| `operator-health` | passed, 6s |
| `expected-resources` | **failed** — hit AICR's own 8m deadline |
| `gpu-operator-version` | passed, 1s |
| `check-nvidia-smi` | passed, 13s |
| `gke-gpu-nic-networks` | passed, <1s |

The `expected-resources` timeout is not an aicrme defect. Every workload the cluster runs was
healthy minutes later, so it is a tight deadline on a fresh 16-component install, not a broken
cluster. It cannot be confirmed, because there is no way to re-run validation and the failed
validator's pod is deleted with its logs.

### The theme, which matters more than any single item

**The console is clear about what happened and vague about what is happening.** Four of the
findings below are the same bug wearing different clothes: a superseding phase appends to the
previous one instead of taking over. Fix them together.

### Findings

1. **Validate shows Apply's component rows, which read as validation progress.** Not merely
   missing progress — actively misleading. The operator read completed install checkmarks and
   durations as live validation state. Worse: `run.validation` does not exist until the phase
   ends, so an exported log carries nothing either. **AICR already emits per-check progress
   through `log/slog`'s default logger** (`running validator name=X`, `validator completed
   name=X status=passed`, `catalog=5 selected=5`) — the console receives those records today and
   drops them. A `slog.Handler` that republishes them onto the bus is the cheap fix; there is no
   SDK progress hook.
2. **Stopping is subordinate to the success screen.** During the one-to-two minute wait, the
   screen is dominated by "Your cluster placed a gang-scheduled workload / This run succeeded",
   and the only copy explaining the wait is small grey text under a disabled button.
3. **Reset never declares completion, and still offers Reset.** After a successful teardown the
   main panel is unchanged and its primary button invites the operation that just finished. The
   only evidence is one wrapped line in the rail. Asked twice, by the operator, whether it was
   done.
4. **The reset summary contradicts its own detail lines.** Summary: "12 left in place because
   this run did not create them." Detail, immediately below: "namespace aicrme skipped: this run
   created it for the snapshot agent". The detail is right; the summary invents a false reason
   for correct behaviour.
5. **Resolved conditions stay on component rows.** `skyhook-operator-controller-manager-…-6vfmk:
   Unhealthy` was resolved at 13:44:44 and the row still showed the failure. 192 resolution
   events in that run.
6. **Post-Apply conditions are attributed to Apply actions.** A cert-manager webhook condition
   recurred at 13:55:49, during Validate, and was labelled "cluster activity while
   kube-prometheus-stack installs" — an action that finished minutes earlier. The attribution
   logic predates there being a phase after Apply.
7. **The AICR version is absent before connect.** It hangs off `ClusterInfo`, which is null until
   a cluster is selected. It should be on every screen.
8. **aicrme's own identity is not shown at all** — wants version, build date and digest, likely
   its own line. Two constraints: use the commit timestamp, not wall-clock, or `mod_timestamp`
   reproducibility breaks; and goreleaser cannot inject the binary's own digest (it is computed
   after the binary exists), so a true artifact digest means self-hashing at startup.
9. **The activity rail is too narrow and the main column too wide.** `Wizard.tsx` pins the rail
   at `w-80`/`w-96` beside a `flex-1` column whose content is capped at `max-w-2xl`. Short lines
   wrap for no reason while the main column pads empty margin.
10. **"show cluster activity" and "download run log" sit below the event list** — hardest to
    find exactly when the list is longest and you need them most.
11. **`REMOVED` and `REMOVING` are visually identical.** Same weight, same colour, two letters
    apart. The operator who raised this then misread it himself a minute later. Apply's ✓ +
    colour + duration treatment already exists and should carry over.
12. **The reset gate says "Remove 14 releases" and removes 16.** It counts components; the
    teardown acts on releases, and the generated `-pre`/`-post` actions make up the difference.
13. **The stopped screen still says the gang "is placed and running"** under a heading that says
    it has stopped.
14. **A failed validator's logs are deleted**, so the one check worth diagnosing is the one that
    cannot be. `WithValidationCleanup` is the lever; flipping it preserves every validator's pods
    and leaves residue in a namespace Reset already will not remove. Also an upstream ask.
15. **klog reflector spam bypasses the logger.** 15+ of `watch ended with error … very short
    watch` from client-go's `*v1.Event` reflector, printed straight to stderr. Worth understanding
    rather than silencing — the Event informer re-establishing constantly may mean dropped events.
16. **The `gpu-operator` driver auto-detect warning repeats ~10×**, five times in one second.
17. **The helm registry-credential warning is accurate but self-inflicted.** aicrme spawns every
    helm process, so it could hand them a registry config without the broken `credsStore` instead
    of asking the operator to. Needs a rule — override only when the named helper is missing —
    since overriding unconditionally would discard working private-registry credentials.

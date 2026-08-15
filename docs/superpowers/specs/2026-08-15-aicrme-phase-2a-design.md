# aicrme Phase 2a — Bundle and Apply

**Date:** 2026-08-15
**Status:** Approved for planning
**Spec:** `approach.md` (§Architecture, §UX phase 4, §Testing, Risk 4)
**Inputs:** `docs/phase-2-handoff.md`, the "Roadmap for Phases 2-5" section of
`docs/superpowers/plans/2026-08-13-aicrme-phase-0-1.md`
**Pinned dependency:** `github.com/NVIDIA/aicr v0.19.0`

---

## Why this is 2a and not Phase 2

`approach.md` lists Phase 2 as "applier, cockpit, observer — the bulk of the
work." Read against the handoff, that phase actually contains eight units: the
Bundle step, the applier, the observer, the ConfigMap-backed store (plus the two
latent bugs it activates), the cockpit layout, the failure state, the slow-step
callout map, and two carried-over review findings.

That is too much for one reviewable spec. The Phase 0-1 run shipped four real
defects in its plan text, caught only by implementers; a larger plan makes that
worse, not better.

Phase 2a is the vertical slice that makes Apply real: **Bundle, applier, minimal
cockpit.** The observer, the ConfigMap store, and the remaining polish are 2b and
2c. The demo arc extends by exactly one phase and stays independently demoable,
which is the property `approach.md`'s phase table asks every phase to have.

---

## Scope

### In

- A `Bundle` step producing a real, on-disk AICR bundle directory.
- A confirm gate between Bundle and Apply.
- An `applier` package that drives the bundle's own `deploy.sh` and parses its
  stable output markers into `bus.Event`s.
- Engine support for retrying a failed run from the step that failed.
- A minimal cockpit rendering the component pipeline, the confirm gate, and the
  failure state.
- Two carried-over review findings that live in files this work already touches.

### Out — deliberately, do not "fix" these in 2a

Observer, ConfigMap-backed `engine.Store` and the `bus` epoch problem it
activates, Reset, Validate, Prove, GitOps export, `StateActive`, the full
Review-and-verify screen with cosign/SLSA status, and the upstream
`AICR_DEPLOY_EVENTS=jsonl` contribution.

The upstream PR is worth restating: the marker parser must exist regardless. This
console pins `aicr v0.19.0`, so any machine-readable event stream added upstream
lands in a later bump and cannot retire a parser we need working today.

All of `approach.md`'s standing non-goals continue to apply — air-gapped
operation, private registries, day-2 operations, multi-user auth, multi-cluster,
in-UI editing of component sets. The `cluster-admin` grant remains deliberate and
disclosed.

---

## The run arc after 2a

```
Discover  →  Recommend  →  [intent, platform]  →  Bundle  →  [apply]  →  Apply  →  done
  auto       2 questions       user decides       auto      Install     deploy.sh
```

`engine.PhaseBundle` and `engine.PhaseApply` are already declared in
`internal/engine/run.go` and have no `Step` implementation. Both get one.

### The confirm gate

The console installs sixteen charts with `cluster-admin`. It should not begin
mutating a cluster without a click.

The gate needs no new engine machinery: the Apply step declares
`Requires() = []string{"apply"}`, so `engine.awaitDecisions` parks the run in
`StateAwaitingDecision` after Bundle completes and before Apply runs. The console
renders the bundle's component list and an Install button; clicking it POSTs
`{"apply": "yes"}` to the existing `POST /api/runs/{id}/decide`.

This does not break `approach.md`'s "exactly two decisions" promise. Intent and
platform are choices. This is a confirmation, and it is the natural home for
Phase 5's Review-and-verify screen when that lands.

---

## Component design

### 1. `internal/steps/bundle.go` — the Bundle step

**Responsibility.** Turn the resolved recipe into an on-disk bundle directory,
and record where it is.

**The recipe-handoff problem.** `aicr.Client.MakeBundle` requires a
Client-owned `*aicr.RecipeResult` — it calls `assertOwns` and reads
`recipe.internal`. `steps.Recommend` today discards that handle after building
its JSON summary, and `engine.Run.Artifacts` is `map[string][]byte`, which cannot
carry an opaque owned handle.

**Decision: Bundle re-resolves the recipe.**

Every input to the resolve is already persisted: the raw snapshot bytes in
`Run.Artifacts["snapshot.yaml"]`, the `intent` and `platform` values in
`Run.Decisions`, and a recipe catalog embedded in the pinned aicr module. The
resolve is therefore deterministic — the same inputs produce the same recipe,
which is the reproducibility property this codebase already holds itself to.

Bundle then **asserts the re-resolved component set matches the stored
`recipe.json` summary** — same count, same names, same versions — and fails
closed with a structured error on any mismatch. What the user approved on the
Recommend screen is provably what gets bundled.

**Alternative rejected.** A `*recipeHolder` shared between `NewRecommend` and
`NewBundle`, wired in `main.go`, carrying the last resolved `*aicr.RecipeResult`.
It works and avoids the second resolve. It was rejected because it is in-memory
state that the ConfigMap-backed store in 2b would then have to lose across a pod
restart — exactly the restart-survival case 2b exists to fix. Re-resolving
survives a restart for free. The cost is one in-process resolve against an
embedded catalog, which is milliseconds and touches no network.

**Refactor this requires.** `buildCriteria` moves from `recommend.go` to a
shared `internal/steps/criteria.go` so both steps derive criteria identically.
Its existing behavior and its `TestRecommendKWOKGPUlessFixtureMatrix` /
`TestRecommendResolvesAgainstSimulatedH100Fixture` coverage are unchanged.

**Output.** The bundle tree on disk, and `Run.Artifacts["bundle.path"]` holding
the absolute directory path so Apply can find it. A `KindLog` event carrying file
count and total size.

### 2. Workdir, and the `readOnlyRootFilesystem` question it settles

A bundle is a directory tree, so it cannot live in `Run.Artifacts`.

The chart adds an `emptyDir` volume mounted at `/var/lib/aicrme`. Bundles land at
`<workdir>/runs/<runID>/bundle`.

`deploy.sh` calls `mktemp -d`, and helm and kubectl both need writable cache and
config directories. The Deployment therefore also sets `TMPDIR`, `HOME`,
`HELM_CACHE_HOME`, `HELM_CONFIG_HOME`, `HELM_DATA_HOME`, and `KUBECACHEDIR`, all
under the emptyDir.

The handoff deferred `readOnlyRootFilesystem` on the pod security context
"until the `deploy.sh` wiring shows which helm/kubectl cache dirs need to be
writable." That list is now known and is exactly the six variables above, so 2a
sets `readOnlyRootFilesystem: true` and closes the finding.

**No `KUBECONFIG`.** Both helm and kubectl fall back to in-cluster configuration
when no kubeconfig is present, authenticating as the pod's ServiceAccount — the
one already bound to `cluster-admin` by `templates/clusterrolebinding.yaml`.

### 3. `internal/applier` — driving `deploy.sh`

**Responsibility.** Execute one bundle directory's `deploy.sh` and convert its
output into typed console events.

**Why `deploy.sh` and not per-component `install.sh`.** `approach.md`'s Decisions
table says "Go orchestrator driving the bundle's own `install.sh`". That is
wrong, and the handoff already corrects it. `deploy.sh` is ~496 generated lines
carrying correctness logic a per-component loop silently drops:

- preflight for terminating namespaces, stale webhooks, orphaned CRD groups, and
  stale nodewright taints;
- per-component wait derivation — `kai-scheduler` at 20m with 1 retry,
  `*-readiness` gates at a bundler-derived timeout, `ASYNC_COMPONENTS` skipping
  `--wait`;
- quadratic-backoff retry with helm hook-Job cleanup and diagnostic capture
  between attempts;
- a post-install block that waits for
  `nvidia.com/gpu-driver-upgrade-state=upgrade-done` on every managed node before
  restarting the DRA kubelet plugin. Skipping this strands DRA pods in
  `ContainerCreating` (AICR issue #973).

Driving `deploy.sh` also preserves the property the applier was chosen for in the
first place: what the console runs is byte-identical to what the user downloads.

**The `Exec` seam.**

```go
// Exec is the process seam. Production runs bash; the test fake writes a
// captured transcript to out and returns a canned error, so golden tests
// need no real process.
type Exec interface {
    Run(ctx context.Context, spec Spec, out io.Writer) error
}
```

`Spec` carries working directory, argv, and environment. The applier passes an
`io.Writer` that splits on newlines and feeds the parser. stdout and stderr are
merged into that one writer so ordering is preserved.

**Invocation.** `bash deploy.sh --retries 5`, from the bundle directory, with
`NO_COLOR=1` exported alongside the empty-by-default `DRY_RUN_FLAG`,
`KUBECONFIG_FLAG`, and `HELM_DEBUG_FLAG` the script expects.

`--best-effort` is deliberately **not** passed. See Failure policy below.

**The marker grammar.** Verified line by line against
`pkg/bundler/deployer/helm/templates/deploy.sh.tmpl` at v0.19.0. With `NO_COLOR`
set, every color variable expands empty, so the literal forms are:

| Line | Source | Event |
|---|---|---|
| `┌─ [N/M] <name>  →  <namespace>` | `_step_header`, tmpl:73 | `KindComponent`, started, index N of M |
| `└─ ✓ <name> installed` | `_step_ok`, tmpl:78 | `KindComponent`, succeeded |
| `└─ ✗ <name> FAILED (after N attempts)` | `_step_fail`, tmpl:82 | `KindComponent`, `LevelError` |
| `  ↺ <name>: attempt N/M failed, retrying in Ss...` | `_step_retry`, tmpl:86 | `KindComponent`, `LevelWarn` |
| `✓ Pre-flight checks passed` | `_ok`, tmpl:312 | `KindPhase` |
| `⚠ <msg>` | `_warn_line`, tmpl:68 | `KindLog`, `LevelWarn` |
| `✗ <msg>` | `_fail`, tmpl:67 | `KindLog`, `LevelError` |

Note the two spaces on either side of the arrow in the header line, and the two
leading spaces on the retry line. Both are literal in the template and both are
load-bearing for the parser.

`parse.go` holds a pure `parseLine(string) (bus.Event, bool)` plus a small
stateful tracker for the current component, its N-of-M position, and elapsed
time.

**Unrecognized lines are not published.** Everything `deploy.sh` emits that is
not a marker is helm, kubectl, and diagnostic output. Publishing it would flood
the bus's 20 000-event replay ring and the browser, and the bus drops live events
for a subscriber more than 256 behind. Instead:

- every raw line goes to `slog`, so `kubectl logs` retains the complete
  transcript;
- the last ~200 lines are held in a bounded ring and attached as `Data` to the
  failure event.

That ring is what captures `deploy.sh`'s own
`--- Failed hook Job <name> diagnostics ---` and `--- <ns> diagnostics ---`
blocks, which is precisely what the failure screen needs to show.

**Guarding the grammar.** `TestDeployTemplateUnchanged` pins the sha256 of
`deploy.sh.tmpl` from the pinned module —
`df919af7e46d565d38fbf12927881ebeec1172227efac8962e4c00f035a8b519` at v0.19.0 —
so an upstream edit fails CI and forces a parser review on the bump rather than
silently degrading the timeline.

### 4. Failure policy

Component failures on real clusters are common, not exceptional. `approach.md`
Risk 4 insists the failure state is designed in, not retrofitted.

**Fail fast, then retry the whole script.**

`deploy.sh` runs with `--retries 5`, so it applies its own quadratic backoff
(5s, 20s, 45s, 80s, 120s) per component, and each `↺` surfaces as a warn event so
the wait is visible rather than silent. Without `--best-effort`, the first
component to exhaust its retries ends the script non-zero and fails the run.

The cockpit then shows the failing component, the captured diagnostic tail, and a
**Retry** that re-executes `deploy.sh` from the top. That is safe and correct:
every `install.sh` is `helm upgrade --install`, which is idempotent, and
`deploy.sh`'s preflight and hook-Job cleanup run again on the retry.

`--best-effort` was rejected. It ends on a cluster that looks installed and is
not, and it converts a clear applier failure into a confusing Validate or Prove
failure one phase later.

### 5. `internal/engine` — the smallest change that supports Retry

Retry requires the engine to re-enter a failed run. Today `finish(StateFailed)`
is terminal and `execute` returns. Three contained changes:

- **Step cursor.** `Run` gains a step index; `execute` starts from it rather than
  always from zero.
- **`Engine.Retry(runID)`.** Valid only from `StateFailed`. Resets the run to
  `StateRunning` and relaunches `execute` at the step that failed.
- **Epoch guard.** `execute`, `runStep`, and `awaitDecisions` re-check that
  `e.current` is still the run their goroutine started for. The handoff flags
  this: `Start`'s `isLive` check is the only protection today. Retry makes a
  second live goroutine genuinely reachable, so the guard stops being theoretical.

`StateActive` remains declared and unreachable. Phase 3 owns it.

### 6. `internal/api` — three thin additions

| Route | Behavior |
|---|---|
| `POST /api/runs/{id}/retry` | Delegates to `engine.Retry`. 409 unless the run is `failed`. |
| `GET /api/runs/{id}/bundle` | Streams the bundle directory as `.tar.gz`. |
| `POST /api/runs/{id}/decide` | Unchanged. Carries `{"apply": "yes"}` for the gate. |

The download route is cheap — the tree is already on disk — and it makes the
confirm gate honest: the user can inspect exactly what they are about to approve.
It is the spec's "Download bundle" action, arriving one phase early because Apply
made it nearly free.

**The 409-latch.** `handleCreateRun` passes the cancellable request context to
`engine.Start`. The handoff files this under 2b, because a `store.Save` failure
would leave `e.current` live and permanently 409 new runs. It becomes reachable
in 2a for a different reason: Apply runs for 10–20 minutes under a context
derived from that request. `Start` gets `context.WithoutCancel` at the call site
now.

### 7. `web/` — minimal cockpit

The layout switches from the centered wizard to the cockpit when the run's phase
reaches `bundle`. One new `Cockpit.tsx` with four states:

- **Gate** — component list with name, version, and namespace; *Download bundle*;
  *Install*.
- **Running** — the component pipeline is the hero: N of M, per-component status
  derived from `KindComponent` events, elapsed time. The existing `Timeline`
  moves to the right rail.
- **Failed** — failing component, the diagnostic tail off the error event's
  `Data`, and *Retry*.
- **Done** — hands off to Phase 3.

State is derived from the event stream, extending `deriveRunState` in
`Wizard.tsx`. This follows the pattern already established there and needs no new
polling endpoint.

**Slow-step callouts.** A static TypeScript record keyed by component name,
surfacing an inline explanation before a known multi-minute stall. Three honest
entries to start: the `gpu-operator` driver DaemonSet compiling a kernel module
per node, `kai-scheduler` installing async without `--wait`, and `*-readiness`
gates polling on a long derived timeout. Real per-node timings are calibrated in
Phase 4 against real hardware; nothing is fabricated for KWOK.

This is the one item in 2a that is presentation rather than mechanism, and it is
included because an unexplained multi-minute stall is exactly where a demo
audience concludes the tool is broken.

**Two carried-over findings**, included because they live in files this work
already touches:

- Nothing resets `authed` on a 401. After the 8-hour session expiry the console
  sticks on "reconnecting…" forever with no path back to the login screen.
  Fixed in `App.tsx` / `useEvents.ts`.
- `Discover.tsx` shows the green "already capable" copy for `gap.Analyze`'s
  degraded early return too, since "No cluster snapshot available" also yields
  nil gaps. The two cases become distinguishable.

---

## Testing

Mirrors the conventions already in the repo: table-driven, `-race`, structured
errors, and the 80% coverage floor from `.settings.yaml`.

| Unit | Approach |
|---|---|
| `applier` parser | Table-driven `parseLine`, plus golden files over a captured `deploy.sh` transcript |
| `applier` exec | Fake `Exec` asserting argv, environment, working directory, and context cancellation |
| template pin | `TestDeployTemplateUnchanged` — sha256 of the v0.19.0 `deploy.sh.tmpl` |
| `steps/bundle` | Fake AICR client; the re-resolve-matches-`recipe.json` guard tested in both directions |
| `engine` Retry | Fake steps driving fail → retry → succeed, plus the epoch guard under `-race` |
| `api` | `httptest` over retry, bundle download, and the gate |
| `web` | Component tests against a recorded apply-phase event stream |
| end to end | `test/e2e/apply-dryrun.sh` on Kind + KWOK |

### The dry-run end-to-end

`deploy.sh` exports `DRY_RUN_FLAG`, and every generated `install.sh` interpolates
it into its `helm upgrade --install` line (verified at
`localformat/templates/install-upstream-helm.sh.tmpl:40`). CI can therefore run
the real bundle, the real `deploy.sh`, and the real helm binary against the
Kind/KWOK cluster with `DRY_RUN_FLAG=--dry-run` — exercising the whole marker
grammar and the parser end to end without installing anything.

The cluster must carry AICR's simulated GPU nodes, or no recipe resolves at all;
`test/e2e/discover-recommend.sh` already does this correctly and is the model.

**This is unproven and is the largest risk in 2a.** Some charts may fail
`helm upgrade --install --dry-run` on KWOK for reasons unrelated to the applier —
a chart templating a custom resource whose CRD is not yet installed is the likely
one. **Task 1 of the implementation plan is therefore a throwaway probe**:
bundle `training/kubeflow` against the committed simulated-H100 fixture and
dry-run it, before any applier code is written. If it fails, the fallback is
golden-files-only coverage plus live verification deferred to Phase 4 — and that
is a scope change to raise, not to absorb silently.

---

## Open questions

1. **Dry-run viability on KWOK.** Resolved by Task 1's probe, above. Everything
   else in the plan is independent of the answer.
2. **Bundle size on the emptyDir.** A non-vendored bundle is small — scripts,
   values, and Chart.yaml stubs — but this has not been measured. Worth checking
   during Task 1 so the chart can carry a `sizeLimit` if it warrants one.
3. **Retry semantics across a pod restart.** 2a's step cursor lives in memory, so
   a restart mid-Apply still loses the run. That is the ConfigMap store's job in
   2b, and 2a's re-resolving Bundle step is designed so 2b needs no rework here.
4. **Ownership and budget.** `approach.md` Open Question 1, still untouched. It
   does not block 2a, but it decides whether Phase 4's real-hardware time exists.

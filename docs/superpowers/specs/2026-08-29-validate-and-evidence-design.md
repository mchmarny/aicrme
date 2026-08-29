# aicrme — Validation and Evidence

**Date:** 2026-08-29
**Status:** Approved for planning
**Pinned dependency:** `github.com/NVIDIA/aicr v0.20.0` (no upgrade required)
**Supersedes:** the deferral recorded in `docs/STATE.md` open item 3 and in
`docs/phase-2-handoff.md`'s "Constraints 2b-iii inherits"

---

## Why this exists now

The console installs a recipe and proves a gang-scheduled workload places on it.
It has never made AICR's own claim about the result: that the cluster passes the
recipe's validation, and that there is attestable evidence of it.

Both capabilities are first-class in the pinned SDK. Nothing needed to change
upstream, and no CLI shell-out is involved:

```go
func (c *Client) ValidateState(ctx, *RecipeResult, *Snapshot, ...ValidateOption) ([]*PhaseResult, error)
func (c *Client) EmitRecipeEvidence(ctx, *RecipeResult, *Snapshot, []*PhaseResult, EvidenceOptions) error
```

`EmitRecipeEvidence`'s own doc comment describes this caller exactly:
*"Interactive keyless-signing disclosure is intentionally NOT performed here —
that is a UI concern the caller handles… can run unattended from a
server/library."*

### The reasoning being reversed

Validation was deferred because `ValidateState` **false-passes on KWOK**: the
validator schedules its orchestrator Job with a blanket toleration
(`{Operator: Exists}`), which defeats the `kwok.x-k8s.io/node=fake:NoSchedule`
taint the fake nodes defend themselves with; KWOK then fakes
`Terminated{ExitCode:0}` without starting the container, and exit 0 becomes CTRF
`"passed"`. Measured, not inferred: 14/14 false passes with nothing installed.

That is a property of the substrate, not of the product. **On real GPU hardware
there are no fake nodes for the blanket toleration to land on.**

> **Ruling, 2026-08-29.** "KWOK is for our testing and quick demo of some
> aspects. The real cluster is where this project solves for and the full tests
> need to pass there. We should not limit functionality by what's possible in
> KWOK."

So the capability is built for the real cluster, and the simulated path **skips
it and says so** — the same way Prove already labels a simulated run without
apology. "The demo runs on KWOK, therefore do not build the capability" is the
inversion being corrected.

---

## Decisions

| Decision | Choice |
|---|---|
| Where Validate runs | The already-reserved `PhaseValidate`, between Apply and Prove |
| On validation failure | **Report-only.** The run continues to Prove |
| Default phase | `deployment` only |
| `conformance` | Operator opt-in, after Stop |
| `performance` | Operator opt-in, after Stop |
| Evidence | Optional, operator-triggered, local and unsigned by default |
| Publishing | Opt-in: registry ref supplied by the operator, signed by default |

### Why `performance` is not in the pipeline

It was asked for as a default, and it cannot be one — **it and Prove both want
the entire GPU inventory.** AICR's own `PhaseOrder` comment records hitting this
internally: the inference-perf benchmark *"saturates every GPU on the node and
the DynamoGraphDeployment teardown releases those DRA ResourceClaims
asynchronously"*, which starved conformance's GPU-needing checks. Prove requests
a full node per gang member and then holds it until Stop.

They therefore collide in either order. Splitting them resolves it without
losing either: `deployment` before Prove (it is the "did the components
reconcile" check, which is what was actually asked for), and `performance` after
Stop, when the GPUs are free.

---

## Run shape

A new `steps.Validate` at `PhaseValidate`, wired between Apply and Prove in
`clusterWiring.steps()`. `PhaseValidate` already exists as a constant and is
already accepted by `recover.go`; nothing implements it. The slot was reserved
for this.

Options passed: `WithValidationPhases(PhaseDeployment)`,
`WithValidationKubeconfig(sessionKubeconfig)`, `WithValidationRunID(run.ID)`,
`WithValidationCleanup(true)`, and `WithValidationTimeout(15 * time.Minute)`.

Fifteen minutes is a deliberate number, not the default. The facade's default cap
is 75 minutes, sized for an all-phase run whose largest single check is the
65-minute inference-perf benchmark. `deployment` is the cheap install/health
phase; a run of it that has not finished in fifteen minutes is stuck, and on a
step that sits in the middle of a demo the right failure is a bounded one. The
opt-in phases keep the SDK default, because `performance` legitimately needs it.

**It never fails the run.** Same posture, and for the same reason, as
`snapshotOwnership`: a validation that could not run, or that reports failures,
is recorded and emitted, and the run proceeds. The success screen then carries
both claims — what validation found, and that a gang actually placed. A flaky
check cannot cost a demo, and a partial failure does not read as a total one.

**Simulated clusters are skipped, loudly.** Detection reuses the signal Prove
already trusts (`report.totalGpus == 0`, `internal/gap`'s own definition). The
run records `validation skipped: simulated cluster` — never a pass.

### Operator actions once the workload is stopped

Both require the GPUs to be free, so both are gated on the run no longer holding
a workload:

- **Validate further** — `conformance` and/or `performance`
- **Generate evidence** — `EmitRecipeEvidence`

The gate reuses the existing rule that `POST /api/runs` answers 409 while a
workload is running, which `test/e2e/prove.sh` already asserts.

---

## The live-RecipeResult constraint

Both SDK calls demand a `*RecipeResult` that **this** `Client` produced:
`assertOwns`, plus an explicit rejection of `rec.internal == nil` with
*"call Client.ResolveRecipe to obtain a validatable RecipeResult"*. The console
persists `recipe.json`, which is a **summary** — it cannot be used.

- **In-pipeline Validate** already has one: Recommend produced it in this
  process. It is passed in memory.
- **Post-Stop actions and any recovered run** re-resolve from the persisted
  snapshot via `ResolveRecipeFromSnapshot`. The raw agent bytes are already
  persisted verbatim, deliberately, so this is available.

Re-resolution is deterministic because the catalog is embedded in the binary
(`EmbeddedSource`) — **but only for the same binary.** So the re-resolved
component set is compared against the stored `recipe.json`, and a mismatch
**refuses** rather than validating a recipe that is not the one installed. An
operator who upgraded aicrme between install and validate gets a clear refusal,
not a quietly wrong attestation.

---

## Persistence

`PhaseResult` carries `RawReport []byte` and a full CTRF `*Report`. The run
record is gzipped and about 30 KB today; CTRF payloads would bloat it.

- The **record** stores a per-phase summary: phase, status, duration, counts,
  plus the path of the report on disk.
- The **raw CTRF** is written into the run directory as a file.

That path is stored in the **persisted** record, not in `ephemeralArtifacts`.
`ephemeralArtifacts` is dropped on encode, which is exactly why
`GET /api/runs/{id}/bundle` 404s for a recovered run today. Repeating that here
would break evidence download on precisely the path where it matters most.

---

## Client seam

Two new role interfaces in `internal/aicrclient`, following the existing
one-role-per-interface pattern (`Snapshotter`, `Resolver`, `CriteriaRegistrar`,
`CatalogLister`) rather than widening an existing one:

```go
type Validator interface {
    ValidateState(ctx context.Context, r *aicr.RecipeResult, s *aicr.Snapshot,
        opts ...aicr.ValidateOption) ([]*aicr.PhaseResult, error)
}

type EvidenceEmitter interface {
    EmitRecipeEvidence(ctx context.Context, r *aicr.RecipeResult, s *aicr.Snapshot,
        results []*aicr.PhaseResult, opts aicr.EvidenceOptions) error
}
```

`aicrclient.Fake` gains results, errors and call counters in the existing style.

---

## API

| Route | Body | Notes |
|---|---|---|
| `POST /api/runs/{id}/validate` | `{"phases":["conformance","performance"]}` | Backgrounded like Reset — a `performance` run can take an hour |
| `POST /api/runs/{id}/evidence` | `{"push","noSign","identityToken","deviceFlow"}` | Backgrounded |
| `GET /api/runs/{id}/evidence` | — | Streams a tarball, reusing `handleBundle`'s path-containment check verbatim |

Both POSTs refuse while a workload is running.

### Signing is coupled to publishing

`NoSign`'s doc is explicit: it *"pushes an unsigned bundle… (requires Push)"*.
There is no such thing as a signed local bundle. So the UI offers **one** opt-in
— publish to a registry — with signing on by default for that publish:

- **No push** → local bundle, unsigned, downloadable. The default.
- **Push, signed** → needs an OIDC identity. `ResolveOptions` supports a
  pre-fetched `IdentityToken` (paste) or `DeviceFlow` (OAuth 2.0 Device
  Authorization Grant), the latter being what a local binary can actually drive:
  show a code and a URL, poll for authorization.
- **Push, `NoSign`** → published unsigned.

`Full` stays false: minimized payloads are the default, so the raw snapshot is
not shipped unless asked for.

---

## UI

- **Validation panel** on the Prove screen: one row per phase with status,
  duration and counts — or the skip reason on a simulated cluster.
- **Once stopped:** *Validate further* (phase checkboxes) and *Generate
  evidence* (with an optional publish section: registry ref, sign toggle, token
  or device flow).
- **Evidence download** beside the existing bundle link.
- Colours use the `pass` / `fail` / `warn` tokens already defined in
  `web/src/index.css`.

---

## Testing

Unit coverage: phase selection, skip-on-simulated, never-fails-the-run, summary
recording, the re-resolution mismatch refusal, route gating, refusal while a
workload runs, and evidence path containment (mirroring the existing bundle
tests).

**The KWOK e2e asserts the SKIP, not a validation.** A pass there would be the
false pass this document opens with. Real validation is verified on hardware by
hand, exactly as Prove was before it. KWOK neither gates the feature nor gets to
fake it — which is the ruling applied to the test suite rather than argued with.

---

## Suggested increments

The two halves are separable and the second depends on the first, so they should
be planned and shipped in that order:

1. **Validate.** The step, the client seam's `Validator`, persistence of the
   per-phase summary, the skip-on-simulated path, the validation panel, and the
   post-Stop *Validate further* action. Independently demoable: a real cluster
   gains a validation verdict it did not have.
2. **Evidence.** `EvidenceEmitter`, the two evidence routes, the download, and
   the publish/signing form. Depends on (1) because `EmitRecipeEvidence` takes
   the `[]*PhaseResult` that only a validation run produces.

Shipping (1) alone is coherent. Shipping (2) alone is not.

---

## Known limits

- **No per-component attribution.** `ctrf.Builder` hardcodes `Suite` to
  `[]string{phase}`, so nothing in the output identifies which component a check
  belongs to. A UI showing green checks beside components would need a lossy
  heuristic; this design does not attempt one.
- **Validation is not re-run automatically.** A cluster that drifts after the
  run says nothing new until an operator asks again.
- **`performance` is never automatic**, by construction — it saturates the GPUs
  the reference workload needs.

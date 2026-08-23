# AICR upstream asks

Gaps in AICR that this console works around, collected so they can be filed as small PRs
**when aicrme is public**. Nothing here goes upstream before then; until it does, this file is
the record.

Each entry states the workaround currently carried, so the cost of *not* fixing it is visible
rather than implied.

---

## 1. `pkg/client/v1.AgentConfig` does not mirror `AKSGPUPoolsPath`

**Found:** 2026-08-23, scoping the AKS step.

`snapshotter.AgentConfig.AKSGPUPoolsPath` points at an operator-supplied
`az aks nodepool list -o json` dump and merges the `aks-gpu-pools` subtype into the snapshot.
Its own doc comment makes it unusually easy to consume — *"The projection is pure file
processing, so unlike ClusterConfigPath it never enters the pod"* — so a caller only has to
write a file and set a path.

`pkg/client/v1.AgentConfig` (`types.go:123`) is a hand-written mirror, not an alias, and it
omits the field. Every other agent knob is mirrored: `Kubeconfig`, `Namespace`, `Image`,
`NodeSelector`, `Tolerations`, `Privileged`, `RequireGPU`, `Requests`, `Limits` and a dozen
more. This one appears to have been missed rather than withheld.

**The ask:** add `AKSGPUPoolsPath string` to `pkg/client/v1.AgentConfig` and thread it through
`CollectSnapshot`. One field, one assignment.

**Workaround carried:** none — **AKS support is deferred instead.** The alternative was calling
`pkg/snapshotter` directly for Discover, replacing a facade call with a deep one for a core
operation. That trades a missing feature for permanent upgrade exposure, on a console whose
whole coupling strategy is to stay inside the v1 freeze. Deferring was the cheaper mistake to
unmake. See `approach.md`'s Phase 5 row.

---

## 2. No machine-readable event stream from `deploy.sh`

**Found:** Phase 2a. Carried in `docs/phase-2-handoff.md`.

`internal/applier/parse.go` transcribes `deploy.sh.tmpl`'s `printf` output formats into seven
hand-written regexes. Nothing in the Go type system connects the two, so an upstream wording
change silently empties the Apply timeline while every test stays green.

**The ask:** an opt-in machine-readable event stream — `AICR_DEPLOY_EVENTS=jsonl` or similar —
emitted by the template alongside its human output.

**Workaround carried:** `TestDeployTemplateUnchanged` pins the template's sha256 against the
module cache, so an upstream edit fails loudly instead of drifting. That converts a silent
failure into a noisy one; it does not remove the parser. This is the single highest-value ask
here: it would retire `internal/applier/parse.go` entirely, which is the one genuinely
upgrade-fragile surface in this repo.

---

## Resolved upstream, no longer an ask

**`Snapshot()` and `Validate()` in `pkg/client/v1`.** `approach.md` Risk 1 was written when the
console had to import `pkg/snapshotter` and `pkg/validator` directly because the v1 surface
covered neither. As of **v0.19.0 both are on the facade** — `Client.CollectSnapshot` and
`Client.ValidateState` — and this console uses `CollectSnapshot` through it. `pkg/validator` is
no longer imported at all.

`pkg/snapshotter` is still imported, but only to decode the `Snapshot` type
(`internal/steps/criteria.go`, `internal/aicrclient/options.go`); the agent itself runs entirely
through the facade. Risk 1's mitigation largely landed upstream without this project having to
ask.

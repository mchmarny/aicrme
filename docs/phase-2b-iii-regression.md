# Phase 2b-iii regression — 2b-iii exonerated; apply-real still fails intermittently

**RESOLVED 2026-08-19: the hypothesis this document was built around is WRONG, and 2b-iii is
not the cause.** 2b-iii is re-merged. The rest of this file is kept because the measurements
are reusable and because the reasoning error is worth not repeating.

## What the measurement showed

An instrumented A/B — identical scripts, differing only in whether the observer's
namespace-scoped Pod and Event informers exist — put **`main` worse than `phase-2b-iii` on
every sample**, on a 2-core/7.75GiB runner:

| elapsed into Apply | `main` avail / control-plane | `2b-iii` avail / control-plane |
|---|---|---|
| ~0:53 | 5436MiB / **1.085GiB** | 5444MiB / 0.913GiB |
| ~1:57 | 4893MiB / **1.526GiB** | 4913MiB / 1.283GiB |
| ~3:00 | 4170MiB / **1.686GiB** | 4578MiB / 1.438GiB |
| ~4:03 | 3760MiB / **1.731GiB** | 4003MiB / 1.520GiB |

Runner memory drains 400-700MiB/min through Apply and never plateaus while components keep
installing. A run that finishes Apply quickly survives with headroom; a slow one keeps
draining. Two slow runs failed, faster ones passed — **on both branches**.

## The reasoning error, stated plainly

The failures started after the 2b-iii merge, so 2b-iii was assumed to be the cause, and three
rounds of instrumentation were built to prove *how* it caused them. Nobody measured whether
`main` did the same thing until the fourth run. The correlation was real; the causation was
invented. **One baseline run dissolved it, and it was the run that should have come first.**

Two further guesses inside that same investigation were also wrong, and both were caught only
by measuring:
- *"A container OOM will show in `docker events`."* `kube-apiserver` and `etcd` are static pods
  **inside** the control-plane container; an in-container process OOM leaves the container
  alive and the events log silent. The first instrument was structurally blind to the most
  likely mechanism.
- *"One worker is enough, the workloads are small."* Kind reports each node's allocatable CPU
  as the **host's** core count, so worker count is the scheduling budget, not just memory.
  Cutting 3 workers to 1 took Apply from ~5 minutes to over 40 without completing.

## What is still open

**Why `apply-real` fails intermittently is still unknown.** The memory drain is real and
measured; the link from that drain to the failures is *inferred and unproven*. No OOM has ever
been captured at failure time — the one run that died hard lost its capture, and the run with
durable capture passed.

Outcomes do differ by branch (`main` 2/2 pass, 2b-iii 1/3) but the measured mechanism does not
support a 2b-iii cause and the samples are far too small to separate from chance.

The instrumentation is retained on `main` for exactly this reason: the next failure on either
branch will now be captured with durable artifacts rather than vanishing with the runner.

---

## Original record (kept — the symptom description is still accurate)

---

## What happened

`phase-2b-iii` merged to `main` as `79c4f95` with `make qualify` green, aggregate coverage
89.9%, and every gate passing locally. The e2e workflow's `apply-real` job then failed on
`main` **twice at the same commit** — a re-run reproduced it, so this is not a flaky runner.

`ci`, `smoke`, `discover-recommend`, and `apply-dryrun` all passed both times. Only
`apply-real` — the real, non-dry-run install of all 14 deployment actions — failed.

## The symptom

Both failures took the same shape: mid-Apply, the Kind cluster's API server stopped answering.

```
curl: (7) Failed to connect to localhost port 18083
Unable to connect to the server: EOF
Error: Kubernetes cluster unreachable: Get "https://127.0.0.1:46127/version": EOF
```

Not an assertion failure. Every diagnostic the script attempted afterward also failed to reach
the cluster, so `apply-real.sh` exited 7 with no usable state dump — **the failure destroys its
own evidence**, which is the first thing to fix.

## The timing, which is the most useful clue

| Run | Commit | Apply duration | Outcome |
|---|---|---|---|
| Pre-merge | `ed95e16` | **4m51s** | `state=done`, 14/14 installed, job green in 15m59s |
| Post-merge #1 | `79c4f95` | 7m22s | cluster died, never reached `done` |
| Post-merge #2 (re-run) | `79c4f95` | 5m43s | cluster died, never reached `done` |

Both failures were **still installing** past the point where the pre-merge run had already
finished. Apply is not merely being interrupted — it is running slower or wedging, and the
cluster dies after that.

## Leading hypothesis — REFUTED 2026-08-19 (kept to show what was tested)

2b-iii added roughly ten namespace-scoped informer factories, each with a Pod **and** an Event
informer, all live during a 14-release install that generates events heavily.

- Every handler delivery passes `scopedInformers.withNamespaceLive`, which takes `s.mu` — the
  same mutex that gates `reconcile` and **every other namespace's** deliveries. That serializes
  all handler work behind one lock.
- The whole-branch review established that client-go's `processorListener` "buffers unboundedly
  on its own goroutine, so even a slow handler can't stall the reflector." Unbounded buffering
  turns a slow handler into **memory growth** rather than backpressure.

### Two facts that argue against it, and must be explained before accepting it

1. **The console has `limits: { memory: 512Mi }`** (`charts/aicrme/values.yaml`). It should be
   OOM-killed long before it could starve the node. That fits the port-forward dying first — but
   not the API server dying.
2. **A console OOM does not explain `kubectl` losing the API server.** Something took out the
   control-plane container, not just our pod.
3. **Probe 2 ran the identical 14-action install locally, same chart, same 512Mi limit, and
   succeeded** minutes before the CI failures. If this is resource-driven, local headroom masks
   it — but that is an assumption, not a measurement.

A competing explanation worth equal weight: the runner is simply at its limit with a 4-node Kind
cluster + KWOK nodes + 14 installs, and 2b-iii's added watches tipped it. That would make this a
CI-capacity problem rather than a product defect.

**Do not fix either one until instrumented.** Guessing at mechanisms is how this project lost
two phases to the dry-run ceiling.

## Why this may be a product defect, not just CI

If the console degrades under install load on a resource-constrained cluster, that matters
beyond CI — demo clusters are routinely small, and a console that dies during Apply is the
worst possible moment for it. That possibility is the reason the merge was reverted rather than
left red while investigating.

## Next step as it stood on 2026-08-18 (superseded — the A/B above replaced it)

**Instrument before changing code.** Add capture to `apply-real.sh` on the investigation branch
so the next failure records, during Apply rather than after the cluster is gone:

- node conditions and allocatable/used memory, sampled periodically
- `docker stats` for the Kind node containers
- kernel OOM events (`dmesg | grep -i oom`) from the runner
- the console pod's own memory and any `OOMKilled` restart reason

One ~16-minute CI run then turns this into evidence. The same move settled the Validate question
twice on 2026-08-18 — both times the measurement contradicted a confident prediction.

Only after that: decide between narrowing the observer's footprint, bounding the listener
buffers, raising the console's memory limit, and moving `apply-real` to a larger runner.

## Where everything is

| What | Where |
|---|---|
| The 32 commits | `phase-2b-iii-investigate` and `phase-2b-iii`, both at `79c4f95` |
| Revert commit | `46e9eca` on `main` |
| SDD ledger — 40 numbered rulings with reasoning | `.superpowers/sdd/2026-08-17-aicrme-phase-2b-iii/progress.md` (git-ignored, on disk only) |
| Per-task reviews, re-reviews, reports | same directory |
| Failing CI runs | GitHub Actions run `32197377434`, jobs `95904036244` and `95910211685` |
| Last green `apply-real` | run `32081305533` |

**The ledger is git-ignored and exists only on this machine.** It holds the reasoning behind all
40 rulings, which the commits do not. `git clean -fdx` destroys it. It is the record needed to
re-merge this work with its history intact — do not delete the workspace until 2b-iii actually
lands.

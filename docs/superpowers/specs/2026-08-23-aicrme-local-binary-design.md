# Local binary — replacing the in-cluster console

**Date:** 2026-08-23
**Status:** Approved for planning (revision 5)

**Revision 2** corrected §2 and §5 against two findings from reading AICR's client and this
repo's own `internal/steps/ownership.go`. Revision 1 claimed §2 made every consumer agree on the
target cluster; it enumerated two of four. It also treated helm version skew as a warning, when
a known hard incompatibility exists at helm 4 in code this repo already ships.

**Revision 3** folds in review. Six corrections, each verified against code rather than
accepted on description:

1. §4's run key (server URL + context name) is not an identity. Both halves are mutable and
   an endpoint can be re-pointed at a replacement cluster — §2 now establishes a real one.
2. Nothing in this repo takes a lock of any kind (`grep -rn "flock\|LOCK_EX\|lockfile"
   internal/` is empty), and the ConfigMap store being deleted carried the *existing*
   multi-writer guard. §2 adds a connection state machine and a work-directory lock.
3. A kubeconfig persisted at `<workdir>/kubeconfig` outlives the process, contradicting §2's
   own claim about credential lifetime.
4. Preflight checked two of the four executables the deleted image supplied.
5. **The token in §3 cannot reach `/api/events`.** `internal/api/server.go:160` already
   records in a comment that EventSource cannot attach custom headers, which is why safe
   methods are exempt from the same-origin check and why `GET /api/session` exists at all.
   Revision 2 claimed the SPA was unchanged while removing the only auth mechanism its
   event stream can carry.
6. Connect created a namespace, which made a read-only-looking probe mutate the cluster.

Open questions 1 and 2 from revision 1, and the in-session-switching question from revision 2,
are resolved and folded in.

**Revision 5** was written while decomposing this spec into tasks, which is where a phantom one
surfaced. §5's "Helm 4 is not skew" — revision 2's "single largest piece of real work created by
deleting the Dockerfile" — described a defect that commit `e36b015` had already fixed before this
spec existed. Revision 2 read a comment explaining *why the code avoids `--all`* as a description
of code that uses it. The section is withdrawn and replaced with what is actually left, which is
a test obligation rather than a code change. §8's e2e claim is corrected in the same pass: two
assertions cannot survive the image's deletion, contrary to "keep their assertions and change
only how the console is started."

**Revision 4** absorbs Phase 4's open decision #1 — whether Prove adopts or recreates a workload
whose spec has changed — which was the only one of that phase's three open decisions the move to
a local binary does not dissolve. §4 answers it: Apply recreates on spec drift and stays
idempotent otherwise. Reading the code to write that answer corrected the diagnosis in
`docs/phase-4-status.md`, which named the wrong mechanism.

**Scope:** The delivery model only. `aicrme` stops being a Helm chart that deploys a console
into the target cluster and becomes a binary an operator runs on their own machine, which
serves the same SPA over loopback and drives the cluster through their kubeconfig. The arc —
Discover, Recommend, Bundle, Apply, Prove, Reset — is unchanged.

**One deliberate exception to that scope**, added in revision 4: `prove.Client.Apply` changes
behavior (§4, *Prove's workload can outlive the spec that produced it*). It is in scope because
this restructure is what turns that defect from a crash-only edge case into the ordinary
reconfigure-restart-retry loop, and because §4's recovery table leans on the same mechanism.
Nothing else in any step moves.

**Not in scope:** release automation (goreleaser, Homebrew tap, install script) lands after the
code works under `make build`. Upstreaming into AICR as `aicr server` is a later, separately
gated piece of work — this design only preserves the seam that makes it a code move.

---

## Why

The console runs `cluster-admin` inside the cluster it is installing. Most of the intricacy in
this repo exists to survive that decision, not to serve the product:

- The run record is checkpointed into a ConfigMap with a resolved `ownerReference`, because the
  pod can be rescheduled mid-Apply.
- The image bundles `helm`, `kubectl`, `jq` and `bash`, and the chart redirects `TMPDIR`,
  `HOME`, three `HELM_*` cache roots and `KUBECACHEDIR` into an `emptyDir`, because the
  container runs `readOnlyRootFilesystem: true`.
- There is a generated password, a session cookie, a rate-limited login and a TLS toggle,
  because a `cluster-admin` console reachable over a `Service` needs them.
- `values.yaml` carries a `nodeSelector` because `nodewright-customizations` **restarts the GPU
  nodes**, and a console sitting on one is killed part-way through the install that rebooted it.
- `test/chart/contract.sh` exists to pin three Go constants against the chart's probe windows
  and `terminationGracePeriodSeconds`.

A process on the operator's machine has none of these problems. The last one is not merely code
— it is a live failure mode that a local binary is categorically immune to.

**The decisive question was durability**, and the answer is that it is not required: this is a
demo and eval tool, the operator watches the run, and a run that dies with the session is
re-run rather than resumed. That is what makes the deletions safe rather than merely relocated.

That sentence is about the *process*, not the record — §4 spells out what the next launch does
with an Apply, a Prove or a Reset that was interrupted, and the answer in all three cases is the
behavior the engine already has today.

**The prerequisite that looked like a cost is not one.** AICR's `helm` deployer already requires
`helm` and `kubectl` on the operator's machine, for the same audience. Requiring them here is
the burden they already have.

---

## 1. Entry point, and the seam that survives it

`cmd/aicrme/main.go` is 844 lines, of which `main()` alone is 328 — five step constructors, two
observer accessor closures, the store selection, the engine, the API server, the observer, and a
two-goroutine shutdown. Almost all of the rest is helpers serving that wiring. None of it is
reachable from a test.

That wiring moves into a new `internal/console` package behind one exported entry point:

```go
package console

type Options struct {
    Addr        string
    WorkDir     string
    Kubeconfig  string // explicit --kubeconfig, empty for the default loading rules
    Context     string // explicit --context, empty for the kubeconfig's current-context
    OpenBrowser bool
}

func Run(ctx context.Context, opts Options) error
```

`cmd/aicrme` becomes flag parsing, `slog` setup, signal wiring, and one call.

This is worth doing on its own terms — it is the difference between wiring that can be tested
and wiring that cannot. It is also the seam that makes `aicr server` a code move: an upstream
cobra command fills `Options` from AICR's existing kubeconfig and context flags and calls `Run`.
Because donation moves the package into AICR's tree rather than importing it across modules, the
`internal/` prefix is not an obstacle.

**Nothing is filed upstream until aicrme is public.** This section preserves a seam; it does not
authorize a PR.

### Flags

| Flag | Default | Notes |
|---|---|---|
| `--addr` | `127.0.0.1:0` | Port 0 means the OS picks; the chosen port is printed and opened. |
| `--kubeconfig` | unset | Falls through to `clientcmd`'s default loading rules, which honor `KUBECONFIG`. |
| `--context` | unset | Preselects a context; the operator can still change it in the UI before connecting. |
| `--open` / `--no-open` | `--open` | Opens the default browser at the tokenized URL. |
| `--work-dir` | `~/.aicrme` | `AICRME_WORK_DIR` overrides. |

Stdlib `flag`, not cobra. The flag parser is not the part that matters for upstreaming — the
`Options` struct is — and adding a dependency to anticipate a move that has not been approved is
speculative.

---

## 2. Connecting to a cluster

### Loading

`clientcmd.NewNonInteractiveDeferredLoadingClientConfig` over
`ClientConfigLoadingRules` (which honors `KUBECONFIG` and `--kubeconfig`), with
`ConfigOverrides{CurrentContext: opts.Context}`. This replaces the `rest.InClusterConfig()` block
in `main.go`, and with it every "kube is nil outside a pod" degradation path — there are three
such warnings today, and all three describe a state that can no longer occur, because a local
binary that cannot reach a cluster has nothing to offer and should say so plainly.

### Endpoints

- `GET /api/contexts` — the context names, the current one, and each one's cluster server URL.
  Reads the kubeconfig only. No cluster contact.
- `POST /api/connect {context}` — builds the clientset, calls `ServerVersion()` under a bounded
  context, reads the cluster identity below, and returns the server version, node count and
  identity on success. **It creates nothing.**

Every other API route returns `409 Conflict` until a connection is established. This is a state
gate, not an auth gate; §3 covers auth.

### Connect is single-assignment, and the work directory is locked

`net/http` serves every request on its own goroutine, so `POST /api/connect` is concurrent with
itself. Connect mutates process-global state (`KUBECONFIG`, below), builds the clientset the
observer and every step read, and selects the run directory. Two in-flight connects interleaving
across those three is a torn connection, not a lost race — the clientset can end up pointing at
one cluster while `KUBECONFIG` names another, which is precisely the split-brain the rest of
this section exists to prevent.

**Connection state is single-assignment.** A `sync.Mutex`-guarded state variable moves
`disconnected → connecting → connected` and never leaves `connected`; a second `POST /api/connect`
returns `409 Conflict` whether the first is still running or already finished. The
already-connected case is the same status as the not-yet-connected one for the routes above, and
it means the same thing: the request does not match the process's state. This is what makes the
four-consumer pin below a property of the process rather than of one request handler.

**A second aicrme against the same work directory is refused.** Two processes sharing
`~/.aicrme` write the same run record, and against the same cluster they also drive the same
install. The in-cluster console never had this problem — one Deployment, one replica, and
`cmstore`'s `resolveDeploymentOwner` resolved an `ownerReference` so a record written by a
*different* deployment was detected and degraded rather than overwritten. §6 deletes that file.
**The guard it carried has to be replaced, not merely dropped**, which revision 2 missed by
listing `cmstore.go` as a pure subtraction.

The replacement is an `O_CREATE|O_EXCL` lock file at `<workdir>/lock` holding the PID, taken at
startup and removed on clean shutdown. A stale lock — the file exists, the PID does not — is
reported with the PID and the path, and cleared by the operator rather than automatically: a
live second process and a crashed first one look identical from the file alone, and guessing
wrong is the case this guard exists to prevent.

This is a local-exclusion guard, not a distributed one. Two operators on two laptops installing
into the same cluster is not something a file lock can see, and this design does not attempt to
solve it — Apply's idempotence and AICR's own release-level behavior are what stand between that
case and damage. What is in scope is the single-machine case, which is the one a demo tool
actually meets.

### Cluster identity is the kube-system UID

Connect reads `kube-system`'s namespace UID and records it with the run. **That UID, not the
server URL and not the context name, is this cluster's identity.**

Both halves of the obvious key are mutable: a context can be renamed or re-pointed in the
operator's kubeconfig between two runs, and an endpoint — a load balancer address, a
`kind-aicrme` local cluster torn down and recreated — can front a completely different cluster at
the same URL. §4 keys the run directory on identity for recovery, and Reset acts on that record
by uninstalling releases; a key that two different clusters can collide on is a key that can
point Reset at the wrong one.

`kube-system` is created by the control plane at bootstrap, is never recreated during a
cluster's life, and is readable by any principal that can do anything else useful here. Using a
namespace UID as an identity is already this repo's idiom: `snapshotOwnership` records one per
namespace for exactly this reason (`internal/steps/ownership.go:101`,
`internal/engine/run.go:244`).

**Identity is revalidated, not merely recorded.** Before recovering a run (§4) and before Reset
acts on one, the stored UID is compared against the connected cluster's. A mismatch is refused
with both UIDs named — it means the record describes a cluster that no longer exists at this
address, and every release name in it is now a name in somebody else's cluster.

### The connection is frozen for the process

On successful connect, a **flattened, minified, single-context kubeconfig** is written to
`<workdir>/session-<pid>/kubeconfig`, in a directory created `0700` with the file at `0600`. The
selected context is baked in as that file's `current-context`, so the file alone is a complete
answer and no consumer needs a separate context argument.

**The file is per-launch and is deleted on shutdown**, in the same deferred cleanup that releases
the lock file. Flattening inlines whatever the source context held — a bearer token, a client
certificate and key, a cached OIDC id_token — so a fixed `<workdir>/kubeconfig` would leave live
credentials on disk after the process exits, indefinitely. That flatly contradicts the promise
this section makes further down, under *Exec credential plugins*: that the binary holds the
operator's credentials **for as long as it runs**. A per-launch path is what makes that sentence
true. (An exec-based context minifies to a stanza rather than a secret and is the benign case;
a context holding a bearer token or a client key is not, and the file cannot know which it got.)

Cleanup is best-effort, so startup also sweeps `session-*` directories whose PID is not live —
the same liveness test the lock file uses, and the same reason: a `SIGKILL` leaves the file
behind, and the next launch is the only thing that will ever come looking. A sweep that finds a
live PID leaves it alone; that case is the second-instance refusal above, and deleting another
process's kubeconfig mid-Apply would break a running install to tidy up.

There is no reconnect path to regenerate for — the connection is single-assignment for the
process, and switching clusters means restarting the binary, which produces a new PID and a new
directory.

**Four things in this binary independently decide which cluster they talk to.** Revision 1
addressed two of them and asserted the property held; it does not. All four must be pinned to
that file:

| Consumer | How it resolves today | What pins it |
|---|---|---|
| client-go clientset — observer, prove, teardown | `rest.InClusterConfig()` | The `rest.Config` built from the selected context (§2 above). |
| `deploy.sh` → `install.sh` → helm/kubectl | ServiceAccount token in-pod | `KUBECONFIG` and `KUBECONFIG_FLAG` in `applier.env()`. |
| **AICR's client, `CollectSnapshot`** | **its own resolution — `KUBECONFIG`, else `~/.kube/config`, else in-cluster** | **`AgentConfig.Kubeconfig`**, threaded from a new `DiscoverConfig.Kubeconfig`. |
| **`steps.helmLister`, the pre-Apply ownership snapshot** | **inherits `os.Environ()`; sets no `Env` at all** | **`KUBECONFIG` in the aicrme process itself.** |

The third is the dangerous one. AICR resolves its own kubeconfig path
(`pkg/k8s/client/client.go`), and `AgentConfig.Kubeconfig` is documented as "the path (or empty
for in-cluster)" (`pkg/client/v1/aicr.go:1395`, field at `types.go:124`). `DiscoverConfig` does
not set it today, and empty is exactly right in a pod. Locally, empty means Discover snapshots
whatever `~/.kube/config` currently points at while Apply installs into the selected context —
a silent split-brain that produces a recipe for one cluster and installs it into another.

The fourth is why the pin cannot live in `applier.env()` alone: `helmLister.List` builds an
`applier.Spec` with `Argv` and no `Env` (`internal/steps/ownership.go:149`), inheriting the
parent's environment. Its result is what `Run.Ownership` is built from, so a lister pointed at
the wrong cluster makes Reset's ownership record wrong.

**Therefore connect sets `KUBECONFIG` in the aicrme process itself**, once, before any cluster
work begins — which covers consumers two, three and four by the mechanism each already uses —
**and additionally sets `AgentConfig.Kubeconfig` explicitly**, because that seam has a real field
and relying on ambient environment for the one call that decides the recipe is not worth the
economy. Process-global mutation is ugly; it is also precisely how these libraries expect to be
told, and the alternative is threading a path through four call chains to reach code that will
read the environment variable anyway.

`KUBECONFIG_FLAG` is the variable `deploy.sh` already consumes (exported empty today,
`internal/applier/applier.go:122`).

**Why a written file rather than passing `--context`:** it removes the question of whether every
tool in the chain supports a context flag and spells it the same way (`helm --kube-context`,
`kubectl --context`), and it makes the run immune to the operator running
`kubectl config use-context` mid-Apply — which, with an ambient kubeconfig, would silently
redirect an in-flight install at the next `helm` invocation. The in-cluster console got this
property for free from its ServiceAccount. This is how a local binary keeps it.

**Exec credential plugins survive minification.** `gke-gcloud-auth-plugin`, `aws eks
get-token` and OIDC refresh are preserved as `exec` stanzas, and the child process inherits
`PATH` and the operator's cloud environment. The consequence belongs in the README: the binary
acts against the cluster with whatever credentials the operator holds, and holds them for as long
as it runs.

### The namespace must be created — by Discover, not by Connect

`steps.NewDiscover` passes `Namespace` to AICR's snapshot Job but does not create it
(`internal/steps/discover.go:160`). In-cluster, `helm --create-namespace` did. Locally nothing
does, so Discover would fail on a fresh cluster.

**Discover creates it, at the start of its own `Run`.** Revision 2 put this in Connect, which
made a probe that reports a server version and a node count also write to the cluster — an
operator clicking through contexts to see which one they are pointed at would leave a namespace
behind on every cluster they looked at, including ones they never installed to.

Point-of-use is also what the cited precedent actually does: `prove.Client.EnsureNamespace`
(`internal/prove/client.go:87`) creates Prove's namespace when Prove runs, not when the process
starts. Revision 2 cited it as a pattern for Connect and read it backwards.

**Whether it pre-existed is recorded on the run, and this needs new state.** The existing
`Ownership.Namespaces[].Existed` field cannot carry it: `recipeNamespaces` builds that set from
recipe.json's components (`internal/steps/ownership.go:183`), and `DiscoverConfig.Namespace` is
aicrme's own agent namespace, which is not one of them. So Discover records its own
create-or-found result, with the namespace's UID, on the run record.

**It is reported, not reclaimed.** AICR's agent deployer already cleans up the Job, the
ServiceAccount and the RoleBinding it created (`DiscoverConfig.Cleanup` is always true); the
namespace is what remains. If Discover created it, Reset names it in the residue as an orphan
with the command to remove it. Adding teardown code to chase it would put aicrme in the business
of undoing a deployer's work, which is the line this repo has already drawn: the deployer owns
its own cleanup, and aicrme prints what is left rather than reaching for it.

---

## 3. Auth becomes a loopback token

**Deleted:** `internal/api/auth.go` and its two test files, the
`Config.{Username,Password,SessionTTL,LoginRate,TLS}` fields and their validation in
`api.New` (`internal/api/server.go:54`), the `AICRME_USERNAME`/`AICRME_PASSWORD`/`AICRME_TLS`
environment surface, `POST /api/login`, `POST /api/logout`, and
`web/src/components/Login.tsx`.

**`internal/api/jar_test.go` stays.** Revision 2 deleted it on the reasoning that it exists to
drive the session cookie and the session cookie was going. The cookie is not going — see below —
so the jar is still how the package's tests hold one. It also defines `newRecorder()`, used
elsewhere in the package's tests, which was already a reason it could not simply be dropped.

**Replaced by:**

- Bind loopback only. A `--addr` that resolves to a non-loopback host is refused, not warned
  about: the token below is a launch secret meant for one browser on one machine, and it is not
  a credential to put on a network.
- A 32-byte random token generated per launch, delivered by opening
  `http://127.0.0.1:<port>/?t=<token>`. The SPA reads it from the query string, drops it from the
  visible URL, and posts it once to `POST /api/session` — which sets it as a `HttpOnly`,
  `SameSite=Strict`, `Secure`-exempt-on-loopback session cookie scoped to the process. Every
  later request, including the event stream, authenticates by that cookie. Middleware compares
  with `crypto/subtle` in both places.
- The existing same-origin wrapper (`internal/api/server.go:152`) **stays**, and so does
  `csrf_test.go`, which covers it via `Sec-Fetch-Site`. There is no `csrf.go` — that check has
  always lived in `server.go`, and it is exactly the anti-DNS-rebinding guard a loopback server
  needs. With the cookie below it is load-bearing rather than defense in depth: it is what stops
  a cross-origin page from riding the cookie on a mutating request. `loggedInClient` becomes a
  tokenized one; the assertions do not move.
- `GET /api/session` **stays** as the 204 liveness probe. Its original job — telling an expired
  session from a network blip, because `EventSource` surfaces no HTTP status — narrows but does
  not vanish: the cookie no longer expires, but the process it belongs to can exit, and the SPA
  still needs to tell "server gone" from "reconnecting." `POST` to the same path establishes the
  session; `GET` probes it.

### Why a cookie and not a header

A header-borne token was revision 2's design, and it does not work here. Two things break:

**`GET /api/events` cannot carry it.** The SPA's timeline is a native `EventSource`
(`web/src/useEvents.ts`), and `EventSource` has no API for request headers. This is not a new
discovery — `internal/api/server.go:160` already documents it, as the reason safe methods are
exempt from the same-origin check, and `GET /api/session` exists at all
(`internal/api/server.go:132`) only because "EventSource surfaces no HTTP status on error."
Revision 2 deleted the cookie, kept the stream, and declared the SPA unchanged. The stream would
have 401'd on first connect.

**A refresh loses the token.** Held in memory and stripped from the URL, the token does not
survive `F5` or a restored tab — the operator would be dropped to a dead screen mid-Apply with
the only copy of the token in a terminal they may have scrolled past. A cookie survives both.

The alternatives were considered and are worse. Putting the token in the `EventSource` URL as a
query parameter leaks a live credential into browser history, the referrer on any outbound link,
and this repo's own request logging. Replacing `EventSource` with a `fetch`-plus-`ReadableStream`
reader would let a header work, but it means hand-rolling the reconnect, `Last-Event-ID` replay,
and gap-detection logic that `useEvents.ts` already implements and tests
(`useEvents.lifecycle.test.tsx` covers reconnect, backoff and the `MAX_GAP_RECONNECT_ATTEMPTS`
cap) — a large rewrite of working, tested code to avoid a cookie.

**The cookie is not what the old auth was.** There is no password, no login form, no 8-hour
session TTL, no rate limiter, and no `Config.SessionTTL` — the cookie carries the launch token,
lives as long as the process, and dies with it. `internal/api/auth.go` still goes; what survives
is one `POST /api/session` handler and one comparison. The same-origin wrapper is what keeps the
cookie from being usable cross-origin.

**Why this is not simply "no auth":** a server on `127.0.0.1` is reachable by every process on
the machine and, via DNS rebinding, by any page the operator browses. A launch token a
cross-origin page cannot read, plus the origin check, is the Jupyter pattern and is the right
size for the threat. It is roughly fifty lines against the several hundred being deleted.

**What the README loses:** most of the Security section. `cluster-admin` stops being something
the product grants itself via a `ClusterRoleBinding` and becomes "it has exactly what your
kubeconfig has" — which needs no justification because it is not a grant. The remaining
disclosure is AICR's privileged snapshot agent, which is existing AICR behavior.

---

## 4. Run state moves to a file

A new `engine.NewFileStore(dir)` implements the **existing `Store` interface**
(`internal/engine/store.go:16`) using the **existing envelope** (`internal/engine/envelope.go`).
`Recover`, `ReconcileWorkloads`, and their tests are untouched — which is the point of doing it
this way rather than inventing a persistence model.

- Writes are temp-file-plus-`os.Rename` within the same directory, mode `0600`.
- `internal/engine/cmstore.go` and `cmstore_test.go` are deleted, along with `newRunStore`,
  `deploymentLookupTimeout`, `runStoreSuffix`, and the `AICRME_DEPLOYMENT_NAME` environment
  variable. `resolveDeploymentOwner` goes with them, but **what it guarded does not** — see §2's
  work-directory lock, and the note in §6.
- The file store runs against the same contract tests as `memoryStore` in `store_test.go`.

### Recovery is keyed by cluster identity

The run directory is `<workdir>/runs/<uid>/`, where `<uid>` is §2's `kube-system` UID.

Revision 2 keyed this on a hash of the server URL and the context name. Neither is stable: a
context is a label in a file the operator edits, and a server URL is an address that can be
re-pointed at a rebuilt cluster — `kind delete cluster && kind create cluster` yields the same
endpoint and a different cluster, which is not a corner case for a demo tool. The UID changes
when the cluster does, which is exactly the property the key needs.

The ConfigMap got this for free by living inside the cluster it described. A flat local file does
not: an operator who demos cluster A, then connects to cluster B, would have B's console recover
A's run and offer a Reset that uninstalls releases in the wrong place. Keying by identity is what
prevents that, and it is a new requirement created by this move.

The UID is also stored *inside* the record, and revalidated before recovery and before Reset per
§2 — the directory name says which cluster a record was filed under, and the field says which
cluster it describes. They should never disagree; if they do, the record is refused rather than
reconciled.

### `Recover` and `ReconcileWorkloads` stay, and now run after connect

The durability question in §Why was "must a run survive the operator disconnecting," and the
answer was no. That is not the same as "no recovery is useful." The binary can still be
`Ctrl-C`'d or crash, and `ReconcileWorkloads` is what adopts a Prove workload with no surviving
record so the operator gets a Stop button instead of a cluster only `kubectl` can clean up. That
workload holds GPUs. Locally this matters more, not less, because there is no pod restart to
trigger reconciliation on the operator's behalf.

**The ordering is new and has to be stated.** In-cluster, the pod restarting *was* the trigger:
`Recover` ran during startup, before the API served anything, because the store was reachable
from the moment the process began. Locally it is not — the store lives under a directory named
for a cluster the process has not yet chosen. So `Recover` and `ReconcileWorkloads` move
**into the connect path**, after identity is established and the run directory is resolved, and
before `POST /api/connect` returns. A connect that recovers a run reports that in its response,
which is what puts the SPA into the recovered state rather than an empty one.

This makes connect the only place recovery can happen, which is a further reason the connection
is single-assignment: a reconnect would have to re-run recovery against a different directory
while the engine still holds the previous run.

**What "re-run rather than resumed" does and does not mean.** §Why's phrase is about the
*process*: no daemon, no resumable session, and a run whose binary died is not picked up where it
left off. It is not a claim that the record is discarded, and the three interrupted states keep
the semantics they already have — all three are existing, tested behavior that this change
inherits rather than redefines:

| Interrupted at | On the next connect | Where it lives today |
|---|---|---|
| Apply | Rewound to `PhaseBundle` and landed `StateFailed`; the operator retries from the bundle. | `recover.go:215`, `TestRecoverRewindsInterruptedRunAtApply` |
| Prove | Run lands `StateFailed`; the orphaned workload is adopted so Stop is offered. | `reconcile.go:95` |
| Reset | `StateResetting` lands `StateFailed` with `Residue.Incomplete` set, and Start, Retry and Discard are all withheld until another Reset establishes what is actually there. | `recover.go:260-269`, `TestRecoverTreatsAnInterruptedTeardownAsIncomplete` |

The partial-Reset row is the one that most needs the identity check in §2 in front of it: a
record that says "a teardown was in flight and I do not know what survived" is a record whose
release names get acted on by the next Reset, and acting on them in the wrong cluster is the
worst outcome this design can produce.

### Prove's workload can outlive the spec that produced it

**This is the one step behavior this design changes**, against §Scope's claim that none are. It
is in scope because the restructure turns a rare failure into the ordinary one, and because the
Prove row above depends on the mechanism that carries the bug.

Phase 4 hit it on real hardware. `docs/phase-4-status.md` records it as "Prove **adopted the
stale Job** from the previous attempt … That is the Phase 3 adoption rule working as designed."
**That attribution is wrong, and the distinction decides the fix.** `ReconcileWorkloads`'
adoption rule (`internal/engine/reconcile.go:95`) governs orphans found at startup, never
deletes anything, and is correct as written. What actually happened is in `prove.Client.Apply`:

```go
_, err = c.kube.BatchV1().Jobs(Namespace).Create(ctx, &job, metav1.CreateOptions{})
if err != nil && !apierrors.IsAlreadyExists(err) {   // internal/prove/client.go:122
```

`WorkloadName(runID)` is `"prove-" + runID` (`manifest.go:37`), so a retried Apply for the same
run creates the same name, gets `AlreadyExists`, and reports success — leaving whatever is
already there. On the H100 cluster that was a Job carrying the *pre-fix* tolerations, so the fix
the operator had just deployed could not reach the run they applied it to.

**Apply's stated premise stopped being true, and its comment still asserts it.** The comment
justifies the swallow with "Render's output for a given run never changes." That held when the
manifest was fully baked into the binary. It stopped holding when Phase 4 added
`c.extraTolerations`, appended after decode (`client.go:120-121`) from `AICRME_GPU_TOLERATIONS`
— **process configuration, not run state.** Two Applies of the same run ID, from two processes
configured differently, now legitimately render different Jobs, and the second one is discarded
without a word.

**The local binary makes this the normal path rather than an edge case.** Two reasons:

- *The window opens more often.* `proveStep.cleanup` (`internal/steps/prove.go:282`) deletes and
  confirms absence on every Apply and gang-placement failure, so a stale Job survives only when
  neither cleanup nor Stop ran at all — the process died, or the cluster went away. In-cluster
  that meant a crash. Locally it means `Ctrl-C`, which §4 has already established is an ordinary
  way to end a session.
- *The fix loop runs straight through it.* In-cluster, changing tolerations meant
  `kubectl set env deploy/aicrme` and a new pod. Locally it is: stop the binary, correct the
  environment, start it, connect, press Retry. That sequence — reconfigure, restart, retry the
  same run — is precisely the one that reproduces Phase 4's failure, and the restructure makes
  it the expected way to recover from a placement problem.

#### Apply recreates on spec drift, and only on spec drift

`prove.Client.Apply` stamps the rendered workload with a hash annotation and compares before
deciding:

- **No Job present** — create. Unchanged.
- **Present, hash matches** — success, no write. This is the genuine idempotence the current
  comment describes, and it is worth keeping: a retried Apply against a healthy running gang
  must not disturb it.
- **Present, hash differs or is absent** — `EnsureAbsent` (foreground delete plus `WaitAbsent`,
  `internal/prove/client.go`), then create. An absent annotation means a Job from a binary that
  predates this change, which is the exact Phase 4 shape, so it recreates rather than trusting
  it.

The hash covers the Job as *this process would create it* — `Render`'s output decoded, with
`extraTolerations` already appended — not the live object read back. Server-side defaulting
fills a `PodSpec` with dozens of fields the client never set, so comparing against a retrieved
Job would report drift on every call and turn "recreate on drift" into "always recreate."

Recreation reuses `EnsureAbsent` rather than a bare `Delete` for the reason that method's own
comment gives: foreground deletion only guarantees the API server has *started* cascading, and a
new gang placed against pods still dying is a scheduling failure that reads as a placement bug.

**This does not loosen the never-delete rule.** Apply runs only inside `proveStep.Run`, on a run
this process is executing, against a name derived from that run's own ID. A workload adopted
from an earlier session lands `StateActive`, where `workloadAdoptedMsg` already tells the
operator Stop is the only way to end it — Apply is not reachable on that path, and nothing here
makes it so.

### The envelope's size ceiling is a ConfigMap artifact

`maxPayload = 800 << 10` exists because "Kubernetes caps a ConfigMap at roughly 1MiB"
(`internal/engine/envelope.go:20`), and `maxDecompressed = 8 << 20` because "the pod runs under a
512Mi cap." Exceeding `maxPayload` sheds artifacts largest-first — a degradation the comment
records as having once made large clusters unusable.

Neither limit describes a local file. `maxPayload` becomes a field the store supplies rather than
a package constant, and the file store supplies a substantially larger value, so artifact
shedding stops being reachable in normal use. `maxDecompressed` stays bounded — it guards against
a malformed record inflating without limit, which is a property of the decoder, not of where the
bytes were stored — but is raised to match.

This is scoped deliberately: the mechanism stays, its trigger point moves. Removing shedding
entirely is not part of this change.

---

## 5. Work directory and prerequisites

`AICRME_WORK_DIR`, else `~/.aicrme`. `defaultWorkDir = "/var/lib/aicrme"` goes.

`workSubdirs` (`cmd/aicrme/main.go:100`) drops `home`, `helm/cache`, `helm/config`, `helm/data`
and `kube/cache`. Those exist only because `readOnlyRootFilesystem: true` left an `emptyDir` as
the container's one writable path. Locally, redirecting them would be actively wrong: the
operator's real `helm` configuration may hold private chart repository credentials and registry
auth that the install needs. What remains is `tmp`, `runs`, and `bundles`, plus the two things
§2 adds: a `lock` file and a per-launch `session-<pid>/` directory holding the frozen kubeconfig.
`runs/` gains one level — `runs/<kube-system-uid>/` — per §4.

### Toolchain preflight

At startup, resolve **`bash`, `jq`, `helm` and `kubectl`** on `PATH` and read each version.

Revision 2 checked two of the four. The deleted image supplied all of them —
`apk add --no-cache bash ca-certificates curl jq tar` at `Dockerfile:44` — and the comment
directly above it names why: "the console shells out to the bundle's `deploy.sh`, which needs
bash, helm, kubectl, and jq (the webhook preflight degrades without jq)."

- **`bash` is not optional and not `sh`.** `applier.Apply` builds
  `Argv: []string{"bash", "deploy.sh", ...}` (`internal/applier/applier.go:83`) — an explicit
  interpreter, not a shebang, so a machine without `bash` on `PATH` fails at `exec` with a
  message about a missing file rather than a missing shell. Relevant beyond the pedantic case:
  `deploy.sh` is AICR-generated and this repo does not control whether it stays POSIX-clean.
- **`jq` degrades rather than fails**, per the Dockerfile's own note, so a missing `jq` is a
  warning naming what degrades — not a refusal.
- `curl` and `tar` were build-time only: the Dockerfile fetches helm and kubectl with them and
  the same `RUN` removes them. Neither is a runtime dependency and neither is preflighted.
- CA certificates are a host property, not a `PATH` lookup. A machine with no trust store fails
  at the first HTTPS call with a clear TLS error, and preflighting it would mean guessing at
  platform-specific paths for a case the error already explains.

**Missing is fatal for `bash`, `helm` and `kubectl`; missing `jq` is a warning. Minor version
skew is a warning. Every resolved version is recorded in the run record and surfaced in the
evidence bundle.**

Refusing to start because an operator has helm 3.20 rather than the 3.19.0 the deleted Dockerfile
pinned would make the tool unusable for the reproducibility it was meant to protect. For a
product whose output is evidence, the honest way to serve "correctness must be reproducible" is
to *record* the toolchain that produced the result, not to block on it. A run's evidence should
be able to answer "which helm installed this," and today — where the version is baked into an
image — nothing ever asks.

### Helm 4 — revision 2 was wrong, and it is already handled

**Revision 2 called this "the single largest piece of real work created by deleting the
Dockerfile." It is zero work.** Revision 5 withdraws the claim.

Revision 2 asserted that `steps.helmLister.List` runs `helm list --all` and would break under
helm 4. It does not run `--all`, and has not since commit `e36b015`, *"fix(steps): list helm
releases in a way both helm majors accept"* — which predates this spec. The current argv is
explicit status flags (`internal/steps/ownership.go:151-153`):

```go
"helm", "list", "--namespace", namespace,
"--deployed", "--failed", "--pending", "--superseded", "--uninstalled", "--uninstalling",
"--short",
```

The comment above it does discuss `--all` and helm 4 at length, which is what revision 2 read.
It is explaining **why the code avoids `--all`**, not describing what the code does. Reading a
comment as a description of present behavior when it is a rationale for past behavior is the
error here, and it is worth naming because the same comment is still in the file and will read
the same way to the next person.

**What remains true:** the Dockerfile's helm 3.19.0 pin does disappear, and version selection
does move to the operator. What that now costs is bounded and different:

- The two version-sensitive helm surfaces are `helm list`'s status flags above and
  `helm uninstall --ignore-not-found --wait --timeout` (`internal/teardown/teardown.go:132-139`).
  Both are believed valid in helm 3 and 4. **That is reasoning, not a tested claim** — no run
  against a helm 4 host has exercised the teardown path.
- So preflight **records** the helm version, per the policy above, and does not fail closed on
  major 4. Blocking on a major that the code was deliberately made compatible with would be
  the reproducibility-over-usability trade this section already rejects.
- The one concrete obligation this creates is a test: exercise `helmLister.List` and
  `uninstallArgv` against a helm 4 binary before the first release. §8 carries it.

`test/e2e/apply-dryrun.sh:134-148` asserts the in-image helm major matches the Dockerfile's
`HELM_VERSION`. That block has no meaning without an image and is deleted rather than ported —
see §8, which revision 2 also overstated.

---

## 6. Deletions

| Path | Reason |
|---|---|
| `charts/aicrme/` (10 files) | The delivery model being replaced. |
| `test/chart/contract.sh`, `make test-chart` | Pins Go constants against chart probe windows that no longer exist. |
| `Dockerfile`, `.dockerignore`, `make image` | No image. It carried the helm 3.19.0 pin, but nothing in this repo depends on helm 3 any more — §5 withdraws revision 2's claim that it does. What goes with it is `apply-dryrun.sh`'s in-image version assertion (§8). |
| `internal/api/auth.go`, `auth_test.go`, `auth_internal_test.go` | §3. `csrf_test.go` and `jar_test.go` are both **kept** — the same-origin check survives, and so does the cookie the jar exists to hold. |
| `internal/engine/cmstore.go`, `cmstore_test.go` | §4. **But it carried `resolveDeploymentOwner`, the only multi-writer guard this repo has ever had — see below.** Deleting it is not purely subtractive. |
| `web/src/components/Login.tsx` | §3. |
| `scripts/demo-remote.sh` | Its whole job is `helm upgrade --install` of the chart onto a remote cluster. Replaced by "point kubectl at the cluster and run the binary." |

Also removed from `cmd/aicrme/main.go`: the probe-window arithmetic in the observer-goroutine
comment, the `terminationGracePeriodSeconds` reasoning on `runShutdownTimeout`, and the "PID 1
under the image's ENTRYPOINT" rationale in the shutdown block. The shutdown *sequence* stays —
draining before cancelling, and reaping the `deploy.sh` process tree before returning, are still
correct — but the numbers must be re-justified against a local process rather than against a
chart that no longer exists.

`applier/exec.go` needs no change: `Setpgid: true` already puts `deploy.sh` in its own process
group, so a `Ctrl-C` delivered to the terminal's foreground group does not reach `helm`
directly. The existing `Drain` → `CancelAndWait` path remains the only thing that stops a run.

### One deletion carries a guard with it

`cmstore.resolveDeploymentOwner` resolved the console Deployment's `ownerReference` and stamped
it on the run ConfigMap, so a record written by a *different* deployment was recognized as
foreign and degraded rather than silently overwritten —
`TestRecoverDegradesAgainstAForeignOwnedRecord` is that behavior. It is the only multi-writer
protection in the repo today; `grep -rn "flock\|LOCK_EX\|singleflight\|lockfile" internal/`
returns nothing.

Deleting `cmstore.go` deletes it, and the local model makes the case it defended *more* likely,
not less: an operator can start a second `aicrme` from a second terminal in one keystroke,
whereas starting a second console Deployment took a deliberate `helm install` under a new
release name. §2's work-directory lock is the replacement, and it must land in the same change
as this deletion rather than after it.

Note also that the shutdown path acquires two new obligations from §2 — releasing
`<workdir>/lock` and removing `<workdir>/session-<pid>/` — and both must survive the same
`Ctrl-C` the drain sequence already handles. Neither can be a bare `defer` in `main()`: the
signal handler is what runs first.

---

## 7. SPA

`App.tsx:21` gates the console on `authed` and renders `<Login>` otherwise. That becomes a
`<Connect>` screen: list contexts, let the operator pick one, show the resolved server URL, and
connect. On success it reports server version, node count, the `kube-system` UID from §2, and the
resolved `bash`/`jq`/`helm`/`kubectl` versions from §5 — which is also the operator's
confirmation that they are about to install into the cluster they think they are.

**Bootstrap gains one step before any of that.** On load, `App.tsx` reads `?t=` from the URL,
`POST`s it to `/api/session`, and strips it from the visible URL with `history.replaceState`. If
there is no `?t=` — a refresh, a restored tab, a URL the operator retyped — it skips straight to
probing `GET /api/session`, because the cookie from the original load is still there. That is the
case revision 2's in-memory token could not serve.

`useEvents.ts` is **unchanged**, and that is the point of §3's cookie: the `EventSource`
constructor sends cookies to a same-origin URL with no configuration, so the reconnect,
`Last-Event-ID` replay and gap-detection logic — and
`useEvents.lifecycle.test.tsx`'s coverage of all three — carry over untouched.

Every other component — `Wizard`, `Discover`, `Recommend`, `Cockpit`, `Prove`, `Reset`,
`Timeline`, `ComponentConditions` — is unchanged. The arc the product performs is not what this
design is changing.

The API client gains a 409 handler that returns the operator to the Connect screen, replacing the
existing 401-to-Login path. A recovered run in the connect response lands the SPA in the same
recovered state the pod-restart path used to produce (§4).

---

## 8. Testing

- **`FileStore`** runs against the same contract tests `memoryStore` does in `store_test.go`.
  Additional cases for the atomic-rename path and for identity-keyed directory isolation.
- **`internal/console`** becomes testable for the first time: `Run` against a fake clientset and
  a temp work dir, asserting the connect gate, the written kubeconfig, and that connect creates
  no namespace.
- **The corrections in revision 3 each get a test**, because every one of them is a case that
  looked handled and was not:
  - Concurrent `POST /api/connect` — the second returns 409, and exactly one clientset and one
    `KUBECONFIG` value result. Run under `-race`.
  - A second process against a locked work directory refuses; a lock whose PID is dead reports
    the stale path rather than clearing it.
  - Recovery against a changed `kube-system` UID at the same server URL is refused, and Reset
    against that record is refused, with both UIDs in the message. This is the
    rebuilt-`kind`-cluster case and is cheap to construct in e2e.
  - The session cookie reaches `GET /api/events`: an `EventSource`-shaped request carrying only
    the cookie streams, and one carrying nothing gets 401. `csrf_test.go`'s existing
    `TestEventsUnaffectedByCSRFMiddleware` is the shape to follow.
  - A reload with no `?t=` in the URL still authenticates — the regression revision 2's design
    would have shipped.
  - `session-<pid>/` is gone after clean shutdown, and a stale one from a dead PID is swept at
    startup while a live one is left alone.
  - Preflight fails closed with `bash` absent from `PATH`, and warns — does not fail — with `jq`
    absent.
- **Prove's recreate-on-drift (§4)** is testable against the fake clientset the package already
  uses, and each case is a distinct assertion:
  - Apply twice with identical configuration issues exactly one `Create` and no `Delete` — the
    idempotence that must not regress.
  - Apply, then Apply again from a client built with different `extraTolerations`, deletes and
    recreates, and the resulting Job carries the *new* tolerations. This is Phase 4's failure
    written as a test, and it fails against today's code.
  - A Job with no hash annotation is recreated — the older-binary case.
  - Recreation waits for absence before creating: a fake whose delete is not immediately
    visible must not produce a `Create` until `WaitAbsent` returns.
  - The hash is computed from the rendered-and-mutated Job, not the retrieved one, so a Job
    read back with server-defaulted `PodSpec` fields still compares equal. Without this, the
    second bullet passes for the wrong reason and the first one silently breaks.
- **e2e gets simpler, but not for free.** `test/e2e/*.sh` currently install the chart into KWOK
  and wait on a rollout. They become: start the binary on the CI host against the KWOK cluster,
  drive the API. No image build, no `kind load`, no rollout wait. `apply-dryrun.sh`,
  `apply-real.sh`, `discover-recommend.sh`, `prove.sh`, `reset.sh` and `smoke.sh` keep their
  *subject-matter* assertions and change how the console is started — with two exceptions
  revision 2 missed by claiming they change "only" that:
  - `apply-dryrun.sh:134-148` asserts the in-image helm major against the Dockerfile's
    `HELM_VERSION`, reading both through `kubectl exec deploy/aicrme`. There is no image, no
    Deployment and no Dockerfile to compare against; the block is deleted. The property it
    protected — knowing which helm produced a result — is served better by §5's recorded
    versions, which travel in the evidence bundle instead of in a CI log.
  - Every script reaches the console through `kubectl -n aicrme exec deploy/aicrme` or a
    `Service`. All of that becomes a local address, and each script needs the launch token from
    §3 to call the API at all. A shared helper that starts the binary, captures the printed
    tokenized URL, and exports a curl-usable cookie jar is the one new piece of e2e
    infrastructure this change requires.
- **A helm 4 host must be exercised before the first release.** §5 withdraws revision 2's claim
  that `helm list --all` breaks — the code already avoids it — but `helm list`'s status flags and
  `helm uninstall --ignore-not-found --wait --timeout` are compatible by reasoning and have never
  run against helm 4 in this repo. One CI job on a helm 4 host covering `helmLister.List` and a
  Reset is what converts that into evidence.
- **`make qualify`** drops `test-chart`; everything else holds, including the 80% coverage floor
  and `check-aicr-pin`.
- **`make demo` / `demo-down` / `demo-status`** are reworked: the Kind + KWOK cluster setup stays,
  the chart install and password-printing steps go, and `demo` ends by running the binary.

### Phase 4's evidence does not transfer

Apply hit 16/16 on real H100s with the in-cluster console. That result is about the recipe and
the bundle, and it stands — but it says nothing about whether a laptop-driven install over a
VPN behaves the same. **The local path needs its own real-cluster run before it is treated as
proven.** This is the one place where the restructure spends evidence rather than earning it,
and it should be scheduled deliberately rather than discovered later.

---

## 9. Deferred

- **Release automation.** goreleaser (darwin/linux × amd64/arm64, matching AICR's own release
  assets), Homebrew tap, `curl -sfL https://get.aicrme.run | bash`, SBOMs and sigstore
  attestations. Land the code first; `make build` is the gate for this change.
- **Windows.** Not shipped. AICR ships darwin and linux only, and WSL covers the case.
- **`aicr server`.** §1 preserves the seam. Whether it subsumes or sits beside AICR's existing
  `aicrd` REST daemon is a question for that work, not this one.

---

## Rejected alternatives

**Keep the chart alongside the binary.** Every line this design deletes would have to stay, and
the test matrix would double. It is also self-defeating: an operator who cannot run a local
binary cannot run `helm install` from their laptop either, so the second mode serves no one.

**Land the kubeconfig path first, delete the chart later.** Lower risk, and it would have proven
the local path on a real cluster before spending the deletions. Rejected on the call that the
deletions are the value and a staged version carries both modes' costs in the interim. The
consequence is §8's last subsection: the real-cluster run becomes a scheduled obligation rather
than a gate that was already passed.

**Vendor helm as a Go library.** Removes the `PATH` dependency and pins the version exactly. The
helm SDK is a heavy dependency, `deploy.sh` is AICR-generated and shells out regardless, and the
audience already has helm installed. §5's record-don't-block policy addresses the reproducibility
concern at a fraction of the cost.

**Pass `--context` to helm and kubectl instead of writing a kubeconfig.** Requires every tool in
a generated script to accept and correctly spell a context flag, and leaves an in-flight install
exposed to an ambient `kubectl config use-context`. §2's written file is strictly safer for
comparable effort.

**A header-borne token instead of a cookie (revision 2's design).** `EventSource` cannot set
request headers, so the timeline would not authenticate; keeping it would mean replacing
`useEvents.ts` with a hand-rolled `fetch`/`ReadableStream` reader and reimplementing its
reconnect, replay and gap handling. A header token also does not survive a page refresh. See §3.

**Keying the run directory on server URL plus context name (revision 2's design).** Both are
mutable and neither identifies a cluster; `kind delete && kind create` reuses the endpoint. §4
keys on the `kube-system` UID.

**Ensuring the Discover namespace at connect (revision 2's design).** Makes a read-shaped probe
write to every cluster the operator inspects. §2 moves it to Discover, which is also what the
`prove.Client.EnsureNamespace` precedent actually does.

**Always recreating Prove's workload (§4).** Simplest to implement and simplest to reason about,
and wrong: a retried Apply against a healthy placed gang would tear it down and re-queue it, so
the cost of the fix lands on the case that was already working.

**Updating the existing Job in place instead of recreating.** Not available. A Job's
placement-defining fields — `completions`, `parallelism`, `selector`, and the whole pod template
— are immutable once created; `prove.Client.Apply`'s existing comment already says so, which is
why it never issues an Update today.

**Putting the spec hash in the workload name.** A changed spec would yield a different object and
need no comparison at all. Rejected because `WorkloadName(runID)`'s determinism is load-bearing
in six places — `matchesRun` and `adoptable` (`internal/engine/reconcile.go`), and `Delete`,
`WaitAbsent`, `EnsureAbsent` and Stop (`internal/prove/client.go`) — all of which reconstruct the
name from a run ID alone. Making the name depend on configuration would give the operator a Stop
button that deletes nothing, which is the outcome `adoptable`'s own comment exists to prevent.

---

## Resolved in revision 2

1. **`~/.aicrme` versus XDG — `~/.aicrme` stands.** AICR establishes no per-user config or state
   directory to match. Its only home-rooted paths are the standard `~/.kube/config` default
   (`pkg/k8s/client/client.go`) and `.claude/skills/…` for skill generation; it is otherwise a
   stateless CLI over files the caller names. There is no convention to diverge from, so this
   carries no rename risk at upstreaming time.
2. **`maxPayload` becomes a plain parameter, not a store field.** `encodeRun` has exactly one
   caller (`cmstore.go:129`) and `decodeRun` one (`cmstore.go:264`); both die with that file, and
   the file store becomes the sole caller of each. So the ceiling passes as an argument, with no
   store indirection and no constructor-parameter fallback needed.
3. **Browser-open failure — always print the URL.** The tokenized URL is written to stdout
   unconditionally, whether or not the open succeeds and whether or not `--open` was passed, so a
   headless or SSH session is never a dead end.

## Resolved in revision 3

4. **No in-session cluster switching.** Confirmed on review. The Connect screen is reachable only
   before a connection is established; switching clusters means restarting the binary. This is
   what makes the rest of revision 3 tractable — the four-consumer pin, the single-assignment
   connection state, the per-PID kubeconfig directory, and recovery running inside the connect
   path all depend on the connection being decided exactly once per process. The apparent tension
   with "let the operator change context before connecting" is not one: that reads as *before*,
   and it is the Connect screen's whole purpose.

## Resolved in revision 4

5. **Prove recreates on spec drift; it does not adopt its own stale workload.** Phase 4's open
   decision #1, answered in §4. The other two open decisions in `docs/phase-4-status.md` are
   dissolved rather than answered: #2 (does pinning the console to a GPU-free pool eliminate the
   churn) stops existing, because a process on the operator's machine is not schedulable and
   cannot be evicted by the install it is running; and #3 (does the in-cluster premise survive
   contact with what AICR does to a cluster) is the question this whole design answers in the
   negative.
6. **`docs/phase-4-status.md` needs a correction, and it is not cosmetic.** It attributes the
   stale-Job failure to "the Phase 3 adoption rule," which is `ReconcileWorkloads` — a different
   code path, one that never deletes and is correct as written. The actual carrier is Apply's
   `IsAlreadyExists` swallow. Left uncorrected, the next person to read that file would harden
   the wrong function. The same file's "Branch state" section is also stale: it says
   `phase-5-reset-shrink` is unmerged, and it merged on 2026-08-23 with ci and e2e green.

## Resolved in revision 5

7. **Helm 4 needs no code change.** `helmLister.List` already uses per-status flags rather than
   `--all` (`e36b015`). Preflight records the version and does not fail closed on major 4. The
   residue is a test obligation in §8, not an implementation task.
8. **Two e2e assertions do not survive the image**, contrary to §8's original wording. The
   in-image helm-major check is deleted; every script's console access needs a local-address and
   launch-token helper.

## Open questions

None outstanding. Revision 3 closed the last design question, revision 4 the last one carried in
from Phase 4, and revision 5 removed one that was never real.

**Settled:** Validate and evidence collection are **the next slice, not this one**. The original
framing for this work listed "validate the cluster and provide option for evidence collection,"
and Validate was previously cut on measured evidence that `ValidateState` false-passes on
simulated nodes. It gets its own design, after this lands, including its own decision about
whether that false-pass still holds on real hardware — one of the six unmeasured Phase 4
questions. Nothing in this spec changes as a result.

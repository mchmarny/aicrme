# Local binary — replacing the in-cluster console

**Date:** 2026-08-23
**Status:** Approved for planning
**Scope:** The delivery model only. `aicrme` stops being a Helm chart that deploys a console
into the target cluster and becomes a binary an operator runs on their own machine, which
serves the same SPA over loopback and drives the cluster through their kubeconfig. The arc —
Discover, Recommend, Bundle, Apply, Prove, Reset — is unchanged, and no step's behavior is
redesigned here.

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
  context, and returns the server version and node count on success.

Every other API route returns `409 Conflict` until a connection is established. This is a state
gate, not an auth gate; §3 covers auth.

### The connection is frozen for the process

On successful connect, a **flattened, minified, single-context kubeconfig** is written to
`<workdir>/kubeconfig` at mode `0600`, and both of these are added to `applier.env()`:

```
KUBECONFIG=<workdir>/kubeconfig
KUBECONFIG_FLAG=--kubeconfig <workdir>/kubeconfig
```

`KUBECONFIG_FLAG` is the variable `deploy.sh` already consumes (it is exported empty today,
`internal/applier/applier.go:122`); the exported `KUBECONFIG` covers anything `deploy.sh` invokes
without threading the flag through. Both, because either alone leaves a gap and neither costs
anything.

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

### The namespace must be created

`steps.NewDiscover` passes `Namespace` to AICR's snapshot Job but does not create it
(`internal/steps/discover.go:160`). In-cluster, `helm --create-namespace` did. Locally nothing
does, so Discover would fail on a fresh cluster.

Connect ensures the namespace exists, idempotently, following the pattern
`prove.Client.EnsureNamespace` already establishes (`internal/prove/client.go:87`).

---

## 3. Auth becomes a loopback token

**Deleted:** `internal/api/auth.go` and its two test files, the test-only cookie jar in
`internal/api/jar_test.go` (which exists to drive the session cookie), the
`Config.{Username,Password,SessionTTL,LoginRate,TLS}` fields and their validation in
`api.New` (`internal/api/server.go:54`), the `AICRME_USERNAME`/`AICRME_PASSWORD`/`AICRME_TLS`
environment surface, and `web/src/components/Login.tsx`.

`jar_test.go` also defines `newRecorder()`, used elsewhere in the package's tests; it moves
rather than dying with the jar.

**Replaced by:**

- Bind loopback only. A `--addr` that resolves to a non-loopback host is refused, not warned
  about: the token below is a launch secret meant for one browser on one machine, and it is not
  a credential to put on a network.
- A 32-byte random token generated per launch, delivered by opening
  `http://127.0.0.1:<port>/?t=<token>`. The SPA reads it from the query string, drops it from the
  visible URL, holds it in memory, and sends it as a request header. Middleware compares it with
  `crypto/subtle`.
- The existing same-origin wrapper (`internal/api/server.go:152`) **stays**, and so does
  `csrf_test.go`, which covers it via `Sec-Fetch-Site`. There is no `csrf.go` — that check has
  always lived in `server.go`, and it is exactly the anti-DNS-rebinding guard a loopback server
  needs. The test changes only where it obtains a client: `loggedInClient` becomes a tokenized
  one.

**Why this is not simply "no auth":** a server on `127.0.0.1` is reachable by every process on
the machine and, via DNS rebinding, by any page the operator browses. A header-borne token that
a cross-origin page cannot read, plus the origin check, is the Jupyter pattern and is the right
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
  `resolveDeploymentOwner`, `deploymentLookupTimeout`, `runStoreSuffix`, and the
  `AICRME_DEPLOYMENT_NAME` environment variable.
- The file store runs against the same contract tests as `memoryStore` in `store_test.go`.

### Recovery is keyed by cluster

The run directory is `<workdir>/runs/<hash>/`, where `<hash>` derives from the connected
cluster's server URL and context name.

The ConfigMap got this property for free by living inside the cluster it described. A flat local
file does not: an operator who demos cluster A, then connects to cluster B, would have B's
console recover A's run and offer a Reset that uninstalls releases in the wrong place. Keying by
cluster is what prevents that, and it is a new requirement created by this move.

### `Recover` and `ReconcileWorkloads` stay

The durability question in §Why was "must a run survive the operator disconnecting," and the
answer was no. That is not the same as "no recovery is useful." The binary can still be
`Ctrl-C`'d or crash, and `ReconcileWorkloads` is what adopts a Prove workload with no surviving
record so the operator gets a Stop button instead of a cluster only `kubectl` can clean up. That
workload holds GPUs. Locally this matters more, not less, because there is no pod restart to
trigger reconciliation on the operator's behalf.

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
auth that the install needs. What remains is `tmp`, `runs`, and `bundles`.

### Toolchain preflight

At startup, resolve `helm` and `kubectl` on `PATH` and read their versions.

**Missing is fatal. Skew is a warning. Both resolved versions are recorded in the run record and
surfaced in the evidence bundle.**

Refusing to start because an operator has helm 3.20 rather than the 3.19.0 the deleted Dockerfile
pinned would make the tool unusable for the reproducibility it was meant to protect. For a
product whose output is evidence, the honest way to serve "correctness must be reproducible" is
to *record* the toolchain that produced the result, not to block on it. A run's evidence should
be able to answer "which helm installed this," and today — where the version is baked into an
image — nothing ever asks.

---

## 6. Deletions

| Path | Reason |
|---|---|
| `charts/aicrme/` (10 files) | The delivery model being replaced. |
| `test/chart/contract.sh`, `make test-chart` | Pins Go constants against chart probe windows that no longer exist. |
| `Dockerfile`, `.dockerignore`, `make image` | No image. Confirmed: nothing else needs one. |
| `internal/api/auth.go`, `auth_test.go`, `auth_internal_test.go`, `jar_test.go` | §3. `csrf_test.go` is **kept** — it covers the same-origin check, which survives. |
| `internal/engine/cmstore.go`, `cmstore_test.go` | §4. |
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

---

## 7. SPA

`App.tsx:21` gates the console on `authed` and renders `<Login>` otherwise. That becomes a
`<Connect>` screen: list contexts, let the operator pick one, show the resolved server URL, and
connect. On success it reports server version, node count, and the resolved `helm`/`kubectl`
versions from §5 — which is also the operator's confirmation that they are about to install into
the cluster they think they are.

Every other component — `Wizard`, `Discover`, `Recommend`, `Cockpit`, `Prove`, `Reset`,
`Timeline`, `ComponentConditions` — is unchanged. The arc the product performs is not what this
design is changing.

The API client gains the token header from §3 and a 409 handler that returns the operator to the
Connect screen, replacing the existing 401-to-Login path.

---

## 8. Testing

- **`FileStore`** runs against the same contract tests `memoryStore` does in `store_test.go`.
  Additional cases for the atomic-rename path and for cluster-keyed directory isolation.
- **`internal/console`** becomes testable for the first time: `Run` against a fake clientset and
  a temp work dir, asserting the connect gate, the written kubeconfig, and namespace creation.
- **e2e gets simpler, not harder.** `test/e2e/*.sh` currently install the chart into KWOK and wait
  on a rollout. They become: start the binary on the CI host against the KWOK cluster, drive the
  API. No image build, no `kind load`, no rollout wait. `apply-dryrun.sh`, `apply-real.sh`,
  `discover-recommend.sh`, `prove.sh`, `reset.sh` and `smoke.sh` keep their assertions and change
  only how the console is started.
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

---

## Open questions

1. **`~/.aicrme` versus XDG.** This design picks `~/.aicrme` for symmetry with how most
   single-binary cluster tools behave. If AICR's CLI already establishes a config-directory
   convention, match that instead — worth checking before implementation rather than diverging
   from the project this may be donated to.
2. **`maxPayload` as a store field.** §4 asserts the ceiling belongs to the store. If `encodeRun`
   turns out to be called from a path with no store in hand, it becomes a constructor parameter
   on the envelope encoder instead. Resolvable during implementation; it does not change the
   design.
3. **Browser-open failure.** Headless CI and SSH sessions have no browser. `--no-open` covers it,
   but the default path should print the tokenized URL unconditionally so a failed open is never
   a dead end.

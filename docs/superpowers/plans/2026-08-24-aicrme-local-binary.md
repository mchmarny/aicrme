# Local Binary Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn `aicrme` from a Helm chart that installs a `cluster-admin` console into the target cluster into a binary the operator runs on their own machine, serving the same SPA over loopback and driving the cluster through their kubeconfig.

**Architecture:** All of `main()`'s wiring moves into `internal/console.Run(ctx, Options)`, which becomes the one testable entry point and the seam a future `aicr server` would fill. The cluster connection is chosen once per process through a Connect screen, frozen to a per-launch flattened kubeconfig, and pinned into all four things that independently resolve a cluster. Run state moves from a ConfigMap to a file keyed on the cluster's `kube-system` UID. Auth collapses from a password/session/TLS surface to a loopback launch token exchanged for a process-lifetime cookie.

**Tech Stack:** Go 1.26.5 (toolchain-pinned), `k8s.io/client-go` (`clientcmd` for kubeconfig loading, `kubernetes.Interface` for the typed client), stdlib `flag` and `net/http`, React + TypeScript + Vite for the SPA, Vitest for SPA tests, `bash`/`shellcheck` for e2e.

**Spec:** `docs/superpowers/specs/2026-08-23-aicrme-local-binary-design.md` (revision 5)

## Global Constraints

- **Toolchain:** run every Go command as `GOTOOLCHAIN=go1.26.5 <cmd>`. A local Go 1.27 breaks the pinned `golangci-lint`. This applies to `make qualify`, `make test`, and `make lint`.
- **Coverage floor:** `make test-coverage` enforces 80%. Every task that adds Go code adds tests in the same commit.
- **Full gate:** `make qualify` = `web lint lint-shell test-chart test-web test-coverage check-aicr-pin`. It must match CI exactly. `test-chart` is removed in Task 14 and not before.
- **Commits:** sign with `-S`. No `Co-Authored-By` lines. No sign-off (`-s`). Never mention Claude or Claude Code in a commit message or PR body.
- **Branch:** all work lands on `local-binary`. Merge to `main` locally and push straight there — no PR. Wait for the `e2e` workflow on `main` before calling a merge done.
- **Never disable a test.** If a test blocks a change, the test's premise is what changed; update it and say why in the commit.
- **Work directory layout after this plan:** `<workdir>/{tmp,runs/<kube-system-uid>,bundles,session-<pid>/kubeconfig,lock}`. Default `~/.aicrme`, overridden by `AICRME_WORK_DIR`.
- **Loopback only.** A `--addr` resolving to a non-loopback host is refused, not warned about.
- **Nothing is filed upstream.** Task 4 preserves the `aicr server` seam; it does not authorize a PR to NVIDIA/aicr. See `docs/superpowers/specs/…` §1.

---

## File Structure

**New:**

| Path | Responsibility |
|---|---|
| `internal/console/console.go` | `Options`, `Run` — all wiring moved out of `main()`. |
| `internal/console/connect.go` | Kubeconfig loading, context listing, connect state machine, cluster identity. |
| `internal/console/lock.go` | Work-directory lock file and stale-PID sweep. |
| `internal/console/session.go` | Per-launch `session-<pid>/` directory and the frozen kubeconfig write. |
| `internal/console/preflight.go` | `bash`/`jq`/`helm`/`kubectl` resolution and version capture. |
| `internal/engine/filestore.go` | `Store` over a directory, temp-file-plus-rename. |
| `internal/api/token.go` | Launch-token middleware and `POST /api/session`. |
| `web/src/components/Connect.tsx` | Context list, connect action, cluster and toolchain confirmation. |
| `test/e2e/lib/console.sh` | Starts the binary, captures the tokenized URL, exports a curl cookie jar. |

**Modified:**

| Path | Change |
|---|---|
| `cmd/aicrme/main.go` | Reduced to flag parsing, `slog` setup, signal wiring, one `console.Run` call. |
| `internal/prove/client.go` | `Apply` stamps a spec hash and recreates on drift. |
| `internal/engine/envelope.go` | `encodeRun`/`decodeRun` take the payload ceiling as a parameter. |
| `internal/api/server.go` | `Config` loses the auth fields, gains the token; routes gain the connect gate. |
| `internal/steps/discover.go` | `DiscoverConfig.Kubeconfig`; Discover creates its own namespace. |
| `internal/applier/applier.go` | `env()` exports `KUBECONFIG` and `KUBECONFIG_FLAG`. |
| `web/src/App.tsx`, `web/src/api.ts` | Session bootstrap, Connect gate, 409 handling. |
| `Makefile` | `test-chart` and `image` targets removed; `demo` runs the binary. |

**Deleted:** `charts/aicrme/` (10 files), `test/chart/contract.sh`, `Dockerfile`, `.dockerignore`, `internal/api/auth.go` + `auth_test.go` + `auth_internal_test.go`, `internal/engine/cmstore.go` + `cmstore_test.go`, `web/src/components/Login.tsx`, `scripts/demo-remote.sh`.

**Explicitly kept** (revision 3 corrected revision 2 on both): `internal/api/csrf_test.go` and `internal/api/jar_test.go`.

---

## Task 1: Prove recreates on spec drift

Independent of everything else in this plan. It fixes a defect that reached real hardware and it can merge to `main` on its own if the rest of the restructure stalls.

**Files:**
- Modify: `internal/prove/client.go:100-126` (`Apply`)
- Modify: `internal/prove/manifest.go` (add the annotation key)
- Test: `internal/prove/client_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `prove.SpecHashAnnotation` (string const), and `Client.Apply(ctx context.Context, runID string) error` with unchanged signature and changed behavior.

**Background the implementer needs:** `WorkloadName(runID)` is `"prove-" + runID` (`manifest.go:37`), so a retried Apply for the same run addresses the same object. Today `Apply` swallows `IsAlreadyExists` and reports success, which silently discards a re-render. That was safe while the manifest was fully baked into the binary; it stopped being safe when `extraTolerations` — read from `AICRME_GPU_TOLERATIONS`, i.e. process configuration — began being appended after decode at `client.go:120-121`.

- [ ] **Step 1: Write the failing test**

Add to `internal/prove/client_test.go`:

```go
func TestApplyRecreatesWhenTolerationsChanged(t *testing.T) {
	kube := fake.NewSimpleClientset()
	const runID = "abcdef0123456789"

	first := NewClient(kube)
	if err := first.Apply(context.Background(), runID); err != nil {
		t.Fatalf("first Apply() error = %v", err)
	}

	second := NewClient(kube, corev1.Toleration{
		Key: "dedicated", Operator: corev1.TolerationOpEqual,
		Value: "gpu-workload", Effect: corev1.TaintEffectNoSchedule,
	})
	if err := second.Apply(context.Background(), runID); err != nil {
		t.Fatalf("second Apply() error = %v", err)
	}

	job, err := kube.BatchV1().Jobs(Namespace).Get(context.Background(), WorkloadName(runID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	var found bool
	for _, tol := range job.Spec.Template.Spec.Tolerations {
		if tol.Key == "dedicated" && tol.Value == "gpu-workload" {
			found = true
		}
	}
	if !found {
		t.Error("the recreated workload does not carry the new toleration -- a fix deployed between two Applies could not reach the run")
	}
}

func TestApplyIsANoOpWhenNothingChanged(t *testing.T) {
	kube := fake.NewSimpleClientset()
	const runID = "abcdef0123456789"
	c := NewClient(kube)

	if err := c.Apply(context.Background(), runID); err != nil {
		t.Fatalf("first Apply() error = %v", err)
	}
	kube.ClearActions()
	if err := c.Apply(context.Background(), runID); err != nil {
		t.Fatalf("second Apply() error = %v", err)
	}

	for _, a := range kube.Actions() {
		if a.GetVerb() == "delete" || a.GetVerb() == "create" {
			t.Errorf("an unchanged Apply issued a %s -- it must not disturb a placed gang", a.GetVerb())
		}
	}
}

func TestApplyRecreatesAWorkloadWithNoHashAnnotation(t *testing.T) {
	const runID = "abcdef0123456789"
	kube := fake.NewSimpleClientset(&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: WorkloadName(runID), Namespace: Namespace},
	})
	if err := NewClient(kube).Apply(context.Background(), runID); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	job, err := kube.BatchV1().Jobs(Namespace).Get(context.Background(), WorkloadName(runID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if job.Annotations[SpecHashAnnotation] == "" {
		t.Error("a Job predating this change was trusted rather than recreated -- this is the Phase 4 shape exactly")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/prove/ -run 'TestApplyRecreates|TestApplyIsANoOp' -v`

Expected: `TestApplyRecreatesWhenTolerationsChanged` FAILs on the missing toleration, `TestApplyRecreatesAWorkloadWithNoHashAnnotation` FAILs on the empty annotation, and `TestApplyIsANoOpWhenNothingChanged` FAILs to compile until `SpecHashAnnotation` exists.

- [ ] **Step 3: Add the annotation key**

In `internal/prove/manifest.go`, below `WorkloadName`:

```go
// SpecHashAnnotation carries a hash of the workload as this process would
// create it. Apply compares it before deciding whether an existing object is
// the one it wants or a survivor of a differently-configured run.
//
// It exists because the workload name is derived from the run ID alone, so a
// retried Apply for the same run always addresses the same object -- and the
// rendered spec is no longer a pure function of the run. Client.extraTolerations
// comes from AICRME_GPU_TOLERATIONS, which is process configuration: two
// Applies of one run, from two differently-configured processes, legitimately
// want different Jobs. Without this the second is discarded in silence, which
// is how a toleration fix deployed mid-demo failed to reach the run it was
// deployed for (docs/phase-4-status.md).
const SpecHashAnnotation = "aicrme.dev/spec-hash"
```

- [ ] **Step 4: Rewrite Apply**

Replace `Apply`'s body in `internal/prove/client.go`. Add `crypto/sha256`, `encoding/hex` and `encoding/json` to the imports:

```go
// Apply renders and creates the workload for runID.
//
// Idempotent for an unchanged spec: a matching hash annotation means the
// object in the cluster is the one this call wants, so it returns without
// writing. That matters -- a retried Apply against a gang that has already
// placed must not disturb it.
//
// A differing or absent hash means the opposite: the object was created by a
// differently-configured process, or by a binary predating SpecHashAnnotation.
// Neither is the workload this call is being asked for, so it is removed and
// recreated. Update is not an option: a Job's placement-defining fields
// (completions, parallelism, selector, and the whole pod template) are
// immutable once created.
func (c *Client) Apply(ctx context.Context, runID string) error {
	job, hash, err := c.render(runID)
	if err != nil {
		return err
	}

	existing, getErr := c.kube.BatchV1().Jobs(Namespace).Get(ctx, WorkloadName(runID), metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(getErr):
		// Nothing there. Fall through to the create below.
	case getErr != nil:
		return fmt.Errorf("prove: checking for an existing workload for run %s: %w", runID, getErr)
	case existing.Annotations[SpecHashAnnotation] == hash:
		return nil
	default:
		// EnsureAbsent rather than Delete: foreground deletion only
		// guarantees the API server has STARTED cascading, and a new gang
		// placed against pods that are still dying fails to schedule in a
		// way that reads as a placement bug rather than a teardown race.
		if err := c.EnsureAbsent(ctx, runID, waitAbsentTimeout); err != nil {
			return fmt.Errorf("prove: replacing the workload for run %s: %w", runID, err)
		}
	}

	if _, err := c.kube.BatchV1().Jobs(Namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		// A racing creator, or a Create the API server accepted while the
		// client saw a failure. Either way something with this name now
		// exists and this call did not put it there, so it is not treated as
		// success the way the old unconditional swallow did.
		return fmt.Errorf("prove: applying workload for run %s: %w", runID, err)
	}
	return nil
}

// render decodes the workload manifest, applies this Client's configuration,
// and returns the Job alongside a hash of it.
//
// The hash covers the object as THIS process would create it -- not one read
// back from the API server. Server-side defaulting fills a PodSpec with dozens
// of fields no client ever set, so hashing a retrieved Job would report drift
// on every call and turn "recreate on drift" into "always recreate".
func (c *Client) render(runID string) (*batchv1.Job, string, error) {
	out, err := Render(runID, Namespace)
	if err != nil {
		return nil, "", fmt.Errorf("prove: rendering workload for run %s: %w", runID, err)
	}
	var job batchv1.Job
	if err := yaml.Unmarshal(out, &job); err != nil {
		return nil, "", fmt.Errorf("prove: decoding rendered workload for run %s: %w", runID, err)
	}
	// Appended after decode rather than templated into workload.yaml: the
	// manifest is rendered by string replacement, and a YAML list spliced in
	// by textual substitution is a whole class of indentation bug for
	// something the typed object expresses directly.
	job.Spec.Template.Spec.Tolerations = append(
		job.Spec.Template.Spec.Tolerations, c.extraTolerations...)

	canonical, err := json.Marshal(job.Spec)
	if err != nil {
		return nil, "", fmt.Errorf("prove: hashing workload for run %s: %w", runID, err)
	}
	sum := sha256.Sum256(canonical)
	hash := hex.EncodeToString(sum[:])

	if job.Annotations == nil {
		job.Annotations = map[string]string{}
	}
	job.Annotations[SpecHashAnnotation] = hash
	return &job, hash, nil
}
```

Add the timeout constant near the other package constants in `client.go`:

```go
// waitAbsentTimeout bounds the wait when Apply replaces a drifted workload.
// Generous relative to a Job delete with no running pods, and short enough
// that a wedged finalizer surfaces as a failed Prove rather than a hang.
const waitAbsentTimeout = 2 * time.Minute
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/prove/ -race -v`

Expected: PASS, including every pre-existing test in the package.

- [ ] **Step 6: Run the full gate**

Run: `GOTOOLCHAIN=go1.26.5 make qualify`

Expected: green. If coverage dipped, the `render` error paths are the uncovered ones; add a case with a `Render` failure.

- [ ] **Step 7: Commit**

```bash
git add internal/prove/client.go internal/prove/manifest.go internal/prove/client_test.go
git commit -S -m "fix(prove): recreate a workload whose spec has drifted, not adopt it

WorkloadName is derived from the run ID, so a retried Apply addresses the
same object, and Apply swallowed AlreadyExists as success. That was safe
while the manifest was baked into the binary. It stopped being safe when
extraTolerations began arriving from AICRME_GPU_TOLERATIONS -- process
configuration, not run state -- so two Applies of one run from two
differently-configured processes render different Jobs and the second was
discarded in silence.

On real H100s that meant a toleration fix could not reach the run it was
deployed for. Apply now stamps a hash of the rendered-and-mutated Job:
matching hash is a no-op, differing or absent hash means EnsureAbsent then
create. The hash covers what this process would create, not the object read
back, because server-side defaulting would otherwise report drift every time."
```

---

## Task 2: FileStore

**Files:**
- Create: `internal/engine/filestore.go`
- Create: `internal/engine/filestore_test.go`
- Modify: `internal/engine/store_test.go` (run the shared contract against both stores)

**Interfaces:**
- Consumes: `engine.Store` (`internal/engine/store.go:16`), `encodeRun`/`decodeRun` (`internal/engine/envelope.go:183,266`).
- Produces: `engine.NewFileStore(dir string) (Store, error)`.

**Background:** `Store` is four methods — `Save`, `Load`, `LoadCurrent`, `Delete`. `LoadCurrent` exists because startup has no run ID to ask for. The ConfigMap implementation keeps one object holding one encoded run; the file store keeps one file per run plus a `current` pointer file, in a directory the caller supplies. Do not invent a new persistence model — reuse the envelope.

- [ ] **Step 1: Extract the shared contract test**

In `internal/engine/store_test.go`, wrap the existing `memoryStore` assertions in a table so both implementations run them. Add at the top of the file:

```go
// storeFactories is every Store implementation, run against one contract.
// A store that passes this is substitutable for the one the engine's own
// tests use -- which is the point of implementing Store rather than
// inventing a persistence model alongside it.
func storeFactories(t *testing.T) map[string]func() Store {
	t.Helper()
	return map[string]func() Store{
		"memory": NewMemoryStore,
		"file": func() Store {
			s, err := NewFileStore(t.TempDir())
			if err != nil {
				t.Fatalf("NewFileStore() error = %v", err)
			}
			return s
		},
	}
}
```

Then convert each existing `TestMemoryStore*` function body into a subtest loop:

```go
func TestStoreRoundTrip(t *testing.T) {
	for name, newStore := range storeFactories(t) {
		t.Run(name, func(t *testing.T) {
			s := newStore()
			run := &Run{ID: "abcdef0123456789", State: StateRunning, Phase: PhaseDiscover}
			if err := s.Save(context.Background(), run); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			got, err := s.LoadCurrent(context.Background())
			if err != nil {
				t.Fatalf("LoadCurrent() error = %v", err)
			}
			if got.ID != run.ID || got.State != run.State {
				t.Errorf("LoadCurrent() = %+v, want ID %q state %q", got, run.ID, run.State)
			}
		})
	}
}

func TestStoreLoadCurrentOnEmptyStoreIsNotFound(t *testing.T) {
	for name, newStore := range storeFactories(t) {
		t.Run(name, func(t *testing.T) {
			_, err := newStore().LoadCurrent(context.Background())
			if !aicrerrors.IsCode(err, aicrerrors.ErrCodeNotFound) {
				t.Errorf("LoadCurrent() error = %v, want ErrCodeNotFound", err)
			}
		})
	}
}

func TestStoreDeleteClearsCurrent(t *testing.T) {
	for name, newStore := range storeFactories(t) {
		t.Run(name, func(t *testing.T) {
			s := newStore()
			if err := s.Save(context.Background(), &Run{ID: "abcdef0123456789", State: StateDone}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			if err := s.Delete(context.Background()); err != nil {
				t.Fatalf("Delete() error = %v", err)
			}
			if _, err := s.LoadCurrent(context.Background()); !aicrerrors.IsCode(err, aicrerrors.ErrCodeNotFound) {
				t.Errorf("LoadCurrent() after Delete error = %v, want ErrCodeNotFound", err)
			}
		})
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/engine/ -run TestStore -v`

Expected: compile failure — `undefined: NewFileStore`.

- [ ] **Step 3: Write the file store**

Create `internal/engine/filestore.go`:

```go
package engine

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

// filePayloadCeiling is the encoded-record ceiling for a file-backed store.
//
// The ConfigMap store's 800 KiB existed because Kubernetes caps a ConfigMap at
// roughly 1 MiB, and exceeding it sheds artifacts largest-first -- a
// degradation that once made large clusters unusable. A file has no such cap.
// This is set high enough that shedding is unreachable in normal use while
// still bounding a runaway record, because "no ceiling at all" would make a
// pathological run fill the operator's disk.
const filePayloadCeiling = 64 << 20

// currentFile names the pointer file holding the current run's ID. Kept
// separate from the run files rather than inferred from mtimes: "most
// recently written" and "current" diverge the moment a terminal save for an
// older run lands after a newer one started.
const currentFile = "current"

type fileStore struct {
	// mu serializes the read-modify-write of the current pointer against
	// concurrent Saves. The rename below is atomic on its own; the pairing of
	// a run write with a pointer write is not.
	mu  sync.Mutex
	dir string
}

// NewFileStore returns a Store over dir, creating it if needed.
//
// dir is expected to be cluster-scoped by the caller -- see the spec's §4,
// "Recovery is keyed by cluster identity". This constructor deliberately does
// not compute that key: a store that decided its own directory would be a
// second place the cluster identity lives.
func NewFileStore(dir string) (Store, error) {
	if dir == "" {
		return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "run store directory must not be empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "creating the run store directory failed", err)
	}
	return &fileStore{dir: dir}, nil
}

func (s *fileStore) runPath(id string) string { return filepath.Join(s.dir, id+".run") }

// writeAtomic writes b to path via a temp file in the same directory followed
// by a rename. Same directory matters: rename is only atomic within a
// filesystem, and a temp file in $TMPDIR can land on a different one.
func writeAtomic(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }() // no-op once the rename below succeeds
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	// Sync before rename: a rename that reaches the directory entry ahead of
	// the data leaves a zero-length record after a crash, which decodeRun
	// reports as corrupt rather than absent -- the one outcome that stops
	// recovery cold.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func (s *fileStore) Save(_ context.Context, r *Run) error {
	blob, err := encodeRun(r, filePayloadCeiling)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := writeAtomic(s.runPath(r.ID), blob); err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "writing the run record failed", err)
	}
	if err := writeAtomic(filepath.Join(s.dir, currentFile), []byte(r.ID)); err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "writing the current-run pointer failed", err)
	}
	return nil
}

func (s *fileStore) Load(_ context.Context, id string) (*Run, error) {
	blob, err := os.ReadFile(s.runPath(id))
	if os.IsNotExist(err) {
		return nil, aicrerrors.New(aicrerrors.ErrCodeNotFound, "run not found: "+id)
	}
	if err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "reading the run record failed", err)
	}
	return decodeRun(blob, filePayloadCeiling)
}

// LoadCurrent reads the pointer and then the record it names.
//
// A missing pointer is ErrCodeNotFound -- nothing to recover. A pointer naming
// a record that is missing or undecodable is deliberately NOT: recovery must
// not read "unreadable" as "nothing there", because that is exactly the
// mistake that lets a new run overwrite a record that was only momentarily
// unreadable. Same distinction the ConfigMap store drew, for the same reason.
func (s *fileStore) LoadCurrent(ctx context.Context) (*Run, error) {
	id, err := os.ReadFile(filepath.Join(s.dir, currentFile))
	if os.IsNotExist(err) {
		return nil, aicrerrors.New(aicrerrors.ErrCodeNotFound, "no current run")
	}
	if err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "reading the current-run pointer failed", err)
	}
	if len(id) == 0 {
		return nil, aicrerrors.New(aicrerrors.ErrCodeNotFound, "no current run")
	}
	run, err := s.Load(ctx, string(id))
	if aicrerrors.IsCode(err, aicrerrors.ErrCodeNotFound) {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInternal,
			"the current-run pointer names a record that is not there", err)
	}
	return run, err
}

func (s *fileStore) Delete(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, err := os.ReadFile(filepath.Join(s.dir, currentFile))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "reading the current-run pointer failed", err)
	}
	if err := os.Remove(s.runPath(string(id))); err != nil && !os.IsNotExist(err) {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "removing the run record failed", err)
	}
	if err := os.Remove(filepath.Join(s.dir, currentFile)); err != nil && !os.IsNotExist(err) {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "removing the current-run pointer failed", err)
	}
	return nil
}
```

- [ ] **Step 4: Add file-store-specific tests**

Create `internal/engine/filestore_test.go`:

```go
package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

func TestFileStoreLeavesNoTempFilesBehind(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore() error = %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := s.Save(context.Background(), &Run{ID: "abcdef0123456789", State: StateRunning}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	for _, e := range entries {
		if len(e.Name()) > 0 && e.Name()[0] == '.' {
			t.Errorf("temp file %q survived a completed Save", e.Name())
		}
	}
}

func TestFileStoreRecordsAreOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewFileStore(dir)
	if err := s.Save(context.Background(), &Run{ID: "abcdef0123456789", State: StateRunning}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "abcdef0123456789.run"))
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("record mode = %#o, want 0600 -- a run record carries cluster detail", perm)
	}
}

// A pointer naming a missing record must not read as "nothing to recover".
// Recover treats NotFound as a clean start and would let the next run
// overwrite a record that was merely unreadable at that instant.
func TestFileStoreDanglingPointerIsNotAbsence(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewFileStore(dir)
	if err := os.WriteFile(filepath.Join(dir, currentFile), []byte("abcdef0123456789"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	_, err := s.LoadCurrent(context.Background())
	if err == nil {
		t.Fatal("LoadCurrent() succeeded against a dangling pointer")
	}
	if aicrerrors.IsCode(err, aicrerrors.ErrCodeNotFound) {
		t.Error("a dangling pointer reported NotFound -- recovery would treat it as a clean start")
	}
}

// Two stores over different directories must not see each other's runs. This
// is what makes the cluster-keyed directory in Task 6 an isolation boundary
// rather than a naming convention.
func TestFileStoresInDifferentDirectoriesAreIsolated(t *testing.T) {
	a, _ := NewFileStore(t.TempDir())
	b, _ := NewFileStore(t.TempDir())
	if err := a.Save(context.Background(), &Run{ID: "aaaaaaaa00000000", State: StateRunning}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if _, err := b.LoadCurrent(context.Background()); !aicrerrors.IsCode(err, aicrerrors.ErrCodeNotFound) {
		t.Errorf("the second store saw the first store's run: err = %v", err)
	}
}
```

- [ ] **Step 5: Run the tests**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/engine/ -race -run 'TestStore|TestFileStore' -v`

Expected: PASS. `encodeRun`/`decodeRun` do not take a ceiling parameter yet — this step will not compile until Task 3. Do Task 3 first if the compiler objects, then return here; the two are one commit if that is simpler.

- [ ] **Step 6: Commit**

```bash
git add internal/engine/filestore.go internal/engine/filestore_test.go internal/engine/store_test.go
git commit -S -m "feat(engine): a file-backed run store behind the existing Store interface

Same four methods, same envelope, so Recover and ReconcileWorkloads need no
changes -- which is the point of implementing Store rather than inventing a
persistence model beside it. Writes go through a temp file and a rename in
the same directory, at 0600.

The current-run pointer is a file rather than an inferred most-recent-mtime:
'last written' and 'current' diverge the moment a terminal save for an older
run lands after a newer one has started. A pointer naming a missing record is
an error and deliberately not NotFound, because recovery reads NotFound as a
clean start and would overwrite a record that was only momentarily unreadable."
```

---

## Task 3: The payload ceiling becomes a parameter

**Files:**
- Modify: `internal/engine/envelope.go:34,40,183,266`
- Modify: `internal/engine/cmstore.go:129,264` (call sites)
- Modify: `internal/engine/envelope_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `encodeRun(r *Run, maxPayload int) ([]byte, error)`, `decodeRun(blob []byte, maxPayload int) (*Run, error)`.

**Background:** `maxPayload = 800 << 10` exists because "Kubernetes caps a ConfigMap at roughly 1MiB" and `maxDecompressed = 8 << 20` because "the pod runs under a 512Mi cap". Neither describes a file. `encodeRun` has exactly one caller today (`cmstore.go:129`) and `decodeRun` one (`cmstore.go:264`) — both die in Task 14, and the file store becomes the sole caller of each. So the ceiling passes as an argument, with no store indirection.

- [ ] **Step 1: Write the failing test**

Add to `internal/engine/envelope_test.go`:

```go
// A larger ceiling must actually keep artifacts a smaller one would shed.
// This is the whole reason the ceiling became a parameter: the ConfigMap's
// 800 KiB is a Kubernetes object limit, not a property of a run.
func TestEncodeRunHonorsTheCeilingItIsGiven(t *testing.T) {
	run := &Run{ID: "abcdef0123456789", State: StateRunning, Artifacts: map[string][]byte{
		"big.json": make([]byte, 900<<10),
	}}

	small, err := encodeRun(run, 800<<10)
	if err != nil {
		t.Fatalf("encodeRun(small) error = %v", err)
	}
	shed, err := decodeRun(small, 800<<10)
	if err != nil {
		t.Fatalf("decodeRun(small) error = %v", err)
	}
	if len(shed.Truncated) == 0 {
		t.Error("the 800 KiB ceiling shed nothing from a 900 KiB artifact")
	}

	large, err := encodeRun(run, 64<<20)
	if err != nil {
		t.Fatalf("encodeRun(large) error = %v", err)
	}
	kept, err := decodeRun(large, 64<<20)
	if err != nil {
		t.Fatalf("decodeRun(large) error = %v", err)
	}
	if len(kept.Truncated) != 0 {
		t.Errorf("the 64 MiB ceiling shed %v -- shedding must be unreachable at the file store's ceiling", kept.Truncated)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/engine/ -run TestEncodeRunHonors -v`

Expected: compile failure — `too many arguments in call to encodeRun`.

- [ ] **Step 3: Thread the parameter**

In `internal/engine/envelope.go`:

- Delete the `maxPayload` const at line 34. Keep its comment text, moved onto the new parameter.
- Change `maxDecompressed` from a const to a derived value: `maxDecompressed := maxPayload * 10`, computed inside `decodeRun`. The 10× ratio preserves today's 800 KiB → 8 MiB relationship. Keep the existing comment explaining that it guards a malformed record inflating without limit — a property of the decoder, not of where the bytes were stored.
- Change the two signatures:

```go
// encodeRun serializes r for persistence.
//
// maxPayload bounds the encoded record. It is a parameter rather than a
// constant because it describes the STORE, not the run: a ConfigMap is capped
// near 1 MiB by Kubernetes, a file is not. A record over the ceiling sheds
// artifacts, largest first, until it fits, and names what it dropped in
// Truncated.
func encodeRun(r *Run, maxPayload int) ([]byte, error) {

// decodeRun deserializes a record written by encodeRun.
//
// maxPayload is the same ceiling encodeRun was given; decompression is bounded
// at ten times it, which reproduces the ConfigMap store's original 800 KiB to
// 8 MiB relationship. That bound guards against a small stored payload
// inflating without limit, which is a property of this decoder rather than of
// wherever the bytes were kept.
func decodeRun(blob []byte, maxPayload int) (*Run, error) {
```

- Update every internal reference to `maxPayload` and `maxDecompressed` inside both functions to use the parameter and the derived local.

In `internal/engine/cmstore.go`, add the ConfigMap ceiling as a package const and pass it at both call sites:

```go
// cmPayloadCeiling bounds a record stored in a ConfigMap. Kubernetes caps a
// ConfigMap at roughly 1 MiB; this leaves headroom for the object's own
// metadata. Deleted with this file in the local-binary restructure.
const cmPayloadCeiling = 800 << 10
```

`cmstore.go:129` becomes `encodeRun(r, cmPayloadCeiling)`; `cmstore.go:264` becomes `decodeRun(blob, cmPayloadCeiling)`.

- [ ] **Step 4: Update the existing envelope tests**

Every existing call to `encodeRun(run)` / `decodeRun(blob)` in `internal/engine/envelope_test.go` and `internal/engine/cmstore_test.go` takes `cmPayloadCeiling` as its second argument. Do not change any assertion — the point of this task is that behavior at the old ceiling is identical.

- [ ] **Step 5: Run the tests**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/engine/ -race -v`

Expected: PASS, including Task 2's file store tests, which now compile.

- [ ] **Step 6: Commit**

```bash
git add internal/engine/envelope.go internal/engine/envelope_test.go internal/engine/cmstore.go internal/engine/cmstore_test.go
git commit -S -m "refactor(engine): the payload ceiling describes the store, not the run

800 KiB exists because Kubernetes caps a ConfigMap near 1 MiB, and 8 MiB
because the pod ran under a 512Mi limit. Neither describes a file. Both
become parameters, with the decompression bound derived at ten times the
ceiling so today's relationship is preserved exactly.

Behavior at the ConfigMap ceiling is unchanged; every existing assertion
holds with the constant passed explicitly."
```

---

## Task 4: Extract `internal/console` — a pure move

No behavior changes. This task exists so the next nine have somewhere to land and something to test against. Resist the urge to fix anything while moving it.

**Files:**
- Create: `internal/console/console.go`
- Create: `internal/console/console_test.go`
- Modify: `cmd/aicrme/main.go` (down to roughly 40 lines)

**Interfaces:**
- Consumes: everything `main()` wires today — `aicrclient.New`, `bus.New`, `engine.New`, `steps.New*`, `api.New`, `observer.New`, `prove.NewClient`, `teardown.NewEngineTeardown`.
- Produces:
  ```go
  package console

  type Options struct {
      Addr        string // listen address; must resolve to loopback
      WorkDir     string
      Kubeconfig  string // explicit --kubeconfig, empty for the default loading rules
      Context     string // explicit --context, empty for the kubeconfig's current-context
      OpenBrowser bool
  }

  func Run(ctx context.Context, opts Options) error
  ```

- [ ] **Step 1: Move the file wholesale**

```bash
git mv cmd/aicrme/main.go internal/console/console.go
```

Change the package clause to `package console`. Rename `main()` to:

```go
// Run starts the console and blocks until ctx is done and shutdown completes.
//
// Every fatal startup condition returns an error rather than calling
// os.Exit: this function has a caller now, and a package that exits the
// process cannot be tested. cmd/aicrme is what turns an error into an exit
// code.
//
// This is also the seam an upstream `aicr server` would fill -- a cobra
// command populating Options from AICR's existing kubeconfig and context
// flags and calling this. Donation moves the package into AICR's tree rather
// than importing it across modules, so the internal/ prefix is not an
// obstacle. Nothing is filed upstream until aicrme is public.
func Run(ctx context.Context, opts Options) error {
```

Mechanical conversions inside the moved body:

- Every `slog.Error(...); os.Exit(1)` becomes `return fmt.Errorf(...)`. Keep the message text; wrap the cause with `%w`.
- `flag.String("addr", ...)` and `flag.Parse()` are deleted — `opts.Addr` replaces them.
- `envOr("AICRME_WORK_DIR", defaultWorkDir)` becomes `opts.WorkDir`.
- The `ctx, stop := signal.NotifyContext(...)` block is deleted; `ctx` is now a parameter. The `defer stop()` sites become `defer cancel()` on a `context.WithCancel(ctx)` derived inside `Run`, so the `httpSrv.ListenAndServe` failure path can still cancel.
- `<-ctx.Done()` stays.

Move `Options` into `internal/console/console.go` above `Run`.

- [ ] **Step 2: Write the new main**

Replace `cmd/aicrme/main.go` entirely:

```go
// Command aicrme serves the AI Cluster Runtime demo console from the
// operator's own machine.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/mchmarny/aicrme/internal/console"
	"github.com/mchmarny/aicrme/internal/version"
)

func main() {
	var opts console.Options
	flag.StringVar(&opts.Addr, "addr", "127.0.0.1:0",
		"listen address; must resolve to loopback. Port 0 lets the OS pick.")
	flag.StringVar(&opts.Kubeconfig, "kubeconfig", "",
		"path to a kubeconfig; unset falls through to the default loading rules, which honor KUBECONFIG")
	flag.StringVar(&opts.Context, "context", "",
		"kubeconfig context to preselect; the operator can still change it before connecting")
	open := flag.Bool("open", true, "open the default browser at the tokenized URL")
	flag.StringVar(&opts.WorkDir, "work-dir", defaultWorkDir(),
		"scratch and state directory; AICRME_WORK_DIR overrides the default")
	flag.Parse()
	opts.OpenBrowser = *open

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	slog.Info("starting aicrme", "version", version.String())

	// SIGINT and SIGTERM cancel ctx, which is the only thing that stops Run.
	// Run's own shutdown reaps the deploy.sh process tree before returning --
	// see its comment on why that ordering is not negotiable.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := console.Run(ctx, opts); err != nil {
		slog.Error("aicrme failed", "error", err)
		os.Exit(1)
	}
}

// defaultWorkDir is AICRME_WORK_DIR, else ~/.aicrme. A home directory that
// cannot be resolved falls back to the current directory rather than failing:
// the console still works, and the operator sees where state landed in the
// startup log.
func defaultWorkDir() string {
	if v := os.Getenv("AICRME_WORK_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		slog.Warn("could not resolve a home directory; using ./.aicrme", "error", err)
		return ".aicrme"
	}
	return filepath.Join(home, ".aicrme")
}
```

Note the log handler changed from JSON on stdout to text on stderr. A local binary's log is read by a person in a terminal, and stdout is reserved for the tokenized URL (Task 8).

- [ ] **Step 3: Move the existing main tests**

```bash
git mv cmd/aicrme/main_test.go internal/console/console_test.go
```

Change the package clause. Every test of `parseTolerations`, `parseNodeSelector`, `parseResourceRequests`, `newRunScopeFn`, `newObserverScopeFn` and `recipeNamespaces` moves unchanged — these are the parts that were already testable, and they must keep passing to prove the move changed nothing.

- [ ] **Step 4: Verify the build and the whole suite**

Run: `GOTOOLCHAIN=go1.26.5 make qualify`

Expected: green. Coverage should *rise* — the moved helpers now count toward a package that is measured, where `cmd/aicrme` was largely unreachable.

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -S -m "refactor(console): move main's wiring into a package with an entry point

main() was 328 lines of wiring -- five step constructors, two observer
accessor closures, the store selection, the engine, the API server, the
observer, and a two-goroutine shutdown -- none of it reachable from a test.
It moves behind console.Run(ctx, Options) unchanged; cmd/aicrme becomes flag
parsing, slog setup, signal wiring and one call.

Every os.Exit inside the moved body becomes a returned error, because a
package that exits the process cannot be tested. The helpers that already had
tests keep them, which is what shows the move changed nothing.

This is also the seam an upstream 'aicr server' would fill. Nothing is filed
upstream; the seam is preserved, not exercised."
```

---

## Task 5: Kubeconfig loading and the connect state machine

**Files:**
- Create: `internal/console/connect.go`
- Create: `internal/console/connect_test.go`
- Modify: `internal/console/console.go` (delete the `rest.InClusterConfig` block)
- Modify: `internal/api/server.go` (the connect gate)

**Interfaces:**
- Consumes: `console.Options` (Task 4).
- Produces:
  ```go
  type ClusterInfo struct {
      Context   string `json:"context"`
      Server    string `json:"server"`
      Version   string `json:"version"`
      NodeCount int    `json:"nodeCount"`
      UID       string `json:"uid"` // filled in Task 6
  }

  type ContextInfo struct {
      Name    string `json:"name"`
      Server  string `json:"server"`
      Current bool   `json:"current"`
  }

  func listContexts(kubeconfig string) ([]ContextInfo, error)

  type connector struct{ /* unexported */ }
  func (c *connector) Connect(ctx context.Context, contextName string) (ClusterInfo, error)
  func (c *connector) State() connState
  ```

**Background:** `console.go` currently builds its client with `rest.InClusterConfig()` and degrades to `kube == nil` with three separate warnings. All three describe a state that can no longer occur: a local binary that cannot reach a cluster has nothing to offer. `clientcmd.NewNonInteractiveDeferredLoadingClientConfig` over `ClientConfigLoadingRules` honors both `KUBECONFIG` and an explicit path.

- [ ] **Step 1: Write the failing tests**

Create `internal/console/connect_test.go`:

```go
package console

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

const twoContextKubeconfig = `apiVersion: v1
kind: Config
current-context: alpha
clusters:
- name: alpha-cluster
  cluster: {server: https://alpha.example:6443}
- name: beta-cluster
  cluster: {server: https://beta.example:6443}
contexts:
- name: alpha
  context: {cluster: alpha-cluster, user: alpha-user}
- name: beta
  context: {cluster: beta-cluster, user: beta-user}
users:
- name: alpha-user
  user: {token: alpha-token}
- name: beta-user
  user: {token: beta-token}
`

func writeKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(twoContextKubeconfig), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

func TestListContextsReadsNamesServersAndCurrent(t *testing.T) {
	got, err := listContexts(writeKubeconfig(t))
	if err != nil {
		t.Fatalf("listContexts() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("listContexts() returned %d contexts, want 2", len(got))
	}
	byName := map[string]ContextInfo{}
	for _, c := range got {
		byName[c.Name] = c
	}
	if byName["alpha"].Server != "https://alpha.example:6443" {
		t.Errorf("alpha server = %q", byName["alpha"].Server)
	}
	if !byName["alpha"].Current {
		t.Error("alpha is the kubeconfig's current-context and was not marked current")
	}
	if byName["beta"].Current {
		t.Error("beta was marked current")
	}
}

// Listing contexts must not touch a cluster: the operator is choosing which
// one to talk to, and two of the three servers in a typical kubeconfig are
// unreachable from wherever they happen to be sitting.
func TestListContextsMakesNoClusterContact(t *testing.T) {
	// Every server above points at a name that does not resolve. A call that
	// dialed would take the resolver timeout; one that only reads the file
	// returns immediately.
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := listContexts(writeKubeconfig(t)); err != nil {
			t.Errorf("listContexts() error = %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("listContexts blocked -- it is contacting a cluster")
	}
}

// Connect is single-assignment. net/http serves every request on its own
// goroutine, and connect mutates process-global state, builds the clientset
// every step reads, and selects the run directory. Two of them interleaving
// is a torn connection, not a lost race.
func TestConcurrentConnectYieldsExactlyOneWinner(t *testing.T) {
	c := newConnector(writeKubeconfig(t), fakeProber{})

	const callers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners int
	)
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			if _, err := c.Connect(context.Background(), "alpha"); err == nil {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if winners != 1 {
		t.Errorf("%d callers connected successfully, want exactly 1", winners)
	}
	if c.State() != stateConnected {
		t.Errorf("State() = %v, want stateConnected", c.State())
	}
}

func TestConnectAfterConnectIsRefused(t *testing.T) {
	c := newConnector(writeKubeconfig(t), fakeProber{})
	if _, err := c.Connect(context.Background(), "alpha"); err != nil {
		t.Fatalf("first Connect() error = %v", err)
	}
	if _, err := c.Connect(context.Background(), "beta"); err == nil {
		t.Fatal("a second Connect succeeded -- switching clusters in-session is prohibited")
	}
}
```

Add a `fakeProber` in the same file implementing whatever narrow interface `connector` takes for the cluster round-trip, returning a fixed version and node count.

- [ ] **Step 2: Run to verify they fail**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/console/ -run 'TestListContexts|TestConcurrentConnect|TestConnectAfter' -v`

Expected: compile failure — `undefined: listContexts`, `undefined: newConnector`.

- [ ] **Step 3: Implement**

Create `internal/console/connect.go`:

```go
package console

import (
	"context"
	"fmt"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type connState int

const (
	stateDisconnected connState = iota
	stateConnecting
	stateConnected
)

// loadingRules honors KUBECONFIG and an explicit --kubeconfig path, in
// clientcmd's own precedence order. Building them here rather than at each
// call site means the context list and the connection can never disagree
// about which file they are reading.
func loadingRules(kubeconfig string) *clientcmd.ClientConfigLoadingRules {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	return rules
}

// listContexts reads the kubeconfig and nothing else. No cluster is
// contacted: the operator is choosing which one to talk to, and most of the
// contexts in a working kubeconfig are unreachable from wherever they are
// sitting at the time.
func listContexts(kubeconfig string) ([]ContextInfo, error) {
	cfg, err := loadingRules(kubeconfig).Load()
	if err != nil {
		return nil, fmt.Errorf("reading kubeconfig: %w", err)
	}
	out := make([]ContextInfo, 0, len(cfg.Contexts))
	for name, kctx := range cfg.Contexts {
		info := ContextInfo{Name: name, Current: name == cfg.CurrentContext}
		if cluster, ok := cfg.Clusters[kctx.Cluster]; ok {
			info.Server = cluster.Server
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// prober is the one cluster round-trip Connect makes, narrowed so tests can
// supply it without a fake clientset for a call that only reads two scalars.
type prober interface {
	probe(ctx context.Context, kube kubernetes.Interface) (version string, nodes int, err error)
}

type liveProber struct{}

func (liveProber) probe(ctx context.Context, kube kubernetes.Interface) (string, int, error) {
	v, err := kube.Discovery().ServerVersion()
	if err != nil {
		return "", 0, fmt.Errorf("asking the cluster for its version: %w", err)
	}
	nodes, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", 0, fmt.Errorf("listing nodes: %w", err)
	}
	return v.GitVersion, len(nodes.Items), nil
}

// connector owns the one connection this process will ever have.
//
// State is single-assignment: disconnected -> connecting -> connected, and it
// never leaves connected. A second Connect is refused whether the first is
// still running or already finished, because connect mutates process-global
// KUBECONFIG, builds the clientset the observer and every step read, and
// selects the run directory -- three things that a torn interleaving would
// leave pointing at two different clusters.
//
// It is also why in-session cluster switching is prohibited: a reconnect
// would have to re-derive all four cluster consumers and re-key the run
// directory mid-process. Restarting the binary is cheap.
type connector struct {
	mu         sync.Mutex
	state      connState
	kubeconfig string
	prober     prober

	// Written once, under mu, at the connected transition.
	info ClusterInfo
	rest *rest.Config
	kube kubernetes.Interface
}

func newConnector(kubeconfig string, p prober) *connector {
	return &connector{kubeconfig: kubeconfig, prober: p}
}

func (c *connector) State() connState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// Cluster returns the connection, or false if none has been established.
func (c *connector) Cluster() (kubernetes.Interface, ClusterInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.kube, c.info, c.state == stateConnected
}

func (c *connector) Connect(ctx context.Context, contextName string) (ClusterInfo, error) {
	c.mu.Lock()
	if c.state != stateDisconnected {
		state := c.state
		c.mu.Unlock()
		if state == stateConnected {
			return ClusterInfo{}, errAlreadyConnected
		}
		return ClusterInfo{}, errConnectInFlight
	}
	c.state = stateConnecting
	c.mu.Unlock()

	info, restCfg, kube, err := c.dial(ctx, contextName)
	if err != nil {
		// Back to disconnected, not stuck in connecting: a wrong context or a
		// sleeping VPN is the ordinary case, and the operator has to be able
		// to pick again without restarting the binary.
		c.mu.Lock()
		c.state = stateDisconnected
		c.mu.Unlock()
		return ClusterInfo{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.info, c.rest, c.kube = info, restCfg, kube
	c.state = stateConnected
	return info, nil
}

func (c *connector) dial(ctx context.Context, contextName string) (ClusterInfo, *rest.Config, kubernetes.Interface, error) {
	overrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules(c.kubeconfig), overrides)

	restCfg, err := cc.ClientConfig()
	if err != nil {
		return ClusterInfo{}, nil, nil, fmt.Errorf("building a client for context %q: %w", contextName, err)
	}
	kube, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return ClusterInfo{}, nil, nil, fmt.Errorf("building a clientset for context %q: %w", contextName, err)
	}

	probeCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	version, nodes, err := c.prober.probe(probeCtx, kube)
	if err != nil {
		return ClusterInfo{}, nil, nil, fmt.Errorf("reaching context %q at %s: %w", contextName, restCfg.Host, err)
	}

	return ClusterInfo{
		Context:   contextName,
		Server:    restCfg.Host,
		Version:   version,
		NodeCount: nodes,
	}, restCfg, kube, nil
}
```

Add near the top:

```go
// connectTimeout bounds the one cluster round-trip Connect makes. Short
// enough that a wrong context or a sleeping VPN reports quickly, long enough
// for an exec credential plugin to run -- gke-gcloud-auth-plugin and
// `aws eks get-token` both shell out and can take seconds on a cold cache.
const connectTimeout = 30 * time.Second

var (
	errAlreadyConnected = aicrerrors.New(aicrerrors.ErrCodeConflict,
		"this console is already connected to a cluster; restart the binary to use a different one")
	errConnectInFlight = aicrerrors.New(aicrerrors.ErrCodeConflict,
		"a connection attempt is already in progress")
	errNotConnected = aicrerrors.New(aicrerrors.ErrCodeConflict,
		"connect to a cluster first")
)
```

- [ ] **Step 4: Delete the in-cluster path from `console.go`**

Remove the `rest.InClusterConfig()` block and all three `kube == nil` warnings. `kube` now comes from the connector and is never nil once connected.

- [ ] **Step 5: Add the routes and the gate**

In `internal/api/server.go`, add two routes outside the connect gate and wrap everything else:

```go
mux.HandleFunc("GET /api/contexts", s.handleContexts)
mux.HandleFunc("POST /api/connect", s.handleConnect)
```

and a middleware over `protected`:

```go
// requireConnected gates every route that needs a cluster behind an
// established connection. This is a state gate, not an auth gate -- the token
// middleware is separate and runs first. 409 rather than 503: the request is
// well-formed and the server is healthy, it just does not yet know which
// cluster the operator means.
func (s *Server) requireConnected(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := s.cluster(); !ok {
			http.Error(w, "connect to a cluster first", http.StatusConflict)
			return
		}
		h.ServeHTTP(w, r)
	})
}
```

- [ ] **Step 6: Run the tests**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/console/ ./internal/api/ -race -v`

Expected: PASS. Run the concurrency test specifically under `-race`, repeated:

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/console/ -race -run TestConcurrentConnect -count=50`

Expected: PASS, 50 times.

- [ ] **Step 7: Commit**

```bash
git add internal/console/connect.go internal/console/connect_test.go internal/console/console.go internal/api/server.go
git commit -S -m "feat(console): choose a cluster from the operator's kubeconfig, exactly once

clientcmd's deferred loading replaces rest.InClusterConfig, and with it all
three 'kube is nil outside a pod' degradation paths -- every one described a
state a local binary cannot be in, because a binary that cannot reach a
cluster has nothing to offer.

Connect is single-assignment. net/http serves every request on its own
goroutine, and connect mutates process-global state, builds the clientset
every step reads, and selects the run directory; two interleaving is a torn
connection, not a lost race. A second attempt is refused whether the first is
in flight or already finished, which is also how in-session cluster switching
stays prohibited.

GET /api/contexts reads the kubeconfig and contacts nothing: most contexts in
a working kubeconfig are unreachable from wherever the operator is sitting."
```

---

## Task 6: Cluster identity, and the run directory keyed on it

**Files:**
- Modify: `internal/console/connect.go`
- Modify: `internal/console/console.go` (store construction)
- Modify: `internal/engine/run.go` (add the identity field)
- Modify: `internal/engine/recover.go`, `internal/engine/reset.go` (revalidation)
- Test: `internal/console/connect_test.go`, `internal/engine/recover_test.go`

**Interfaces:**
- Consumes: `connector.Connect` (Task 5), `engine.NewFileStore` (Task 2).
- Produces: `ClusterInfo.UID` populated; `Run.ClusterUID string \`json:"clusterUid,omitempty"\``; `engine.ErrClusterMismatch`.

**Background:** the run directory must be keyed on something a rebuilt cluster cannot collide with. A context name is a label in a file the operator edits; a server URL is an address — `kind delete cluster && kind create cluster` yields the same endpoint and a different cluster. `kube-system` is created by the control plane at bootstrap and never recreated. `snapshotOwnership` already records namespace UIDs for the same class of reason (`internal/steps/ownership.go:101`).

- [ ] **Step 1: Write the failing tests**

Add to `internal/console/connect_test.go`:

```go
func TestConnectRecordsTheKubeSystemUID(t *testing.T) {
	kube := fake.NewSimpleClientset(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "11111111-2222-3333-4444-555555555555"},
	})
	info, err := connectWith(t, kube)
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	if info.UID != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("UID = %q, want the kube-system UID", info.UID)
	}
}

// Two clusters at the same address must not share a run directory. This is
// the rebuilt-kind-cluster case, and it is not exotic for a demo tool.
func TestRunDirectoryDiffersForADifferentUIDAtTheSameServer(t *testing.T) {
	root := t.TempDir()
	a := runDir(root, "11111111-2222-3333-4444-555555555555")
	b := runDir(root, "99999999-8888-7777-6666-555555555555")
	if a == b {
		t.Fatalf("two cluster UIDs mapped to the same run directory: %s", a)
	}
}
```

Add to `internal/engine/recover_test.go`:

```go
func TestRecoverRefusesARecordFromADifferentCluster(t *testing.T) {
	store := NewMemoryStore()
	seed := New(bus.New(64), store, func() Step { return &fakeStep{phase: PhaseBundle} }())
	run := seedRun(t, seed, StateFailed)
	run.ClusterUID = "11111111-2222-3333-4444-555555555555"
	if err := store.Save(context.Background(), run); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	e := New(bus.New(64), store, func() Step { return &fakeStep{phase: PhaseBundle} }())
	e.SetClusterUID("99999999-8888-7777-6666-555555555555")

	err := e.Recover(context.Background())
	if !errors.Is(err, ErrClusterMismatch) {
		t.Fatalf("Recover() error = %v, want ErrClusterMismatch", err)
	}
	if !strings.Contains(err.Error(), "11111111") || !strings.Contains(err.Error(), "99999999") {
		t.Errorf("the error names neither UID: %v", err)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/console/ ./internal/engine/ -run 'UID|ClusterMismatch|RunDirectory' -v`

Expected: compile failures on `runDir`, `SetClusterUID`, `ErrClusterMismatch`, `Run.ClusterUID`.

- [ ] **Step 3: Read the UID at connect**

In `connect.go`'s `dial`, after the probe succeeds:

```go
ns, err := kube.CoreV1().Namespaces().Get(probeCtx, "kube-system", metav1.GetOptions{})
if err != nil {
	return ClusterInfo{}, nil, nil, fmt.Errorf("reading this cluster's identity from kube-system: %w", err)
}
```

and set `UID: string(ns.UID)` on the returned `ClusterInfo`. Document it:

```go
// The kube-system UID is this cluster's identity, and it is what §4 keys the
// run directory on. Neither the server URL nor the context name will do:
// a context is a label in a file the operator edits, and an address can front
// a rebuilt cluster -- `kind delete && kind create` reuses the endpoint. The
// UID changes when the cluster does, which is the property the key needs.
//
// kube-system is created by the control plane at bootstrap, is never
// recreated during a cluster's life, and is readable by any principal that
// can do anything else useful here.
```

- [ ] **Step 4: Key the run directory**

In `console.go`, after a successful connect:

```go
// runDir is the per-cluster run directory. The ConfigMap store got this
// property free by living inside the cluster it described; a flat local file
// does not. An operator who demos cluster A then connects to cluster B would
// otherwise have B's console recover A's run and offer a Reset that
// uninstalls releases in the wrong place.
func runDir(workDir, clusterUID string) string {
	return filepath.Join(workDir, "runs", clusterUID)
}
```

Construct the store with it and call `eng.SetClusterUID(info.UID)`.

- [ ] **Step 5: Revalidate before recovery and before Reset**

Add to `internal/engine/run.go`:

```go
// ClusterUID is the kube-system UID of the cluster this run describes.
//
// Stored in the record as well as in the directory name it lives under: the
// directory says which cluster the record was FILED under, the field says
// which cluster it DESCRIBES. They should never disagree, and a record that
// does is refused rather than reconciled.
ClusterUID string `json:"clusterUid,omitempty"`
```

In `recover.go`, before installing a loaded record:

```go
if e.clusterUID != "" && r.ClusterUID != "" && r.ClusterUID != e.clusterUID {
	return fmt.Errorf("%w: the persisted run describes cluster %s but this console is connected to %s",
		ErrClusterMismatch, r.ClusterUID, e.clusterUID)
}
```

Apply the identical guard at the top of `Reset`, with its own message naming Reset. Reset is the one that matters most: it acts on release names, and acting on them in the wrong cluster is the worst outcome this design can produce.

Add the error:

```go
// ErrClusterMismatch means a persisted record describes a different cluster
// than the one this console is connected to. Not recoverable and not
// reconcilable: every release name in that record is now a name in somebody
// else's cluster.
var ErrClusterMismatch = errors.New("run record belongs to a different cluster")
```

- [ ] **Step 6: Run the tests**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/console/ ./internal/engine/ -race -v`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add -A
git commit -S -m "feat(console): identify a cluster by its kube-system UID, and key runs on it

A run key made of the server URL and the context name is not an identity.
Both are mutable -- a context is a label in a file the operator edits, and
'kind delete && kind create' reuses the endpoint while replacing the cluster.
Reset acts on the release names in a record, so a key two clusters can
collide on is a key that can point Reset at the wrong one.

kube-system is created at bootstrap and never recreated, so its UID changes
exactly when the cluster does. Using a namespace UID as identity is already
this repo's idiom: snapshotOwnership records one per namespace.

The UID is stored in the record as well as in the directory name, and
revalidated before recovery and before Reset. The directory says which
cluster a record was filed under; the field says which one it describes."
```

---

## Task 7: The work-directory lock and the per-launch session directory

**Files:**
- Create: `internal/console/lock.go`, `internal/console/session.go`
- Create: `internal/console/lock_test.go`, `internal/console/session_test.go`
- Modify: `internal/console/console.go`

**Interfaces:**
- Consumes: `Options.WorkDir`.
- Produces: `acquireLock(workDir string) (release func(), err error)`, `writeSessionKubeconfig(workDir, kubeconfig, contextName string) (path string, cleanup func(), err error)`, `sweepStaleSessions(workDir string)`.

**Background:** `grep -rn "flock\|LOCK_EX\|singleflight\|lockfile" internal/` returns nothing — this repo has never taken a lock. The one multi-writer guard it had was `cmstore.resolveDeploymentOwner`, which stamped an `ownerReference` so a record written by a different deployment was detected (`TestRecoverDegradesAgainstAForeignOwnedRecord`). Task 14 deletes that file, so this guard must land in the same change or before it.

- [ ] **Step 1: Write the failing tests**

Create `internal/console/lock_test.go`:

```go
package console

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestSecondLockOnTheSameWorkDirIsRefused(t *testing.T) {
	dir := t.TempDir()
	release, err := acquireLock(dir)
	if err != nil {
		t.Fatalf("first acquireLock() error = %v", err)
	}
	defer release()

	if _, err := acquireLock(dir); err == nil {
		t.Fatal("a second process acquired the same work directory -- both would write the same run record")
	}
}

func TestLockIsReleasedOnClose(t *testing.T) {
	dir := t.TempDir()
	release, err := acquireLock(dir)
	if err != nil {
		t.Fatalf("acquireLock() error = %v", err)
	}
	release()

	second, err := acquireLock(dir)
	if err != nil {
		t.Fatalf("acquireLock() after release error = %v", err)
	}
	second()
}

// A lock whose PID is dead is reported, not cleared. A live second process
// and a crashed first one look identical from the file alone, and guessing
// wrong is the case this guard exists to prevent.
func TestStaleLockIsReportedWithItsPathAndPID(t *testing.T) {
	dir := t.TempDir()
	// PID 0 is never a live user process, so the liveness probe must say dead.
	if err := os.WriteFile(filepath.Join(dir, "lock"), []byte("0"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	_, err := acquireLock(dir)
	if err == nil {
		t.Fatal("acquireLock() cleared a stale lock automatically")
	}
	if !strings.Contains(err.Error(), filepath.Join(dir, "lock")) {
		t.Errorf("the error does not name the lock path: %v", err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(0)) {
		t.Errorf("the error does not name the recorded PID: %v", err)
	}
}
```

Create `internal/console/session_test.go`:

```go
func TestSessionKubeconfigIsOwnerOnlyInAnOwnerOnlyDirectory(t *testing.T) {
	work := t.TempDir()
	path, cleanup, err := writeSessionKubeconfig(work, writeKubeconfig(t), "alpha")
	if err != nil {
		t.Fatalf("writeSessionKubeconfig() error = %v", err)
	}
	defer cleanup()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("kubeconfig mode = %#o, want 0600", perm)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("Stat(dir) error = %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("session dir mode = %#o, want 0700", perm)
	}
}

func TestSessionKubeconfigBakesInTheSelectedContext(t *testing.T) {
	work := t.TempDir()
	path, cleanup, err := writeSessionKubeconfig(work, writeKubeconfig(t), "beta")
	if err != nil {
		t.Fatalf("writeSessionKubeconfig() error = %v", err)
	}
	defer cleanup()

	cfg, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatalf("LoadFromFile() error = %v", err)
	}
	if cfg.CurrentContext != "beta" {
		t.Errorf("current-context = %q, want beta -- the file alone must be a complete answer", cfg.CurrentContext)
	}
	if len(cfg.Contexts) != 1 {
		t.Errorf("minified kubeconfig has %d contexts, want 1", len(cfg.Contexts))
	}
}

func TestSessionDirectoryIsGoneAfterCleanup(t *testing.T) {
	work := t.TempDir()
	path, cleanup, err := writeSessionKubeconfig(work, writeKubeconfig(t), "alpha")
	if err != nil {
		t.Fatalf("writeSessionKubeconfig() error = %v", err)
	}
	cleanup()
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Error("the session directory survived cleanup -- flattened credentials would outlive the process")
	}
}

// A SIGKILL leaves the directory behind and the next launch is the only thing
// that will ever come looking. A live PID's directory is left alone: deleting
// another process's kubeconfig mid-Apply would break a running install.
func TestSweepRemovesDeadSessionsAndSparesLiveOnes(t *testing.T) {
	work := t.TempDir()
	dead := filepath.Join(work, "session-0")
	live := filepath.Join(work, "session-"+strconv.Itoa(os.Getpid()))
	for _, d := range []string{dead, live} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
	}
	sweepStaleSessions(work)
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Error("a dead session directory survived the sweep")
	}
	if _, err := os.Stat(live); err != nil {
		t.Error("the sweep removed a live process's session directory")
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/console/ -run 'Lock|Session|Sweep' -v`

Expected: compile failures on all three new functions.

- [ ] **Step 3: Implement the lock**

Create `internal/console/lock.go`. Use `O_CREATE|O_EXCL` rather than `flock`: it is portable across the two platforms this ships on, it survives an NFS-mounted home directory better, and the file's contents carry the diagnostic. Liveness is `syscall.Kill(pid, 0)`.

Key comment to carry:

```go
// acquireLock takes exclusive ownership of workDir for this process.
//
// Two aicrme processes sharing a work directory write the same run record,
// and against the same cluster they also drive the same install. The
// in-cluster console never had this problem -- one Deployment, one replica,
// and cmstore.resolveDeploymentOwner detected a record written by a different
// deployment. That file is deleted in this restructure, so this replaces the
// guard it carried rather than merely dropping it.
//
// A stale lock is REPORTED, not cleared. A live second process and a crashed
// first one look identical from the file alone, and guessing wrong is exactly
// the case this exists to prevent.
//
// This is local exclusion, not distributed. Two operators on two laptops
// installing into the same cluster is not something a file lock can see, and
// this does not try: Apply's idempotence and AICR's own release-level
// behavior are what stand between that case and damage.
```

- [ ] **Step 4: Implement the session directory**

Create `internal/console/session.go`. Flatten and minify with `clientcmd.MinifyConfig` and `clientcmd.FlattenConfig` over a config whose `CurrentContext` is set to the chosen context, then `clientcmd.WriteToFile`.

Key comment:

```go
// writeSessionKubeconfig freezes the chosen context into a single-context
// kubeconfig for the life of this process.
//
// Per-launch and deleted on shutdown, not a fixed <workdir>/kubeconfig.
// Flattening inlines whatever the source context held -- a bearer token, a
// client certificate and key, a cached OIDC id_token -- so a fixed path would
// leave live credentials on disk after the process exits, indefinitely, which
// contradicts what the README tells the operator: that the binary holds their
// credentials for as long as it runs. (An exec-based context minifies to a
// stanza rather than a secret and is the benign case; a context holding a
// bearer token or a client key is not, and this cannot know which it got.)
//
// The file rather than a --context flag: it removes the question of whether
// every tool in the chain supports a context flag and spells it the same way
// (helm --kube-context, kubectl --context), and it makes the run immune to
// the operator running `kubectl config use-context` mid-Apply -- which with
// an ambient kubeconfig would silently redirect an in-flight install at the
// next helm invocation. The in-cluster console got that property free from
// its ServiceAccount.
```

- [ ] **Step 5: Wire both into `console.Run`**

Both cleanups must run on the signal path, not as bare `defer`s in `main()` — the signal handler cancels `ctx` and `Run` returns through its own shutdown sequence, so register them inside `Run` after the shutdown `wg.Wait()`:

```go
sweepStaleSessions(opts.WorkDir)
releaseLock, err := acquireLock(opts.WorkDir)
if err != nil {
	return err
}
defer releaseLock()
```

- [ ] **Step 6: Run the tests**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/console/ -race -v`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/console/lock.go internal/console/session.go internal/console/lock_test.go internal/console/session_test.go internal/console/console.go
git commit -S -m "feat(console): one process per work directory, and credentials that die with it

Nothing in this repo has ever taken a lock. The one multi-writer guard it had
was cmstore.resolveDeploymentOwner, which stamped an ownerReference so a
record written by a different deployment was detected rather than silently
overwritten -- and the local model makes that case likelier, not rarer: a
second aicrme is one keystroke in a second terminal, where a second console
Deployment took a deliberate helm install under a new release name.

A stale lock is reported with its path and PID rather than cleared. A live
second process and a crashed first one look identical from the file alone.

The frozen kubeconfig moves to a per-launch session-<pid>/ directory, removed
on shutdown and swept on the next start if a SIGKILL left it behind. A fixed
path would leave flattened credentials -- tokens, client keys, cached OIDC --
on disk indefinitely after the process exits."
```

---

## Task 8: The launch token and the session cookie

**Files:**
- Create: `internal/api/token.go`, `internal/api/token_test.go`
- Delete: `internal/api/auth.go`, `internal/api/auth_test.go`, `internal/api/auth_internal_test.go`
- Modify: `internal/api/server.go`, `internal/api/csrf_test.go`
- Keep unchanged: `internal/api/jar_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `api.Config.Token string`; `POST /api/session`; `Server.requireToken(http.Handler) http.Handler`.

**Background and the trap:** revision 2 of the spec proposed a request header. That cannot work. The SPA's timeline is a native `EventSource` (`web/src/useEvents.ts`), which has no API for request headers — and `internal/api/server.go:160` already says so in a comment, as the reason safe methods are exempt from the same-origin check. A header token also does not survive a page refresh. So: the token arrives once in the URL, is exchanged for an `HttpOnly` cookie, and the cookie authenticates everything afterwards including the stream.

**`jar_test.go` stays.** Revision 2 deleted it on the reasoning that it exists to drive the session cookie and the cookie was going. The cookie is not going. It also defines `newRecorder()`, used elsewhere in the package.

- [ ] **Step 1: Write the failing tests**

Create `internal/api/token_test.go`:

```go
package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPostSessionExchangesTheTokenForACookie(t *testing.T) {
	ts, _ := newTestServer(t, "launch-token-value")
	defer ts.Close()

	client := newJarClient(t, ts)
	resp, err := client.Post(ts.URL+"/api/session", "application/json", strings.NewReader(`{"token":"launch-token-value"}`))
	if err != nil {
		t.Fatalf("POST /api/session error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}

	probe, err := client.Get(ts.URL + "/api/session")
	if err != nil {
		t.Fatalf("GET /api/session error = %v", err)
	}
	defer func() { _ = probe.Body.Close() }()
	if probe.StatusCode != http.StatusNoContent {
		t.Errorf("probe status = %d, want 204 -- the cookie did not authenticate", probe.StatusCode)
	}
}

func TestPostSessionRejectsTheWrongToken(t *testing.T) {
	ts, _ := newTestServer(t, "launch-token-value")
	defer ts.Close()

	resp, err := newJarClient(t, ts).Post(ts.URL+"/api/session", "application/json", strings.NewReader(`{"token":"nope"}`))
	if err != nil {
		t.Fatalf("POST /api/session error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// This is the regression revision 2's header design would have shipped: the
// event stream is a native EventSource, which cannot attach a header, so the
// cookie is the only thing that can authenticate it.
func TestTheEventStreamAuthenticatesByCookie(t *testing.T) {
	ts, _ := newTestServer(t, "launch-token-value")
	defer ts.Close()

	client := newJarClient(t, ts)
	if _, err := client.Post(ts.URL+"/api/session", "application/json", strings.NewReader(`{"token":"launch-token-value"}`)); err != nil {
		t.Fatalf("POST /api/session error = %v", err)
	}

	// No custom headers, exactly as EventSource would issue it.
	resp, err := client.Get(ts.URL + "/api/events")
	if err != nil {
		t.Fatalf("GET /api/events error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
}

func TestUnauthenticatedRequestsAreRejected(t *testing.T) {
	ts, _ := newTestServer(t, "launch-token-value")
	defer ts.Close()

	for _, path := range []string{"/api/events", "/api/runs", "/api/contexts"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s error = %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s status = %d, want 401", path, resp.StatusCode)
		}
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/api/ -run TestPostSession -v`

Expected: failures — the routes do not exist.

- [ ] **Step 3: Write the token middleware**

Create `internal/api/token.go` with `POST /api/session` (constant-time compare via `crypto/subtle`, then `http.SetCookie` with `HttpOnly: true`, `SameSite: http.SameSiteStrictMode`, `Path: "/"`, no `Secure` — loopback is plain HTTP and the same-origin wrapper is the cross-origin guard) and `requireToken`.

Carry this comment:

```go
// The launch token authenticates one browser on one machine for the life of
// one process. It is not a credential, has no expiry of its own, and dies
// when the process does.
//
// It arrives once in the URL and is immediately exchanged for a cookie,
// rather than being held in memory and sent as a header, for two reasons that
// are both hard constraints rather than preferences:
//
//   - GET /api/events is a native EventSource (web/src/useEvents.ts), and
//     EventSource has no API for request headers. server.go's own comment on
//     requireSameOrigin already records this. A header token simply cannot
//     reach the timeline.
//   - A token held in memory does not survive a page refresh or a restored
//     tab, which would drop the operator to a dead screen mid-Apply with the
//     only copy of the token in a terminal they may have scrolled past.
//
// Putting the token in the EventSource URL instead would leak a live
// credential into browser history, the referrer on any outbound link, and
// this repo's own request logging.
```

- [ ] **Step 4: Delete the password auth**

```bash
git rm internal/api/auth.go internal/api/auth_test.go internal/api/auth_internal_test.go
```

In `server.go`: drop `Username`, `Password`, `SessionTTL`, `LoginRate`, `TLS` from `Config` and their validation from `New`; add `Token string` with a non-empty check. Drop the `POST /api/login` and `POST /api/logout` routes. Replace `s.auth.require(protected)` with `s.requireToken(s.requireConnected(protected))` — token first, then the state gate, so an unauthenticated caller learns nothing about connection state.

Keep `GET /api/session` as the 204 probe, and update its comment: the cookie no longer expires, but the process it belongs to can exit, and the SPA still needs to tell "server gone" from "reconnecting".

- [ ] **Step 5: Update `csrf_test.go`**

`loggedInClient` becomes a tokenized one — it posts the launch token and returns a jar-backed client. Do not change a single assertion; the same-origin behavior under test is unchanged.

- [ ] **Step 6: Generate the token in `console.Run`**

```go
var raw [32]byte
if _, err := rand.Read(raw[:]); err != nil {
	return fmt.Errorf("generating the launch token: %w", err)
}
token := base64.RawURLEncoding.EncodeToString(raw[:])
```

Refuse a non-loopback `--addr` before binding, print the tokenized URL to stdout unconditionally — whether or not the open succeeds and whether or not `--open` was passed, so a headless or SSH session is never a dead end — and open the browser if `opts.OpenBrowser`.

- [ ] **Step 7: Run the tests**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/api/ -race -v`

Expected: PASS, including every retained `csrf_test.go` case.

- [ ] **Step 8: Commit**

```bash
git add -A
git commit -S -m "feat(api): a loopback launch token, exchanged once for a session cookie

Deletes the password, the login form, the 8-hour session TTL, the rate
limiter and the TLS toggle -- a cluster-admin console reachable over a Service
needed all of them; a process on the operator's machine needs none.

What replaces them is not a header. GET /api/events is a native EventSource,
which has no API for request headers -- server.go's own comment already
recorded this, as the reason safe methods are exempt from the same-origin
check. A header token could not have reached the timeline at all, and would
not have survived a page refresh either. So the token arrives once in the URL
and is exchanged for an HttpOnly SameSite=Strict cookie that lives exactly as
long as the process.

jar_test.go stays: the cookie it exists to hold is still here, and it defines
newRecorder() for the rest of the package. csrf_test.go stays and keeps every
assertion -- the same-origin check is now load-bearing rather than defense in
depth, since it is what stops a cross-origin page riding the cookie."
```

---

## Task 9: Toolchain preflight

**Files:**
- Create: `internal/console/preflight.go`, `internal/console/preflight_test.go`
- Modify: `internal/console/console.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `type Toolchain map[string]string`, `func preflight() (Toolchain, error)`.

**Background:** the deleted image supplied four executables — `Dockerfile:44` is `apk add --no-cache bash ca-certificates curl jq tar` plus the helm and kubectl fetch, and the comment above it says the console "shells out to the bundle's deploy.sh, which needs bash, helm, kubectl, and jq (the webhook preflight degrades without jq)". `applier.Apply` execs `bash` by name (`internal/applier/applier.go:83`), not via a shebang. `curl` and `tar` were build-time only and the same `RUN` removes them.

**Do not fail closed on helm 4.** Spec revision 5 withdrew that: `helmLister.List` has used explicit per-status flags rather than `--all` since commit `e36b015`.

- [ ] **Step 1: Write the failing tests**

```go
func TestPreflightFailsWhenBashIsMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // an empty PATH: nothing resolves
	_, err := preflight()
	if err == nil {
		t.Fatal("preflight() succeeded with no bash on PATH")
	}
	if !strings.Contains(err.Error(), "bash") {
		t.Errorf("the error does not name bash: %v", err)
	}
}

func TestPreflightWarnsButSucceedsWithoutJq(t *testing.T) {
	dir := t.TempDir()
	for _, tool := range []string{"bash", "helm", "kubectl"} {
		stubTool(t, dir, tool, "v1.2.3")
	}
	t.Setenv("PATH", dir)

	tc, err := preflight()
	if err != nil {
		t.Fatalf("preflight() error = %v -- a missing jq degrades, it does not block", err)
	}
	if _, ok := tc["jq"]; ok {
		t.Error("jq was recorded despite being absent")
	}
}

func TestPreflightRecordsEveryResolvedVersion(t *testing.T) {
	dir := t.TempDir()
	for _, tool := range []string{"bash", "jq", "helm", "kubectl"} {
		stubTool(t, dir, tool, "v9.9.9")
	}
	t.Setenv("PATH", dir)

	tc, err := preflight()
	if err != nil {
		t.Fatalf("preflight() error = %v", err)
	}
	for _, tool := range []string{"bash", "jq", "helm", "kubectl"} {
		if tc[tool] == "" {
			t.Errorf("%s has no recorded version -- a run's evidence must be able to answer 'which helm installed this'", tool)
		}
	}
}

// Helm 4 is not a blocker. helmLister.List has used explicit per-status flags
// rather than --all since e36b015, "list helm releases in a way both helm
// majors accept".
func TestPreflightDoesNotBlockOnHelm4(t *testing.T) {
	dir := t.TempDir()
	for _, tool := range []string{"bash", "jq", "kubectl"} {
		stubTool(t, dir, tool, "v1.0.0")
	}
	stubTool(t, dir, "helm", "v4.0.1")
	t.Setenv("PATH", dir)

	tc, err := preflight()
	if err != nil {
		t.Fatalf("preflight() refused helm 4: %v", err)
	}
	if !strings.Contains(tc["helm"], "4.0.1") {
		t.Errorf("helm version = %q, want the reported 4.0.1", tc["helm"])
	}
}
```

`stubTool` writes an executable shell script to `dir` that echoes the version string.

- [ ] **Step 2: Run to verify they fail**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/console/ -run TestPreflight -v`

Expected: compile failure — `undefined: preflight`.

- [ ] **Step 3: Implement**

`exec.LookPath` each tool, then run its version subcommand (`bash --version`, `jq --version`, `helm version --template '{{.Version}}'`, `kubectl version --client -o json`) with a short timeout. Missing `bash`, `helm` or `kubectl` is an error naming the tool and what needs it. Missing `jq` is a `slog.Warn` naming what degrades.

Carry the policy comment:

```go
// Missing is fatal for bash, helm and kubectl; missing jq is a warning.
// Version skew is a warning in every case, and every resolved version is
// recorded on the run and surfaced in the evidence bundle.
//
// Refusing to start because an operator has helm 3.20 rather than the 3.19.0
// the deleted Dockerfile pinned would make the tool unusable for the
// reproducibility it was meant to protect. For a product whose output is
// evidence, the honest way to serve "correctness must be reproducible" is to
// RECORD the toolchain that produced the result, not to block on it -- and
// today, with the version baked into an image, nothing ever asks.
//
// bash is not optional and not sh: applier.Apply builds
// Argv: []string{"bash", "deploy.sh", ...} -- an explicit interpreter, not a
// shebang -- so a machine without bash fails at exec with a message about a
// missing file rather than a missing shell. deploy.sh is AICR-generated and
// this repo does not control whether it stays POSIX-clean.
//
// curl and tar are deliberately absent: the Dockerfile used them to fetch
// helm and kubectl and the same RUN removed them. Neither is a runtime
// dependency. CA certificates are a host property rather than a PATH lookup,
// and a machine with no trust store fails at the first HTTPS call with a
// clear TLS error.
```

- [ ] **Step 4: Thread the result onto the run record**

Add `Toolchain map[string]string \`json:"toolchain,omitempty"\`` to `engine.Run` and populate it when a run starts, so the evidence bundle carries it.

- [ ] **Step 5: Run the tests**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/console/ ./internal/engine/ -race -v`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add -A
git commit -S -m "feat(console): preflight the four executables the image used to supply

Dockerfile:44 installed bash, jq, helm and kubectl, and the comment above it
named why: the console shells out to the bundle's deploy.sh, which needs all
four. An earlier draft of this restructure checked two.

bash is not optional and not sh -- applier.Apply execs it by name rather than
relying on a shebang, so a machine without it fails with a message about a
missing file. jq degrades the webhook preflight rather than breaking it, so a
missing jq warns and names what degrades.

Every resolved version is recorded on the run and travels in the evidence
bundle. Nothing fails closed on version skew, including helm 4: recording the
toolchain that produced a result serves reproducibility better than refusing
to run, and helmLister has accepted both helm majors since e36b015."
```

---

## Task 10: Pin all four cluster consumers

**Files:**
- Modify: `internal/console/console.go`
- Modify: `internal/applier/applier.go:114-126` (`env`)
- Modify: `internal/steps/discover.go` (`DiscoverConfig.Kubeconfig`, `AgentConfig.Kubeconfig`)
- Test: `internal/applier/applier_test.go`, `internal/steps/discover_test.go`

**Interfaces:**
- Consumes: the session kubeconfig path from Task 7.
- Produces: `applier.Options.Kubeconfig string`; `steps.DiscoverConfig.Kubeconfig string`.

**Background — the table that matters:**

| Consumer | How it resolves today | What pins it |
|---|---|---|
| client-go clientset — observer, prove, teardown | `rest.InClusterConfig()` | The `rest.Config` from the selected context (Task 5). |
| `deploy.sh` → `install.sh` → helm/kubectl | ServiceAccount token in-pod | `KUBECONFIG` and `KUBECONFIG_FLAG` in `applier.env()`. |
| **AICR's client, `CollectSnapshot`** | **its own resolution — `KUBECONFIG`, else `~/.kube/config`, else in-cluster** | **`AgentConfig.Kubeconfig`**, threaded from a new `DiscoverConfig.Kubeconfig`. |
| **`steps.helmLister`** | **inherits `os.Environ()`; sets no `Env` at all** | **`KUBECONFIG` in the aicrme process itself.** |

The third is the dangerous one: empty means Discover snapshots whatever `~/.kube/config` points at while Apply installs into the selected context — a recipe generated for one cluster, installed into another, silently. The fourth is why the pin cannot live in `applier.env()` alone: `helmLister.List` builds an `applier.Spec` with `Argv` and no `Env` (`internal/steps/ownership.go:149`), and its result is what `Run.Ownership` is built from.

- [ ] **Step 1: Write the failing tests**

```go
func TestApplierEnvCarriesTheSessionKubeconfig(t *testing.T) {
	a := New(BashExec{})
	env := a.env(Options{Kubeconfig: "/tmp/session-42/kubeconfig"})

	var sawPath, sawFlag bool
	for _, kv := range env {
		if kv == "KUBECONFIG=/tmp/session-42/kubeconfig" {
			sawPath = true
		}
		if kv == "KUBECONFIG_FLAG=--kubeconfig /tmp/session-42/kubeconfig" {
			sawFlag = true
		}
	}
	if !sawPath {
		t.Error("KUBECONFIG is not exported to deploy.sh")
	}
	if !sawFlag {
		t.Error("KUBECONFIG_FLAG is not set -- deploy.sh consumes it and would fall back to ambient config")
	}
}

func TestDiscoverPinsTheAgentConfigKubeconfig(t *testing.T) {
	client := &fakeAICR{}
	d := NewDiscover(client, DiscoverConfig{
		Namespace:  "aicrme",
		Image:      "ghcr.io/nvidia/aicr:v0.19.0",
		Kubeconfig: "/tmp/session-42/kubeconfig",
	})
	if err := d.Run(context.Background(), &engine.Run{ID: "abcdef0123456789"}, func(bus.Event) {}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if client.lastConfig.Kubeconfig != "/tmp/session-42/kubeconfig" {
		t.Errorf("AgentConfig.Kubeconfig = %q -- empty means AICR resolves ~/.kube/config itself, "+
			"so Discover would snapshot one cluster while Apply installs into another",
			client.lastConfig.Kubeconfig)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/applier/ ./internal/steps/ -run 'Kubeconfig' -v`

Expected: FAIL — the fields do not exist.

- [ ] **Step 3: Implement**

`applier.env` gains:

```go
kubeconfigFlag := ""
if opts.Kubeconfig != "" {
	kubeconfigFlag = "--kubeconfig " + opts.Kubeconfig
}
return []string{
	"NO_COLOR=1",
	"DRY_RUN_FLAG=" + dryRun,
	"KUBECONFIG=" + opts.Kubeconfig,
	"KUBECONFIG_FLAG=" + kubeconfigFlag,
	"HELM_DEBUG_FLAG=",
}
```

`DiscoverConfig` gains `Kubeconfig string`, forwarded to `aicr.AgentConfig{Kubeconfig: d.cfg.Kubeconfig}` with this comment:

```go
// Kubeconfig is set explicitly rather than left to AICR's own resolution.
// AgentConfig.Kubeconfig is documented as "the path (or empty for
// in-cluster)", and empty was exactly right in a pod. Locally, empty means
// AICR reads KUBECONFIG, else ~/.kube/config -- so Discover would snapshot
// whatever the operator's ambient config points at while Apply installs into
// the context they selected. That is a recipe generated for one cluster and
// installed into another, with nothing in the timeline saying so.
```

In `console.Run`, after the session kubeconfig is written:

```go
// KUBECONFIG in this process covers three of the four consumers by the
// mechanism each already uses: deploy.sh inherits it through applier.env,
// AICR's client reads it during resolution, and steps.helmLister -- which
// builds an applier.Spec with no Env at all -- inherits it from os.Environ.
//
// Process-global mutation is ugly. It is also precisely how these libraries
// expect to be told, and the alternative is threading a path through four
// call chains to reach code that will read the environment variable anyway.
//
// AgentConfig.Kubeconfig is ALSO set explicitly, even though the variable
// above would cover it: that seam has a real field, and relying on ambient
// environment for the one call that decides the recipe is not worth the
// economy.
if err := os.Setenv("KUBECONFIG", sessionKubeconfig); err != nil {
	return fmt.Errorf("pinning KUBECONFIG for this process: %w", err)
}
```

This must run before any cluster work begins.

- [ ] **Step 4: Run the tests**

Run: `GOTOOLCHAIN=go1.26.5 make qualify`

Expected: green.

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -S -m "feat: pin all four cluster consumers to the selected context

Four things in this binary independently decide which cluster they talk to,
and an earlier draft addressed two. AICR's client resolves its own kubeconfig
path -- KUBECONFIG, else ~/.kube/config, else in-cluster -- and
DiscoverConfig never set AgentConfig.Kubeconfig, which was exactly right in a
pod. Locally it means Discover snapshots one cluster while Apply installs
into another, producing a recipe for the wrong cluster with nothing in the
timeline saying so.

steps.helmLister is why the pin cannot live in applier.env alone: List builds
an applier.Spec with Argv and no Env, inheriting os.Environ. Its result is
what Run.Ownership is built from, so a lister pointed at the wrong cluster
makes Reset's ownership record wrong.

So KUBECONFIG is set in the process itself, covering three consumers by the
mechanism each already uses, and AgentConfig.Kubeconfig is set explicitly on
top -- that seam has a real field, and the one call that decides the recipe
should not depend on ambient environment."
```

---

## Task 11: Discover creates its own namespace

**Files:**
- Modify: `internal/steps/discover.go`
- Modify: `internal/engine/run.go` (record the created namespace)
- Test: `internal/steps/discover_test.go`

**Interfaces:**
- Consumes: `DiscoverConfig` (Task 10).
- Produces: `Run.AgentNamespace struct{ Name string; UID string; Created bool }`.

**Background:** `steps.NewDiscover` passes `Namespace` to AICR's snapshot Job but does not create it (`internal/steps/discover.go:160`). In-cluster, `helm --create-namespace` did. **Do not put this in Connect.** A probe that reports a version and a node count must not write to the cluster — an operator clicking through contexts would leave a namespace on every one they looked at. The precedent is `prove.Client.EnsureNamespace` (`internal/prove/client.go:87`), which creates Prove's namespace when Prove runs.

Note `Ownership.Namespaces[].Existed` cannot carry this: `recipeNamespaces` builds that set from recipe.json's components (`internal/steps/ownership.go:183`), and the agent namespace is not one of them.

- [ ] **Step 1: Write the failing tests**

```go
func TestDiscoverCreatesItsNamespaceAndRecordsThat(t *testing.T) {
	kube := fake.NewSimpleClientset()
	run := &engine.Run{ID: "abcdef0123456789"}
	d := NewDiscover(&fakeAICR{}, DiscoverConfig{Namespace: "aicrme", Kube: kube, Image: "x"})

	if err := d.Run(context.Background(), run, func(bus.Event) {}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if _, err := kube.CoreV1().Namespaces().Get(context.Background(), "aicrme", metav1.GetOptions{}); err != nil {
		t.Fatalf("the namespace was not created: %v", err)
	}
	if !run.AgentNamespace.Created {
		t.Error("Created is false for a namespace this run made -- Reset cannot tell it from one that pre-existed")
	}
}

func TestDiscoverRecordsAPreExistingNamespaceAsNotCreated(t *testing.T) {
	kube := fake.NewSimpleClientset(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "aicrme", UID: "existing-uid"},
	})
	run := &engine.Run{ID: "abcdef0123456789"}
	d := NewDiscover(&fakeAICR{}, DiscoverConfig{Namespace: "aicrme", Kube: kube, Image: "x"})

	if err := d.Run(context.Background(), run, func(bus.Event) {}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if run.AgentNamespace.Created {
		t.Error("a pre-existing namespace was recorded as created by this run")
	}
	if run.AgentNamespace.UID != "existing-uid" {
		t.Errorf("UID = %q, want the pre-existing namespace's", run.AgentNamespace.UID)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/steps/ -run TestDiscover.*Namespace -v`

Expected: FAIL — the namespace is not created and the field does not exist.

- [ ] **Step 3: Implement**

At the top of `discover.Run`, before `CollectSnapshot`, create-or-find the namespace and record the result on the run. Follow `prove.Client.EnsureNamespace`'s idempotence exactly: `IsAlreadyExists` is success, not an error.

- [ ] **Step 4: Report it in Reset's residue**

In `internal/engine/reset.go`, add the agent namespace to the residue when `Created` is true, with the same shape `namespaceResidue` already produces:

```go
// The agent namespace is reported, never removed. AICR's deployer already
// cleans up the Job, ServiceAccount and RoleBinding it created
// (DiscoverConfig.Cleanup is always true); the namespace is what remains.
// Adding teardown code to chase it would put aicrme in the business of
// undoing a deployer's work, which is the line this repo has drawn: the
// deployer owns its cleanup, and aicrme prints what is left.
```

- [ ] **Step 5: Run the tests**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/steps/ ./internal/engine/ -race -v`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add -A
git commit -S -m "feat(discover): create the agent namespace at point of use, not at connect

steps.NewDiscover passes Namespace to AICR's snapshot Job and does not create
it; in-cluster, helm --create-namespace did, and locally nothing does, so
Discover fails on a fresh cluster.

Discover creates it rather than Connect. A probe that reports a server
version and a node count must not also write to the cluster -- an operator
clicking through contexts to see which one they are pointed at would leave a
namespace behind on every cluster they looked at, including ones they never
installed to. Point of use is also what the cited precedent actually does:
prove.Client.EnsureNamespace creates Prove's namespace when Prove runs.

Whether it pre-existed is recorded with its UID, and needs its own field:
Ownership.Namespaces is built from recipe.json's components, and the agent
namespace is not one of them. Reported in Reset's residue when this run
created it, never reclaimed."
```

---

## Task 12: Recovery moves into the connect path

**Files:**
- Modify: `internal/console/console.go`, `internal/console/connect.go`
- Modify: `internal/api/server.go` (connect response carries the recovered run)
- Test: `internal/console/connect_test.go`

**Interfaces:**
- Consumes: `engine.Recover`, `engine.ReconcileWorkloads`, `connector.Connect`.
- Produces: `ClusterInfo.RecoveredRun *engine.Run \`json:"recoveredRun,omitempty"\``.

**Background:** in-cluster, the pod restarting *was* the recovery trigger — `Recover` ran during startup, before the API served anything, because the store was reachable from the moment the process began. Locally it is not: the store lives under a directory named for a cluster the process has not yet chosen. So both calls move into the connect path, after identity is established and before `POST /api/connect` returns.

The three interrupted states keep the semantics the engine already has and tests — this task changes *when* recovery runs, not *what* it does:

| Interrupted at | On the next connect | Existing code |
|---|---|---|
| Apply | Rewound to `PhaseBundle`, lands `StateFailed`; operator retries from the bundle. | `recover.go:215`, `TestRecoverRewindsInterruptedRunAtApply` |
| Prove | Lands `StateFailed`; the orphaned workload is adopted so Stop is offered. | `reconcile.go:95` |
| Reset | `StateResetting` lands `StateFailed` with `Residue.Incomplete`; Start, Retry and Discard all withheld. | `recover.go:260-269`, `TestRecoverTreatsAnInterruptedTeardownAsIncomplete` |

- [ ] **Step 1: Write the failing test**

```go
func TestConnectRecoversTheRunForThatCluster(t *testing.T) {
	work := t.TempDir()
	const uid = "11111111-2222-3333-4444-555555555555"

	// Seed a failed run in the directory this cluster's UID maps to.
	store, err := engine.NewFileStore(runDir(work, uid))
	if err != nil {
		t.Fatalf("NewFileStore() error = %v", err)
	}
	if err := store.Save(context.Background(), &engine.Run{
		ID: "abcdef0123456789", State: engine.StateFailed, ClusterUID: uid,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	info, err := connectTo(t, work, uid)
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	if info.RecoveredRun == nil {
		t.Fatal("connect did not recover the run for this cluster -- in-cluster the pod restart did this, and locally nothing else can")
	}
	if info.RecoveredRun.ID != "abcdef0123456789" {
		t.Errorf("recovered run ID = %q", info.RecoveredRun.ID)
	}
}

func TestConnectDoesNotRecoverAnotherClustersRun(t *testing.T) {
	work := t.TempDir()
	store, _ := engine.NewFileStore(runDir(work, "aaaa-1111"))
	_ = store.Save(context.Background(), &engine.Run{ID: "abcdef0123456789", State: engine.StateFailed, ClusterUID: "aaaa-1111"})

	info, err := connectTo(t, work, "bbbb-2222")
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	if info.RecoveredRun != nil {
		t.Error("connecting to one cluster recovered another's run -- Reset would uninstall releases in the wrong place")
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/console/ -run TestConnectRecover -v`

Expected: FAIL — `RecoveredRun` is always nil.

- [ ] **Step 3: Implement**

In the connect path, after the identity is read and the run directory resolved: build the `FileStore`, construct the engine against it, call `eng.Recover(ctx)` then `eng.ReconcileWorkloads(ctx, proveClient)`, and put the resulting current run on the response.

`Recover`'s `ErrStepConfig` stays fatal — it is a programming error in this binary's own wiring, not a runtime condition. `ReconcileWorkloads` stays never-fatal: an unreachable API server here costs the console its bearings on a leftover workload, which is worth a loud warning and not worth refusing to connect over.

Carry this comment:

```go
// Recover and ReconcileWorkloads run here, not at startup.
//
// In-cluster the pod restarting WAS the trigger: Recover ran during startup,
// before the API served anything, because the store was reachable from the
// moment the process began. Locally it is not -- the store lives under a
// directory named for a cluster this process has not chosen yet. So recovery
// runs after identity is established and before POST /api/connect returns.
//
// This is a further reason the connection is single-assignment: a reconnect
// would have to re-run recovery against a different directory while the
// engine still holds the previous run.
```

- [ ] **Step 4: Run the tests**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/console/ ./internal/engine/ -race -v`

Expected: PASS, including every existing `recover_test.go` and `reset_test.go` case — this task must not change any recovery *semantics*.

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -S -m "feat(console): recover on connect, because the pod restart no longer exists

Recover and ReconcileWorkloads ran at startup, before the API served
anything, because in a pod the store was reachable from the first instruction
and a restart was itself the trigger. Locally neither holds: the store lives
under a directory named for a cluster the process has not chosen yet.

Both move into the connect path, after identity is established and before
POST /api/connect returns, and the recovered run travels in the response so
the SPA lands in the same state the pod-restart path used to produce.

Semantics are unchanged and every existing recovery test still passes. An
interrupted Apply still rewinds to the bundle, an interrupted Prove still
leaves an adoptable workload with a Stop button, and an interrupted Reset
still lands failed with Residue.Incomplete and withholds Start, Retry and
Discard until another Reset establishes what is actually there."
```

---

## Task 13: The SPA — Connect screen and session bootstrap

**Files:**
- Create: `web/src/components/Connect.tsx`, `web/src/components/Connect.test.tsx`
- Delete: `web/src/components/Login.tsx`
- Modify: `web/src/App.tsx`, `web/src/api.ts`
- Unchanged: `web/src/useEvents.ts` and its two test files

**Interfaces:**
- Consumes: `GET /api/contexts`, `POST /api/connect`, `POST /api/session`, `GET /api/session`.
- Produces: `<Connect onConnected={(info: ClusterInfo) => void} />`.

**`useEvents.ts` must not change.** That is the payoff of Task 8's cookie: the `EventSource` constructor sends cookies to a same-origin URL with no configuration, so the reconnect, `Last-Event-ID` replay and gap-detection logic — and `useEvents.lifecycle.test.tsx`'s coverage of all three, including the `MAX_GAP_RECONNECT_ATTEMPTS` cap — carry over untouched. If a change to that file seems necessary, the cookie wiring in Task 8 is wrong.

- [ ] **Step 1: Write the failing tests**

```tsx
it('exchanges a ?t= token for a session and then shows Connect', async () => {
  window.history.replaceState({}, '', '/?t=launch-token-value')
  const postSession = vi.fn().mockResolvedValue(undefined)
  vi.mocked(api.establishSession).mockImplementation(postSession)
  vi.mocked(api.fetchContexts).mockResolvedValue([
    { name: 'alpha', server: 'https://alpha.example:6443', current: true },
  ])

  render(<App />)

  await waitFor(() => expect(postSession).toHaveBeenCalledWith('launch-token-value'))
  expect(window.location.search).toBe('')
  expect(await screen.findByText('alpha')).toBeInTheDocument()
})

it('authenticates on a reload with no token in the URL', async () => {
  window.history.replaceState({}, '', '/')
  const probe = vi.fn().mockResolvedValue(true)
  vi.mocked(api.probeSession).mockImplementation(probe)
  vi.mocked(api.fetchContexts).mockResolvedValue([
    { name: 'alpha', server: 'https://alpha.example:6443', current: true },
  ])

  render(<App />)

  await waitFor(() => expect(probe).toHaveBeenCalled())
  expect(api.establishSession).not.toHaveBeenCalled()
  expect(await screen.findByText('alpha')).toBeInTheDocument()
})

it('shows the cluster and toolchain the operator is about to install into', async () => {
  vi.mocked(api.connect).mockResolvedValue({
    context: 'alpha', server: 'https://alpha.example:6443', version: 'v1.31.4',
    nodeCount: 6, uid: '1111-2222',
    toolchain: { helm: 'v3.19.0', kubectl: 'v1.31.0', bash: '5.2.15', jq: '1.7' },
  })
  render(<Connect onConnected={() => {}} />)

  await userEvent.click(await screen.findByRole('button', { name: /connect/i }))

  expect(await screen.findByText(/v1\.31\.4/)).toBeInTheDocument()
  expect(await screen.findByText(/6 nodes/)).toBeInTheDocument()
  expect(await screen.findByText(/v3\.19\.0/)).toBeInTheDocument()
})

it('returns to Connect on a 409 from any run route', async () => {
  vi.mocked(api.startRun).mockRejectedValue(new ApiError(409, 'connect to a cluster first'))
  render(<App />)
  expect(await screen.findByRole('heading', { name: /connect/i })).toBeInTheDocument()
})
```

- [ ] **Step 2: Run to verify they fail**

Run: `npm --prefix web test -- Connect`

Expected: FAIL — `Connect` does not exist, `api.establishSession` is undefined.

- [ ] **Step 3: Add the API client functions**

In `web/src/api.ts`:

```ts
/**
 * establishSession exchanges the launch token for a session cookie.
 *
 * Called once on load with the ?t= value, which App then strips from the
 * visible URL. Everything afterwards -- including the EventSource timeline,
 * which cannot attach request headers -- authenticates by the cookie this
 * sets. See internal/api/token.go.
 */
export async function establishSession(token: string): Promise<void> {
  const res = await fetch('/api/session', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ token }),
  })
  if (!res.ok) throw new ApiError(res.status, 'This launch token was not accepted')
}

/**
 * probeSession reports whether the cookie from a previous load is still good.
 *
 * This is the reload path: a restored tab has no ?t= in its URL, and the
 * cookie is the only thing that can authenticate it. It also still does its
 * original job of telling "server gone" from "reconnecting", because
 * EventSource surfaces no HTTP status on error.
 */
export async function probeSession(): Promise<boolean> {
  const res = await fetch('/api/session')
  return res.status === 204
}

export interface ContextInfo {
  name: string
  server: string
  current: boolean
}

export async function fetchContexts(): Promise<ContextInfo[]> {
  const res = await fetch('/api/contexts')
  if (!res.ok) throw new ApiError(res.status, 'Failed to read your kubeconfig')
  return res.json()
}

export interface ClusterInfo {
  context: string
  server: string
  version: string
  nodeCount: number
  uid: string
  toolchain: Record<string, string>
  recoveredRun?: Run
}

export async function connect(contextName: string): Promise<ClusterInfo> {
  const res = await fetch('/api/connect', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ context: contextName }),
  })
  if (!res.ok) throw new ApiError(res.status, 'Failed to connect to that cluster')
  return res.json()
}
```

Delete `login()`.

- [ ] **Step 4: Write `Connect.tsx` and rewire `App.tsx`**

`App.tsx`'s gate goes from `authed ? <Console/> : <Login/>` to a three-state bootstrap: `authenticating` → `connecting` → `console`. On mount, read `?t=`, `establishSession` if present (then `history.replaceState` to strip it), else `probeSession`. Then render `<Connect>` until it reports a connection.

The API client's 401 handler is replaced by a 409 handler that returns to `<Connect>`.

- [ ] **Step 5: Delete Login**

```bash
git rm web/src/components/Login.tsx
```

- [ ] **Step 6: Run the SPA tests**

Run: `npm --prefix web test`

Expected: PASS, including both `useEvents` test files with zero changes to them.

- [ ] **Step 7: Commit**

```bash
git add -A
git commit -S -m "feat(web): a Connect screen, and a session that survives a reload

App gated on a password; it now gates on a cluster. Connect lists the
kubeconfig's contexts, shows each one's server, and on success reports the
server version, node count, cluster UID and the resolved bash/jq/helm/kubectl
versions -- which is also the operator's confirmation that they are about to
install into the cluster they think they are.

Bootstrap gains one step before that: read ?t= from the URL, POST it to
/api/session, and strip it with history.replaceState. With no ?t= -- a
reload, a restored tab, a retyped URL -- it probes GET /api/session instead,
because the cookie from the original load is still there. That is the case an
in-memory token could not serve.

useEvents.ts is untouched, which is the point of the cookie: EventSource
sends cookies to a same-origin URL with no configuration, so the reconnect,
replay and gap-detection logic and all of its coverage carry over as-is."
```

---

## Task 14: The deletions

Everything this restructure exists to remove. Do it in one commit so the tree is never half-converted.

**Files:**
- Delete: `charts/aicrme/` (10 files), `test/chart/contract.sh`, `Dockerfile`, `.dockerignore`, `internal/engine/cmstore.go`, `internal/engine/cmstore_test.go`, `scripts/demo-remote.sh`
- Modify: `Makefile`, `README.md`, `internal/console/console.go`

**Interfaces:**
- Consumes: `engine.NewFileStore` (Task 2) is now the only store.
- Produces: nothing new.

- [ ] **Step 1: Delete**

```bash
git rm -r charts/aicrme test/chart Dockerfile .dockerignore scripts/demo-remote.sh
git rm internal/engine/cmstore.go internal/engine/cmstore_test.go
```

- [ ] **Step 2: Remove the dead constants and helpers from `console.go`**

Delete `defaultWorkDir`, `runStoreSuffix`, `deploymentLookupTimeout`, `resolveDeploymentOwner`, `newRunStore`, the `AICRME_DEPLOYMENT_NAME` read, and from `workSubdirs` drop `home`, `helm/cache`, `helm/config`, `helm/data` and `kube/cache`, leaving `tmp`, `runs` and `bundles`.

Add this where `workSubdirs` is now defined:

```go
// The helm and kube cache redirections are gone deliberately, not by
// oversight. They existed because readOnlyRootFilesystem: true left an
// emptyDir as the container's one writable path. Locally, redirecting them
// would be actively WRONG: the operator's real helm configuration may hold
// private chart repository credentials and registry auth that the install
// needs, and pointing HELM_* at a scratch directory would hide it.
```

- [ ] **Step 3: Re-justify the shutdown constants**

`runShutdownTimeout`'s comment argues from `terminationGracePeriodSeconds: 45` and `test/chart/contract.sh`. Both are gone. The underlying arithmetic is unchanged and still correct — `killGrace` (10s) plus `terminalSaveTimeout` (5s) is roughly 15s, and 30s gives real headroom — so keep the value and rewrite the justification against a local process. Delete the probe-window arithmetic from the observer goroutine's comment and the "PID 1 under the image's ENTRYPOINT" rationale from the shutdown block.

Keep the shutdown *sequence*: draining before cancelling, and reaping the `deploy.sh` process tree before returning, are both still correct. `applier/exec.go` needs no change — `Setpgid: true` already puts `deploy.sh` in its own process group, so a `Ctrl-C` delivered to the terminal's foreground group does not reach `helm` directly.

- [ ] **Step 4: Update the Makefile**

Remove the `image` and `test-chart` targets. Drop `test-chart` from `qualify`. Rewrite `demo` to keep the Kind + KWOK cluster setup, drop the chart install and password printing, and end by running the binary.

- [ ] **Step 5: Update the README's Security section**

`cluster-admin` stops being something the product grants itself via a `ClusterRoleBinding` and becomes "it has exactly what your kubeconfig has" — which needs no justification because it is not a grant. Add the exec-credential consequence: the binary acts against the cluster with whatever credentials the operator holds, and holds them for as long as it runs. The remaining disclosure is AICR's privileged snapshot agent, which is existing AICR behavior.

- [ ] **Step 6: Run the full gate**

Run: `GOTOOLCHAIN=go1.26.5 make qualify`

Expected: green, with `test-chart` no longer in the list.

- [ ] **Step 7: Commit**

```bash
git add -A
git commit -S -m "refactor: delete the chart, the image, and the ConfigMap run store

The delivery model being replaced, and everything that existed only to
survive it: the emptyDir cache redirections that readOnlyRootFilesystem
forced, the contract script pinning three Go constants against chart probe
windows, and the run store whose whole design problem was a pod that could be
rescheduled mid-Apply.

The helm and kube cache redirections are gone deliberately. Locally,
redirecting them would be actively wrong -- the operator's real helm
configuration may hold private chart repository credentials the install needs.

runShutdownTimeout keeps its value and loses its justification: the
arithmetic behind 30s is unchanged, but it argued from a
terminationGracePeriodSeconds that no longer exists. The shutdown sequence
stays exactly as it was -- draining before cancelling, and reaping the
deploy.sh process tree before returning, are both still correct."
```

---

## Task 15: e2e and demo against a local binary

**Files:**
- Create: `test/e2e/lib/console.sh`
- Modify: `test/e2e/{apply-dryrun,apply-real,discover-recommend,prove,reset,smoke}.sh`
- Modify: `Makefile` (`demo`, `demo-down`, `demo-status`)

**Interfaces:**
- Consumes: the binary's stdout contract — the tokenized URL is printed unconditionally.
- Produces: `start_console`, `stop_console`, `api` (a curl wrapper carrying the cookie jar).

**Background:** every script currently reaches the console through `kubectl -n aicrme exec deploy/aicrme` or a `Service`. All of that becomes a local address plus a launch token. Two assertions cannot survive: `apply-dryrun.sh:134-148` compares the in-image helm major against the Dockerfile's `HELM_VERSION` through `kubectl exec`, and there is no image, Deployment or Dockerfile left. Delete that block — the property it protected is served better by Task 9's recorded versions, which travel in the evidence bundle rather than a CI log.

- [ ] **Step 1: Write the shared helper**

Create `test/e2e/lib/console.sh`:

```bash
# start_console launches aicrme against the current kubectl context and
# exports CONSOLE_URL plus a cookie jar every later `api` call uses.
#
# The binary prints its tokenized URL to stdout unconditionally -- whether or
# not --open was passed and whether or not the open succeeded -- which is what
# makes it drivable from CI at all.
start_console() {
  CONSOLE_LOG="$(mktemp)"
  CONSOLE_JAR="$(mktemp)"
  export CONSOLE_LOG CONSOLE_JAR

  "${REPO_ROOT}/bin/aicrme" --addr 127.0.0.1:0 --no-open >"${CONSOLE_LOG}" 2>&1 &
  CONSOLE_PID=$!
  export CONSOLE_PID

  local url=""
  for _ in $(seq 1 50); do
    url="$(grep -oE 'http://127\.0\.0\.1:[0-9]+/\?t=[A-Za-z0-9_-]+' "${CONSOLE_LOG}" | head -1 || true)"
    [[ -n "${url}" ]] && break
    sleep 0.2
  done
  [[ -n "${url}" ]] || {
    echo "console did not print a tokenized URL within 10s; log:" >&2
    cat "${CONSOLE_LOG}" >&2
    return 1
  }

  CONSOLE_URL="${url%%/\?t=*}"
  export CONSOLE_URL
  local token="${url##*t=}"

  curl -sf -c "${CONSOLE_JAR}" -X POST "${CONSOLE_URL}/api/session" \
    -H 'Content-Type: application/json' \
    -d "{\"token\":\"${token}\"}" >/dev/null
}

# api issues an authenticated request. Sec-Fetch-Site is set because the
# server's same-origin wrapper rejects mutating requests without it, and a
# non-browser client sends neither that nor Origin.
api() {
  local method="$1" path="$2"
  shift 2
  curl -sf -b "${CONSOLE_JAR}" -X "${method}" \
    -H 'Sec-Fetch-Site: same-origin' \
    "${CONSOLE_URL}${path}" "$@"
}

stop_console() {
  [[ -n "${CONSOLE_PID:-}" ]] || return 0
  kill "${CONSOLE_PID}" 2>/dev/null || true
  wait "${CONSOLE_PID}" 2>/dev/null || true
}
```

- [ ] **Step 2: Convert one script and prove the shape**

Start with `test/e2e/smoke.sh` — the shortest. Replace the chart install and rollout wait with `make build` plus `start_console`, add a connect step, and leave every assertion alone:

```bash
source "${REPO_ROOT}/test/e2e/lib/console.sh"
trap stop_console EXIT

start_console
api POST /api/connect -H 'Content-Type: application/json' \
  -d "{\"context\":\"$(kubectl config current-context)\"}" >/dev/null
```

- [ ] **Step 3: Run it**

Run: `make demo && test/e2e/smoke.sh`

Expected: PASS.

- [ ] **Step 4: Convert the remaining five**

`discover-recommend.sh`, `prove.sh`, `reset.sh`, `apply-dryrun.sh`, `apply-real.sh`. Mechanical after the first: replace the install/rollout preamble with `start_console` plus connect, replace every `kubectl exec deploy/aicrme -- curl …` with `api`, and delete `apply-dryrun.sh:134-148`.

Add a line where that block was:

```bash
# The in-image helm-major assertion lived here. There is no image to compare
# against; the run record now carries every resolved tool version and the
# evidence bundle ships it (internal/console/preflight.go).
```

- [ ] **Step 5: Rework the demo targets**

`make demo` keeps the Kind + KWOK cluster setup, drops the chart install and password printing, and ends by running the binary. `demo-status` reports whether the binary is running and its URL rather than a Service and a password. Remember the KWOK prerequisite: demoing on KWOK needs AICR's simulated GPU nodes or recipe resolution fails.

- [ ] **Step 6: Run the whole gate plus e2e**

Run: `GOTOOLCHAIN=go1.26.5 make qualify && make demo && for f in test/e2e/*.sh; do "$f" || break; done`

Expected: all green.

- [ ] **Step 7: Commit**

```bash
git add -A
git commit -S -m "test(e2e): drive a local binary instead of an in-cluster Deployment

No image build, no kind load, no rollout wait: each script builds the binary,
starts it against the KWOK cluster, reads the tokenized URL off stdout, and
exchanges the token for a cookie jar every later call carries.

Two things could not be ported rather than merely moved. apply-dryrun.sh
asserted the in-image helm major against the Dockerfile's HELM_VERSION
through kubectl exec, and there is no image, Deployment or Dockerfile left --
the property it protected is served better by the recorded tool versions,
which travel in the evidence bundle rather than a CI log. And every script
reached the console through a Deployment or Service, which is what the shared
helper replaces.

Subject-matter assertions are unchanged in all six."
```

---

## Merge

- [ ] **Run the full gate one final time**

Run: `GOTOOLCHAIN=go1.26.5 make qualify`

- [ ] **Merge locally and push straight to main** — no PR, per this repo's pattern.

```bash
git checkout main
git merge --no-ff local-binary -S -m "Merge the local binary: aicrme stops installing itself into the cluster it installs"
git push origin main
```

- [ ] **Wait for the `e2e` workflow on main.** It triggers on push and takes roughly 23 minutes. A merge is not done until it is green.

Run: `gh run list --branch main --limit 4`

- [ ] **Schedule the real-cluster run.** Phase 4's 16/16 on real H100s was earned with the in-cluster console. That result is about the recipe and the bundle and it stands, but it says nothing about whether a laptop-driven install over a VPN behaves the same. This is the one place the restructure spends evidence rather than earning it. Record the outcome in `docs/phase-4-status.md` alongside the existing session.

---

## Self-Review Notes

**Spec coverage.** §1 → Task 4. §2 → Tasks 5, 6, 7, 10, 11. §3 → Task 8. §4 → Tasks 1, 2, 3, 6, 12. §5 → Tasks 7, 9, 14. §6 → Task 14. §7 → Task 13. §8 → every task's test steps plus Task 15. §9 (deferred) → no task, correctly.

**Two spec items deliberately carry no task:**

1. **The helm 4 compatibility test** (§5, §8). It needs a helm 4 binary in CI, which is a workflow change rather than a code change, and it gates the first *release* rather than this merge. File it as an issue at merge time.
2. **Release automation** (§9) — goreleaser, Homebrew, install script, SBOMs. Explicitly deferred until the code works under `make build`.

**Ordering constraint worth restating:** Task 7's work-directory lock and Task 14's deletion of `cmstore.go` are two halves of one guarantee. If the plan is executed out of order, Task 7 must still precede Task 14, or there is a window where the repo has no multi-writer protection at all.

**Task 1 is independently mergeable.** If the restructure stalls, cherry-pick it to `main` on its own — it fixes a defect that reached real hardware and depends on nothing else here.

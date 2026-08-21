# Phase 5 — Reset Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An operator can tear down exactly what one run installed — the Prove workload, the helm releases that run created, and the namespaces it created and left empty — and nothing else.

**Architecture:** Apply records ownership evidence before it installs anything (a `helm list` snapshot and per-namespace UIDs), because `helm upgrade --install` destroys the created-vs-adopted distinction the instant it runs. `engine.Reset` is a backgrounded engine operation with its own epoch and cancellation, driving a new `internal/teardown` package behind the same process seam the applier uses. Anything Reset cannot prove it created is skipped and named, never removed.

**Tech Stack:** Go 1.26.5, `k8s.io/client-go` (fake clientset for tests), helm 3 via a subprocess seam, React + Vitest, bash e2e on Kind + KWOK.

**Spec:** `docs/superpowers/specs/2026-08-21-aicrme-phase-5-reset-design.md`

## Global Constraints

- **Reset is never automatic.** Operator-initiated and operator-confirmed, always. Nothing may trigger it: not a failed run, not a restart, not a timeout, not a discard.
- **Anything Reset cannot prove it created is skipped and named** — never uninstalled or deleted on a guess.
- **Fail closed.** Any error listing, discovering, or reading leaves the object in place and is reported. An unanswered question is not an empty namespace.
- Run the gate as `GOTOOLCHAIN=go1.26.5 make qualify` (local Go 1.27 breaks the pinned golangci-lint; see `docs/phase-3-status.md`).
- No new imports from outside AICR's `pkg/client/v1` surface. `pkg/bundler/deployer/localformat` is off limits (`approach.md` Risk 1).
- Coverage floor 80% aggregate; `-race` on every Go test run.
- Never disable or weaken an existing test to make new code pass.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/engine/run.go` | `ComponentState.Namespace`; `Run.Ownership`; `Run.Residue`; `StateResetting` |
| `internal/engine/envelope.go` | Persist the three new `Run` shapes |
| `internal/engine/reset.go` | `Engine.Reset` — guards, epoch, orchestration, residue bookkeeping |
| `internal/engine/engine.go` | Guard changes only: `isLive`, `validState`, `Start`/`Retry`/`Discard` rejections |
| `internal/teardown/teardown.go` | `helm uninstall` sequencing behind an `Exec` seam |
| `internal/teardown/namespaces.go` | Ownership + discovery-based emptiness + delete |
| `internal/prove/client.go` | Extract `EnsureAbsent` (the delete-then-wait primitive `Stop` currently inlines) |
| `internal/steps/apply.go` | Take the pre-Apply ownership snapshot |
| `internal/api/reset.go` | `POST /api/runs/{id}/reset` with the confirm body |
| `web/src/pipeline.ts` | Operation boundary, teardown statuses, reverse order |
| `web/src/components/Reset.tsx` | Confirm gate listing removals and skips; teardown progress |
| `test/e2e/reset.sh` | Real install → Reset → assert, with a bystander release that must survive |

---

## Task 1: Nested-field parity, before any field is added

The spec's named trap: `ComponentState` is nested inside `Run`, and `TestEnvelopeRoundTripsEveryRunField` walks only `Run`'s top-level exported fields. A field added to `ComponentState` but not to `envelope.go` would persist as empty and pass every test. Close the hole *first*, so the next three tasks cannot fall into it.

**Files:**
- Test: `internal/engine/envelope_test.go:517` (extend `TestEnvelopeRoundTripsEveryRunField`)

**Interfaces:**
- Produces: nothing consumed by later tasks; it is a guard those tasks rely on.

- [ ] **Step 1: Write the failing test**

Add below the existing parity test. `setDistinctFieldValue` already handles String/Int/Bool, so a `ComponentState` populated field-by-field via reflection needs no new kind handling.

```go
// TestEnvelopeRoundTripsEveryComponentStateField is the nested half of
// Ruling 20's parity guard. The top-level test above walks Run's own
// fields, so a field added to ComponentState -- which Run carries as a
// slice -- is invisible to it: it would persist as its zero value, survive
// the whole suite, and surface as a Reset that uninstalls from the wrong
// namespace after a restart. That is the CleanupUnconfirmed defect exactly
// (fix round 2's N1), one level of nesting down.
func TestEnvelopeRoundTripsEveryComponentStateField(t *testing.T) {
	var cs ComponentState
	rv := reflect.ValueOf(&cs).Elem()
	rt := rv.Type()
	for i := range rt.NumField() {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		setDistinctFieldValue(t, rv.Field(i), f.Name)
	}

	in := &Run{
		ID:        "0123456789abcdef",
		State:     StateDone,
		Phase:     PhaseApply,
		StartedAt: time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC),
		UpdatedAt: time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC),
		Components: []ComponentState{cs},
	}

	blob, err := encodeRun(in)
	if err != nil {
		t.Fatalf("encodeRun() error = %v", err)
	}
	out, err := decodeRun(blob)
	if err != nil {
		t.Fatalf("decodeRun() error = %v", err)
	}
	if len(out.Components) != 1 {
		t.Fatalf("decoded Components = %d rows, want 1", len(out.Components))
	}

	outV := reflect.ValueOf(&out.Components[0]).Elem()
	for i := range rt.NumField() {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		want := rv.Field(i).Interface()
		got := outV.Field(i).Interface()
		if !reflect.DeepEqual(want, got) {
			t.Errorf("ComponentState.%s round-tripped as %#v, want %#v -- envelope.go does not carry it",
				f.Name, got, want)
		}
	}
}
```

- [ ] **Step 2: Run to verify it passes today**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/engine/ -run TestEnvelopeRoundTripsEveryComponentStateField -v`
Expected: PASS. `ComponentState`'s four current fields are all carried today; this test exists to fail the moment Task 2 adds a fifth without updating `envelope.go`.

- [ ] **Step 3: Bite-proof**

Temporarily add `Foo string` to `ComponentState` in `internal/engine/run.go`. Re-run the test alone. Confirm it FAILS with `ComponentState.Foo round-tripped as ""`. Remove the field.

- [ ] **Step 4: Commit**

```bash
git add internal/engine/envelope_test.go
git commit -S -m "test(engine): extend envelope parity to ComponentState's own fields"
```

---

## Task 2: `ComponentState.Namespace`

**Files:**
- Modify: `internal/engine/run.go` (the `ComponentState` struct)
- Modify: `internal/engine/envelope.go` (the `envelope.Components` projection is `[]ComponentState`, so verify it carries the field rather than a parallel type)
- Modify: `internal/steps/apply.go:86` (`upsertComponent`)
- Test: `internal/steps/apply_test.go`

**Interfaces:**
- Consumes: `applier.ComponentData.Namespace` (already parsed — `internal/applier/parse.go:89`).
- Produces: `engine.ComponentState.Namespace string` — Tasks 6, 7 and 8 read it.

- [ ] **Step 1: Write the failing test**

```go
// deploy.sh's own header carries the target namespace
// ("┌─ [1/14] cert-manager  →  cert-manager"), applier.ComponentData
// already parses it, and the engine dropped it. Reset needs it: a release
// name alone does not identify a helm release, and the bundle directory
// that would otherwise supply it dies with the pod's emptyDir.
func TestApplyRecordsEachComponentsNamespace(t *testing.T) {
	run := newTestRun()
	emit := trackComponents(run, func(bus.Event) {})

	emit(componentEventFor(t, applier.ComponentData{
		Name: "cert-manager", Namespace: "cert-manager", Index: 1, Total: 14, Status: "started",
	}))
	emit(componentEventFor(t, applier.ComponentData{
		Name: "nfd", Namespace: "node-feature-discovery", Index: 2, Total: 14, Status: "started",
	}))
	// A later status marker carries neither index nor namespace; the header's
	// values must survive it, exactly as Index/Total already do.
	emit(componentEventFor(t, applier.ComponentData{Name: "nfd", Status: "installed"}))

	got := map[string]string{}
	for _, c := range run.Components {
		got[c.Name] = c.Namespace
	}
	want := map[string]string{"cert-manager": "cert-manager", "nfd": "node-feature-discovery"}
	if !maps.Equal(got, want) {
		t.Errorf("component namespaces = %v, want %v", got, want)
	}
}
```

Add this helper alongside it if `apply_test.go` has no equivalent:

```go
func componentEventFor(t *testing.T, d applier.ComponentData) bus.Event {
	t.Helper()
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshaling ComponentData: %v", err)
	}
	return bus.Event{Kind: bus.KindComponent, Component: d.Name, Data: raw}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/steps/ -run TestApplyRecordsEachComponentsNamespace -v`
Expected: FAIL — `component namespaces = map[cert-manager: nfd:]`.

- [ ] **Step 3: Implement**

In `internal/engine/run.go`, add to `ComponentState`:

```go
	// Namespace is the helm release's target namespace, carried from
	// deploy.sh's own per-action header ("[1/14] cert-manager  →
	// cert-manager"). Recorded because Reset addresses a release as
	// (name, namespace) and has no other durable source for the second
	// half: the bundle directory that holds it lives in the pod's emptyDir
	// and is gone after any restart.
	Namespace string `json:"namespace,omitempty"`
```

In `internal/steps/apply.go`'s `upsertComponent`, alongside the existing Index/Total carry-forward:

```go
		if data.Namespace != "" {
			row.Namespace = data.Namespace
		}
```

and set `Namespace: data.Namespace` in the append branch that creates a new row.

- [ ] **Step 4: Run to verify both tests pass**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/steps/ ./internal/engine/ -count=1`
Expected: PASS, including Task 1's nested parity test — which is what proves `envelope.go` carries the new field.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/run.go internal/steps/apply.go internal/steps/apply_test.go
git commit -S -m "feat(engine): record each component's target namespace"
```

---

## Task 3: The ownership snapshot types

**Files:**
- Modify: `internal/engine/run.go`
- Test: `internal/engine/envelope_test.go`

**Interfaces:**
- Produces:
  ```go
  type ReleaseRef struct{ Name, Namespace string }
  type NamespaceRef struct {
      Name     string
      UID      string // empty when the namespace did not exist pre-Apply
      Existed  bool
      SnapshotErr string // non-empty when this namespace could not be snapshotted
  }
  type Ownership struct {
      Releases   []ReleaseRef   // releases present BEFORE Apply ran
      Namespaces []NamespaceRef // namespace state BEFORE Apply ran
  }
  ```
  `Run.Ownership Ownership` — Tasks 4, 6, 7 read it.

- [ ] **Step 1: Write the failing test**

```go
// The ownership snapshot is the only evidence that separates a release this
// console created from one it adopted via `helm upgrade --install`, and it
// is worthless if it does not survive a restart -- Reset runs long after
// Apply, frequently in a different pod.
func TestEnvelopeRoundTripsOwnership(t *testing.T) {
	in := baseRunForEnvelope(t)
	in.Ownership = Ownership{
		Releases: []ReleaseRef{{Name: "gpu-operator", Namespace: "gpu-operator"}},
		Namespaces: []NamespaceRef{
			{Name: "gpu-operator", UID: "ns-uid-1", Existed: true},
			{Name: "kai-scheduler", Existed: false},
			{Name: "monitoring", Existed: false, SnapshotErr: "connection refused"},
		},
	}

	blob, err := encodeRun(in)
	if err != nil {
		t.Fatalf("encodeRun() error = %v", err)
	}
	out, err := decodeRun(blob)
	if err != nil {
		t.Fatalf("decodeRun() error = %v", err)
	}
	if !reflect.DeepEqual(out.Ownership, in.Ownership) {
		t.Errorf("Ownership round-tripped as %#v, want %#v", out.Ownership, in.Ownership)
	}
}
```

Add `baseRunForEnvelope` if the file has no equivalent minimal-valid-Run helper:

```go
func baseRunForEnvelope(t *testing.T) *Run {
	t.Helper()
	now := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	return &Run{ID: "0123456789abcdef", State: StateDone, Phase: PhaseApply, StartedAt: now, UpdatedAt: now}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/engine/ -run TestEnvelopeRoundTripsOwnership -v`
Expected: FAIL to compile — `in.Ownership undefined`.

- [ ] **Step 3: Implement**

Add the three types and the `Run.Ownership` field to `internal/engine/run.go`, with this doc comment on the field:

```go
	// Ownership is what the cluster looked like immediately BEFORE this
	// run's Apply, and it is the only thing that separates a release this
	// console created from one it adopted. AICR's generated install.sh runs
	// `helm upgrade --install`, so a release a human already had at the
	// same (name, namespace) is upgraded, prints a deploy header like any
	// other action, and lands in Components indistinguishable from one this
	// run created. Reset uninstalls only what is ABSENT here.
	//
	// Recorded before Apply because that is the only moment the answer
	// exists: --install and --create-namespace both erase the distinction
	// the instant they run.
	Ownership Ownership `json:"ownership,omitzero"`
```

Add `Ownership` to the `envelope` struct and to both `encodeRun` and `decodeRun`, following `Workload`'s existing shape (optional on both sides, no `envelopeVersion` bump — a record written before this field existed decodes to the zero value, which is a correct decode: it means "no ownership evidence", and Task 6 treats that as "prove nothing, skip everything", which is the fail-closed direction).

Extend `setDistinctFieldValue`'s struct switch with an `Ownership` case so the top-level parity test covers it.

- [ ] **Step 4: Run to verify it passes**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/engine/ -count=1`
Expected: PASS, including both parity tests.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/run.go internal/engine/envelope.go internal/engine/envelope_test.go
git commit -S -m "feat(engine): persist what the cluster held before Apply"
```

---

## Task 4: Take the snapshot in Apply

**Files:**
- Create: `internal/steps/ownership.go`
- Modify: `internal/steps/apply.go:42` (`Run`)
- Test: `internal/steps/ownership_test.go`

**Interfaces:**
- Consumes: `engine.Ownership`, `engine.ReleaseRef`, `engine.NamespaceRef` (Task 3).
- Produces: `func snapshotOwnership(ctx context.Context, h HelmLister, k kubernetes.Interface, namespaces []string) engine.Ownership`, and `type HelmLister interface { List(ctx context.Context, namespace string) ([]string, error) }`.

The namespace list comes from the run's own component rows once Bundle has resolved the recipe; for Apply it is `recipe.json`'s per-component namespaces, which `internal/steps/recommend.go` already extracts.

- [ ] **Step 1: Write the failing tests**

```go
// A release that existed before Apply must be recorded, because Reset's
// entire ownership claim rests on this list.
func TestSnapshotOwnershipRecordsPreexistingReleases(t *testing.T) {
	h := &fakeHelmLister{byNamespace: map[string][]string{
		"gpu-operator":  {"gpu-operator", "somebody-elses-thing"},
		"kai-scheduler": {},
	}}
	kube := fake.NewSimpleClientset(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-operator", UID: "ns-uid-1"},
	})

	got := snapshotOwnership(context.Background(), h, kube, []string{"gpu-operator", "kai-scheduler"})

	wantReleases := []engine.ReleaseRef{
		{Name: "gpu-operator", Namespace: "gpu-operator"},
		{Name: "somebody-elses-thing", Namespace: "gpu-operator"},
	}
	if !reflect.DeepEqual(got.Releases, wantReleases) {
		t.Errorf("Releases = %#v, want %#v", got.Releases, wantReleases)
	}
}

// Existence AND UID: a namespace deleted and recreated between Apply and
// Reset is a different object wearing the same name, and deleting it would
// be deleting someone else's.
func TestSnapshotOwnershipRecordsNamespaceExistenceAndUID(t *testing.T) {
	h := &fakeHelmLister{byNamespace: map[string][]string{}}
	kube := fake.NewSimpleClientset(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-operator", UID: "ns-uid-1"},
	})

	got := snapshotOwnership(context.Background(), h, kube, []string{"gpu-operator", "kai-scheduler"})

	byName := map[string]engine.NamespaceRef{}
	for _, ns := range got.Namespaces {
		byName[ns.Name] = ns
	}
	if ns := byName["gpu-operator"]; !ns.Existed || ns.UID != "ns-uid-1" {
		t.Errorf("gpu-operator = %#v, want Existed=true UID=ns-uid-1", ns)
	}
	if ns := byName["kai-scheduler"]; ns.Existed {
		t.Errorf("kai-scheduler = %#v, want Existed=false", ns)
	}
}

// A snapshot that fails must not fail the install -- Apply is the long pole
// of the demo -- but it must be recorded, because every release in that
// namespace becomes unprovable and Reset has to skip it.
func TestSnapshotOwnershipRecordsPerNamespaceFailure(t *testing.T) {
	h := &fakeHelmLister{
		byNamespace: map[string][]string{"gpu-operator": {"gpu-operator"}},
		errFor:      map[string]error{"monitoring": errors.New("connection refused")},
	}
	kube := fake.NewSimpleClientset()

	got := snapshotOwnership(context.Background(), h, kube, []string{"gpu-operator", "monitoring"})

	byName := map[string]engine.NamespaceRef{}
	for _, ns := range got.Namespaces {
		byName[ns.Name] = ns
	}
	if byName["monitoring"].SnapshotErr == "" {
		t.Error("monitoring has no SnapshotErr -- an unprovable namespace must say so")
	}
	if byName["gpu-operator"].SnapshotErr != "" {
		t.Errorf("gpu-operator carries SnapshotErr %q -- one namespace's failure must not taint another",
			byName["gpu-operator"].SnapshotErr)
	}
}
```

With this double:

```go
type fakeHelmLister struct {
	byNamespace map[string][]string
	errFor      map[string]error
}

func (f *fakeHelmLister) List(_ context.Context, namespace string) ([]string, error) {
	if err, ok := f.errFor[namespace]; ok {
		return nil, err
	}
	return f.byNamespace[namespace], nil
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/steps/ -run TestSnapshotOwnership -v`
Expected: FAIL to compile — `snapshotOwnership` undefined.

- [ ] **Step 3: Implement `internal/steps/ownership.go`**

```go
package steps

// HelmLister reports the helm releases installed in one namespace. The
// seam exists so this package's tests never shell out; production wires a
// helm subprocess through the same Exec the applier uses.
type HelmLister interface {
	List(ctx context.Context, namespace string) ([]string, error)
}

// snapshotOwnership records what the cluster held before Apply installs
// anything: the releases already present per namespace, and whether each
// namespace existed (with its UID).
//
// It never returns an error. A namespace this cannot read is recorded with
// SnapshotErr set, which makes every release in it unprovable and therefore
// off limits to Reset -- the fail-closed direction. Failing the whole
// install because one snapshot call hiccuped would trade a certain cost for
// a hypothetical one, and Apply is the long pole of the demo.
//
// Sorted output, so a record diffed across two runs reads cleanly.
func snapshotOwnership(ctx context.Context, h HelmLister, k kubernetes.Interface, namespaces []string) engine.Ownership {
	// ... iterate sorted(namespaces): List per namespace, Get the namespace
	// for UID/existence, append refs, record errors per namespace.
}
```

Wire it into `apply.Run` before `a.applier.Apply(...)`, assigning `run.Ownership`. Emit one `bus.KindLog` event naming any namespace with a `SnapshotErr`, so the operator learns at the confirm gate rather than at Reset time.

- [ ] **Step 4: Run to verify they pass**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/steps/ -count=1 -race`
Expected: PASS.

- [ ] **Step 5: Bite-proof**

Make `snapshotOwnership` return an empty `Ownership` unconditionally. Confirm all three tests fail. Restore.

- [ ] **Step 6: Commit**

```bash
git add internal/steps/ownership.go internal/steps/ownership_test.go internal/steps/apply.go
git commit -S -m "feat(steps): snapshot release and namespace ownership before Apply installs"
```

---

## Task 5: Extract the delete-then-wait-absent primitive

`Engine.Stop` inlines `Delete` then `WaitAbsent`. Reset needs the same sequence but cannot call `Stop`: `stoppable()` rejects an ordinary `StateFailed` run and rejects `StateResetting` outright.

**Files:**
- Modify: `internal/prove/client.go`
- Modify: `internal/engine/engine.go:1218-1223` (`Stop` calls the primitive)
- Test: `internal/prove/client_test.go`

**Interfaces:**
- Produces: `func (c *Client) EnsureAbsent(ctx context.Context, runID string, timeout time.Duration) error` — Task 7 calls it.

- [ ] **Step 1: Write the failing test**

```go
// EnsureAbsent is Stop's delete-then-confirm sequence as one callable unit,
// so Reset can require the same guarantee without going through Stop --
// whose stoppable() guard rejects both an ordinary failed run and a run
// already moved to StateResetting.
func TestEnsureAbsentDeletesAndConfirms(t *testing.T) {
	cs := fake.NewSimpleClientset()
	c := prove.NewClient(cs)
	if err := c.Apply(context.Background(), "run-abc"); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	if err := c.EnsureAbsent(context.Background(), "run-abc", time.Second); err != nil {
		t.Fatalf("EnsureAbsent() error = %v", err)
	}
	if _, err := cs.BatchV1().Jobs(prove.Namespace).
		Get(context.Background(), prove.WorkloadName("run-abc"), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("workload Get() = %v, want NotFound", err)
	}
}

// Nothing to delete is success: a run that never reached Prove has no
// workload, and Reset must not treat that as a precondition failure.
func TestEnsureAbsentSucceedsWhenNothingWasEverApplied(t *testing.T) {
	if err := prove.NewClient(fake.NewSimpleClientset()).
		EnsureAbsent(context.Background(), "run-never-proved", time.Second); err != nil {
		t.Errorf("EnsureAbsent() error = %v, want nil", err)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/prove/ -run TestEnsureAbsent -v`
Expected: FAIL to compile — `EnsureAbsent` undefined.

- [ ] **Step 3: Implement**

```go
// EnsureAbsent deletes runID's workload and does not return until the API
// server has confirmed it gone. It is the guarantee Stop makes, factored
// out so a second caller (engine.Reset) can require it without going
// through Stop's own state guard.
//
// Idempotent in both halves: Delete treats NotFound as success, and
// WaitAbsent returns immediately for an object that was never there. A run
// that never reached Prove therefore satisfies this trivially.
func (c *Client) EnsureAbsent(ctx context.Context, runID string, timeout time.Duration) error {
	if err := c.Delete(ctx, runID); err != nil {
		return err
	}
	return c.WaitAbsent(ctx, runID, timeout)
}
```

Rewrite `Stop`'s two calls as one `client.EnsureAbsent(ctx, runID, stopWaitAbsentTimeout)`, keeping its existing error wrapping so the operator-facing message is unchanged.

- [ ] **Step 4: Run to verify everything passes**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/prove/ ./internal/engine/ -count=1 -race`
Expected: PASS. `TestStopEndsTheRunAtDone`, `TestFailedStopLeavesRunActive` and `TestFailedDeleteLeavesRunActive` all still pass — the refactor is behaviour-preserving, and those three are what prove it.

- [ ] **Step 5: Commit**

```bash
git add internal/prove/client.go internal/prove/client_test.go internal/engine/engine.go
git commit -S -m "refactor(prove): extract Stop's delete-then-confirm as EnsureAbsent"
```

---

## Task 6: The teardown executor

**Files:**
- Create: `internal/teardown/teardown.go`, `internal/teardown/teardown_test.go`

**Interfaces:**
- Consumes: `engine.ComponentState` (Task 2), `engine.Ownership` (Task 3).
- Produces:
  ```go
  type Exec interface { Run(ctx context.Context, argv []string, out io.Writer) error }
  type ReleaseOutcome struct { Name, Namespace, Skip, Err string }
  type Options struct { Timeout time.Duration }
  func Releases(ctx, cancel context.Context, e Exec, comps []engine.ComponentState, own engine.Ownership, opts Options, emit func(ReleaseOutcome)) []ReleaseOutcome
  ```
  `ctx` runs each command; `cancel` is checked only *between* commands (spec §8).

- [ ] **Step 1: Write the failing tests**

```go
// Reverse install order, with the exact flags: --ignore-not-found is what
// makes a second Reset clean rather than a wall of "release: not found",
// and --wait is what makes the namespace emptiness check that follows
// meaningful rather than a race against deletion.
func TestReleasesUninstallsInReverseOrderWithTheRightFlags(t *testing.T) {
	e := &fakeExec{}
	comps := []engine.ComponentState{
		{Name: "cert-manager", Namespace: "cert-manager", Index: 1},
		{Name: "nfd", Namespace: "node-feature-discovery", Index: 2},
		{Name: "gpu-operator", Namespace: "gpu-operator", Index: 3},
	}

	teardown.Releases(context.Background(), context.Background(), e, comps,
		engine.Ownership{}, teardown.Options{Timeout: 5 * time.Minute}, func(teardown.ReleaseOutcome) {})

	want := [][]string{
		{"helm", "uninstall", "gpu-operator", "-n", "gpu-operator", "--ignore-not-found", "--wait", "--timeout", "5m0s"},
		{"helm", "uninstall", "nfd", "-n", "node-feature-discovery", "--ignore-not-found", "--wait", "--timeout", "5m0s"},
		{"helm", "uninstall", "cert-manager", "-n", "cert-manager", "--ignore-not-found", "--wait", "--timeout", "5m0s"},
	}
	if !reflect.DeepEqual(e.calls, want) {
		t.Errorf("commands =\n%v\nwant\n%v", e.calls, want)
	}
}

// The ownership bite-proof. A release present before Apply was adopted by
// `helm upgrade --install`, not created -- uninstalling it would remove
// something a human installed.
func TestReleasesSkipsWhatItDidNotCreate(t *testing.T) {
	e := &fakeExec{}
	comps := []engine.ComponentState{
		{Name: "gpu-operator", Namespace: "gpu-operator", Index: 1},
		{Name: "somebody-elses-thing", Namespace: "gpu-operator", Index: 2},
	}
	own := engine.Ownership{Releases: []engine.ReleaseRef{
		{Name: "somebody-elses-thing", Namespace: "gpu-operator"},
	}}

	out := teardown.Releases(context.Background(), context.Background(), e, comps, own,
		teardown.Options{Timeout: time.Minute}, func(teardown.ReleaseOutcome) {})

	for _, call := range e.calls {
		for _, arg := range call {
			if arg == "somebody-elses-thing" {
				t.Fatal("uninstalled a release that existed before Apply")
			}
		}
	}
	var skipped bool
	for _, o := range out {
		if o.Name == "somebody-elses-thing" && o.Skip != "" {
			skipped = true
		}
	}
	if !skipped {
		t.Error("the skipped release is not reported -- it must be named, not silently ignored")
	}
}

// Every release in a namespace whose snapshot failed is unprovable.
func TestReleasesSkipsNamespacesWhoseSnapshotFailed(t *testing.T) {
	e := &fakeExec{}
	comps := []engine.ComponentState{{Name: "prometheus-adapter", Namespace: "monitoring", Index: 1}}
	own := engine.Ownership{Namespaces: []engine.NamespaceRef{
		{Name: "monitoring", SnapshotErr: "connection refused"},
	}}

	out := teardown.Releases(context.Background(), context.Background(), e, comps, own,
		teardown.Options{Timeout: time.Minute}, func(teardown.ReleaseOutcome) {})

	if len(e.calls) != 0 {
		t.Errorf("ran %v, want nothing -- ownership could not be established", e.calls)
	}
	if out[0].Skip == "" {
		t.Error("skip reason is empty")
	}
}

// One failure must not end the teardown: stopping early leaves strictly
// more residue than finishing, which inverts Apply's own policy for a
// reason (spec section 6).
func TestReleasesContinuesPastAFailure(t *testing.T) {
	e := &fakeExec{failFor: map[string]error{"nfd": errors.New("release is in a failed state")}}
	comps := []engine.ComponentState{
		{Name: "cert-manager", Namespace: "cert-manager", Index: 1},
		{Name: "nfd", Namespace: "node-feature-discovery", Index: 2},
		{Name: "gpu-operator", Namespace: "gpu-operator", Index: 3},
	}

	out := teardown.Releases(context.Background(), context.Background(), e, comps, engine.Ownership{},
		teardown.Options{Timeout: time.Minute}, func(teardown.ReleaseOutcome) {})

	if len(e.calls) != 3 {
		t.Errorf("ran %d commands, want 3 -- a failed uninstall must not end the teardown", len(e.calls))
	}
	var failed int
	for _, o := range out {
		if o.Err != "" {
			failed++
		}
	}
	if failed != 1 {
		t.Errorf("reported %d failures, want exactly 1", failed)
	}
}

// Operator cancellation is cooperative: the in-flight uninstall completes,
// and the NEXT one does not start. internal/applier/exec.go's BashExec
// SIGTERMs the whole process group the moment its context is cancelled, so
// handing it the cancellable context would interrupt helm mid-uninstall --
// which is how a release ends up half-removed, the exact residue Reset
// exists to eliminate.
func TestReleasesCancellationDoesNotInterruptTheInFlightCommand(t *testing.T) {
	cancelCtx, cancel := context.WithCancel(context.Background())
	e := &fakeExec{onCall: func(n int) {
		if n == 1 {
			cancel() // cancel DURING the first uninstall
		}
	}}
	comps := []engine.ComponentState{
		{Name: "a", Namespace: "ns-a", Index: 1},
		{Name: "b", Namespace: "ns-b", Index: 2},
	}

	teardown.Releases(context.Background(), cancelCtx, e, comps, engine.Ownership{},
		teardown.Options{Timeout: time.Minute}, func(teardown.ReleaseOutcome) {})

	if len(e.calls) != 1 {
		t.Errorf("ran %d commands, want exactly 1 -- the in-flight one completes, the next does not start", len(e.calls))
	}
	if e.interrupted {
		t.Error("the in-flight command saw a cancelled context")
	}
}
```

With this double:

```go
type fakeExec struct {
	calls       [][]string
	failFor     map[string]error
	onCall      func(n int)
	interrupted bool
}

func (f *fakeExec) Run(ctx context.Context, argv []string, _ io.Writer) error {
	f.calls = append(f.calls, argv)
	if f.onCall != nil {
		f.onCall(len(f.calls))
	}
	if ctx.Err() != nil {
		f.interrupted = true
	}
	for name, err := range f.failFor {
		if slices.Contains(argv, name) {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/teardown/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement `internal/teardown/teardown.go`**

Sort a copy of `comps` by descending `Index`; for each, decide ownership (skip when the release appears in `own.Releases`, or when its namespace carries a `SnapshotErr`, or when `Namespace` is empty), otherwise run the helm argv above with `ctx`; check `cancel.Err()` between iterations and stop; call `emit` per outcome and return the full slice.

- [ ] **Step 4: Run to verify they pass**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/teardown/ -count=1 -race`
Expected: PASS.

- [ ] **Step 5: Bite-proof**

Reverse the sort to ascending. Confirm `TestReleasesUninstallsInReverseOrderWithTheRightFlags` fails alone. Restore. Then delete the ownership skip; confirm the two skip tests fail. Restore.

- [ ] **Step 6: Commit**

```bash
git add internal/teardown/
git commit -S -m "feat(teardown): uninstall in reverse order, and only what the run created"
```

---

## Task 7: Namespace ownership and emptiness

**Files:**
- Create: `internal/teardown/namespaces.go`, `internal/teardown/namespaces_test.go`

**Interfaces:**
- Produces:
  ```go
  type NamespaceOutcome struct { Name, Skip, Err string; Deleted bool }
  func Namespaces(ctx context.Context, k kubernetes.Interface, d discovery.DiscoveryInterface, dyn dynamic.Interface, names []string, own engine.Ownership, emit func(NamespaceOutcome)) []NamespaceOutcome
  ```

- [ ] **Step 1: Write the failing tests**

```go
// The whole rule in one test: created by this run, UID unchanged, nothing
// left in it.
func TestNamespacesDeletesOnlyWhatThisRunCreatedAndEmptied(t *testing.T) { /* asserts Deleted: true */ }

// A namespace that existed before Apply is never deleted, whatever is in
// it -- this console did not make it and does not get to unmake it.
func TestNamespacesKeepsOneThatExistedBeforeApply(t *testing.T) { /* asserts Deleted: false, Skip non-empty */ }

// Same name, different object. A namespace deleted and recreated between
// Apply and Reset belongs to whoever recreated it.
func TestNamespacesKeepsOneWhoseUIDChanged(t *testing.T) { /* asserts Deleted: false, Skip names the UID mismatch */ }

// Discovery-driven, not a hardcoded kind list: revision 1 checked six
// workload kinds and would have deleted a namespace holding Services,
// Secrets, ConfigMaps, RBAC, CronJobs, PDBs or any custom resource.
func TestNamespacesKeepsOneHoldingAResourceOfAnyKind(t *testing.T) {
	// table over: Service, Secret, ConfigMap, Role, CronJob,
	// PodDisruptionBudget, and a custom resource -- each alone must keep
	// the namespace.
}

// Fail closed. An unanswered question is not an empty namespace.
func TestNamespacesKeepsOneItCannotInspect(t *testing.T) { /* discovery reactor returns an error */ }

// A chart that ships its own Namespace manifest has already had it removed
// by helm uninstall (AICR downgrades --create-namespace for exactly these).
func TestNamespacesTreatsAnAlreadyGoneNamespaceAsSuccess(t *testing.T) { /* asserts no error, Deleted true-or-noop */ }
```

Write each body out in full when implementing; the six names above are the required coverage, one behaviour each.

- [ ] **Step 2: Run to verify they fail**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/teardown/ -run TestNamespaces -v`
Expected: FAIL — `Namespaces` undefined.

- [ ] **Step 3: Implement**

Enumerate namespaced resources from `d.ServerPreferredNamespacedResources()`, list each via `dyn`, and treat any single item as a bystander. Skip kinds that exist in every namespace by construction (`ServiceAccount` named `default`, `ConfigMap` named `kube-root-ca.crt`) — document each exclusion inline, since an over-broad exclusion is how this rule becomes unsafe again.

- [ ] **Step 4: Run to verify they pass**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/teardown/ -count=1 -race`

- [ ] **Step 5: Bite-proof**

Make the emptiness check return "empty" on a discovery error. Confirm `TestNamespacesKeepsOneItCannotInspect` fails alone. Restore.

- [ ] **Step 6: Commit**

```bash
git add internal/teardown/namespaces.go internal/teardown/namespaces_test.go
git commit -S -m "feat(teardown): delete only namespaces this run created and emptied"
```

---

## Task 8: `StateResetting`, the teardown-incomplete guard, and `Engine.Reset`

**Files:**
- Create: `internal/engine/reset.go`, `internal/engine/reset_test.go`
- Modify: `internal/engine/run.go` (`StateResetting`, `Run.Residue`), `internal/engine/engine.go` (`isLive`, `Start`, `Retry`, `Discard`), `internal/engine/recover.go` (`validState`)

**Interfaces:**
- Consumes: `prove.Client.EnsureAbsent` (Task 5), `teardown.Releases` (Task 6), `teardown.Namespaces` (Task 7).
- Produces: `func (e *Engine) Reset(ctx context.Context, runID string) error`, `func hasIncompleteTeardown(r *Run) bool`.

- [ ] **Step 1: Write the failing tests**

Required coverage, one behaviour each — full bodies at implementation time:

```go
func TestResetRejectsALiveRun(t *testing.T)                          // 409
func TestResetRejectsARunThatInstalledNothing(t *testing.T)          // empty Components
func TestResetRequiresTheConfirmedWorkloadStop(t *testing.T)         // EnsureAbsent fails -> nothing uninstalled, guard set
func TestResetUninstallsInReverseAndDeletesTheRecordWhenClean(t *testing.T)
func TestResetKeepsTheRecordAndSetsTheGuardOnFailure(t *testing.T)
func TestResetClearsRecoveredPendingWhenClean(t *testing.T)          // Stop's M2, one state over

// The three hazards an ordinary StateFailed leaves open. Each is a
// separate test because each is a separate call site that must learn to
// refuse:
func TestIncompleteTeardownBlocksStart(t *testing.T)
func TestIncompleteTeardownBlocksRetry(t *testing.T)   // NEW vs Ruling 12: Retry would re-run Apply over a half-removed cluster
func TestIncompleteTeardownBlocksDiscard(t *testing.T) // discarding loses the only residue inventory
func TestIncompleteTeardownStillAllowsReset(t *testing.T)
```

- [ ] **Step 2: Run to verify they fail**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/engine/ -run 'TestReset|TestIncompleteTeardown' -v`

- [ ] **Step 3: Implement**

`Reset` mirrors `Start`'s shape: guard under `e.mu`, install `StateResetting` + epoch + cancel + done, unlock, `Save`, launch the goroutine. The goroutine runs §4's three steps, accumulates outcomes into `Run.Residue`, and finishes via `finish` — `StateDone` and record deletion when clean, `StateFailed` with the guard set otherwise.

Add `StateResetting` to `isLive` and `validState`. Add `hasIncompleteTeardown(e.current)` rejections to `Start`, `Retry` and `Discard`, each naming Reset as the remedy.

- [ ] **Step 4: Run to verify they pass**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/engine/ -count=1 -race`

- [ ] **Step 5: Bite-proof**

Remove the `Retry` rejection. Confirm `TestIncompleteTeardownBlocksRetry` fails alone. Restore.

- [ ] **Step 6: Commit**

```bash
git add internal/engine/
git commit -S -m "feat(engine): Reset, and the guard that stops a half-torn-down run looking ordinary"
```

---

## Task 9: The API endpoint

**Files:**
- Create: `internal/api/reset.go`, `internal/api/reset_test.go`
- Modify: `internal/api/server.go:137`

**Interfaces:**
- Consumes: `engine.Reset` (Task 8).

- [ ] **Step 1: Write the failing tests**

```go
func TestResetRequiresTheConfirmationBody(t *testing.T)   // bare POST -> 400, engine.Reset never called
func TestResetAcceptsTheConfirmationBody(t *testing.T)    // {"confirm":"reset"} -> 200
func TestResetSurfacesTheEnginesConflict(t *testing.T)    // live run -> 409
```

- [ ] **Step 2: Run to verify they fail**

Run: `GOTOOLCHAIN=go1.26.5 go test ./internal/api/ -run TestReset -v`

- [ ] **Step 3: Implement**

`handleReset` decodes `{"confirm":"reset"}`, rejects anything else with 400, then calls `s.engine.Reset(context.WithoutCancel(r.Context()), r.PathValue("id"))` — detached for the same reason `handleStop` detaches: the teardown outlives the tab. Register `protected.HandleFunc("POST /api/runs/{id}/reset", s.handleReset)`.

- [ ] **Step 4: Run to verify they pass**

- [ ] **Step 5: Commit**

```bash
git add internal/api/reset.go internal/api/reset_test.go internal/api/server.go
git commit -S -m "feat(api): POST /api/runs/{id}/reset, confirmation required"
```

---

## Task 10: Teardown in the SPA

**Files:**
- Modify: `web/src/pipeline.ts`, `web/src/api.ts`, `web/src/components/Wizard.tsx`
- Create: `web/src/components/Reset.tsx`, `web/src/components/Reset.test.tsx`

**Interfaces:**
- Consumes: `POST /api/runs/{id}/reset`, the teardown events from Task 8.

- [ ] **Step 1: Write the failing tests**

```tsx
it('labels rows removing and removed during a teardown, not installing', () => {})
it('renders teardown rows in reverse install order', () => {})
it('lists every release and namespace before the second confirm click', () => {})
it('lists separately what will be skipped for want of ownership', () => {})
it('offers only Reset for a run with an incomplete teardown', () => {})
it('does not offer Reset mid-run', () => {})
```

- [ ] **Step 2: Run to verify they fail** — `cd web && npx vitest run`

- [ ] **Step 3: Implement**

Extend `ComponentData['status']` with `'removing' | 'removed' | 'skipped'`, add the operation discriminator to the event payload, and have `deriveComponents` order teardown rows by descending index rather than first-seen.

- [ ] **Step 4: Run to verify they pass** — web total must be ≥ 138 (132 baseline + 6)

- [ ] **Step 5: Bite-proof**

Render teardown rows in install order. Confirm the ordering test fails alone. Restore.

- [ ] **Step 6: Commit**

```bash
git add web/src
git commit -S -m "feat(cockpit): render a teardown as removal, not as an install running backwards"
```

---

## Task 11: e2e

**Files:**
- Create: `test/e2e/reset.sh`
- Modify: `.github/workflows/e2e.yaml`

**This task exists because the ownership rule is unfalsifiable against a fake clientset: nothing in a fake cluster distinguishes a release helm adopted from one it created.**

- [ ] **Step 1: Add the assertions**

1. **A bystander survives.** Before Apply, `helm install` a trivial chart into a recipe namespace under a name the recipe also uses. After Reset, assert it is still there and that the run reported skipping it. This is the assertion the whole ownership design exists for.
2. **Every owned release is gone** — `helm list -A` shows none of `run.Components`' names except the bystander.
3. **Namespaces this run created are gone; one seeded with a bystander ConfigMap is kept** and named.
4. **The console accepts a new run** afterward.
5. **A failed Reset blocks Start, Retry and Discard**, and Reset again succeeds — block deletion with the same admission webhook `prove.sh` uses.

Each assertion prints the counts it matched and carries an inverted-input self-check, following `apply-real.sh`'s pattern — an e2e assertion that matches nothing passes silently.

- [ ] **Step 2: Run it locally and paste real output**

- [ ] **Step 3: Commit**

```bash
git add test/e2e/reset.sh .github/workflows/e2e.yaml
git commit -S -m "test(e2e): Reset removes what the run created and leaves what it did not"
```

---

## Task 12: Documentation

**Files:**
- Modify: `DEMO.md`, `approach.md`, `docs/phase-2-handoff.md`

- [ ] **Step 1** `DEMO.md`: Reset replaces `make demo-down` for a repeat demo; state what it does not remove (CRDs, adopted releases, namespaces it did not create).
- [ ] **Step 2** `approach.md`: mark Reset delivered in the Phase 5 row; record that ownership is snapshot-based because `helm upgrade --install` adopts.
- [ ] **Step 3** `docs/phase-2-handoff.md`: close the "Phase 3's Reset must still route through `finish` before bumping the epoch" constraint, and record the ownership finding for whoever builds GitOps export next.
- [ ] **Step 4** `GOTOOLCHAIN=go1.26.5 make qualify`, commit.

---

## Self-Review

**Spec coverage.** §1 guards → Task 8. §2 ownership → Tasks 2, 3, 4, and the skip logic in 6. §3 execution/state → Task 8. §3a guard → Task 8. §3b primitive → Task 5. §4 order → Tasks 6, 7, 8. §5 namespaces → Task 7. §6 failure policy → Task 6. §7 afterward → Task 8. §8 cancellation → Task 6. §9 screen → Task 10. §10 confirm gate → Tasks 9, 10. Testing strategy → every task, plus Task 11. The nested-parity trap → Task 1, deliberately first.

**Placeholder scan.** Tasks 7, 8, 9 and 10 name their tests by required behaviour rather than pasting every body, and Task 7 Step 3 describes the discovery walk rather than coding it. That is deliberate where the body must adopt an existing harness — `internal/engine`'s `newTestEngine`/`startAndWait`, the fake clientset's reactor idiom, the dynamic fake's scheme registration — and inventing a parallel one would be the defect. Literal code appears wherever a new type, flag string, or non-obvious control flow is introduced. **Task 7's six test bodies and Task 8's ten are the largest such gap; an implementer should read `internal/prove/client_test.go` and `internal/engine/active_test.go` first.**

**Type consistency.** `engine.ReleaseRef`, `engine.NamespaceRef`, `engine.Ownership` are defined in Task 3 and consumed unchanged in 4, 6, 7. `teardown.Exec` (Task 6) is narrower than `applier.Exec` — argv, not a `Spec` — so `applier.BashExec` does **not** satisfy it directly; Task 8 wires a small adapter, and that adapter is named there rather than assumed.

**One ordering hazard.** Task 1 must land before Task 2, or the parity guard it exists to provide arrives after the field it was meant to protect.

## Unresolved questions

1. **Uninstall timeout per release** — 5 minutes, provisional, to be revisited against a real Reset.
2. **Discovery cost** for the emptiness check across ten namespaces. If slow, cache the discovery document per Reset — do not narrow to a kind list, which is what made revision 1 unsafe.
3. **Does the kai-scheduler wedge** (`docs/phase-3-status.md`) survive a Reset-then-Apply cycle? Task 11 is where to find out.

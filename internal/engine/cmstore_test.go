package engine_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/engine"
)

// payloadKey mirrors cmstore.go's unexported constant of the same name. The
// test lives in package engine_test (matching store_test.go and
// internal/observer's black-box pattern) so it cannot reference the
// unexported constant directly; it must agree with it by convention instead.
const payloadKey = "run"

const testNamespace = "aicrme"
const testName = "aicrme-run"

func testOwner() metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       "aicrme",
		UID:        "owner-uid-1",
	}
}

func TestConfigMapStoreSaveThenLoadCurrent(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := engine.NewConfigMapStore(client, testNamespace, testName, testOwner())
	ctx := context.Background()

	run := &engine.Run{
		ID:        "run-a",
		State:     engine.StateRunning,
		Phase:     engine.PhaseApply,
		Artifacts: map[string][]byte{"snapshot.yaml": []byte("nodes: []\n")},
	}
	if err := s.Save(ctx, run); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := s.LoadCurrent(ctx)
	if err != nil {
		t.Fatalf("LoadCurrent() error = %v", err)
	}
	if got.ID != "run-a" {
		t.Errorf("ID = %q, want %q", got.ID, "run-a")
	}
	if got.State != engine.StateRunning || got.Phase != engine.PhaseApply {
		t.Errorf("State/Phase = %v/%v, want %v/%v", got.State, got.Phase, engine.StateRunning, engine.PhaseApply)
	}
	if string(got.Artifacts["snapshot.yaml"]) != "nodes: []\n" {
		t.Errorf("Artifacts[snapshot.yaml] = %q, want the saved bytes", got.Artifacts["snapshot.yaml"])
	}
}

// Assert on Kind, not just presence: a ReplicaSet owner would be
// garbage-collected on the next rollout, deleting run state along with it.
func TestConfigMapStoreCreatesWithOwnerReference(t *testing.T) {
	client := fake.NewSimpleClientset()
	owner := testOwner()
	s := engine.NewConfigMapStore(client, testNamespace, testName, owner)

	if err := s.Save(context.Background(), &engine.Run{ID: "run-a"}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	cm, err := client.CoreV1().ConfigMaps(testNamespace).Get(context.Background(), testName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if len(cm.OwnerReferences) != 1 {
		t.Fatalf("OwnerReferences = %v, want exactly one", cm.OwnerReferences)
	}
	got := cm.OwnerReferences[0]
	if got.Kind != "Deployment" {
		t.Errorf("Kind = %q, want %q", got.Kind, "Deployment")
	}
	if got.UID != owner.UID {
		t.Errorf("UID = %q, want %q", got.UID, owner.UID)
	}
}

func TestConfigMapStoreUpdatesExisting(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := engine.NewConfigMapStore(client, testNamespace, testName, testOwner())
	ctx := context.Background()

	if err := s.Save(ctx, &engine.Run{ID: "run-a"}); err != nil {
		t.Fatalf("Save(run-a) error = %v", err)
	}
	if err := s.Save(ctx, &engine.Run{ID: "run-b"}); err != nil {
		t.Fatalf("Save(run-b) error = %v", err)
	}

	list, err := client.CoreV1().ConfigMaps(testNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("ConfigMaps = %d, want exactly 1 after two Saves", len(list.Items))
	}

	got, err := s.LoadCurrent(ctx)
	if err != nil {
		t.Fatalf("LoadCurrent() error = %v", err)
	}
	if got.ID != "run-b" {
		t.Errorf("LoadCurrent().ID = %q, want %q -- the second Save's content", got.ID, "run-b")
	}
}

// An existing ConfigMap can have no BinaryData at all -- e.g. hand-created
// via kubectl before this store ever wrote to it -- and Save must
// initialize the map rather than panic on a nil-map write.
func TestConfigMapStoreUpdatesRecordWithNilBinaryData(t *testing.T) {
	owner := testOwner()
	client := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       testNamespace,
			Name:            testName,
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		// BinaryData deliberately left nil.
	})

	s := engine.NewConfigMapStore(client, testNamespace, testName, owner)
	if err := s.Save(context.Background(), &engine.Run{ID: "run-a"}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := s.LoadCurrent(context.Background())
	if err != nil {
		t.Fatalf("LoadCurrent() error = %v", err)
	}
	if got.ID != "run-a" {
		t.Errorf("LoadCurrent().ID = %q, want %q", got.ID, "run-a")
	}
}

// Assert the code, not the message: recovery keys off exactly this
// distinction to tell "nothing to recover" apart from every other failure
// mode a real backing store can produce.
func TestConfigMapStoreLoadCurrentNotFound(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := engine.NewConfigMapStore(client, testNamespace, testName, testOwner())

	_, err := s.LoadCurrent(context.Background())
	if err == nil {
		t.Fatal("LoadCurrent() error = nil, want ErrCodeNotFound")
	}
	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) || se.Code != aicrerrors.ErrCodeNotFound {
		t.Errorf("error = %v, want a StructuredError with ErrCodeNotFound", err)
	}
}

// The single most load-bearing branch in this file: a non-404 Get error
// must not collapse into NotFound. If it did, a transient failure (an
// apiserver 5xx, or -- the realistic case, since the console runs
// cluster-admin today but that is not guaranteed to stay that way -- a
// future RBAC narrowing) would look exactly like "no prior run" to Task 3's
// recovery, and the next Start would overwrite a perfectly good record
// instead of surfacing the read failure.
func TestConfigMapStoreLoadCurrentNonNotFoundGetErrorIsNotNotFound(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("get", "configmaps", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(corev1.Resource("configmaps"), testName, errors.New("no access"))
	})

	s := engine.NewConfigMapStore(client, testNamespace, testName, testOwner())
	_, err := s.LoadCurrent(context.Background())
	if err == nil {
		t.Fatal("LoadCurrent() error = nil, want the Forbidden error to surface")
	}
	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) {
		t.Fatalf("error = %v, want a StructuredError", err)
	}
	if se.Code == aicrerrors.ErrCodeNotFound {
		t.Errorf("Code = %v, want anything but ErrCodeNotFound -- "+
			"a Forbidden error must not look like a cold start", se.Code)
	}
}

// A present-but-undecodable record must not collapse into the same error
// code as a cold start: Task 3's recovery treats "nothing to recover" and
// "something to recover, but it is unreadable" very differently, and if
// both looked like NotFound the latter would silently be treated as the
// former and overwritten by a fresh run.
func TestConfigMapStoreCorruptRecordIsNotNotFound(t *testing.T) {
	assertNotNotFound := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("LoadCurrent() error = nil, want a decode error")
		}
		var se *aicrerrors.StructuredError
		if !errors.As(err, &se) {
			t.Fatalf("error = %v, want a StructuredError", err)
		}
		if se.Code == aicrerrors.ErrCodeNotFound {
			t.Errorf("Code = %v, want anything but ErrCodeNotFound", se.Code)
		}
	}

	t.Run("garbled payload", func(t *testing.T) {
		client := fake.NewSimpleClientset(&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testName},
			BinaryData: map[string][]byte{payloadKey: []byte("not a gzip envelope")},
		})
		s := engine.NewConfigMapStore(client, testNamespace, testName, testOwner())
		_, err := s.LoadCurrent(context.Background())
		assertNotNotFound(t, err)
	})

	t.Run("missing payload key", func(t *testing.T) {
		client := fake.NewSimpleClientset(&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testName},
		})
		s := engine.NewConfigMapStore(client, testNamespace, testName, testOwner())
		_, err := s.LoadCurrent(context.Background())
		assertNotNotFound(t, err)
	})
}

func TestConfigMapStoreRetriesOnConflict(t *testing.T) {
	owner := testOwner()
	client := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       testNamespace,
			Name:            testName,
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		BinaryData: map[string][]byte{payloadKey: []byte("placeholder")},
	})

	var updateAttempts int
	client.PrependReactor("update", "configmaps", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		updateAttempts++
		if updateAttempts == 1 {
			return true, nil, apierrors.NewConflict(corev1.Resource("configmaps"), testName, errors.New("stale resourceVersion"))
		}
		return false, nil, nil
	})

	s := engine.NewConfigMapStore(client, testNamespace, testName, owner)
	if err := s.Save(context.Background(), &engine.Run{ID: "run-a"}); err != nil {
		t.Fatalf("Save() error = %v, want the retry to absorb the injected conflict", err)
	}
	if updateAttempts != 2 {
		t.Errorf("update attempts = %d, want exactly 2", updateAttempts)
	}
}

// A Create racing another creator surfaces as IsAlreadyExists, not
// IsConflict; the brief calls this out explicitly as a case the loop must
// still fold into a retry rather than treat as a hard failure.
func TestConfigMapStoreRetriesOnCreateRace(t *testing.T) {
	owner := testOwner()
	client := fake.NewSimpleClientset()

	var createAttempts int
	client.PrependReactor("create", "configmaps", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		createAttempts++
		if createAttempts == 1 {
			// A different writer wins the race and creates the object first.
			winner := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:       testNamespace,
					Name:            testName,
					OwnerReferences: []metav1.OwnerReference{owner},
				},
				BinaryData: map[string][]byte{payloadKey: []byte("placeholder")},
			}
			if err := client.Tracker().Create(corev1.SchemeGroupVersion.WithResource("configmaps"), winner, testNamespace); err != nil {
				t.Fatalf("seeding the racing writer's object failed: %v", err)
			}
			return true, nil, apierrors.NewAlreadyExists(corev1.Resource("configmaps"), testName)
		}
		return false, nil, nil
	})

	s := engine.NewConfigMapStore(client, testNamespace, testName, owner)
	if err := s.Save(context.Background(), &engine.Run{ID: "run-a"}); err != nil {
		t.Fatalf("Save() error = %v, want the AlreadyExists race to fold into a retry", err)
	}
	if createAttempts != 1 {
		t.Errorf("create attempts = %d, want exactly 1 (the racing writer's object wins Create)", createAttempts)
	}

	got, err := s.LoadCurrent(context.Background())
	if err != nil {
		t.Fatalf("LoadCurrent() error = %v", err)
	}
	if got.ID != "run-a" {
		t.Errorf("LoadCurrent().ID = %q, want %q -- the retried Update should have landed", got.ID, "run-a")
	}
}

func TestConfigMapStoreGivesUpAfterBoundedConflicts(t *testing.T) {
	owner := testOwner()
	client := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       testNamespace,
			Name:            testName,
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		BinaryData: map[string][]byte{payloadKey: []byte("placeholder")},
	})

	client.PrependReactor("update", "configmaps", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(corev1.Resource("configmaps"), testName, errors.New("always stale"))
	})

	s := engine.NewConfigMapStore(client, testNamespace, testName, owner)
	if err := s.Save(context.Background(), &engine.Run{ID: "run-a"}); err == nil {
		t.Fatal("Save() error = nil, want an error after exhausting bounded conflict retries")
	}
}

// A non-conflict Update failure (RBAC narrowing, an apiserver 5xx) must
// surface as an error on the first attempt rather than being retried --
// retries exist for IsConflict specifically, not for API failures in
// general.
func TestConfigMapStoreUpdateNonConflictErrorIsNotRetried(t *testing.T) {
	owner := testOwner()
	client := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       testNamespace,
			Name:            testName,
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		BinaryData: map[string][]byte{payloadKey: []byte("placeholder")},
	})

	var updateAttempts int
	client.PrependReactor("update", "configmaps", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		updateAttempts++
		return true, nil, apierrors.NewForbidden(corev1.Resource("configmaps"), testName, errors.New("no access"))
	})

	s := engine.NewConfigMapStore(client, testNamespace, testName, owner)
	if err := s.Save(context.Background(), &engine.Run{ID: "run-a"}); err == nil {
		t.Fatal("Save() error = nil, want the Forbidden Update error to surface")
	}
	if updateAttempts != 1 {
		t.Errorf("update attempts = %d, want exactly 1 -- a non-conflict error must not be retried", updateAttempts)
	}
}

// This is what stops the console clobbering a record left by a different
// install that reused the ConfigMap name.
func TestConfigMapStoreRejectsForeignOwner(t *testing.T) {
	foreign := metav1.OwnerReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "other-install", UID: "foreign-uid"}
	client := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       testNamespace,
			Name:            testName,
			OwnerReferences: []metav1.OwnerReference{foreign},
		},
		BinaryData: map[string][]byte{payloadKey: []byte("placeholder")},
	})

	s := engine.NewConfigMapStore(client, testNamespace, testName, testOwner())
	if err := s.Save(context.Background(), &engine.Run{ID: "run-a"}); err == nil {
		t.Fatal("Save() error = nil, want a foreign-owner record to be refused rather than overwritten")
	}

	got, err := client.CoreV1().ConfigMaps(testNamespace).Get(context.Background(), testName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if string(got.BinaryData[payloadKey]) != "placeholder" {
		t.Errorf("BinaryData[%s] changed, want the foreign-owned record left untouched", payloadKey)
	}
}

// foreignOwnedClient returns a fake clientset holding a run checkpoint owned
// by a different install -- what `helm uninstall && helm install` leaves
// behind for the window before ownerReference garbage collection reaps it,
// since the new Deployment gets a new UID while the old object is still
// present.
//
// The record is written through a store owned by that other install rather
// than hand-stuffed with a placeholder byte string, and that detail is the
// whole test. A garbage payload fails decodeRun on its own, so every
// assertion below would have passed with no owner check in loadCurrent at
// all -- verified by writing it the lazy way first and watching the mutation
// run stay green. Only a genuinely decodable envelope leaves the ownership
// check as the sole reason a read can fail.
//
// It also returns the encoded bytes, so a caller can assert the record came
// back byte-identical instead of merely "not the string I typed."
func foreignOwnedClient(t *testing.T) (*fake.Clientset, []byte) {
	t.Helper()
	foreign := metav1.OwnerReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "other-install", UID: "foreign-uid"}
	client := fake.NewSimpleClientset()

	other := engine.NewConfigMapStore(client, testNamespace, testName, foreign)
	if err := other.Save(context.Background(), baseRun(testRunID, engine.StateFailed, engine.PhaseApply, 2)); err != nil {
		t.Fatalf("seeding the other install's record failed: %v", err)
	}

	cm, err := client.CoreV1().ConfigMaps(testNamespace).Get(context.Background(), testName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading back the seeded record failed: %v", err)
	}
	return client, append([]byte(nil), cm.BinaryData[payloadKey]...)
}

// assertRecordUntouched reports whether the seeded foreign-owned payload is
// still byte-for-byte what foreignOwnedClient wrote.
func assertRecordUntouched(t *testing.T, client *fake.Clientset, want []byte) {
	t.Helper()
	got, err := client.CoreV1().ConfigMaps(testNamespace).Get(context.Background(), testName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get() error = %v, want the foreign-owned record still present", err)
	}
	if !bytes.Equal(got.BinaryData[payloadKey], want) {
		t.Errorf("BinaryData[%s] changed, want the other install's record left byte-identical", payloadKey)
	}
}

// LoadCurrent must refuse a record another install owns, and must refuse it
// as something other than NotFound: recovery keys "cold start" off NotFound
// alone, so collapsing the two would hand the previous install's ConfigMap to
// the next Start to overwrite. Save has always checked this; LoadCurrent did
// not, which made the read path strictly worse than the unreadable one --
// the record got installed as the current run, Start 409'd on the recovery
// gate, and Retry 409'd on Save's own check.
func TestConfigMapStoreLoadCurrentRejectsForeignOwner(t *testing.T) {
	client, seeded := foreignOwnedClient(t)
	s := engine.NewConfigMapStore(client, testNamespace, testName, testOwner())

	_, err := s.LoadCurrent(context.Background())
	if err == nil {
		t.Fatal("LoadCurrent() error = nil, want a foreign-owned record refused rather than installed")
	}
	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) {
		t.Fatalf("error = %v, want a StructuredError", err)
	}
	if se.Code == aicrerrors.ErrCodeNotFound {
		t.Errorf("Code = %v, want anything but ErrCodeNotFound -- a foreign-owned record must not look like a cold start", se.Code)
	}
	// The seeded record decodes cleanly, so "refused" must mean the owner
	// check refused it, not that decodeRun choked on the payload.
	if se.Code == aicrerrors.ErrCodeInvalidRequest {
		t.Errorf("Code = %v, want the ownership refusal -- ErrCodeInvalidRequest means the payload failed to decode and the owner check never ran", se.Code)
	}

	assertRecordUntouched(t, client, seeded)
}

// Delete needs the same check for the mirror-image reason: a discard is an
// operator action, and it must not reap a record this install does not own.
func TestConfigMapStoreDeleteRefusesForeignOwner(t *testing.T) {
	client, seeded := foreignOwnedClient(t)
	s := engine.NewConfigMapStore(client, testNamespace, testName, testOwner())

	if err := s.Delete(context.Background()); err == nil {
		t.Fatal("Delete() error = nil, want a foreign-owned record refused rather than reaped")
	}

	assertRecordUntouched(t, client, seeded)
}

// The end-to-end consequence I3 is actually about: a pod starting inside the
// reinstall window must degrade cleanly (in-memory store, nothing installed,
// Start works) instead of wedging on a record it can neither retry nor
// discard. Driven through a real configMapStore rather than a Store double,
// because the whole finding is that the store's own read path disagreed with
// its write path.
func TestRecoverDegradesAgainstAForeignOwnedRecord(t *testing.T) {
	client, seeded := foreignOwnedClient(t)
	store := engine.NewConfigMapStore(client, testNamespace, testName, testOwner())
	e := fourStepEngine(store)

	if err := e.Recover(context.Background()); err != nil {
		t.Fatalf("Recover() error = %v, want nil", err)
	}
	if got := e.Current(); got != nil {
		t.Errorf("Current() = %+v, want nil -- another install's run must not be installed", got)
	}
	if !e.StoreUnreadable() {
		t.Error("StoreUnreadable() = false, want true -- a foreign-owned record must degrade the store, not be adopted")
	}

	// The gate is clear, so the SPA's automatic POST /api/runs works rather
	// than 409ing forever against a run the operator can neither retry nor
	// discard.
	if _, err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v, want nil after degrading", err)
	}

	assertRecordUntouched(t, client, seeded)
}

func TestConfigMapStoreDelete(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := engine.NewConfigMapStore(client, testNamespace, testName, testOwner())
	ctx := context.Background()

	if err := s.Save(ctx, &engine.Run{ID: "run-a"}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := s.Delete(ctx); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	_, err := s.LoadCurrent(ctx)
	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) || se.Code != aicrerrors.ErrCodeNotFound {
		t.Errorf("LoadCurrent() after Delete error = %v, want ErrCodeNotFound", err)
	}

	t.Run("on empty store is not an error", func(t *testing.T) {
		empty := engine.NewConfigMapStore(fake.NewSimpleClientset(), testNamespace, testName, testOwner())
		if err := empty.Delete(ctx); err != nil {
			t.Errorf("Delete() on empty store error = %v, want nil", err)
		}
	})
}

// A non-NotFound Delete failure must surface rather than be swallowed as
// success -- only absence means the caller's intent is already satisfied.
// The store must actually reach its Delete call for that to be under test, so
// the fixture seeds a record this install owns: delete now reads before it
// deletes (see delete's own doc comment on why), and an absent record short-
// circuits to success before any Delete is issued.
func TestConfigMapStoreDeleteNonNotFoundErrorSurfaces(t *testing.T) {
	owner := testOwner()
	client := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       testNamespace,
			Name:            testName,
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		BinaryData: map[string][]byte{payloadKey: []byte("placeholder")},
	})
	client.PrependReactor("delete", "configmaps", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(corev1.Resource("configmaps"), testName, errors.New("no access"))
	})

	s := engine.NewConfigMapStore(client, testNamespace, testName, owner)
	if err := s.Delete(context.Background()); err == nil {
		t.Fatal("Delete() error = nil, want the Forbidden error to surface")
	}
}

// The ownership read delete performs first has its own failure mode: a
// non-NotFound Get error must surface rather than be mistaken for "already
// absent, nothing to do" -- which would report a discard as successful while
// the record is still there.
func TestConfigMapStoreDeleteOwnershipReadErrorSurfaces(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("get", "configmaps", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(corev1.Resource("configmaps"), testName, errors.New("no access"))
	})

	s := engine.NewConfigMapStore(client, testNamespace, testName, testOwner())
	if err := s.Delete(context.Background()); err == nil {
		t.Fatal("Delete() error = nil, want the Forbidden Get error to surface rather than read as absence")
	}
}

// Load is the by-ID lookup Task 3 does not use (it uses LoadCurrent), but it
// is still part of the Store contract: a mismatched ID must be NotFound
// rather than silently returning the wrong run.
func TestConfigMapStoreLoadByID(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := engine.NewConfigMapStore(client, testNamespace, testName, testOwner())
	ctx := context.Background()

	if err := s.Save(ctx, &engine.Run{ID: "run-a"}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := s.Load(ctx, "run-a")
	if err != nil {
		t.Fatalf("Load(run-a) error = %v", err)
	}
	if got.ID != "run-a" {
		t.Errorf("Load(run-a).ID = %q, want %q", got.ID, "run-a")
	}

	_, err = s.Load(ctx, "run-b")
	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) || se.Code != aicrerrors.ErrCodeNotFound {
		t.Errorf("Load(run-b) error = %v, want ErrCodeNotFound for a mismatched ID", err)
	}
}

// resourceVersionGuard is the optimistic-concurrency check
// k8s.io/client-go/testing's ObjectTracker deliberately does not perform (see
// its versionedObject comment: "Object content does not get changed to
// preserve the traditional behavior"). Without it there is no staleness for
// configMapStore's retry loop to detect, so a test over the bare fake cannot
// tell a mutex-protected Save from an unprotected one at all -- which is
// exactly how the previous version of the test below passed with both of
// cmstore.go's mutex lines deleted.
//
// It also widens the read-to-write window with a short sleep. A real API
// call is milliseconds of network; the fake's is a map lookup, so without
// this the interleaving the mutex prevents almost never happens and the test
// would fail only intermittently on the mutation it exists to catch.
type resourceVersionGuard struct {
	mu        sync.Mutex
	version   int
	updates   int
	conflicts int
}

const rvGuardReadDelay = 200 * time.Microsecond

// install wires the guard into client and seeds a record at version 1 owned
// by owner, so every writer takes the Get/Update path rather than racing on
// Create.
func (g *resourceVersionGuard) install(t *testing.T, client *fake.Clientset, owner metav1.OwnerReference) {
	t.Helper()
	g.version = 1
	seed := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       testNamespace,
			Name:            testName,
			OwnerReferences: []metav1.OwnerReference{owner},
			ResourceVersion: "1",
		},
		BinaryData: map[string][]byte{payloadKey: []byte("placeholder")},
	}
	gvr := corev1.SchemeGroupVersion.WithResource("configmaps")
	if err := client.Tracker().Create(gvr, seed, testNamespace); err != nil {
		t.Fatalf("seeding the guarded record failed: %v", err)
	}

	client.PrependReactor("get", "configmaps", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		time.Sleep(rvGuardReadDelay)
		return false, nil, nil
	})

	client.PrependReactor("update", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		cm, ok := action.(k8stesting.UpdateAction).GetObject().(*corev1.ConfigMap)
		if !ok {
			return false, nil, nil
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		g.updates++
		// The writer read at some version and is writing back at that same
		// version; anything else means another writer landed in between.
		if cm.ResourceVersion != strconv.Itoa(g.version) {
			g.conflicts++
			return true, nil, apierrors.NewConflict(corev1.Resource("configmaps"), testName,
				errors.New("stale resourceVersion "+cm.ResourceVersion))
		}
		g.version++
		cm.ResourceVersion = strconv.Itoa(g.version)
		// Handled here rather than falling through: Fake.Invokes hands each
		// reactor its own DeepCopy of the action, so a version stamped on this
		// copy would never reach the default tracker reactor.
		if err := client.Tracker().Update(gvr, cm, testNamespace); err != nil {
			return true, nil, err
		}
		return true, cm, nil
	})
}

func (g *resourceVersionGuard) counts() (updates, conflicts int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.updates, g.conflicts
}

// TestConfigMapStoreSerializesWrites pins what configMapStore's mutex is
// actually for: not correctness -- Save overwrites the whole payload, so
// interleaved writes cannot tear -- but conserving the bounded conflict-retry
// budget, which exists for writers OUTSIDE this process (a leftover replica
// mid-rollout, a human `kubectl edit`) and must not be spent on contention
// this process inflicted on itself.
//
// Under resourceVersionGuard, that is directly observable: with the mutex
// held across each Save's Get and Update, 20 concurrent writers produce
// exactly 20 Updates and zero conflicts. Delete the mutex and the same 20
// writers read the same version concurrently, collide, and start burning
// retries.
//
// The counting assertions are the point. "No errors, exactly one ConfigMap"
// -- what this test used to assert -- holds either way, which is why it
// passed with both mutex lines removed and quietly disarmed the Tripwire
// comment it was supposed to be guarding.
func TestConfigMapStoreSerializesWrites(t *testing.T) {
	client := fake.NewSimpleClientset()
	owner := testOwner()
	guard := &resourceVersionGuard{}
	guard.install(t, client, owner)

	s := engine.NewConfigMapStore(client, testNamespace, testName, owner)
	ctx := context.Background()

	const writers = 20
	var wg sync.WaitGroup
	errCh := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errCh <- s.Save(ctx, &engine.Run{ID: fmt.Sprintf("run-%d", i)})
		}(i)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			t.Errorf("Save() error = %v, want nil under concurrent writers", err)
		}
	}

	updates, conflicts := guard.counts()
	if conflicts != 0 {
		t.Errorf("the API server rejected %d Update(s) as stale, want 0 -- this process's own Saves are contending with each other and spending a retry budget reserved for external writers", conflicts)
	}
	if updates != writers {
		t.Errorf("Update calls = %d, want exactly %d (one per writer, none retried)", updates, writers)
	}

	list, err := client.CoreV1().ConfigMaps(testNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list.Items) != 1 {
		t.Errorf("ConfigMaps = %d, want exactly 1 after %d concurrent Saves", len(list.Items), writers)
	}
}

// waitStateSlow is engine_test.go's waitState with a longer budget. Every
// checkpoint in the test below gzips a 1MiB incompressible artifact twice --
// once to find it does not fit, once after shedding it -- and half a dozen of
// those under -race comfortably outruns waitState's 2s.
func waitStateSlow(t *testing.T, e *engine.Engine, id string, want engine.State) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last engine.State
	for time.Now().Before(deadline) {
		r, err := e.Get(context.Background(), id)
		if err == nil {
			if r.State == want {
				return
			}
			last = r.State
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run never reached state %q, last = %q", want, last)
}

// oversizeStep writes an artifact that cannot be compressed under
// cmstore.go's cmPayloadCeiling, so every checkpoint after it has to deal
// with a record over the limit. 1MiB of random bytes is past the 800KiB cap
// on its own and DEFLATE cannot claw any of it back.
type oversizeStep struct{ phase engine.Phase }

func (s oversizeStep) Phase() engine.Phase { return s.phase }
func (s oversizeStep) Requires() []string  { return nil }
func (s oversizeStep) Run(_ context.Context, r *engine.Run, _ engine.Emit) error {
	big := make([]byte, 1<<20)
	if _, err := rand.Read(big); err != nil {
		return err
	}
	r.Artifacts["snapshot.yaml"] = big
	return nil
}

// TestOversizedStateDoesNotWedgeTheRun is I5's end-to-end consequence, driven
// through a real configMapStore so the encode path is the production one.
//
// Decide is the engine's one mandatory checkpoint. While an oversized record
// failed the encode outright, a cluster whose snapshot exceeded the limit put
// the console somewhere no operator action reached: Decide 503'd and rolled
// the run back to awaiting_decision, Discard refused because that state is
// live, and a restart replayed the same oversized artifact into the same
// wall. The run must instead reach StateDone with the record still readable.
func TestOversizedStateDoesNotWedgeTheRun(t *testing.T) {
	client := fake.NewSimpleClientset()
	store := engine.NewConfigMapStore(client, testNamespace, testName, testOwner())
	e := engine.New(bus.New(64), store,
		oversizeStep{phase: engine.PhaseDiscover},
		newFakeStep(engine.PhaseRecommend, "intent"),
	)

	run, err := e.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitStateSlow(t, e, run.ID, engine.StateAwaitingDecision)

	if decErr := e.Decide(context.Background(), run.ID, map[string]string{"intent": "training"}); decErr != nil {
		t.Fatalf("Decide() error = %v -- an oversized artifact must not fail the one checkpoint the run cannot proceed without", decErr)
	}
	waitStateSlow(t, e, run.ID, engine.StateDone)

	// Degraded, not absent: the record is still there and still decodes, so
	// the next restart recovers a real state machine rather than taking the
	// unreadable path. Polled rather than read once -- finish() flips
	// e.current to StateDone under the lock and only then issues its detached
	// terminal save, so the state Get reports leads the persisted record by
	// one write.
	var got *engine.Run
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var loadErr error
		got, loadErr = store.LoadCurrent(context.Background())
		if loadErr != nil {
			t.Fatalf("LoadCurrent() error = %v, want the truncated record still readable", loadErr)
		}
		if got.State == engine.StateDone {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got.ID != run.ID || got.State != engine.StateDone {
		t.Errorf("recovered record = %s/%s, want %s/%s", got.ID, got.State, run.ID, engine.StateDone)
	}
	if _, ok := got.Artifacts["snapshot.yaml"]; ok {
		t.Error("snapshot.yaml survived, want the oversized artifact shed to keep the record writable")
	}
}

// TestConfigMapStoreCallTimeoutReturnsRatherThanBlockingForever pins Ruling
// 15: every ConfigMap call this store issues is bounded on its own,
// regardless of what the caller's context does or does not enforce.
// client-go's fake clientset does not consult ctx at all when invoking a
// reactor (verified against k8s.io/client-go/gentype.FakeClient.Get, which
// discards its ctx parameter before calling Invokes) -- so a reactor that
// blocks forever hangs the calling goroutine unconditionally, exactly what
// a wedged real API server looks like from this store's perspective.
// context.Background() is used deliberately: the property under test is
// that the store enforces its own bound even when the caller supplies none.
func TestConfigMapStoreCallTimeoutReturnsRatherThanBlockingForever(t *testing.T) {
	client := fake.NewSimpleClientset()
	hang := make(chan struct{}) // never closed: a permanently wedged API server
	client.PrependReactor("get", "configmaps", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		<-hang
		return false, nil, nil
	})
	s := engine.NewConfigMapStore(client, testNamespace, testName, testOwner())

	start := time.Now()
	err := s.Save(context.Background(), &engine.Run{ID: "run-a"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Save() error = nil, want the store's own call timeout to surface against a reactor that never returns")
	}
	// Generous relative to the store's own bound (cmStoreCallTimeout, 10s):
	// this asserts "returned promptly," not an exact deadline, so it stays
	// robust against scheduler noise under -race without also accepting
	// "eventually" as a passing outcome.
	if elapsed > 20*time.Second {
		t.Errorf("Save() took %s to return against a hung reactor, want it bounded near the store's own call timeout, not blocking indefinitely", elapsed)
	}
}

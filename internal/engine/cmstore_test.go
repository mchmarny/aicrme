package engine_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

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

// 20 concurrent Saves must all succeed and leave exactly one ConfigMap. Run
// under -race: the mutex is what this test actually exercises.
func TestConfigMapStoreSerializesWrites(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := engine.NewConfigMapStore(client, testNamespace, testName, testOwner())
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

	list, err := client.CoreV1().ConfigMaps(testNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list.Items) != 1 {
		t.Errorf("ConfigMaps = %d, want exactly 1 after %d concurrent Saves", len(list.Items), writers)
	}
}

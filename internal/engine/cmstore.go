package engine

import (
	"context"
	"sync"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// payloadKey is the single binaryData key holding the gzipped envelope.
// binaryData, not data: the payload is compressed bytes, and client-go
// handles the base64 transport encoding itself.
const payloadKey = "run"

// maxConflictRetries bounds optimistic-concurrency retries. The chart sets
// strategy: Recreate so overlapping writers should not exist; this is the
// belt to that braces, and it is bounded because an unbounded retry against a
// genuinely contended object is an infinite loop, not resilience.
const maxConflictRetries = 5

type configMapStore struct {
	client    kubernetes.Interface
	namespace string
	name      string
	owner     metav1.OwnerReference

	// mu serializes writes. Two concurrent Saves would each read-modify-write
	// the same object and one would silently lose; conflict retries recover
	// from *external* races, not from this process racing itself.
	mu sync.Mutex
}

// NewConfigMapStore returns a Store backed by a single ConfigMap.
//
// owner must reference the Deployment, never its ReplicaSet: a ReplicaSet is
// replaced on every rollout and ownerReference garbage collection would then
// delete the run state as a side effect of upgrading the console.
func NewConfigMapStore(client kubernetes.Interface, namespace, name string, owner metav1.OwnerReference) Store {
	return &configMapStore{client: client, namespace: namespace, name: name, owner: owner}
}

// Save writes the whole run as one ConfigMap, creating it on first use and
// updating it thereafter. The loop absorbs conflicts from writers outside
// this process (a leftover replica mid-rollout, a human `kubectl edit`); it
// is bounded so a genuinely contended object fails loudly instead of
// spinning forever.
func (s *configMapStore) Save(ctx context.Context, r *Run) error {
	blob, err := encodeRun(r)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var lastErr error
	for attempt := 0; attempt < maxConflictRetries; attempt++ {
		existing, getErr := s.client.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(getErr):
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:            s.name,
					Namespace:       s.namespace,
					OwnerReferences: []metav1.OwnerReference{s.owner},
				},
				BinaryData: map[string][]byte{payloadKey: blob},
			}
			_, createErr := s.client.CoreV1().ConfigMaps(s.namespace).Create(ctx, cm, metav1.CreateOptions{})
			if createErr == nil {
				return nil
			}
			// A racing creator wins the Create; retry folds into the Get/Update
			// path above on the next pass.
			if apierrors.IsAlreadyExists(createErr) || apierrors.IsConflict(createErr) {
				lastErr = createErr
				continue
			}
			return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "creating run checkpoint failed", createErr)

		case getErr != nil:
			return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "reading run checkpoint failed", getErr)

		case foreignOwner(existing.OwnerReferences, s.owner.UID):
			return aicrerrors.New(aicrerrors.ErrCodeConflict,
				"run checkpoint "+s.namespace+"/"+s.name+" is owned by a different install; refusing to overwrite")

		default:
			if existing.BinaryData == nil {
				existing.BinaryData = map[string][]byte{}
			}
			existing.BinaryData[payloadKey] = blob
			_, updateErr := s.client.CoreV1().ConfigMaps(s.namespace).Update(ctx, existing, metav1.UpdateOptions{})
			if updateErr == nil {
				return nil
			}
			if apierrors.IsConflict(updateErr) {
				lastErr = updateErr
				continue
			}
			return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "updating run checkpoint failed", updateErr)
		}
	}
	return aicrerrors.Wrap(aicrerrors.ErrCodeConflict,
		"giving up on run checkpoint after repeated conflicts", lastErr)
}

// foreignOwner reports whether existing carries an owner UID that is set and
// differs from want -- a record another install's controller owns.
func foreignOwner(existing []metav1.OwnerReference, want types.UID) bool {
	for _, o := range existing {
		if o.UID != "" && o.UID != want {
			return true
		}
	}
	return false
}

// LoadCurrent reads the ConfigMap and decodes its payload. A missing
// ConfigMap is the only case mapped to ErrCodeNotFound: a present-but-broken
// record (missing key, undecodable payload) is deliberately a different
// code, because Task 3's recovery must not treat "unreadable" as "nothing to
// recover" -- that is exactly the mistake that would let a new run overwrite
// a record that was merely unreadable at that moment.
func (s *configMapStore) LoadCurrent(ctx context.Context) (*Run, error) {
	cm, err := s.client.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, aicrerrors.New(aicrerrors.ErrCodeNotFound,
			"no run checkpoint at "+s.namespace+"/"+s.name)
	}
	if err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "reading run checkpoint failed", err)
	}
	blob, ok := cm.BinaryData[payloadKey]
	if !ok {
		return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			"run checkpoint is missing its payload key")
	}
	r, err := decodeRun(blob)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// Load returns the current run if its ID matches; this store holds one run,
// so any other ID genuinely is not found.
func (s *configMapStore) Load(ctx context.Context, id string) (*Run, error) {
	r, err := s.LoadCurrent(ctx)
	if err != nil {
		return nil, err
	}
	if r.ID != id {
		return nil, aicrerrors.New(aicrerrors.ErrCodeNotFound, "run not found: "+id)
	}
	return r, nil
}

// Delete removes the ConfigMap. A missing ConfigMap is success: the caller's
// intent (no checkpoint should remain) is already satisfied.
func (s *configMapStore) Delete(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	err := s.client.CoreV1().ConfigMaps(s.namespace).Delete(ctx, s.name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "deleting run checkpoint failed", err)
	}
	return nil
}

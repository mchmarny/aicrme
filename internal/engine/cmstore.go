package engine

import (
	"context"
	"sync"
	"time"

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

// cmStoreCallTimeout bounds every ConfigMap API call this store issues, on
// behalf of every caller -- Get's Load, Retry's checkpoint Save, and
// Discard's Delete previously relied on nothing but the raw caller context,
// which internal/api's handlers thread from the request today (Task 7).
// Once main.go wires this store in for real, a wedged API server -- a
// control-plane partition, a stuck proxy, anything that accepts a
// connection and never answers -- would otherwise hang the HTTP request
// that triggered it indefinitely: main.go sets no http.Server.WriteTimeout,
// and this store cannot assume every caller remembers to supply its own
// deadline. Ruling 15: the store is what knows it performs network I/O, so
// the guarantee belongs here, not at each call site.
//
// Composes with a caller's own shorter deadline rather than replacing it --
// nested context.WithTimeout calls always resolve to the earlier of the two
// deadlines, so Decide's decideSaveTimeout (5s, engine.go) still governs its
// own save even though this bound is longer. The two are not duplicates and
// neither should be collapsed into the other: decideSaveTimeout also
// detaches cancellation (context.WithoutCancel) so an already-acknowledged
// operator decision survives a canceled caller context, a concern this
// timeout does not address at all -- see engine.go's Decide.
const cmStoreCallTimeout = 10 * time.Second

// withCallTimeout runs fn against a context bounded by cmStoreCallTimeout
// (composed with ctx's own deadline, see cmStoreCallTimeout's comment) and
// returns as soon as either fn completes or the bound expires, whichever
// comes first.
//
// fn runs in its own goroutine because the bound must hold even against a
// callee that does not itself respect context cancellation. That is exactly
// the case for client-go's fake clientset, which this package's own tests
// use: its generated Get/Create/Update/Delete methods discard ctx entirely
// before invoking a reactor (verified against
// k8s.io/client-go/gentype.FakeClient.Get), so a reactor that blocks
// ignores any deadline passed in unless something outside the fake itself
// enforces one -- which is what this function is for. A timed-out fn's
// goroutine is abandoned, not stopped (Go has no way to preempt a running
// goroutine); it is left to exit on its own whenever whatever it was
// blocked on eventually resolves. This call's caller is unblocked either
// way, which is the property that matters to an HTTP request waiting on it.
func withCallTimeout[T any](ctx context.Context, fn func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, cmStoreCallTimeout)
	defer cancel()

	type result struct {
		val T
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := fn(ctx)
		done <- result{v, err}
	}()

	select {
	case r := <-done:
		return r.val, r.err
	case <-ctx.Done():
		var zero T
		return zero, aicrerrors.Wrap(aicrerrors.ErrCodeTimeout,
			"ConfigMap call did not complete within the store's call timeout", ctx.Err())
	}
}

type configMapStore struct {
	client    kubernetes.Interface
	namespace string
	name      string
	owner     metav1.OwnerReference

	// mu serializes this process's own Save calls. It is not what makes
	// writes correct: Save always overwrites the whole payload rather than
	// merging into the existing record, so two interleaved Saves cannot tear
	// or corrupt state -- last write wins either way, and the conflict-retry
	// loop below already recovers a stale write against a real API server on
	// its own. What mu buys is avoiding *self-inflicted* conflicts: without
	// it, this process's own concurrent Saves would burn through the bounded
	// retry budget on each other, a budget sized for genuine external races
	// (a leftover replica mid-rollout, a human `kubectl edit`), not for
	// contention this process could have avoided for free.
	//
	// Tripwire: if Save ever changes to merge into the existing record
	// instead of overwriting it wholesale, mu stops being optional --
	// a read-modify-write of a sub-field needs it for correctness, not
	// just to conserve retry budget.
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
	_, err = withCallTimeout(ctx, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, s.save(ctx, blob)
	})
	return err
}

// save is Save's body, split out so withCallTimeout can run it in its own
// goroutine. s.mu.Lock is acquired in here rather than in Save itself: if it
// were acquired before withCallTimeout is even called, a Save contending
// against an already-wedged prior Save (whose goroutine is still holding mu,
// abandoned past its own timeout) would block on Lock before the timeout
// logic ever ran, defeating the bound entirely.
func (s *configMapStore) save(ctx context.Context, blob []byte) error {
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
			return s.foreignOwnerErr("overwrite it")

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

// foreignOwnerErr is the one error shape every operation returns for a record
// another install owns; verb names what this call refused to do.
//
// ErrCodeConflict, deliberately not ErrCodeNotFound: Recover keys "cold
// start" off NotFound alone, so returning that here would tell recovery the
// other install's record does not exist -- and the very next Start would
// overwrite it. Conflict instead lands recovery on markStoreUnreadable: the
// console degrades to an in-memory store, leaves the foreign record exactly
// as it found it, and says so at error level. It is also outside
// loadCurrentRetryable's ErrCodeInternal set, so it fails on the first
// attempt rather than spending the startup budget on an answer that cannot
// change.
func (s *configMapStore) foreignOwnerErr(verb string) error {
	return aicrerrors.New(aicrerrors.ErrCodeConflict,
		"run checkpoint "+s.namespace+"/"+s.name+" is owned by a different install; refusing to "+verb)
}

// LoadCurrent reads the ConfigMap and decodes its payload. A missing
// ConfigMap is the only case mapped to ErrCodeNotFound: a present-but-broken
// record (missing key, undecodable payload) is deliberately a different
// code, because Task 3's recovery must not treat "unreadable" as "nothing to
// recover" -- that is exactly the mistake that would let a new run overwrite
// a record that was merely unreadable at that moment.
func (s *configMapStore) LoadCurrent(ctx context.Context) (*Run, error) {
	return withCallTimeout(ctx, s.loadCurrent)
}

func (s *configMapStore) loadCurrent(ctx context.Context) (*Run, error) {
	cm, err := s.client.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, aicrerrors.New(aicrerrors.ErrCodeNotFound,
			"no run checkpoint at "+s.namespace+"/"+s.name)
	}
	if err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "reading run checkpoint failed", err)
	}
	// The same check save performs, for the same reason and one step earlier.
	// `helm uninstall && helm install` gives the Deployment a new UID, but
	// ownerReference garbage collection is asynchronous, so a pod starting in
	// that window finds the PREVIOUS install's record still present. Without
	// this, recovery installs it as the current run: Start then 409s on the
	// recovery gate, Retry 409s on save's own foreign-owner check, and the
	// console stays wedged even after GC finally reaps the ConfigMap --
	// strictly worse than the unreadable path, which degrades cleanly. With
	// it, recovery takes exactly that unreadable path instead.
	if foreignOwner(cm.OwnerReferences, s.owner.UID) {
		return nil, s.foreignOwnerErr("read it")
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
	_, err := withCallTimeout(ctx, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, s.delete(ctx)
	})
	return err
}

// delete is Delete's body, split out for the same reason save is: s.mu.Lock
// must happen inside the goroutine withCallTimeout races against the bound,
// not before it is spawned.
//
// It reads before it deletes so a discard cannot reap another install's
// record -- the same asymmetry loadCurrent closes, in the other direction.
// The gap between the Get and the Delete is not a race worth closing with a
// UID precondition: the chart pins strategy: Recreate, so this process is the
// only writer, and the case being defended against is a leftover record from
// an install that is already gone, not one being written concurrently.
func (s *configMapStore) delete(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, err := s.client.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return nil // already absent: the caller's intent is satisfied
	case err != nil:
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "reading run checkpoint before deleting it failed", err)
	case foreignOwner(existing.OwnerReferences, s.owner.UID):
		return s.foreignOwnerErr("delete it")
	}

	err = s.client.CoreV1().ConfigMaps(s.namespace).Delete(ctx, s.name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "deleting run checkpoint failed", err)
	}
	return nil
}

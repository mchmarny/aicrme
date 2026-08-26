package engine

import (
	"context"
	"errors"
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
	// mu makes every method safe to call from separate goroutines with no
	// lock of the caller's own held. engine.go documents that Store I/O
	// never runs under e.mu, and Discard's store.Delete in particular runs
	// fully unlocked -- so it can genuinely race a concurrent LoadCurrent
	// from another goroutine. Save and Delete take the write lock; Load and
	// LoadCurrent take the read lock.
	//
	// The read lock is not only for the Save/Delete write pairing (the
	// pointer file and the run file are two separate writes, and the rename
	// below is only atomic on its own). LoadCurrent is itself a two-step
	// READ -- the current pointer, then the record it names -- and without a
	// lock a Delete landing between those two steps is observed as "pointer
	// present, record gone": a dangling pointer LoadCurrent cannot tell
	// apart from real corruption, turning a clean concurrent Delete into a
	// spurious error. memoryStore (store.go) takes its RWMutex on all four
	// methods for the same reason -- a Store that is not independently
	// concurrency-safe is not a substitute for it.
	mu  sync.RWMutex
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
	// Sync before rename: without it, the temp file's data can still be
	// unflushed when the rename that publishes it lands, so a crash in that
	// window can leave a torn or zero-length record at path -- which
	// decodeRun reports as corrupt rather than absent, the one outcome that
	// stops recovery cold. That is the full scope of what this buys: it
	// keeps a torn record from ever being observable. It does NOT make the
	// rename itself crash-durable -- the directory entry Rename creates is
	// never fsynced, so a crash immediately after a successful Save can
	// still lose that write on some filesystems. Accepted here: this is a
	// local, single-operator console, and a lost last write is re-derivable
	// by re-running, unlike a torn record, which is not.
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
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.load(id)
}

// load is Load's body without the lock, so LoadCurrent can reuse it while
// already holding the read lock it took for the pointer read. A second RLock
// from the same goroutine is not safe to add on top of that: sync.RWMutex is
// not reentrant, and a writer (Save or Delete) queued in between the two
// RLock calls would deadlock the goroutine against itself.
func (s *fileStore) load(id string) (*Run, error) {
	blob, err := os.ReadFile(s.runPath(id))
	if os.IsNotExist(err) {
		return nil, aicrerrors.New(aicrerrors.ErrCodeNotFound, "run not found: "+id)
	}
	if err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "reading the run record failed", err)
	}
	return decodeRun(blob, filePayloadCeiling)
}

// LoadCurrent reads the pointer and then the record it names, under a single
// read lock -- see mu's doc comment for why the two-step read has to be one
// critical section rather than two.
//
// A missing pointer is ErrCodeNotFound -- nothing to recover. A pointer naming
// a record that is missing or undecodable is deliberately NOT: recovery must
// not read "unreadable" as "nothing there", because that is exactly the
// mistake that lets a new run overwrite a record that was only momentarily
// unreadable. Same distinction the ConfigMap store drew, for the same reason.
func (s *fileStore) LoadCurrent(_ context.Context) (*Run, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
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
	run, err := s.load(string(id))
	var se *aicrerrors.StructuredError
	if errors.As(err, &se) && se.Code == aicrerrors.ErrCodeNotFound {
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

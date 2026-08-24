package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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
		if saveErr := s.Save(context.Background(), &Run{ID: "abcdef0123456789", State: StateRunning}); saveErr != nil {
			t.Fatalf("Save() error = %v", saveErr)
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
	var se *aicrerrors.StructuredError
	if errors.As(err, &se) && se.Code == aicrerrors.ErrCodeNotFound {
		t.Error("a dangling pointer reported NotFound -- recovery would treat it as a clean start")
	}
}

// The dangling-pointer requirement has two arms: a pointer naming a record
// that is missing (TestFileStoreDanglingPointerIsNotAbsence, above) and one
// naming a record that is present but undecodable. Both must refuse
// ErrCodeNotFound for the same reason -- recovery must not read either one as
// "nothing to recover". decodeRun already returns ErrCodeInvalidRequest for a
// garbled payload, so LoadCurrent's NotFound-only branch falls through
// correctly on its own; this pins that as a regression test rather than
// leaving it merely true by construction.
func TestFileStoreDanglingPointerToCorruptRecordIsNotAbsence(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewFileStore(dir)
	const id = "abcdef0123456789"
	if err := os.WriteFile(filepath.Join(dir, currentFile), []byte(id), 0o600); err != nil {
		t.Fatalf("WriteFile(current) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".run"), []byte("not a gzip envelope"), 0o600); err != nil {
		t.Fatalf("WriteFile(record) error = %v", err)
	}
	_, err := s.LoadCurrent(context.Background())
	if err == nil {
		t.Fatal("LoadCurrent() succeeded against a corrupt record")
	}
	var se *aicrerrors.StructuredError
	if errors.As(err, &se) && se.Code == aicrerrors.ErrCodeNotFound {
		t.Error("a corrupt record reported NotFound -- recovery would treat it as a clean start")
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
	_, err := b.LoadCurrent(context.Background())
	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) || se.Code != aicrerrors.ErrCodeNotFound {
		t.Errorf("the second store saw the first store's run: err = %v", err)
	}
}

// TestFileStoreConcurrentSaveDeleteLoadCurrentDoesNotRace drives the
// concurrency mu exists for: engine.go documents that Store I/O never runs
// under e.mu, and Discard's store.Delete in particular runs fully unlocked,
// so it can race a concurrent LoadCurrent (or Save) from another goroutine.
// Before mu covered Load and LoadCurrent, a Delete landing between
// LoadCurrent's pointer read and its record read was observable as "pointer
// present, record gone" -- indistinguishable from real corruption, so
// LoadCurrent returned the same ErrCodeInternal a dangling pointer produces
// even though the true post-race state is just "no current run".
//
// This does not assert a single expected outcome for LoadCurrent -- which
// run, if any, is current at a given instant is inherently racy against
// concurrent Saves and Deletes. What it pins is that every LoadCurrent
// result is one this Store contract actually promises: a run, or
// ErrCodeNotFound. Anything else -- in particular the spurious
// ErrCodeInternal the missing read lock produced -- fails the test. Run
// under -race so a genuine data race in the locking itself, not just the
// logical one this guards against, would also be caught.
func TestFileStoreConcurrentSaveDeleteLoadCurrentDoesNotRace(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore() error = %v", err)
	}

	const writers = 6
	const readers = 6
	const iterations = 25
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				id := fmt.Sprintf("worker%02drun%04d0000", worker, j)
				if saveErr := s.Save(ctx, &Run{ID: id, State: StateRunning}); saveErr != nil {
					t.Errorf("Save() error = %v", saveErr)
				}
			}
		}(i)
	}
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				if delErr := s.Delete(ctx); delErr != nil {
					t.Errorf("Delete() error = %v", delErr)
				}
			}
		}()
	}
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				if _, loadErr := s.LoadCurrent(ctx); loadErr != nil {
					var se *aicrerrors.StructuredError
					if !errors.As(loadErr, &se) || se.Code != aicrerrors.ErrCodeNotFound {
						t.Errorf("LoadCurrent() error = %v, want nil or a StructuredError with ErrCodeNotFound", loadErr)
					}
				}
			}
		}()
	}
	wg.Wait()
}

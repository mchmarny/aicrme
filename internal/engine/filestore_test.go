package engine

import (
	"context"
	"errors"
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

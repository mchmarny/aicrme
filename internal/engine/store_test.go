package engine_test

import (
	"context"
	"errors"
	"testing"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/mchmarny/aicrme/internal/engine"
)

func TestMemoryStoreLoadCurrentBeforeAnySaveIsNotFound(t *testing.T) {
	s := engine.NewMemoryStore()
	_, err := s.LoadCurrent(context.Background())
	if err == nil {
		t.Fatal("LoadCurrent() error = nil, want ErrCodeNotFound")
	}
	var se *aicrerrors.StructuredError
	// Assert the code, not the message: recovery keys off exactly this
	// distinction to tell "nothing to recover" apart from every other
	// failure mode a real backing store can produce.
	if !errors.As(err, &se) || se.Code != aicrerrors.ErrCodeNotFound {
		t.Errorf("error = %v, want a StructuredError with ErrCodeNotFound", err)
	}
}

func TestMemoryStoreLoadCurrentAfterSaveReturnsIt(t *testing.T) {
	s := engine.NewMemoryStore()
	ctx := context.Background()
	run := &engine.Run{
		ID:        "run-a",
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
	if string(got.Artifacts["snapshot.yaml"]) != "nodes: []\n" {
		t.Errorf("Artifacts[snapshot.yaml] = %q, want the saved bytes", got.Artifacts["snapshot.yaml"])
	}
}

// TestMemoryStoreLoadCurrentTracksTheLatestSave pins the single-pointer
// semantic: `current` is one string, overwritten on every Save, not a set of
// every run that ever existed. Saving B after A does not delete A -- Load(A)
// still succeeds by ID -- it only moves what "current" points at. That is
// the deliberate shape for a console that runs exactly one recovery-worthy
// flow at a time: LoadCurrent's job is "what was in flight most recently",
// not "list everything". A future reader must not "fix" this into a
// multi-run store without first changing that product assumption.
func TestMemoryStoreLoadCurrentTracksTheLatestSave(t *testing.T) {
	s := engine.NewMemoryStore()
	ctx := context.Background()
	a := &engine.Run{ID: "run-a"}
	b := &engine.Run{ID: "run-b"}
	if err := s.Save(ctx, a); err != nil {
		t.Fatalf("Save(a) error = %v", err)
	}
	if err := s.Save(ctx, b); err != nil {
		t.Fatalf("Save(b) error = %v", err)
	}
	got, err := s.LoadCurrent(ctx)
	if err != nil {
		t.Fatalf("LoadCurrent() error = %v", err)
	}
	if got.ID != "run-b" {
		t.Errorf("LoadCurrent().ID = %q, want %q (the most recent Save)", got.ID, "run-b")
	}
	if _, err := s.Load(ctx, "run-a"); err != nil {
		t.Errorf("Load(run-a) error = %v, want run-a still retrievable by ID after B superseded it as current", err)
	}
}

func TestMemoryStoreDeleteClearsCurrent(t *testing.T) {
	s := engine.NewMemoryStore()
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
}

func TestMemoryStoreDeleteOnEmptyStoreIsNotAnError(t *testing.T) {
	s := engine.NewMemoryStore()
	if err := s.Delete(context.Background()); err != nil {
		t.Errorf("Delete() on empty store error = %v, want nil", err)
	}
}

// TestMemoryStoreLoadCurrentReturnsAClone mirrors TestGetReturnsCopy's
// contract (engine_test.go) for the store's read path: Save already clones
// on the write side, so a caller mutating what LoadCurrent handed back must
// not be able to reach into engine-owned state and corrupt what the next
// LoadCurrent (or the engine's own in-memory Run) sees. A live pointer here
// would let a caller outside the engine lock mutate state the lock is
// supposed to protect.
func TestMemoryStoreLoadCurrentReturnsAClone(t *testing.T) {
	s := engine.NewMemoryStore()
	ctx := context.Background()
	if err := s.Save(ctx, &engine.Run{
		ID:        "run-a",
		Artifacts: map[string][]byte{"k": []byte("v1")},
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	first, err := s.LoadCurrent(ctx)
	if err != nil {
		t.Fatalf("LoadCurrent() error = %v", err)
	}
	first.ID = "mutated"
	first.Artifacts["k"] = []byte("mutated")

	second, err := s.LoadCurrent(ctx)
	if err != nil {
		t.Fatalf("LoadCurrent() error = %v", err)
	}
	if second.ID != "run-a" {
		t.Errorf("second.ID = %q, want %q -- LoadCurrent handed out a live pointer", second.ID, "run-a")
	}
	if string(second.Artifacts["k"]) != "v1" {
		t.Errorf("second.Artifacts[k] = %q, want %q -- LoadCurrent handed out a live pointer",
			second.Artifacts["k"], "v1")
	}
}

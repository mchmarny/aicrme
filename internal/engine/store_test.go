package engine_test

import (
	"context"
	"errors"
	"testing"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/mchmarny/aicrme/internal/engine"
)

// storeFactories is every Store implementation, run against one contract.
// A store that passes this is substitutable for the one the engine's own
// tests use -- which is the point of implementing Store rather than
// inventing a persistence model alongside it.
func storeFactories(t *testing.T) map[string]func() engine.Store {
	t.Helper()
	return map[string]func() engine.Store{
		"memory": engine.NewMemoryStore,
		"file": func() engine.Store {
			s, err := engine.NewFileStore(t.TempDir())
			if err != nil {
				t.Fatalf("NewFileStore() error = %v", err)
			}
			return s
		},
	}
}

func TestStoreRoundTrip(t *testing.T) {
	for name, newStore := range storeFactories(t) {
		t.Run(name, func(t *testing.T) {
			s := newStore()
			run := &engine.Run{ID: "abcdef0123456789", State: engine.StateRunning, Phase: engine.PhaseDiscover}
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
			var se *aicrerrors.StructuredError
			// Assert the code, not the message: recovery keys off exactly this
			// distinction to tell "nothing to recover" apart from every other
			// failure mode a real backing store can produce.
			if !errors.As(err, &se) || se.Code != aicrerrors.ErrCodeNotFound {
				t.Errorf("LoadCurrent() error = %v, want a StructuredError with ErrCodeNotFound", err)
			}
		})
	}
}

// TestStoreLoadCurrentAfterSaveReturnsIt is the Load side of the round trip:
// not just the state-machine fields TestStoreRoundTrip checks, but the
// artifact bytes too -- the part the envelope's compression and (for a file
// store) the temp-file-and-rename write path both have to survive intact.
func TestStoreLoadCurrentAfterSaveReturnsIt(t *testing.T) {
	for name, newStore := range storeFactories(t) {
		t.Run(name, func(t *testing.T) {
			s := newStore()
			ctx := context.Background()
			run := &engine.Run{
				ID:        "abcdef0123456789",
				Artifacts: map[string][]byte{"snapshot.yaml": []byte("nodes: []\n")},
			}
			if err := s.Save(ctx, run); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			got, err := s.LoadCurrent(ctx)
			if err != nil {
				t.Fatalf("LoadCurrent() error = %v", err)
			}
			if got.ID != run.ID {
				t.Errorf("ID = %q, want %q", got.ID, run.ID)
			}
			if string(got.Artifacts["snapshot.yaml"]) != "nodes: []\n" {
				t.Errorf("Artifacts[snapshot.yaml] = %q, want the saved bytes", got.Artifacts["snapshot.yaml"])
			}
		})
	}
}

// TestStoreLoadCurrentTracksTheLatestSave pins the single-pointer semantic:
// "current" names one run, overwritten on every Save, not a set of every run
// that ever existed. Saving B after A does not delete A -- Load(A) still
// succeeds by ID -- it only moves what "current" points at. That is the
// deliberate shape for a console that runs exactly one recovery-worthy flow
// at a time: LoadCurrent's job is "what was in flight most recently", not
// "list everything". A future reader must not "fix" this into a multi-run
// store without first changing that product assumption.
func TestStoreLoadCurrentTracksTheLatestSave(t *testing.T) {
	for name, newStore := range storeFactories(t) {
		t.Run(name, func(t *testing.T) {
			s := newStore()
			ctx := context.Background()
			a := &engine.Run{ID: "aaaaaaaa00000000"}
			b := &engine.Run{ID: "bbbbbbbb11111111"}
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
			if got.ID != b.ID {
				t.Errorf("LoadCurrent().ID = %q, want %q (the most recent Save)", got.ID, b.ID)
			}
			if _, err := s.Load(ctx, a.ID); err != nil {
				t.Errorf("Load(a) error = %v, want %s still retrievable by ID after B superseded it as current", err, a.ID)
			}
		})
	}
}

func TestStoreDeleteClearsCurrent(t *testing.T) {
	for name, newStore := range storeFactories(t) {
		t.Run(name, func(t *testing.T) {
			s := newStore()
			if err := s.Save(context.Background(), &engine.Run{ID: "abcdef0123456789", State: engine.StateDone}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			if err := s.Delete(context.Background()); err != nil {
				t.Fatalf("Delete() error = %v", err)
			}
			_, err := s.LoadCurrent(context.Background())
			var se *aicrerrors.StructuredError
			if !errors.As(err, &se) || se.Code != aicrerrors.ErrCodeNotFound {
				t.Errorf("LoadCurrent() after Delete error = %v, want a StructuredError with ErrCodeNotFound", err)
			}
		})
	}
}

func TestStoreDeleteOnEmptyStoreIsNotAnError(t *testing.T) {
	for name, newStore := range storeFactories(t) {
		t.Run(name, func(t *testing.T) {
			if err := newStore().Delete(context.Background()); err != nil {
				t.Errorf("Delete() on empty store error = %v, want nil", err)
			}
		})
	}
}

// TestStoreLoadCurrentReturnsAnIndependentCopy mirrors TestGetReturnsCopy's
// contract (engine_test.go) for the store's read path: a caller mutating
// what LoadCurrent handed back must not be able to reach into store-owned
// state and corrupt what the next LoadCurrent sees. Each implementation earns
// this differently -- the memory store clones on Save, the file store
// re-decodes from disk on every read -- but the observable guarantee is the
// same, so it belongs in the shared contract rather than in either store's
// own test file.
func TestStoreLoadCurrentReturnsAnIndependentCopy(t *testing.T) {
	for name, newStore := range storeFactories(t) {
		t.Run(name, func(t *testing.T) {
			s := newStore()
			ctx := context.Background()
			if err := s.Save(ctx, &engine.Run{
				ID:        "abcdef0123456789",
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
			if second.ID != "abcdef0123456789" {
				t.Errorf("second.ID = %q, want %q -- LoadCurrent handed out a live pointer", second.ID, "abcdef0123456789")
			}
			if string(second.Artifacts["k"]) != "v1" {
				t.Errorf("second.Artifacts[k] = %q, want %q -- LoadCurrent handed out a live pointer",
					second.Artifacts["k"], "v1")
			}
		})
	}
}

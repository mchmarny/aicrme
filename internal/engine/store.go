package engine

import (
	"context"
	"sync"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

// Store persists run state so a pod restart mid-demo does not wipe the
// timeline. The memory implementation is the development and test default;
// the ConfigMap-backed implementation is what makes restart recovery real.
//
// LoadCurrent exists because startup has no run ID to ask for: recovery's
// whole problem is finding what was in flight, not fetching something known.
type Store interface {
	Save(ctx context.Context, r *Run) error
	Load(ctx context.Context, id string) (*Run, error)
	LoadCurrent(ctx context.Context) (*Run, error)
	Delete(ctx context.Context) error
}

type memoryStore struct {
	mu      sync.RWMutex
	runs    map[string]*Run
	current string
}

// NewMemoryStore returns an in-process Store.
func NewMemoryStore() Store {
	return &memoryStore{runs: make(map[string]*Run)}
}

func (m *memoryStore) Save(_ context.Context, r *Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs[r.ID] = r.Clone()
	m.current = r.ID
	return nil
}

func (m *memoryStore) Load(_ context.Context, id string) (*Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.runs[id]
	if !ok {
		return nil, aicrerrors.New(aicrerrors.ErrCodeNotFound, "run not found: "+id)
	}
	return r.Clone(), nil
}

func (m *memoryStore) LoadCurrent(_ context.Context) (*Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.current == "" {
		return nil, aicrerrors.New(aicrerrors.ErrCodeNotFound, "no current run")
	}
	r, ok := m.runs[m.current]
	if !ok {
		return nil, aicrerrors.New(aicrerrors.ErrCodeNotFound, "no current run")
	}
	return r.Clone(), nil
}

func (m *memoryStore) Delete(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.runs, m.current)
	m.current = ""
	return nil
}

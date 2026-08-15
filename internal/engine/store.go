package engine

import (
	"context"
	"sync"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

// Store persists run state so a pod restart mid-demo does not wipe the
// timeline. The memory implementation is the development and test default;
// the ConfigMap-backed implementation lands with Phase 2, when Apply's
// 10-to-20-minute duration makes restart survival worth the complexity.
type Store interface {
	Save(ctx context.Context, r *Run) error
	Load(ctx context.Context, id string) (*Run, error)
}

type memoryStore struct {
	mu   sync.RWMutex
	runs map[string]*Run
}

// NewMemoryStore returns an in-process Store.
func NewMemoryStore() Store {
	return &memoryStore{runs: make(map[string]*Run)}
}

func (m *memoryStore) Save(_ context.Context, r *Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs[r.ID] = r.Clone()
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

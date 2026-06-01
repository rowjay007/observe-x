package sampling

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// StateStore persists periodic snapshots of the sampler's
// learned-baseline state (EWMA mean/variance per service) so that
// process restarts don't wipe out cold-start protection.
//
// Implementations:
//
//   - InMemoryStore   — default; persists only for the process lifetime.
//   - RedisStore      — backed by a Redis key (`observex:sampling:state:<tenant>`).
//
// The store is decoupled from the hot path: the sampler keeps its
// EWMA in process memory and flushes a snapshot every FlushInterval
// (default 30s). A flush failure does not block samping; it is logged
// at WARN level and retried on the next tick.
type StateStore interface {
	Save(ctx context.Context, tenantID string, state map[string]ewmaSnapshot) error
	Load(ctx context.Context, tenantID string) (map[string]ewmaSnapshot, error)
	Close() error
}

// ─── In-memory store ──────────────────────────────────────────────────────

type InMemoryStore struct {
	mu   sync.RWMutex
	data map[string]map[string]ewmaSnapshot
}

func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{data: map[string]map[string]ewmaSnapshot{}}
}

func (s *InMemoryStore) Save(_ context.Context, tenantID string, state map[string]ewmaSnapshot) error {
	cp := make(map[string]ewmaSnapshot, len(state))
	for k, v := range state {
		cp[k] = v
	}
	s.mu.Lock()
	s.data[tenantID] = cp
	s.mu.Unlock()
	return nil
}

func (s *InMemoryStore) Load(_ context.Context, tenantID string) (map[string]ewmaSnapshot, error) {
	s.mu.RLock()
	v := s.data[tenantID]
	s.mu.RUnlock()
	out := make(map[string]ewmaSnapshot, len(v))
	for k, vv := range v {
		out[k] = vv
	}
	return out, nil
}

func (s *InMemoryStore) Close() error { return nil }

// ─── JSON encode/decode helpers shared by Redis impl ──────────────────────

func encodeState(s map[string]ewmaSnapshot) ([]byte, error) {
	return json.Marshal(s)
}

func decodeState(b []byte) (map[string]ewmaSnapshot, error) {
	if len(b) == 0 {
		return map[string]ewmaSnapshot{}, nil
	}
	out := map[string]ewmaSnapshot{}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Ensure the time package is imported in case we later add wall-clock
// timestamps to snapshots.
var _ = time.Now

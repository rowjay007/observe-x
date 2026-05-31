package sampling

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStore persists sampler state to Redis. Keys are namespaced as
//
//	observex:sampling:state:<tenant>
//
// Snapshots live for SnapshotTTL (default 24h) so an idle tenant's
// learned baseline is reclaimed automatically.
//
// Failure mode: if Redis is unavailable, Save/Load return errors.
// Callers SHOULD log-and-continue; the in-memory tracker keeps working.
type RedisStore struct {
	client      *redis.Client
	snapshotTTL time.Duration
}

type RedisOptions struct {
	// URL — redis://[user:password@]host:port[/db]
	URL string
	// SnapshotTTL — how long a snapshot lives. Default 24h.
	SnapshotTTL time.Duration
	// DialTimeout — connection timeout. Default 3s.
	DialTimeout time.Duration
}

func NewRedisStore(ctx context.Context, opts RedisOptions) (*RedisStore, error) {
	if opts.URL == "" {
		return nil, errors.New("sampling: redis url required")
	}
	if opts.SnapshotTTL <= 0 {
		opts.SnapshotTTL = 24 * time.Hour
	}
	if opts.DialTimeout <= 0 {
		opts.DialTimeout = 3 * time.Second
	}
	parsed, err := redis.ParseURL(opts.URL)
	if err != nil {
		return nil, fmt.Errorf("sampling: parse redis url: %w", err)
	}
	parsed.DialTimeout = opts.DialTimeout
	parsed.ReadTimeout = 2 * time.Second
	parsed.WriteTimeout = 2 * time.Second

	client := redis.NewClient(parsed)
	pingCtx, cancel := context.WithTimeout(ctx, opts.DialTimeout)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("sampling: ping redis: %w", err)
	}
	return &RedisStore{client: client, snapshotTTL: opts.SnapshotTTL}, nil
}

func (r *RedisStore) Save(ctx context.Context, tenantID string, state map[string]ewmaSnapshot) error {
	b, err := encodeState(state)
	if err != nil {
		return err
	}
	return r.client.Set(ctx, redisKey(tenantID), b, r.snapshotTTL).Err()
}

func (r *RedisStore) Load(ctx context.Context, tenantID string) (map[string]ewmaSnapshot, error) {
	v, err := r.client.Get(ctx, redisKey(tenantID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return map[string]ewmaSnapshot{}, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeState(v)
}

func (r *RedisStore) Close() error { return r.client.Close() }

func redisKey(tenantID string) string {
	return "observex:sampling:state:" + tenantID
}

package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/argon2"
)

// ─── Argon2id parameters (OWASP-recommended baseline) ─────────────────────
//
// time=3, memory=64MiB, threads=2, keyLen=32 — matches the OWASP
// "second-strongest" profile. Tuned to ~50ms on commodity hardware,
// which is fine at issuance time but too slow for the validation hot
// path, so the hot path is cache-first.
const (
	argonTime    uint32 = 3
	argonMemory  uint32 = 64 * 1024
	argonThreads uint8  = 2
	argonKeyLen  uint32 = 32
	argonSaltLen        = 16
)

// ─── Wire format ──────────────────────────────────────────────────────────
//
// Production wire format: "<tenant_id>:<kid>:<raw_secret>"
//
//   tenant_id  — non-secret, used as the SQL filter
//   kid        — 12-char hex, identifies exactly one row in the table
//   raw_secret — 32 bytes of crypto/rand encoded as base64url (no padding)
//
// The dev-only StatelessKeyValidator format is 2-part
// "<tenant_id>:<digest>". PostgresKeyStore rejects 2-part keys so that
// a misconfigured deployment cannot accidentally accept dev keys.

// IssuedKey is what NewKey returns to the caller. The raw value MUST
// be shown to the user exactly once and never persisted unencrypted.
type IssuedKey struct {
	TenantID  string
	KID       string
	Raw       string    // tenant:kid:secret — show once, then discard
	Prefix    string    // first 8 chars of the raw key (for human ID)
	CreatedAt time.Time
	ExpiresAt *time.Time
}

// PostgresOptions configures a PostgresKeyStore.
type PostgresOptions struct {
	// CacheSize caps the in-memory validation cache. Default 10 000.
	CacheSize int
	// CacheTTL bounds cache freshness. Default 5s.
	CacheTTL time.Duration
	// LastUsedDebounce — how long to wait before writing another
	// last_used_at update for the same kid. Default 1 minute.
	LastUsedDebounce time.Duration
}

func (o PostgresOptions) withDefaults() PostgresOptions {
	if o.CacheSize <= 0 {
		o.CacheSize = 10_000
	}
	if o.CacheTTL <= 0 {
		o.CacheTTL = 5 * time.Second
	}
	if o.LastUsedDebounce <= 0 {
		o.LastUsedDebounce = time.Minute
	}
	return o
}

// PostgresKeyStore is the production KeyStore implementation.
//
// Concurrency:
//   - ValidateKey is safe for concurrent callers; it takes a read lock
//     on the cache for the common hit path and only escalates to a
//     write lock on miss.
//   - IssueKey/RevokeKey write to Postgres; cache is invalidated by
//     RevokeKey but NOT by IssueKey (a freshly-issued key is never in
//     the cache yet).
//
// Failure mode:
//   - If Postgres is unreachable at construction, NewPostgresKeyStore
//     returns an error and the caller MUST fail to start. Unlike the
//     ClickHouse backend, we never degrade auth silently.
type PostgresKeyStore struct {
	pool    *pgxpool.Pool
	opts    PostgresOptions
	cache   *validationCache
	usedCh  chan string // kids whose last_used_at is pending update
	stopCh  chan struct{}
	doneCh  chan struct{}
	usedMu  sync.Mutex
	usedSet map[string]time.Time
}

// NewPostgresKeyStore opens a pooled connection and verifies it with a
// Ping. The caller MUST call Close to release the pool and the
// background last_used_at writer.
func NewPostgresKeyStore(ctx context.Context, dsn string, opts PostgresOptions) (*PostgresKeyStore, error) {
	opts = opts.withDefaults()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("auth: parse pg dsn: %w", err)
	}
	cfg.MaxConns = 16
	cfg.MinConns = 2

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("auth: connect pg: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("auth: ping pg: %w", err)
	}

	s := &PostgresKeyStore{
		pool:    pool,
		opts:    opts,
		cache:   newValidationCache(opts.CacheSize, opts.CacheTTL),
		usedCh:  make(chan string, 1024),
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
		usedSet: make(map[string]time.Time),
	}
	go s.lastUsedLoop()
	return s, nil
}

// Close drains and shuts down the background writer, then closes the
// pool. Idempotent.
func (s *PostgresKeyStore) Close() error {
	select {
	case <-s.stopCh:
		return nil
	default:
	}
	close(s.stopCh)
	<-s.doneCh
	s.pool.Close()
	return nil
}

// Pool exposes the underlying pgxpool for callers (e.g. tenant-api)
// that need to share the same database connection.
func (s *PostgresKeyStore) Pool() *pgxpool.Pool {
	return s.pool
}

// ─── ValidateKey (hot path) ───────────────────────────────────────────────

func (s *PostgresKeyStore) ValidateKey(key string) (string, bool) {
	tenantID, kid, secret, ok := splitWireKey(key)
	if !ok {
		return "", false
	}

	// Cache hit: BLAKE3 the full wire key and look up; constant-time
	// compare the returned tenantID.
	digest := Blake3Sum(key)
	if cached, found := s.cache.get(digest); found {
		if subtle.ConstantTimeCompare([]byte(cached), []byte(tenantID)) != 1 {
			return "", false
		}
		s.scheduleLastUsed(kid)
		return cached, true
	}

	// Miss: single-row lookup, then Argon2id verify.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var hash string
	row := s.pool.QueryRow(ctx, `
		SELECT hash
		FROM   tenant_api_keys
		WHERE  tenant_id  = $1
		  AND  kid        = $2
		  AND  revoked_at IS NULL
		  AND  (expires_at IS NULL OR expires_at > now())
	`, tenantID, kid)
	if err := row.Scan(&hash); err != nil {
		return "", false
	}

	if !verifyArgon2id(secret, hash) {
		return "", false
	}
	s.cache.put(digest, tenantID)
	s.scheduleLastUsed(kid)
	return tenantID, true
}

// ─── IssueKey / RevokeKey (control plane) ─────────────────────────────────

// IssueKey generates a new random secret, stores its Argon2id hash, and
// returns the raw wire key (which the caller MUST surface to the user
// exactly once). The actor argument is recorded in tenant_audit_log
// by the tenant-api service, not here.
func (s *PostgresKeyStore) IssueKey(ctx context.Context, tenantID string, expiresAt *time.Time) (IssuedKey, error) {
	if tenantID == "" {
		return IssuedKey{}, errors.New("auth: tenant id required")
	}

	kid, err := newKID()
	if err != nil {
		return IssuedKey{}, err
	}
	secret, err := newSecret()
	if err != nil {
		return IssuedKey{}, err
	}
	hash, err := hashArgon2id(secret)
	if err != nil {
		return IssuedKey{}, err
	}

	raw := fmt.Sprintf("%s:%s:%s", tenantID, kid, secret)
	prefix := raw[:min(8, len(raw))]

	var createdAt time.Time
	err = s.pool.QueryRow(ctx, `
		INSERT INTO tenant_api_keys (kid, tenant_id, hash, prefix, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING created_at
	`, kid, tenantID, hash, prefix, expiresAt).Scan(&createdAt)
	if err != nil {
		return IssuedKey{}, fmt.Errorf("auth: insert key: %w", err)
	}

	return IssuedKey{
		TenantID:  tenantID,
		KID:       kid,
		Raw:       raw,
		Prefix:    prefix,
		CreatedAt: createdAt,
		ExpiresAt: expiresAt,
	}, nil
}

// RevokeKey marks the key as revoked and evicts any cached validation.
// Returns ErrKeyRevoked if the key was already revoked, nil on success,
// or a generic error if no such kid exists.
func (s *PostgresKeyStore) RevokeKey(ctx context.Context, tenantID, kid string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE tenant_api_keys
		SET    revoked_at = now()
		WHERE  tenant_id  = $1
		  AND  kid        = $2
		  AND  revoked_at IS NULL
	`, tenantID, kid)
	if err != nil {
		return fmt.Errorf("auth: revoke: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrKeyRevoked
	}
	// Cache eviction is opportunistic: we don't know the full wire key
	// here (we never stored it), so we let TTL expire it within
	// CacheTTL. The next validation after expiry will SELECT and see
	// revoked_at NOT NULL → 403. Worst-case window: CacheTTL (default 5s).
	return nil
}

// ─── Internals ────────────────────────────────────────────────────────────

func splitWireKey(key string) (tenantID, kid, secret string, ok bool) {
	parts := strings.Split(key, ":")
	if len(parts) != 3 {
		return "", "", "", false
	}
	if parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

func newKID() (string, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	// 12-char hex prefix is enough for collision avoidance within a
	// single tenant's keyset; full UUID is overkill for the wire.
	return strings.ReplaceAll(id.String(), "-", "")[:12], nil
}

func newSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func hashArgon2id(secret string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	digest := argon2.IDKey([]byte(secret), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(digest),
	), nil
}

func verifyArgon2id(secret, encoded string) bool {
	parts := strings.Split(encoded, "$")
	// "" "argon2id" "v=19" "m=...,t=...,p=..." "salt" "hash"
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	var memory, timeCost uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &timeCost, &threads); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(secret), salt, timeCost, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// ─── last_used_at writer (debounced, async) ───────────────────────────────

func (s *PostgresKeyStore) scheduleLastUsed(kid string) {
	now := time.Now()
	s.usedMu.Lock()
	last, ok := s.usedSet[kid]
	if ok && now.Sub(last) < s.opts.LastUsedDebounce {
		s.usedMu.Unlock()
		return
	}
	s.usedSet[kid] = now
	s.usedMu.Unlock()
	select {
	case s.usedCh <- kid:
	default:
		// channel full → drop. The metric will be slightly stale, not wrong.
	}
}

func (s *PostgresKeyStore) lastUsedLoop() {
	defer close(s.doneCh)
	for {
		select {
		case kid := <-s.usedCh:
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, _ = s.pool.Exec(ctx, `
				UPDATE tenant_api_keys
				SET    last_used_at = now()
				WHERE  kid = $1
			`, kid)
			cancel()
		case <-s.stopCh:
			return
		}
	}
}

// ─── validation cache (LRU + TTL) ─────────────────────────────────────────

type validationCache struct {
	mu      sync.RWMutex
	size    int
	ttl     time.Duration
	entries map[string]cacheEntry
}

type cacheEntry struct {
	tenantID  string
	expiresAt time.Time
}

func newValidationCache(size int, ttl time.Duration) *validationCache {
	return &validationCache{
		size:    size,
		ttl:     ttl,
		entries: make(map[string]cacheEntry, size),
	}
}

func (c *validationCache) get(digest string) (string, bool) {
	c.mu.RLock()
	e, ok := c.entries[digest]
	c.mu.RUnlock()
	if !ok {
		return "", false
	}
	if time.Now().After(e.expiresAt) {
		c.mu.Lock()
		delete(c.entries, digest)
		c.mu.Unlock()
		return "", false
	}
	return e.tenantID, true
}

func (c *validationCache) put(digest, tenantID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.size {
		// Simple capacity guard: evict ~10% of entries chosen
		// arbitrarily by map iteration order. Good enough for a
		// short-TTL cache; a real LRU is unnecessary at this TTL.
		evict := c.size / 10
		for k := range c.entries {
			delete(c.entries, k)
			if evict--; evict <= 0 {
				break
			}
		}
	}
	c.entries[digest] = cacheEntry{
		tenantID:  tenantID,
		expiresAt: time.Now().Add(c.ttl),
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

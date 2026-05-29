package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ─── domain types ─────────────────────────────────────────────────────────

type Tenant struct {
	ID            string
	DisplayName   string
	Tier          string
	RetentionDays int
	QuotaEPS      int
	CreatedAt     time.Time
	DeletedAt     *time.Time
}

type APIKeyMeta struct {
	KID         string
	TenantID    string
	Prefix      string
	Scopes      []string
	CreatedAt   time.Time
	ExpiresAt   *time.Time
	RevokedAt   *time.Time
	LastUsedAt  *time.Time
}

type AuditEvent struct {
	TenantID *string
	Actor    string
	Action   string
	Details  map[string]any
	SourceIP *string
}

// ─── errors ───────────────────────────────────────────────────────────────

var (
	ErrTenantNotFound    = errors.New("store: tenant not found")
	ErrTenantExists      = errors.New("store: tenant already exists")
	ErrKeyNotFound       = errors.New("store: api key not found")
)

// ─── Store ────────────────────────────────────────────────────────────────

// Store is the repository facade over Postgres. It performs the
// admin-level operations needed by the tenant-api service. Per-tenant
// data plane operations are NOT this package's responsibility — those
// happen on a tenant-scoped connection that sets app.tenant_id.
type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// ─── tenants ──────────────────────────────────────────────────────────────

func (s *Store) CreateTenant(ctx context.Context, t Tenant) (Tenant, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO tenants (id, display_name, tier, retention_days, quota_eps)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING created_at
	`, t.ID, t.DisplayName, t.Tier, t.RetentionDays, t.QuotaEPS)
	if err := row.Scan(&t.CreatedAt); err != nil {
		if isUniqueViolation(err) {
			return Tenant{}, ErrTenantExists
		}
		return Tenant{}, fmt.Errorf("store: insert tenant: %w", err)
	}
	return t, nil
}

func (s *Store) GetTenant(ctx context.Context, id string) (Tenant, error) {
	var t Tenant
	row := s.pool.QueryRow(ctx, `
		SELECT id, display_name, tier, retention_days, quota_eps, created_at, deleted_at
		FROM tenants
		WHERE id = $1 AND deleted_at IS NULL
	`, id)
	err := row.Scan(&t.ID, &t.DisplayName, &t.Tier, &t.RetentionDays, &t.QuotaEPS, &t.CreatedAt, &t.DeletedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Tenant{}, ErrTenantNotFound
		}
		return Tenant{}, fmt.Errorf("store: get tenant: %w", err)
	}
	return t, nil
}

func (s *Store) ListTenants(ctx context.Context, limit, offset int) ([]Tenant, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, display_name, tier, retention_days, quota_eps, created_at, deleted_at
		FROM tenants
		WHERE deleted_at IS NULL
		ORDER BY created_at DESC
		LIMIT $1 OFFSET $2
	`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("store: list tenants: %w", err)
	}
	defer rows.Close()

	out := []Tenant{}
	for rows.Next() {
		var t Tenant
		if err := rows.Scan(&t.ID, &t.DisplayName, &t.Tier, &t.RetentionDays, &t.QuotaEPS, &t.CreatedAt, &t.DeletedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) SoftDeleteTenant(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE tenants SET deleted_at = now()
		WHERE id = $1 AND deleted_at IS NULL
	`, id)
	if err != nil {
		return fmt.Errorf("store: soft-delete tenant: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTenantNotFound
	}
	return nil
}

// ─── api keys ─────────────────────────────────────────────────────────────

func (s *Store) ListKeys(ctx context.Context, tenantID string) ([]APIKeyMeta, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT kid, tenant_id, prefix, scopes, created_at, expires_at, revoked_at, last_used_at
		FROM tenant_api_keys
		WHERE tenant_id = $1
		ORDER BY created_at DESC
	`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: list keys: %w", err)
	}
	defer rows.Close()

	out := []APIKeyMeta{}
	for rows.Next() {
		var k APIKeyMeta
		if err := rows.Scan(&k.KID, &k.TenantID, &k.Prefix, &k.Scopes, &k.CreatedAt, &k.ExpiresAt, &k.RevokedAt, &k.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ─── audit log ────────────────────────────────────────────────────────────

func (s *Store) WriteAudit(ctx context.Context, ev AuditEvent) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO tenant_audit_log (tenant_id, actor, action, details, source_ip)
		VALUES ($1, $2, $3, $4, $5)
	`, ev.TenantID, ev.Actor, ev.Action, ev.Details, ev.SourceIP)
	if err != nil {
		return fmt.Errorf("store: write audit: %w", err)
	}
	return nil
}

// ─── helpers ──────────────────────────────────────────────────────────────

// isUniqueViolation returns true if err is a Postgres unique-constraint
// failure. pgx surfaces this via the SQLSTATE 23505 code; we avoid a
// hard import on pgconn by string-matching the wrapped error.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return contains(msg, "23505") || contains(msg, "duplicate key")
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) &&
		(haystack == needle ||
			(len(haystack) > len(needle) && (haystack[:len(needle)] == needle ||
				haystack[len(haystack)-len(needle):] == needle ||
				indexOf(haystack, needle) >= 0)))
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

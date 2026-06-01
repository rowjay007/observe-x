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
	KID        string
	TenantID   string
	Prefix     string
	Scopes     []string
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	RevokedAt  *time.Time
	LastUsedAt *time.Time
}

type AuditEvent struct {
	TenantID *string
	Actor    string
	Action   string
	Details  map[string]any
	SourceIP *string
}

// Dashboard is a saved panel layout owned by a tenant. The Layout
// field is opaque JSON the SPA owns; tenant-api treats it as bytes
// once validated as `{}`-shaped JSON upstream. See ADR-0032.
type Dashboard struct {
	ID        string
	TenantID  string
	Name      string
	Layout    []byte // raw JSONB; the SPA owns the schema
	CreatedBy string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ─── errors ───────────────────────────────────────────────────────────────

var (
	ErrTenantNotFound    = errors.New("store: tenant not found")
	ErrTenantExists      = errors.New("store: tenant already exists")
	ErrKeyNotFound       = errors.New("store: api key not found")
	ErrDashboardNotFound = errors.New("store: dashboard not found")
	ErrDashboardExists   = errors.New("store: dashboard name already used by this tenant")
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

// AuditRecord is the read shape returned by ListAudit. Includes the
// server-assigned id + created_at the writer doesn't carry.
type AuditRecord struct {
	ID        int64          `json:"id"`
	TenantID  *string        `json:"tenant_id,omitempty"`
	Actor     string         `json:"actor"`
	Action    string         `json:"action"`
	Details   map[string]any `json:"details,omitempty"`
	SourceIP  *string        `json:"source_ip,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

// ListAudit returns the most recent audit entries, optionally
// filtered by tenant. limit is clamped to [1, 500] to bound the
// page size; callers that need pagination over many pages should
// add a `before_id` parameter in a follow-up. For the operator UI
// the latest-N pattern is sufficient.
func (s *Store) ListAudit(ctx context.Context, tenantID string, limit int) ([]AuditRecord, error) {
	if limit < 1 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	var rows pgx.Rows
	var err error
	if tenantID == "" {
		rows, err = s.pool.Query(ctx, `
			SELECT id, tenant_id, actor, action, details, source_ip, created_at
			FROM tenant_audit_log
			ORDER BY id DESC
			LIMIT $1
		`, limit)
	} else {
		rows, err = s.pool.Query(ctx, `
			SELECT id, tenant_id, actor, action, details, source_ip, created_at
			FROM tenant_audit_log
			WHERE tenant_id = $1
			ORDER BY id DESC
			LIMIT $2
		`, tenantID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("store: list audit: %w", err)
	}
	defer rows.Close()
	out := []AuditRecord{}
	for rows.Next() {
		var r AuditRecord
		if err := rows.Scan(&r.ID, &r.TenantID, &r.Actor, &r.Action, &r.Details, &r.SourceIP, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
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

// ─── dashboards (Phase E-4) ───────────────────────────────────────────────

// ListDashboards returns every dashboard for the tenant, newest
// first. Caller must already have authenticated the tenant.
func (s *Store) ListDashboards(ctx context.Context, tenantID string) ([]Dashboard, error) {
	if tenantID == "" {
		return nil, errors.New("store: tenant_id required")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, tenant_id, name, layout, COALESCE(created_by,''),
		       created_at, updated_at
		FROM dashboards
		WHERE tenant_id = $1
		ORDER BY updated_at DESC
		LIMIT 200`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: list dashboards: %w", err)
	}
	defer rows.Close()
	out := make([]Dashboard, 0)
	for rows.Next() {
		var d Dashboard
		if err := rows.Scan(&d.ID, &d.TenantID, &d.Name, &d.Layout,
			&d.CreatedBy, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scan dashboard: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetDashboard fetches one dashboard by id; tenant scoping is
// enforced by RLS, but we re-check on the way out so a misconfigured
// connection can never leak across tenants.
func (s *Store) GetDashboard(ctx context.Context, id string) (Dashboard, error) {
	var d Dashboard
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, tenant_id, name, layout, COALESCE(created_by,''),
		       created_at, updated_at
		FROM dashboards
		WHERE id = $1::uuid`, id).Scan(
		&d.ID, &d.TenantID, &d.Name, &d.Layout, &d.CreatedBy,
		&d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Dashboard{}, ErrDashboardNotFound
	}
	if err != nil {
		return Dashboard{}, fmt.Errorf("store: get dashboard: %w", err)
	}
	return d, nil
}

// CreateDashboard inserts a new dashboard. Returns ErrDashboardExists
// on (tenant_id, name) uniqueness violation so callers can prompt
// the operator to rename instead of silently shadowing.
func (s *Store) CreateDashboard(ctx context.Context, d Dashboard) (Dashboard, error) {
	if d.TenantID == "" || d.Name == "" || len(d.Layout) == 0 {
		return Dashboard{}, errors.New("store: tenant_id, name, layout required")
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO dashboards (tenant_id, name, layout, created_by)
		VALUES ($1, $2, $3, NULLIF($4,''))
		RETURNING id::text, created_at, updated_at`,
		d.TenantID, d.Name, d.Layout, d.CreatedBy)
	if err := row.Scan(&d.ID, &d.CreatedAt, &d.UpdatedAt); err != nil {
		if isUniqueViolation(err) {
			return Dashboard{}, ErrDashboardExists
		}
		return Dashboard{}, fmt.Errorf("store: insert dashboard: %w", err)
	}
	return d, nil
}

// UpdateDashboard replaces the name + layout of an existing
// dashboard. updated_at is touched by a row trigger.
func (s *Store) UpdateDashboard(ctx context.Context, d Dashboard) (Dashboard, error) {
	if d.ID == "" || d.Name == "" || len(d.Layout) == 0 {
		return Dashboard{}, errors.New("store: id, name, layout required")
	}
	row := s.pool.QueryRow(ctx, `
		UPDATE dashboards
		SET name = $2, layout = $3
		WHERE id = $1::uuid
		RETURNING tenant_id, created_at, updated_at`,
		d.ID, d.Name, d.Layout)
	if err := row.Scan(&d.TenantID, &d.CreatedAt, &d.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Dashboard{}, ErrDashboardNotFound
		}
		if isUniqueViolation(err) {
			return Dashboard{}, ErrDashboardExists
		}
		return Dashboard{}, fmt.Errorf("store: update dashboard: %w", err)
	}
	return d, nil
}

// DeleteDashboard hard-deletes a dashboard. RLS gates which tenant's
// dashboards a connection can see; even without RLS the id is a UUID
// so blind-guess deletes across tenants are infeasible.
func (s *Store) DeleteDashboard(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM dashboards WHERE id = $1::uuid`, id)
	if err != nil {
		return fmt.Errorf("store: delete dashboard: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrDashboardNotFound
	}
	return nil
}

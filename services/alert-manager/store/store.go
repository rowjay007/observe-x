package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Alert is the Go-level representation of a row in the `alerts`
// table. Fingerprint is computed by the caller via Fingerprint(); the
// store does not synthesize it because the alert-manager wants
// reproducible fingerprints across restarts.
type Alert struct {
	Fingerprint string
	TenantID    string
	RuleID      string
	Severity    string
	Title       string
	Description string
	Labels      map[string]string
	Annotations map[string]string
	State       string
	StartsAt    time.Time
	LastSeenAt  time.Time
	ResolvedAt  *time.Time
	NotifiedAt  *time.Time
	NotifyCount int
}

// Silence is an operator-applied suppression keyed by a label matcher.
type Silence struct {
	ID        uuid.UUID
	TenantID  string
	Matcher   map[string]string
	Reason    string
	CreatedBy string
	CreatedAt time.Time
	ExpiresAt time.Time
}

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// ─── Alert upsert / lifecycle ────────────────────────────────────────────

// UpsertFiring records (or refreshes) a firing alert. If a row with
// the same fingerprint already exists in 'firing' state, only the
// last_seen_at + notify_count fields are updated. If it was
// 'resolved', it transitions back to firing with a new starts_at.
// Returns whether this was a brand-new firing transition (i.e. should
// the caller dispatch a notification).
func (s *Store) UpsertFiring(ctx context.Context, a Alert) (newTransition bool, err error) {
	labelsJSON, err := json.Marshal(a.Labels)
	if err != nil {
		return false, err
	}
	annoJSON, err := json.Marshal(a.Annotations)
	if err != nil {
		return false, err
	}

	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var existingState string
		row := tx.QueryRow(ctx, `SELECT state FROM alerts WHERE fingerprint = $1`, a.Fingerprint)
		switch err := row.Scan(&existingState); err {
		case nil:
			// already exists
		case pgx.ErrNoRows:
			existingState = ""
		default:
			return err
		}

		if existingState == "" {
			_, err = tx.Exec(ctx, `
                INSERT INTO alerts
                  (fingerprint, tenant_id, rule_id, severity, title, description,
                   labels, annotations, state, starts_at, last_seen_at)
                VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'firing',$9,$10)`,
				a.Fingerprint, a.TenantID, a.RuleID, a.Severity, a.Title, a.Description,
				labelsJSON, annoJSON, a.StartsAt, a.LastSeenAt)
			if err != nil {
				return err
			}
			newTransition = true
		} else if existingState == "resolved" {
			_, err = tx.Exec(ctx, `
                UPDATE alerts
                   SET state='firing', severity=$2, title=$3, description=$4,
                       labels=$5, annotations=$6,
                       starts_at=$7, last_seen_at=$8, resolved_at=NULL
                 WHERE fingerprint=$1`,
				a.Fingerprint, a.Severity, a.Title, a.Description,
				labelsJSON, annoJSON, a.StartsAt, a.LastSeenAt)
			if err != nil {
				return err
			}
			newTransition = true
		} else {
			_, err = tx.Exec(ctx, `
                UPDATE alerts SET last_seen_at=$2 WHERE fingerprint=$1`,
				a.Fingerprint, a.LastSeenAt)
			if err != nil {
				return err
			}
		}

		event := "updated"
		if newTransition {
			event = "created"
		}
		_, err = tx.Exec(ctx, `
            INSERT INTO alert_history (fingerprint, tenant_id, event, severity, payload)
            VALUES ($1,$2,$3,$4,$5)`,
			a.Fingerprint, a.TenantID, event, a.Severity, labelsJSON)
		return err
	})
	return newTransition, err
}

// MarkNotified records that we successfully dispatched a notification
// for fingerprint. This drives the dedup window: callers consult
// notified_at + notify_count to decide whether to re-notify.
func (s *Store) MarkNotified(ctx context.Context, fingerprint string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `
        UPDATE alerts SET notified_at = $2, notify_count = notify_count + 1
         WHERE fingerprint = $1`,
		fingerprint, at)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
        INSERT INTO alert_history (fingerprint, tenant_id, event, payload)
        SELECT $1, tenant_id, 'notified', '{}'::jsonb FROM alerts WHERE fingerprint = $1`,
		fingerprint)
	return err
}

// Resolve marks an alert as resolved.
func (s *Store) Resolve(ctx context.Context, fingerprint string, at time.Time) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx, `
            UPDATE alerts SET state='resolved', resolved_at=$2
             WHERE fingerprint = $1 AND state <> 'resolved'`,
			fingerprint, at)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return nil // already resolved or unknown
		}
		_, err = tx.Exec(ctx, `
            INSERT INTO alert_history (fingerprint, tenant_id, event, payload)
            SELECT $1, tenant_id, 'resolved', '{}'::jsonb FROM alerts WHERE fingerprint = $1`,
			fingerprint)
		return err
	})
}

// ListAlerts returns the most recent N alerts for a tenant, newest
// last_seen_at first.
func (s *Store) ListAlerts(ctx context.Context, tenantID string, limit int) ([]Alert, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
        SELECT fingerprint, tenant_id, rule_id, severity, title, description,
               labels, annotations, state, starts_at, last_seen_at, resolved_at,
               notified_at, notify_count
          FROM alerts WHERE tenant_id = $1
         ORDER BY last_seen_at DESC LIMIT $2`,
		tenantID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Alert
	for rows.Next() {
		var a Alert
		var labelsJSON, annoJSON []byte
		if err := rows.Scan(&a.Fingerprint, &a.TenantID, &a.RuleID, &a.Severity, &a.Title,
			&a.Description, &labelsJSON, &annoJSON, &a.State, &a.StartsAt, &a.LastSeenAt,
			&a.ResolvedAt, &a.NotifiedAt, &a.NotifyCount); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(labelsJSON, &a.Labels)
		_ = json.Unmarshal(annoJSON, &a.Annotations)
		out = append(out, a)
	}
	return out, rows.Err()
}

// ─── Silences ────────────────────────────────────────────────────────────

func (s *Store) CreateSilence(ctx context.Context, sil Silence) (uuid.UUID, error) {
	if sil.ID == uuid.Nil {
		sil.ID = uuid.New()
	}
	if sil.ExpiresAt.Before(time.Now()) {
		return uuid.Nil, errors.New("silence: ExpiresAt must be in the future")
	}
	matcherJSON, err := json.Marshal(sil.Matcher)
	if err != nil {
		return uuid.Nil, err
	}
	_, err = s.pool.Exec(ctx, `
        INSERT INTO alert_silences (id, tenant_id, matcher, reason, created_by, expires_at)
        VALUES ($1, $2, $3, $4, $5, $6)`,
		sil.ID, sil.TenantID, matcherJSON, sil.Reason, sil.CreatedBy, sil.ExpiresAt)
	return sil.ID, err
}

// IsSilenced reports whether any active silence for tenantID has a
// matcher that's a subset of the alert's labels. Matching is plain
// label equality; regex matchers are Phase D.
func (s *Store) IsSilenced(ctx context.Context, tenantID string, alertLabels map[string]string) (bool, error) {
	rows, err := s.pool.Query(ctx, `
        SELECT matcher FROM alert_silences
         WHERE tenant_id = $1 AND expires_at > now()`,
		tenantID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return false, err
		}
		var matcher map[string]string
		if err := json.Unmarshal(raw, &matcher); err != nil {
			continue
		}
		if labelsSubset(matcher, alertLabels) {
			return true, nil
		}
	}
	return false, rows.Err()
}

func labelsSubset(matcher, candidate map[string]string) bool {
	for k, v := range matcher {
		if candidate[k] != v {
			return false
		}
	}
	return true
}

// ─── Fingerprint ─────────────────────────────────────────────────────────

// Fingerprint computes a stable identifier for an alert. Given the
// same (tenantID, ruleID, labels) the result is identical across
// processes, so deduplication survives restarts and rolling deploys.
func Fingerprint(tenantID, ruleID string, labels map[string]string) string {
	h := sha256.New()
	h.Write([]byte(tenantID))
	h.Write([]byte{0})
	h.Write([]byte(ruleID))
	h.Write([]byte{0})

	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "%s=%s;", k, labels[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

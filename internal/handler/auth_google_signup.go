package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/uid"
)

// resolveGoogleUser commits an identity before a session can be issued. SQLite's
// single connection serializes callbacks; the unique index is a second backstop.
func (h *Handler) resolveGoogleUser(ctx context.Context, info *googleIdentity) (string, string, error) {
	for attempt := 0; attempt < 2; attempt++ {
		id, code, err := h.resolveGoogleUserTx(ctx, info)
		if err == nil || !db.IsUniqueViolation(err) {
			return id, code, err
		}
	}
	return "", "identity_conflict", nil
}

func (h *Handler) resolveGoogleUserTx(ctx context.Context, info *googleIdentity) (string, string, error) {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback() //nolint:errcheck
	var boundID string
	var archived sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT id, archived_at FROM users WHERE provider = 'google' AND provider_id = ?`, info.Subject).Scan(&boundID, &archived)
	if err != nil && err != sql.ErrNoRows {
		return "", "", err
	}
	if boundID != "" && archived.Valid {
		return "", "archived", nil
	}

	// Case-equivalent legacy rows must never be silently merged.
	rows, err := tx.QueryContext(ctx, `SELECT id, provider, provider_id, archived_at FROM users WHERE lower(email) = ? LIMIT 2`, info.Email)
	if err != nil {
		return "", "", err
	}
	var emailID string
	var provider, subject, emailArchived sql.NullString
	count := 0
	for rows.Next() {
		count++
		if err := rows.Scan(&emailID, &provider, &subject, &emailArchived); err != nil {
			rows.Close()
			return "", "", err
		}
	}
	rowErr := rows.Err()
	rows.Close()
	if rowErr != nil {
		return "", "", rowErr
	}
	if count > 1 || (boundID != "" && emailID != "" && boundID != emailID) {
		return "", "identity_conflict", nil
	}
	if boundID != "" {
		// Preserve the local profile on a Google rename; never claim another row.
		if err := tx.Commit(); err != nil {
			return "", "", err
		}
		return boundID, "", nil
	}
	if emailID != "" {
		if emailArchived.Valid {
			return "", "archived", nil
		}
		if provider.String != "" || subject.String != "" {
			return "", "identity_conflict", nil
		}
		// One-time legacy binding retains the pre-existing email-login trust. Bind the
		// known owner before enabling new staff admission; do not use this as linking.
		result, err := tx.ExecContext(ctx, `UPDATE users SET provider = 'google', provider_id = ? WHERE id = ? AND coalesce(provider, '') = '' AND coalesce(provider_id, '') = '' AND archived_at IS NULL`, info.Subject, emailID)
		if err != nil {
			return "", "", err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return "", "", err
		}
		if changed != 1 {
			return "", "identity_conflict", nil
		}
		if err := tx.Commit(); err != nil {
			return "", "", err
		}
		return emailID, "", nil
	}
	var enabled bool
	var rawDomains string
	if err := tx.QueryRowContext(ctx, `SELECT google_signup_enabled, google_signup_domains FROM server_settings WHERE id = 1`).Scan(&enabled, &rawDomains); err != nil {
		return "", "", err
	}
	var domains []string
	if err := json.Unmarshal([]byte(rawDomains), &domains); err != nil {
		return "", "", fmt.Errorf("invalid Google signup policy")
	}
	var owners int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE is_owner = 1 AND archived_at IS NULL`).Scan(&owners); err != nil {
		return "", "", err
	}
	domain := strings.ToLower(info.HostedDomain)
	parts := strings.Split(info.Email, "@")
	eligible := enabled && owners > 0 && domain != "" && len(parts) == 2 && parts[0] != "" && parts[1] == domain
	allowed := false
	for _, d := range domains {
		if domain == d {
			allowed = true
			break
		}
	}
	if !eligible || !allowed {
		return "", "signup", nil
	}
	id := uid.New()
	name := strings.TrimSpace(info.Name)
	if name == "" {
		name = info.Email
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO users (id, email, name, provider, provider_id, is_admin, is_owner, email_login) VALUES (?, ?, ?, 'google', ?, 0, 0, 0)`, id, info.Email, name, info.Subject); err != nil {
		return "", "", err
	}
	if err := tx.Commit(); err != nil {
		return "", "", err
	}
	return id, "", nil
}

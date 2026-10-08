package handler

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/gcal"
	"github.com/calnode/calnode/internal/secret"
)

// GoogleOAuthConfig holds decrypted Google OAuth settings loaded from the DB.
// Used by server.go to build the initial gcal and auth clients on startup.
type GoogleOAuthConfig struct {
	ClientID     string
	ClientSecret string
}

// LoadGoogleSettingsFromDB reads Google OAuth credentials from server_settings
// and decrypts the client secret. Returns nil (not an error) when client_id is empty.
func LoadGoogleSettingsFromDB(db *sql.DB, encKey [32]byte) (*GoogleOAuthConfig, error) {
	var clientID, secretEnc string
	err := db.QueryRow(`
		SELECT google_client_id, google_client_secret_enc
		FROM server_settings WHERE id = 1`).
		Scan(&clientID, &secretEnc)
	if err == sql.ErrNoRows || clientID == "" {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var clientSecret string
	if secretEnc != "" {
		clientSecret, err = secret.Decrypt(encKey, secretEnc)
		if err != nil {
			return nil, fmt.Errorf("decrypt google client secret: %w", err)
		}
	}
	return &GoogleOAuthConfig{ClientID: clientID, ClientSecret: clientSecret}, nil
}

// GetGoogleSettings returns effective credentials without secrets and the saved admission policy.
func (h *Handler) GetGoogleSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	var clientID, secretEnc, rawDomains string
	var enabled bool
	err := h.db.QueryRowContext(r.Context(), `SELECT google_client_id, google_client_secret_enc, google_signup_enabled, google_signup_domains FROM server_settings WHERE id = 1`).Scan(&clientID, &secretEnc, &enabled, &rawDomains)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "could not load Google settings")
		return
	}
	secretSet := secretEnc != ""
	if ga := h.getGoogleAuth(); ga != nil {
		clientID, secretSet = ga.ClientID, ga.ClientSecret != ""
	}
	domains := []string{}
	if err := json.Unmarshal([]byte(rawDomains), &domains); err != nil {
		h.writeError(w, http.StatusInternalServerError, "could not load Google signup policy")
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]any{
		"client_id": clientID, "client_secret_set": secretSet, "configured": clientID != "",
		"base_url": h.baseURL, "signup_enabled": enabled, "signup_domains": domains,
	})
}

// normalizeGoogleDomains accepts exact DNS domains, never URLs or wildcard rules.
func normalizeGoogleDomains(values []string) ([]string, error) {
	if len(values) > 20 {
		return nil, fmt.Errorf("at most 20 signup domains are allowed")
	}
	domains := []string{}
	seen := map[string]bool{}
	for _, value := range values {
		d := strings.ToLower(strings.TrimSpace(value))
		labels := strings.Split(d, ".")
		valid := len(d) <= 253 && len(labels) >= 2
		for _, label := range labels {
			if len(label) == 0 || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
				valid = false
				break
			}
			for _, c := range label {
				if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
					valid = false
				}
			}
		}
		alpha := false
		for _, c := range labels[len(labels)-1] {
			if c >= 'a' && c <= 'z' {
				alpha = true
			}
		}
		if !valid || !alpha {
			return nil, fmt.Errorf("signup domains must be exact DNS domains")
		}
		if !seen[d] {
			domains = append(domains, d)
			seen[d] = true
		}
	}
	return domains, nil
}

// PatchGoogleSettings preserves omitted credential fields, including env-backed
// credentials on policy-only writes. Empty client_id explicitly clears clients.
func (h *Handler) PatchGoogleSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	if h.demoMode {
		h.writeError(w, http.StatusServiceUnavailable, "not available in the demo")
		return
	}
	h.googleSettingsMu.Lock()
	defer h.googleSettingsMu.Unlock()
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var req struct {
		ClientID      *string   `json:"client_id"`
		ClientSecret  *string   `json:"client_secret"`
		SignupEnabled *bool     `json:"signup_enabled"`
		SignupDomains *[]string `json:"signup_domains"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	tx, err := h.db.BeginTx(r.Context(), nil)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "could not save Google settings")
		return
	}
	defer tx.Rollback() //nolint:errcheck
	var clientID, secretEnc, rawDomains string
	var enabled bool
	if err := tx.QueryRowContext(r.Context(), `SELECT google_client_id, google_client_secret_enc, google_signup_enabled, google_signup_domains FROM server_settings WHERE id = 1`).Scan(&clientID, &secretEnc, &enabled, &rawDomains); err != nil {
		h.writeError(w, http.StatusInternalServerError, "could not load Google settings")
		return
	}
	if req.SignupEnabled != nil {
		enabled = *req.SignupEnabled
	}
	if req.SignupDomains != nil {
		domains, err := normalizeGoogleDomains(*req.SignupDomains)
		if err != nil {
			h.writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		encoded, _ := json.Marshal(domains)
		rawDomains = string(encoded)
	}
	var domains []string
	if err := json.Unmarshal([]byte(rawDomains), &domains); err != nil {
		h.writeError(w, http.StatusInternalServerError, "invalid stored signup policy")
		return
	}
	if enabled && len(domains) == 0 {
		h.writeError(w, http.StatusBadRequest, "enabled signup requires an allowed domain")
		return
	}
	credentialsChanged := req.ClientID != nil || (req.ClientSecret != nil && *req.ClientSecret != "")
	var resolvedSecret string
	var googleProvider calendar.Provider
	if credentialsChanged {
		// A secret-only update of env credentials intentionally saves the effective ID.
		if clientID == "" {
			if ga := h.getGoogleAuth(); ga != nil {
				clientID, resolvedSecret = ga.ClientID, ga.ClientSecret
			}
		}
		if req.ClientID != nil {
			clientID = strings.TrimSpace(*req.ClientID)
		}
		if clientID == "" {
			secretEnc, resolvedSecret = "", ""
		} else {
			if req.ClientSecret != nil && *req.ClientSecret != "" {
				resolvedSecret = *req.ClientSecret
			} else if secretEnc != "" {
				resolvedSecret, err = secret.Decrypt(h.encKey, secretEnc)
				if err != nil {
					h.writeError(w, http.StatusInternalServerError, "could not decrypt Google credentials")
					return
				}
			}
			if resolvedSecret != "" {
				if secretEnc == "" || (req.ClientSecret != nil && *req.ClientSecret != "") {
					secretEnc, err = secret.Encrypt(h.encKey, resolvedSecret)
				}
				if err != nil {
					h.writeError(w, http.StatusInternalServerError, "could not encrypt Google credentials")
					return
				}
				gc, err := gcal.New(h.db, clientID, resolvedSecret, h.baseURL+"/v1/calendar/callback", hex.EncodeToString(h.encKey[:]))
				if err != nil {
					h.writeError(w, http.StatusInternalServerError, "failed to initialize calendar client")
					return
				}
				googleProvider = gc
			}
		}
	}
	if _, err := tx.ExecContext(r.Context(), `UPDATE server_settings SET google_client_id = ?, google_client_secret_enc = ?, google_signup_enabled = ?, google_signup_domains = ?, updated_at = datetime('now') WHERE id = 1`, clientID, secretEnc, enabled, rawDomains); err != nil {
		h.writeError(w, http.StatusInternalServerError, "could not save Google settings")
		return
	}
	if err := tx.Commit(); err != nil {
		h.writeError(w, http.StatusInternalServerError, "could not save Google settings")
		return
	}
	if credentialsChanged {
		h.setGoogleCalendar(googleProvider)
		if clientID == "" {
			h.authMu.Lock()
			h.googleAuth, h.googleVerifier = nil, nil
			h.authMu.Unlock()
		} else {
			h.SetGoogleAuth(clientID, resolvedSecret, h.baseURL+"/v1/auth/callback", h.secureCookie)
		}
	}
	h.GetGoogleSettings(w, r)
}

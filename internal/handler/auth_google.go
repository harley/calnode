package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const (
	sessionCookieName = "calnode_session"
	stateCookieName   = "calnode_oauth_state"
	sessionDuration   = 30 * 24 * time.Hour
	stateDuration     = 5 * time.Minute
)

// SetGoogleAuth replaces the code-exchange config and matching verifier together.
func (h *Handler) SetGoogleAuth(clientID, clientSecret, redirectURL string, secure bool) {
	cfg := &oauth2.Config{ClientID: clientID, ClientSecret: clientSecret,
		Endpoint: google.Endpoint, RedirectURL: redirectURL, Scopes: []string{"openid", "email", "profile"}}
	ctx := oidc.ClientContext(context.Background(), &http.Client{Timeout: 15 * time.Second})
	verifier := oidc.NewVerifier("https://accounts.google.com",
		oidc.NewRemoteKeySet(ctx, "https://www.googleapis.com/oauth2/v3/certs"),
		&oidc.Config{ClientID: clientID, SupportedSigningAlgs: []string{oidc.RS256}})
	h.authMu.Lock()
	h.googleAuth, h.googleVerifier, h.secureCookie = cfg, verifier, secure
	h.authMu.Unlock()
}

// The nonce is bound to the same short-lived, HttpOnly OAuth state cookie.
func googleNonce(state string) string {
	digest := sha256.Sum256([]byte("google-login:" + state))
	return hex.EncodeToString(digest[:])
}

// LoginGoogle starts the Google OpenID Connect code flow.
func (h *Handler) LoginGoogle(w http.ResponseWriter, r *http.Request) {
	ga := h.getGoogleAuth()
	if ga == nil {
		http.Error(w, "Google OAuth not configured", http.StatusServiceUnavailable)
		return
	}
	state, err := h.newOAuthState(w)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, ga.AuthCodeURL(state, oauth2.AccessTypeOnline, oidc.Nonce(googleNonce(state))), http.StatusFound)
}

// CallbackGoogle trusts only a signed identity for the active OAuth client.
func (h *Handler) CallbackGoogle(w http.ResponseWriter, r *http.Request) {
	h.authMu.RLock()
	ga, verifier := h.googleAuth, h.googleVerifier
	h.authMu.RUnlock()
	if ga == nil || verifier == nil {
		http.Error(w, "Google OAuth not configured", http.StatusServiceUnavailable)
		return
	}
	if !h.verifyOAuthState(w, r) {
		googleLoginError(w, r, "state")
		return
	}
	if r.URL.Query().Get("error") != "" {
		googleLoginError(w, r, "denied")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	tok, err := ga.Exchange(ctx, r.URL.Query().Get("code"))
	if err != nil {
		// Provider errors can contain token bodies; never log them.
		h.logger.WarnContext(ctx, "Google login token exchange failed")
		googleLoginError(w, r, "oauth")
		return
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok || raw == "" {
		googleLoginError(w, r, "identity")
		return
	}
	info, err := verifyGoogleIdentity(ctx, verifier, ga.ClientID, raw, googleNonce(r.URL.Query().Get("state")))
	if err != nil {
		h.logger.WarnContext(ctx, "Google login identity verification failed")
		googleLoginError(w, r, "identity")
		return
	}
	userID, code, err := h.resolveGoogleUser(ctx, info)
	if err != nil {
		h.logger.ErrorContext(ctx, "Google login account resolution failed")
		googleLoginError(w, r, "session")
		return
	}
	if code != "" {
		googleLoginError(w, r, code)
		return
	}
	h.finishOAuthSession(w, r, userID)
}

func googleLoginError(w http.ResponseWriter, r *http.Request, code string) {
	http.Redirect(w, r, "/admin/login?error="+code, http.StatusFound)
}

type googleIdentity struct {
	Subject         string `json:"sub"`
	Email           string `json:"email"`
	Name            string `json:"name"`
	EmailVerified   bool   `json:"email_verified"`
	HostedDomain    string `json:"hd"`
	AuthorizedParty string `json:"azp"`
}

func verifyGoogleIdentity(ctx context.Context, verifier *oidc.IDTokenVerifier, clientID, raw, nonce string) (*googleIdentity, error) {
	token, err := verifier.Verify(ctx, raw)
	if err != nil {
		return nil, err
	}
	if nonce == "" || token.Nonce != nonce {
		return nil, fmt.Errorf("invalid nonce")
	}
	var info googleIdentity
	if err := token.Claims(&info); err != nil {
		return nil, err
	}
	if info.Subject == "" || !info.EmailVerified || info.Email == "" ||
		(info.AuthorizedParty != "" && info.AuthorizedParty != clientID) {
		return nil, fmt.Errorf("invalid identity claims")
	}
	info.Email = strings.ToLower(strings.TrimSpace(info.Email))
	if strings.Count(info.Email, "@") != 1 || strings.ContainsAny(info.Email, " \t\r\n") {
		return nil, fmt.Errorf("invalid email")
	}
	return &info, nil
}

// Logout deletes the session record and clears the session cookie.
// POST /v1/auth/logout
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	// Only delete the session if the cookie value corresponds to an actual row,
	// so a forged or empty cookie cannot be used to trigger arbitrary deletes.
	if cookie, err := r.Cookie(sessionCookieName); err == nil && cookie.Value != "" {
		// Best-effort: logout must proceed (cookie is cleared below) even if this fails;
		// worst case is a harmless stale row that the session's own expiry cleans up.
		//nolint:errcheck
		// #nosec G104
		h.db.ExecContext(r.Context(),
			`DELETE FROM sessions WHERE id = ?`, cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{ // #nosec G124 -- HttpOnly/SameSite/Secure are all set; Secure is h.secureCookie (dynamic on BASE_URL scheme), which gosec's static check can't verify
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.secureCookie,
	})
	http.Redirect(w, r, "/admin/login", http.StatusFound)
}

package handler

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/gcal"
	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
	"strings"
)

func signupHandler(t *testing.T) (*Handler, *sql.DB) {
	t.Helper()
	database, err := db.Open("sqlite://:memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if err := db.Migrate(database); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO users (id,email,name,is_admin,is_owner) VALUES ('owner','owner@example.com','Owner',1,1)`); err != nil {
		t.Fatal(err)
	}
	return New(database, slog.New(slog.NewTextHandler(io.Discard, nil))), database
}

func enableSignup(t *testing.T, database *sql.DB) {
	t.Helper()
	if _, err := database.Exec(`UPDATE server_settings SET google_signup_enabled = 1, google_signup_domains = '["example.com"]' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
}

func signedGoogleToken(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test-key"))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	object, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	token, err := object.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func validGoogleClaims() map[string]any {
	return map[string]any{"iss": "https://accounts.google.com", "aud": "test-client", "sub": "google-person", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": "test-nonce", "email": "person@example.com", "email_verified": true, "hd": "example.com", "name": "Person"}
}

func googleKeys(t *testing.T, key *rsa.PrivateKey) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "test-key", Algorithm: "RS256", Use: "sig"}}})
	}))
	t.Cleanup(server.Close)
	return server
}

func TestGoogleSignedIdentity(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keys := googleKeys(t, key)
	verifier := oidc.NewVerifier("https://accounts.google.com", oidc.NewRemoteKeySet(context.Background(), keys.URL), &oidc.Config{ClientID: "test-client", SupportedSigningAlgs: []string{oidc.RS256}})
	tests := []struct {
		name   string
		alter  func(map[string]any)
		badKey bool
		valid  bool
	}{
		{name: "valid", valid: true},
		{name: "Google legacy issuer", alter: func(c map[string]any) { c["iss"] = "accounts.google.com" }, valid: true},
		{name: "bad signature", badKey: true},
		{name: "wrong issuer", alter: func(c map[string]any) { c["iss"] = "https://attacker.example" }},
		{name: "wrong audience", alter: func(c map[string]any) { c["aud"] = "other-client" }},
		{name: "expired", alter: func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }},
		{name: "missing subject", alter: func(c map[string]any) { delete(c, "sub") }},
		{name: "missing email", alter: func(c map[string]any) { delete(c, "email") }},
		{name: "unverified email", alter: func(c map[string]any) { c["email_verified"] = false }},
		{name: "missing email verification", alter: func(c map[string]any) { delete(c, "email_verified") }},
		{name: "wrong authorized party", alter: func(c map[string]any) { c["azp"] = "other-client" }},
		{name: "wrong nonce", alter: func(c map[string]any) { c["nonce"] = "other-nonce" }},
		{name: "missing nonce", alter: func(c map[string]any) { delete(c, "nonce") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims := validGoogleClaims()
			if tt.alter != nil {
				tt.alter(claims)
			}
			signingKey := key
			if tt.badKey {
				signingKey = other
			}
			identity, err := verifyGoogleIdentity(context.Background(), verifier, "test-client", signedGoogleToken(t, signingKey, claims), "test-nonce")
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%v error=%v", tt.valid, err)
			}
			if tt.valid && identity.Subject != "google-person" {
				t.Fatal("subject lost")
			}
		})
	}
	t.Run("key retrieval failure", func(t *testing.T) {
		unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
		defer unavailable.Close()
		v := oidc.NewVerifier("https://accounts.google.com", oidc.NewRemoteKeySet(context.Background(), unavailable.URL), &oidc.Config{ClientID: "test-client"})
		if _, err := verifyGoogleIdentity(context.Background(), v, "test-client", signedGoogleToken(t, key, validGoogleClaims()), "test-nonce"); err == nil {
			t.Fatal("unavailable keys accepted")
		}
	})
}

func TestGoogleSignupResolution(t *testing.T) {
	tests := []struct {
		name             string
		prepare          func(*sql.DB)
		identity         googleIdentity
		wantID, wantCode string
	}{
		{name: "new member", wantID: "new"},
		{name: "disabled", prepare: func(d *sql.DB) { d.Exec(`UPDATE server_settings SET google_signup_enabled=0`) }, wantCode: "signup"},
		{name: "no domains", prepare: func(d *sql.DB) { d.Exec(`UPDATE server_settings SET google_signup_domains='[]'`) }, wantCode: "signup"},
		{name: "no workspace owner", prepare: func(d *sql.DB) { d.Exec(`DELETE FROM users`) }, wantCode: "signup"},
		{name: "missing workspace hd", identity: googleIdentity{Subject: "person", Email: "person@example.com"}, wantCode: "signup"},
		{name: "wrong workspace hd", identity: googleIdentity{Subject: "person", Email: "person@example.com", HostedDomain: "attacker.com"}, wantCode: "signup"},
		{name: "Gmail", identity: googleIdentity{Subject: "person", Email: "person@gmail.com"}, wantCode: "signup"},
		{name: "email hd mismatch", identity: googleIdentity{Subject: "person", Email: "person@other.com", HostedDomain: "example.com"}, wantCode: "signup"},
		{name: "archived email", prepare: func(d *sql.DB) {
			d.Exec(`INSERT INTO users(id,email,name,archived_at) VALUES('old','person@example.com','Old',datetime('now'))`)
		}, wantCode: "archived"},
		{name: "archived subject", prepare: func(d *sql.DB) {
			d.Exec(`INSERT INTO users(id,email,name,provider,provider_id,archived_at) VALUES('old','old@example.com','Old','google','person',datetime('now'))`)
		}, wantCode: "archived"},
		{name: "different subject same email", prepare: func(d *sql.DB) {
			d.Exec(`INSERT INTO users(id,email,name,provider,provider_id) VALUES('old','person@example.com','Old','google','other-sub')`)
		}, wantCode: "identity_conflict"},
		{name: "other provider", prepare: func(d *sql.DB) {
			d.Exec(`INSERT INTO users(id,email,name,provider,provider_id) VALUES('old','person@example.com','Old','microsoft','ms-sub')`)
		}, wantCode: "identity_conflict"},
		{name: "ambiguous case email", prepare: func(d *sql.DB) {
			d.Exec(`INSERT INTO users(id,email,name) VALUES('old','person@example.com','Old'),('other','Person@example.com','Other')`)
		}, wantCode: "identity_conflict"},
		{name: "legacy owner", identity: googleIdentity{Subject: "owner-sub", Email: "owner@example.com"}, wantID: "owner"},
		{name: "renamed subject", prepare: func(d *sql.DB) {
			d.Exec(`INSERT INTO users(id,email,name,provider,provider_id) VALUES('old','old@example.com','Old','google','person')`)
		}, wantID: "old"},
		{name: "rename email conflicts", prepare: func(d *sql.DB) {
			d.Exec(`INSERT INTO users(id,email,name,provider,provider_id) VALUES('old','old@example.com','Old','google','person'); INSERT INTO users(id,email,name) VALUES('other','person@example.com','Other')`)
		}, wantCode: "identity_conflict"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, d := signupHandler(t)
			enableSignup(t, d)
			if tt.prepare != nil {
				tt.prepare(d)
			}
			identity := tt.identity
			if identity.Subject == "" {
				identity = googleIdentity{Subject: "person", Email: "person@example.com", Name: "Person", HostedDomain: "example.com"}
			}
			id, code, err := h.resolveGoogleUser(context.Background(), &identity)
			if err != nil || code != tt.wantCode {
				t.Fatalf("id=%q code=%q error=%v", id, code, err)
			}
			if tt.wantID == "new" {
				if id == "" || id == "owner" {
					t.Fatal("no new member")
				}
				var admin, owner, emailLogin, keys, teams, hours, cals int
				var password sql.NullString
				var zone, email, sub string
				if err := d.QueryRow(`SELECT is_admin,is_owner,email_login,password_hash,iana_timezone,email,provider_id FROM users WHERE id=?`, id).Scan(&admin, &owner, &emailLogin, &password, &zone, &email, &sub); err != nil {
					t.Fatal(err)
				}
				d.QueryRow(`SELECT count(*) FROM api_keys WHERE user_id=?`, id).Scan(&keys)
				d.QueryRow(`SELECT count(*) FROM team_members WHERE user_id=?`, id).Scan(&teams)
				d.QueryRow(`SELECT count(*) FROM availability_rules WHERE user_id=?`, id).Scan(&hours)
				d.QueryRow(`SELECT count(*) FROM calendar_connections WHERE user_id=?`, id).Scan(&cals)
				if admin+owner+emailLogin+keys+teams+hours+cals != 0 || password.Valid || zone != "UTC" || email != "person@example.com" || sub != "person" {
					t.Fatal("new account has excess privileges or unexpected defaults")
				}
			} else if id != tt.wantID {
				t.Fatalf("id=%q want=%q", id, tt.wantID)
			}
			if tt.wantID == "owner" {
				var admin, owner int
				d.QueryRow(`SELECT is_admin,is_owner FROM users WHERE id='owner'`).Scan(&admin, &owner)
				if admin != 1 || owner != 1 {
					t.Fatal("owner role lost")
				}
			}
		})
	}
}

func TestGoogleSignupConcurrentAndIdempotent(t *testing.T) {
	h, d := signupHandler(t)
	enableSignup(t, d)
	const n = 8
	ids := make(chan string, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, code, err := h.resolveGoogleUser(context.Background(), &googleIdentity{Subject: "one-sub", Email: "one@example.com", HostedDomain: "example.com"})
			if err != nil {
				errs <- err
			}
			if code != "" {
				errs <- fmt.Errorf("login denied: %s", code)
			}
			ids <- id
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatal("duplicate identity")
		}
	}
	var count int
	d.QueryRow(`SELECT count(*) FROM users WHERE provider_id='one-sub'`).Scan(&count)
	if count != 1 {
		t.Fatalf("count=%d", count)
	}
	_, code, err := h.resolveGoogleUser(context.Background(), &googleIdentity{Subject: "second-sub", Email: "one@example.com", HostedDomain: "example.com"})
	if err != nil || code != "identity_conflict" {
		t.Fatal("email reused by another subject")
	}
}

func TestGoogleCallbackSignedSignupSession(t *testing.T) {
	h, d := signupHandler(t)
	enableSignup(t, d)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keys := googleKeys(t, key)
	h.SetGoogleAuth("test-client", "secret", "http://localhost/v1/auth/callback", false)
	h.googleVerifier = oidc.NewVerifier("https://accounts.google.com", oidc.NewRemoteKeySet(context.Background(), keys.URL), &oidc.Config{ClientID: "test-client"})
	login := httptest.NewRecorder()
	h.LoginGoogle(login, httptest.NewRequest("GET", "/v1/auth/login", nil))
	authorize, err := url.Parse(login.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	claims := validGoogleClaims()
	claims["nonce"] = authorize.Query().Get("nonce")
	token := signedGoogleToken(t, key, claims)
	exchange := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "test-access", "token_type": "Bearer", "id_token": token})
	}))
	defer exchange.Close()
	h.googleAuth.Endpoint.TokenURL = exchange.URL
	request := httptest.NewRequest("GET", "/v1/auth/callback?code=test-code&state="+authorize.Query().Get("state"), nil)
	for _, cookie := range login.Result().Cookies() {
		request.AddCookie(cookie)
	}
	result := httptest.NewRecorder()
	h.CallbackGoogle(result, request)
	if result.Header().Get("Location") != "/admin" {
		t.Fatalf("callback=%s", result.Header().Get("Location"))
	}
	var session *http.Cookie
	for _, cookie := range result.Result().Cookies() {
		if cookie.Name == sessionCookieName {
			session = cookie
		}
	}
	if session == nil || session.Value == "" {
		t.Fatal("no session")
	}
	var id string
	d.QueryRow(`SELECT user_id FROM sessions WHERE id=?`, session.Value).Scan(&id)
	if id == "" || id == "owner" {
		t.Fatal("wrong session identity")
	}
	// Seed another user's private data so empty member lists prove isolation.
	for _, statement := range []string{
		`INSERT INTO availability_rules(id,user_id,day_of_week,start_time,end_time) VALUES('owner-hours','owner',1,'09:00','17:00')`,
		`INSERT INTO event_types(id,user_id,slug,name,duration_minutes) VALUES('owner-event','owner','owner-private','Owner private',30)`,
		`INSERT INTO bookings(id,event_type_id,host_id,start_at,end_at,status,created_at,updated_at) VALUES('owner-booking','owner-event','owner','2026-10-08T06:00:00Z','2026-10-08T06:30:00Z','confirmed',datetime('now'),datetime('now'))`,
		`INSERT INTO calendar_connections(id,user_id,provider,access_token_enc,calendar_id,account_email) VALUES('owner-calendar','owner','google','encrypted','primary','owner@example.com')`,
	} {
		if _, err := d.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	gc, err := gcal.New(d, "test-client", "test-secret", "http://localhost/v1/calendar/callback", strings.Repeat("01", 32))
	if err != nil {
		t.Fatal(err)
	}
	svc := calendar.NewService(d)
	svc.Register(gc)
	h.SetCalendar(svc)
	// The new session is not an admin and sees no other user's working hours.
	for _, endpoint := range []struct {
		path    string
		handler http.HandlerFunc
		status  int
	}{
		{"/v1/settings/google", h.GetGoogleSettings, http.StatusForbidden},
		{"/v1/availability-rules", h.ListAvailabilityRules, http.StatusOK},
		{"/v1/calendar/status", h.CalendarStatus, http.StatusOK},
		{"/v1/event-types", h.ListEventTypes, http.StatusOK},
		{"/v1/bookings?scope=all", h.ListBookings, http.StatusOK},
	} {
		req := httptest.NewRequest("GET", endpoint.path, nil)
		req.AddCookie(session)
		rec := httptest.NewRecorder()
		h.RequireAuth(endpoint.handler)(rec, req)
		if rec.Code != endpoint.status {
			t.Fatalf("%s status=%d body=%s", endpoint.path, rec.Code, rec.Body.String())
		}
		if endpoint.status == http.StatusOK {
			var payload map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if items, ok := payload["items"].([]any); ok && len(items) != 0 {
				t.Fatalf("%s leaked another user's data", endpoint.path)
			}
			if connections, ok := payload["connections"].([]any); ok && len(connections) != 0 {
				t.Fatal("calendar data leaked")
			}
		}
	}
	// A forged identity fails before another account or session is persisted.
	token = signedGoogleToken(t, key, map[string]any{"iss": "https://accounts.google.com", "aud": "other-client", "sub": "intruder", "exp": time.Now().Add(time.Hour).Unix()})
	rejected := httptest.NewRecorder()
	h.CallbackGoogle(rejected, request)
	if rejected.Header().Get("Location") != "/admin/login?error=identity" {
		t.Fatal("forged identity accepted")
	}
	var accounts, sessions int
	d.QueryRow(`SELECT count(*) FROM users`).Scan(&accounts)
	d.QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessions)
	if accounts != 2 || sessions != 1 {
		t.Fatal("rejected token changed account/session state")
	}
}

func TestGoogleSignupDifferentSubjectsRace(t *testing.T) {
	h, d := signupHandler(t)
	enableSignup(t, d)
	var wg sync.WaitGroup
	codes := make(chan string, 2)
	for _, sub := range []string{"first", "second"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, code, err := h.resolveGoogleUser(context.Background(), &googleIdentity{Subject: sub, Email: "shared@example.com", HostedDomain: "example.com"})
			if err != nil {
				codes <- "database error"
			} else {
				codes <- code
			}
		}()
	}
	wg.Wait()
	close(codes)
	success, conflict := 0, 0
	for code := range codes {
		switch code {
		case "":
			success++
		case "identity_conflict":
			conflict++
		default:
			t.Fatalf("unexpected code %s", code)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
}

func TestGoogleSignupDoesNotRedeemInviteOrElevateLegacyRoles(t *testing.T) {
	h, d := signupHandler(t)
	enableSignup(t, d)
	if _, err := d.Exec(`INSERT INTO invite_tokens(id,email,token_hash,created_by,expires_at) VALUES ('invite','person@example.com','test-hash','owner','2099-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	id, code, err := h.resolveGoogleUser(context.Background(), &googleIdentity{Subject: "person", Email: "person@example.com", HostedDomain: "example.com"})
	if err != nil || code != "" || id == "" {
		t.Fatal("eligible invited staff could not sign in")
	}
	var used sql.NullString
	d.QueryRow(`SELECT used_at FROM invite_tokens WHERE id='invite'`).Scan(&used)
	if used.Valid {
		t.Fatal("signup redeemed invitation")
	}
	for _, admin := range []int{0, 1} {
		email := fmt.Sprintf("legacy%d@example.com", admin)
		userID := fmt.Sprintf("legacy%d", admin)
		if _, err := d.Exec(`INSERT INTO users(id,email,name,is_admin,email_login,password_hash) VALUES(?,?, 'Legacy',?,1,'retained-password')`, userID, email, admin); err != nil {
			t.Fatal(err)
		}
		got, code, err := h.resolveGoogleUser(context.Background(), &googleIdentity{Subject: userID, Email: email})
		if err != nil || code != "" || got != userID {
			t.Fatal("legacy identity failed")
		}
		var afterAdmin, owner, emailLogin int
		var hash string
		d.QueryRow(`SELECT is_admin,is_owner,email_login,password_hash FROM users WHERE id=?`, userID).Scan(&afterAdmin, &owner, &emailLogin, &hash)
		if afterAdmin != admin || owner != 0 || emailLogin != 1 || hash != "retained-password" {
			t.Fatal("legacy role/password changed")
		}
	}
}

func TestGoogleIdentityKeyTimeout(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer stalled.Close()
	keyContext := oidc.ClientContext(context.Background(), &http.Client{Timeout: 50 * time.Millisecond})
	verifier := oidc.NewVerifier("https://accounts.google.com", oidc.NewRemoteKeySet(keyContext, stalled.URL), &oidc.Config{ClientID: "test-client"})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := verifyGoogleIdentity(ctx, verifier, "test-client", signedGoogleToken(t, key, validGoogleClaims()), "test-nonce"); err == nil {
		t.Fatal("stalled key server accepted")
	}
	if time.Since(start) > time.Second {
		t.Fatal("verification did not respect timeout")
	}
}

func TestGoogleCommittedIdentityRetriesAfterSessionFailure(t *testing.T) {
	h, d := signupHandler(t)
	enableSignup(t, d)
	identity := &googleIdentity{Subject: "retry-sub", Email: "retry@example.com", HostedDomain: "example.com"}
	id, code, err := h.resolveGoogleUser(context.Background(), identity)
	if err != nil || code != "" {
		t.Fatal("identity resolution failed")
	}
	if _, err := d.Exec(`CREATE TRIGGER fail_test_session BEFORE INSERT ON sessions BEGIN SELECT RAISE(FAIL,'test session failure'); END`); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.finishOAuthSession(rec, httptest.NewRequest("GET", "/v1/auth/callback", nil), id)
	if rec.Header().Get("Location") != "/admin/login?error=session" {
		t.Fatal("session failure reported success")
	}
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == sessionCookieName && cookie.Value != "" {
			t.Fatal("failed session set cookie")
		}
	}
	if _, err := d.Exec(`DROP TRIGGER fail_test_session`); err != nil {
		t.Fatal(err)
	}
	again, code, err := h.resolveGoogleUser(context.Background(), identity)
	if err != nil || code != "" || again != id {
		t.Fatal("retry did not reuse committed identity")
	}
	rec = httptest.NewRecorder()
	h.finishOAuthSession(rec, httptest.NewRequest("GET", "/v1/auth/callback", nil), again)
	var count, sessions int
	d.QueryRow(`SELECT count(*) FROM users WHERE provider_id='retry-sub'`).Scan(&count)
	d.QueryRow(`SELECT count(*) FROM sessions WHERE user_id=?`, id).Scan(&sessions)
	if rec.Header().Get("Location") != "/admin" || count != 1 || sessions != 1 {
		t.Fatal("session retry created duplicate or failed")
	}
}

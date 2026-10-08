package handler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmailDomainPolicy_CreateEventType(t *testing.T) {
	h, key, _ := setupWorkspace(t)
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"slug":"work","name":"Work meeting","duration_minutes":30,"blocked_email_domains":[" Gmail.COM ","hotmail.com"]}`, http.StatusCreated},
		{`{"slug":"invalid","name":"Invalid","duration_minutes":30,"blocked_email_domains":["@gmail.com"]}`, http.StatusBadRequest},
	} {
		w := httptest.NewRecorder()
		h.RequireAuth(h.CreateEventType)(w, authReq(http.MethodPost, "/v1/event-types", tc.body, key))
		if w.Code != tc.status {
			t.Fatalf("create policy: %d %s", w.Code, w.Body.String())
		}
		if tc.status == http.StatusCreated && !strings.Contains(w.Body.String(), `"blocked_email_domains":["gmail.com","hotmail.com"]`) {
			t.Fatalf("create lost normalized policy: %s", w.Body.String())
		}
	}
}

func TestEmailDomainPolicy_APIAndBooking(t *testing.T) {
	h, key, _ := setupWorkspace(t)
	slug, _ := seedEventTypeHTTP(t, h, key)
	patch := func(body string) *httptest.ResponseRecorder {
		r := authReq(http.MethodPatch, "/v1/event-types/"+slug, body, key)
		r.SetPathValue("slug", slug)
		w := httptest.NewRecorder()
		h.RequireAuth(h.PatchEventType)(w, r)
		return w
	}
	res := patch(`{"blocked_email_domains":[" Gmail.COM ","hotmail.com","gmail.com"]}`)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"blocked_email_domains":["gmail.com","hotmail.com"]`) {
		t.Fatalf("save policy: %d %s", res.Code, res.Body.String())
	}
	if invalid := patch(`{"blocked_email_domains":["https://gmail.com"]}`); invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid policy: %d %s", invalid.Code, invalid.Body.String())
	}
	for _, email := range []string{"a@GMAIL.COM", "Name <a@hotmail.com>", "a@sub.gmail.com"} {
		res := bookWithEmail(t, h, slug, "09:00", email)
		if res.Code != http.StatusUnprocessableEntity || !strings.Contains(res.Body.String(), "work email") {
			t.Fatalf("blocked booking %q: %d %s", email, res.Code, res.Body.String())
		}
	}
	// An unrelated save preserves the policy, and a duplicate inherits it.
	if res = patch(`{"name":"Updated"}`); res.Code != http.StatusOK {
		t.Fatal(res.Body.String())
	}
	r := authReq(http.MethodPost, "/v1/event-types/"+slug+"/duplicate", "", key)
	r.SetPathValue("slug", slug)
	w := httptest.NewRecorder()
	h.RequireAuth(h.DuplicateEventType)(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("duplicate: %d %s", w.Code, w.Body.String())
	}
	var copy struct {
		Blocked []string `json:"blocked_email_domains"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &copy); err != nil || len(copy.Blocked) != 2 {
		t.Fatalf("duplicate lost policy: %s, err=%v", w.Body.String(), err)
	}
	if res = patch(`{"blocked_email_domains":[]}`); res.Code != http.StatusOK {
		t.Fatal(res.Body.String())
	}
	if res := bookWithEmail(t, h, slug, "09:00", "a@gmail.com"); res.Code != http.StatusCreated {
		t.Fatalf("policy removal: %d %s", res.Code, res.Body.String())
	}
}

package handler_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/booking"
	"github.com/calnode/calnode/internal/handler"
	"github.com/calnode/calnode/internal/uid"
)

// patchSlug sends a slug rename through the admin API and returns the status and body.
func patchSlug(t *testing.T, h *handler.Handler, apiKey, slug, newSlug string) (int, map[string]any) {
	t.Helper()
	req := authReq(http.MethodPatch, "/v1/event-types/"+slug,
		fmt.Sprintf(`{"slug": %q}`, newSlug), apiKey)
	req.SetPathValue("slug", slug)
	rec := httptest.NewRecorder()
	h.RequireAuth(h.PatchEventType)(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

// bookOnce creates a future booking against slug.
func bookOnce(t *testing.T, h *handler.Handler, slug string) {
	t.Helper()
	createBookingViaHTTP(t, h, slug, futureAt(10, 10, 0).Format(time.RFC3339))
}

// TestPatchEventType_renamesWhileUnbooked is the case this exists for: a duplicate lands
// as "<slug>-copy" and there was previously no way to give it a real name (#22).
func TestPatchEventType_renamesWhileUnbooked(t *testing.T) {
	h, apiKey, _ := setupWorkspace(t)
	slug, _ := seedEventTypeHTTP(t, h, apiKey)

	code, body := patchSlug(t, h, apiKey, slug, "intro-call-v2")
	if code != http.StatusOK {
		t.Fatalf("rename: %d - %v", code, body)
	}
	if got := body["slug"]; got != "intro-call-v2" {
		t.Errorf("slug = %v, want intro-call-v2", got)
	}

	// The response must describe the row as it now is. The handler re-reads after the
	// UPDATE, and re-reading under the name the request arrived on would 404 a patch
	// that actually succeeded.
	if got := body["name"]; got == nil || got == "" {
		t.Errorf("response lost the rest of the event type after a rename: %v", body)
	}

	// And the new slug is the one that resolves from here on.
	if code, _ := patchSlug(t, h, apiKey, "intro-call-v2", "intro-call-v3"); code != http.StatusOK {
		t.Errorf("second rename through the new slug: %d; want 200", code)
	}
}

// Tokens issued before a rename must keep managing the same booking. Cover both
// an active booking and one already cancelled when the URL changes.
func TestPatchEventType_renamePreservesBookingsAndManageLinks(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancelled=%t", cancelled), func(t *testing.T) {
			h, database, apiKey, hostID := setupWorkspaceWithDB(t)
			slug, etID := seedEventTypeHTTP(t, h, apiKey)
			start := futureAt(10, 10, 0).Format(time.RFC3339)
			bookingID := createBookingViaHTTP(t, h, slug, start)
			token := issueTestToken(t, database, bookingID)
			wantStatus := "confirmed"
			if cancelled {
				if err := booking.New(database).Cancel(t.Context(), hostID, bookingID, "test"); err != nil {
					t.Fatal(err)
				}
				wantStatus = "cancelled"
			}

			const newSlug = "renamed-meeting"
			code, body := patchSlug(t, h, apiKey, slug, newSlug)
			if code != http.StatusOK || body["slug"] != newSlug || body["id"] != etID {
				t.Fatalf("rename: %d - %v", code, body)
			}
			var gotETID, gotHostID, status, gotStart string
			if err := database.QueryRow(`SELECT event_type_id, host_id, status, start_at FROM bookings WHERE id = ?`, bookingID).
				Scan(&gotETID, &gotHostID, &status, &gotStart); err != nil {
				t.Fatal(err)
			}
			if gotETID != etID || gotHostID != hostID || status != wantStatus || gotStart != start {
				t.Fatalf("booking changed on rename: event=%s host=%s status=%s start=%s", gotETID, gotHostID, status, gotStart)
			}

			// The new booking page and slots resolve; the old URL has no redirect.
			for _, tc := range []struct {
				slug   string
				status int
			}{{newSlug, http.StatusOK}, {slug, http.StatusNotFound}} {
				for _, page := range []bool{false, true} {
					path := "/v1/event-types/" + tc.slug + "/slots"
					if page {
						path = "/book/" + tc.slug
					}
					req := httptest.NewRequest(http.MethodGet, path, nil)
					req.SetPathValue("slug", tc.slug)
					rec := httptest.NewRecorder()
					if page {
						h.BookPage(rec, req)
					} else {
						h.GetSlots(rec, req)
					}
					if rec.Code != tc.status || rec.Header().Get("Location") != "" {
						t.Fatalf("%s: %d - %s", path, rec.Code, rec.Body.String())
					}
				}
			}

			req := httptest.NewRequest(http.MethodGet, "/manage/"+token, nil)
			req.SetPathValue("token", token)
			rec := httptest.NewRecorder()
			h.ManagePage(rec, req)
			if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `id="token-invalid-view"`) {
				t.Fatalf("pre-rename manage link: %d - %s", rec.Code, rec.Body.String())
			}
			if !cancelled && !strings.Contains(rec.Body.String(), `const SLUG        = "`+newSlug+`"`) {
				t.Fatal("manage page did not use the new slug for slots")
			}
			if cancelled && !strings.Contains(rec.Body.String(), "already cancelled") {
				t.Fatal("manage page lost cancelled state")
			}

			// Use the original token to reschedule and cancel after the rename.
			newStart := futureAt(11, 14, 0).Format(time.RFC3339)
			req = httptest.NewRequest(http.MethodPost, "/manage/"+token+"/reschedule", strings.NewReader(fmt.Sprintf(`{"start_at":%q}`, newStart)))
			req.SetPathValue("token", token)
			rec = httptest.NewRecorder()
			h.RescheduleByToken(rec, req)
			wantCode := http.StatusOK
			if cancelled {
				wantCode = http.StatusConflict
			}
			if rec.Code != wantCode {
				t.Fatalf("reschedule: %d - %s", rec.Code, rec.Body.String())
			}
			if !cancelled {
				var updated map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
					t.Fatal(err)
				}
				if updated["id"] != bookingID || updated["start_at"] != newStart {
					t.Fatalf("reschedule changed booking identity or missed time: %v", updated)
				}
			}
			req = httptest.NewRequest(http.MethodPost, "/manage/"+token+"/cancel", strings.NewReader(`{"reason":"test"}`))
			req.SetPathValue("token", token)
			rec = httptest.NewRecorder()
			h.CancelByToken(rec, req)
			if rec.Code != wantCode {
				t.Fatalf("cancel: %d - %s", rec.Code, rec.Body.String())
			}
			if err := database.QueryRow(`SELECT event_type_id, status FROM bookings WHERE id = ?`, bookingID).Scan(&gotETID, &status); err != nil {
				t.Fatal(err)
			}
			if gotETID != etID || status != "cancelled" {
				t.Fatalf("booking after manage actions: event=%s status=%s", gotETID, status)
			}
			createBookingViaHTTP(t, h, newSlug, futureAt(12, 10, 0).Format(time.RFC3339))
		})
	}
}

// Patching other fields on a booked event type must keep working.
func TestPatchEventType_bookedEventTypeStillEditable(t *testing.T) {
	h, apiKey, _ := setupWorkspace(t)
	slug, _ := seedEventTypeHTTP(t, h, apiKey)
	bookOnce(t, h, slug)

	req := authReq(http.MethodPatch, "/v1/event-types/"+slug, `{"name": "Renamed, not re-slugged"}`, apiKey)
	req.SetPathValue("slug", slug)
	rec := httptest.NewRecorder()
	h.RequireAuth(h.PatchEventType)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch name on a booked event type: %d - %s", rec.Code, rec.Body.String())
	}
}

// The editor submits every field; sending the same slug remains a no-op.
func TestPatchEventType_unchangedSlugOnBookedEventTypeIsFine(t *testing.T) {
	h, apiKey, _ := setupWorkspace(t)
	slug, _ := seedEventTypeHTTP(t, h, apiKey)
	bookOnce(t, h, slug)

	code, body := patchSlug(t, h, apiKey, slug, slug)
	if code != http.StatusOK {
		t.Fatalf("resubmitting the same slug: %d; want 200 - %v", code, body)
	}
}

func TestPatchEventType_renameRejectsCollisionAndEmpty(t *testing.T) {
	h, apiKey, _ := setupWorkspace(t)
	slugA, _ := seedEventTypeHTTP(t, h, apiKey)
	slugB, _ := seedEventTypeHTTP(t, h, apiKey)
	bookOnce(t, h, slugA)

	if code, _ := patchSlug(t, h, apiKey, slugA, slugB); code != http.StatusConflict {
		t.Errorf("rename onto an existing slug: %d; want 409", code)
	}
	// slugify strips everything usable out of this, so it is empty rather than invalid.
	for _, empty := range []string{"", "  ", "!!!"} {
		if code, _ := patchSlug(t, h, apiKey, slugA, empty); code != http.StatusBadRequest {
			t.Errorf("rename to %q: %d; want 400", empty, code)
		}
	}
	// And it is normalised rather than taken literally, matching team slugs.
	if code, body := patchSlug(t, h, apiKey, slugA, "  Intro Call  "); code != http.StatusOK {
		t.Errorf("rename with spaces and caps: %d - %v", code, body)
	} else if got := body["slug"]; got != "intro-call" {
		t.Errorf("slug = %v, want intro-call (slugified)", got)
	}
}

func TestPatchEventType_renameRequiresOwner(t *testing.T) {
	h, database, apiKey, _ := setupWorkspaceWithDB(t)
	slug, etID := seedEventTypeHTTP(t, h, apiKey)
	bookOnce(t, h, slug)
	otherID := uid.New()
	if _, err := database.Exec(`INSERT INTO users (id, email, name, iana_timezone) VALUES (?, 'other@example.com', 'Other', 'UTC')`, otherID); err != nil {
		t.Fatal(err)
	}
	// Even an authenticated admin cannot rename another owner's event type.
	if _, err := database.Exec(`UPDATE event_types SET user_id = ? WHERE id = ?`, otherID, etID); err != nil {
		t.Fatal(err)
	}
	if code, _ := patchSlug(t, h, apiKey, slug, "not-yours"); code != http.StatusNotFound {
		t.Fatalf("non-owner rename: %d; want 404", code)
	}
	if code, _ := patchSlug(t, h, "", slug, "anonymous"); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated rename: %d; want 401", code)
	}
	var gotSlug string
	if err := database.QueryRow(`SELECT slug FROM event_types WHERE id = ?`, etID).Scan(&gotSlug); err != nil {
		t.Fatal(err)
	}
	if gotSlug != slug {
		t.Fatalf("unauthorized rename changed slug to %q", gotSlug)
	}
}

package handler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/mailer"
)

func TestReassignBooking_movesHost(t *testing.T) {
	h, database, ownerKey, _ := setupWorkspaceWithDB(t)
	cap := bookingEmailCapture{messages: make(chan mailer.Message, 4)}
	h.SetMailer(cap, "https://book.example.com")
	// Two members who can host.
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u2','h2@example.com','Host2','UTC',0)`)
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u3','h3@example.com','Host3','UTC',0)`)
	database.Exec(`INSERT INTO event_types (id,user_id,slug,name,duration_minutes) VALUES ('et1','u2','et-slug','Intro',30)`)
	database.Exec(`INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status)
		VALUES ('b1','et1','u2','2099-01-01T10:00:00Z','2099-01-01T10:30:00Z','confirmed')`)
	database.Exec(`INSERT INTO booking_attendees (id,booking_id,name,email,iana_timezone,is_organizer)
		VALUES ('a1','b1','Alice','alice@example.com','UTC',1)`)
	if _, err := database.Exec(`INSERT INTO event_type_questions (id,event_type_id,label,type)
		VALUES ('q1','et1','What should we discuss?','text')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO booking_answers (id,booking_id,question_id,value)
		VALUES ('answer1','b1','q1','The project scope')`); err != nil {
		t.Fatal(err)
	}

	req := authReq(http.MethodPost, "/v1/bookings/b1/reassign", `{"host_id":"u3"}`, ownerKey)
	req.SetPathValue("id", "b1")
	rec := httptest.NewRecorder()
	h.RequireAuth(h.ReassignBooking)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("reassign: got %d — %s", rec.Code, rec.Body.String())
	}

	var hostID string
	database.QueryRow(`SELECT host_id FROM bookings WHERE id='b1'`).Scan(&hostID)
	if hostID != "u3" {
		t.Errorf("host_id = %q; want u3", hostID)
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case msg := <-cap.messages:
			if len(msg.To) == 0 || msg.To[0] != "h3@example.com" {
				continue
			}
			for _, part := range []string{"What should we discuss?", "The project scope"} {
				if !strings.Contains(msg.Text, part) || !strings.Contains(msg.HTML, part) {
					t.Errorf("reassigned host email missing %q in text or HTML", part)
				}
			}
			return
		case <-deadline.C:
			t.Fatal("timed out waiting for reassigned host email")
		}
	}
}

func TestReassignBooking_conflictWhenNewHostBusy(t *testing.T) {
	h, database, ownerKey, _ := setupWorkspaceWithDB(t)
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u2','h2@example.com','Host2','UTC',0)`)
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u3','h3@example.com','Host3','UTC',0)`)
	database.Exec(`INSERT INTO event_types (id,user_id,slug,name,duration_minutes) VALUES ('et1','u2','et-slug','Intro',30)`)
	// Booking to move (hosted by u2) and a conflicting booking already on u3 at the same time.
	database.Exec(`INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status)
		VALUES ('b1','et1','u2','2099-01-01T10:00:00Z','2099-01-01T10:30:00Z','confirmed')`)
	database.Exec(`INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status)
		VALUES ('b2','et1','u3','2099-01-01T10:15:00Z','2099-01-01T10:45:00Z','confirmed')`)
	// Availability is computed from booking_hosts (every attended seat), as real
	// bookings are recorded — so u3's conflicting booking needs its host row.
	database.Exec(`INSERT INTO booking_hosts (id,booking_id,user_id,is_primary) VALUES ('bh2','b2','u3',1)`)

	req := authReq(http.MethodPost, "/v1/bookings/b1/reassign", `{"host_id":"u3"}`, ownerKey)
	req.SetPathValue("id", "b1")
	rec := httptest.NewRecorder()
	h.RequireAuth(h.ReassignBooking)(rec, req)
	if rec.Code != http.StatusConflict {
		t.Errorf("got %d; want 409 (new host busy) — %s", rec.Code, rec.Body.String())
	}
	// Original host unchanged.
	var hostID string
	database.QueryRow(`SELECT host_id FROM bookings WHERE id='b1'`).Scan(&hostID)
	if hostID != "u2" {
		t.Errorf("host_id = %q; want u2 (unchanged after conflict)", hostID)
	}
}

func TestReassignBooking_rejectsArchivedHost(t *testing.T) {
	h, database, ownerKey, _ := setupWorkspaceWithDB(t)
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u2','h2@example.com','Host2','UTC',0)`)
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin,archived_at) VALUES ('u3','h3@example.com','Host3','UTC',0,'2026-01-01T00:00:00Z')`)
	database.Exec(`INSERT INTO event_types (id,user_id,slug,name,duration_minutes) VALUES ('et1','u2','et-slug','Intro',30)`)
	database.Exec(`INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status)
		VALUES ('b1','et1','u2','2099-01-01T10:00:00Z','2099-01-01T10:30:00Z','confirmed')`)

	req := authReq(http.MethodPost, "/v1/bookings/b1/reassign", `{"host_id":"u3"}`, ownerKey)
	req.SetPathValue("id", "b1")
	rec := httptest.NewRecorder()
	h.RequireAuth(h.ReassignBooking)(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("got %d; want 400 (archived host) — %s", rec.Code, rec.Body.String())
	}
}

func TestListUserUpcomingBookings_returnsHostedUpcoming(t *testing.T) {
	h, database, ownerKey, _ := setupWorkspaceWithDB(t)
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u2','h2@example.com','Host2','UTC',0)`)
	database.Exec(`INSERT INTO event_types (id,user_id,slug,name,duration_minutes) VALUES ('et1','u2','et-slug','Intro',30)`)
	// One upcoming, one past, one cancelled — only the upcoming should appear.
	database.Exec(`INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status)
		VALUES ('b1','et1','u2','2099-01-01T10:00:00Z','2099-01-01T10:30:00Z','confirmed')`)
	database.Exec(`INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status)
		VALUES ('b2','et1','u2','2000-01-01T10:00:00Z','2000-01-01T10:30:00Z','confirmed')`)
	database.Exec(`INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status)
		VALUES ('b3','et1','u2','2099-02-01T10:00:00Z','2099-02-01T10:30:00Z','cancelled')`)

	req := authReq(http.MethodGet, "/v1/users/u2/upcoming-bookings", "", ownerKey)
	req.SetPathValue("id", "u2")
	rec := httptest.NewRecorder()
	h.RequireAuth(h.ListUserUpcomingBookings)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d — %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Items) != 1 || resp.Items[0].ID != "b1" {
		t.Errorf("expected only upcoming b1; got %+v", resp.Items)
	}
}

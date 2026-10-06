package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/mailer"
)

type bookingEmailCapture struct{ messages chan mailer.Message }

func (c bookingEmailCapture) Send(_ context.Context, msg mailer.Message) error {
	c.messages <- msg
	return nil
}

func TestCreateBooking_hostEmailIncludesSavedAnswer(t *testing.T) {
	h, _, key, _ := setupWorkspaceWithDB(t)
	cap := bookingEmailCapture{messages: make(chan mailer.Message, 4)}
	h.SetMailer(cap, "https://book.example.com")
	slug, _ := seedEventTypeHTTP(t, h, key)
	questionID := createQuestion(t, h, slug, key, `{"label":"What should we discuss?","type":"text","required":true}`)

	body := fmt.Sprintf(`{"event_type_slug":%q,"start_at":"2026-06-20T10:00:00Z","name":"Guest","email":"guest@example.com","answers":[{"question_id":%q,"value":"The project scope"}]}`, slug, questionID)
	req := httptest.NewRequest(http.MethodPost, "/v1/bookings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.CreateBooking(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create booking: %d — %s", rec.Code, rec.Body.String())
	}

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case msg := <-cap.messages:
			if len(msg.To) == 0 || msg.To[0] != "host@example.com" {
				continue
			}
			for _, part := range []string{"What should we discuss?", "The project scope"} {
				if !strings.Contains(msg.Text, part) || !strings.Contains(msg.HTML, part) {
					t.Errorf("host notification missing %q in text or HTML", part)
				}
			}
			return
		case <-timer.C:
			t.Fatal("timed out waiting for host notification")
		}
	}
}

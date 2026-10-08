package handler_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/stripe"
)

func TestStripeReturnFollowsRenamedEvent(t *testing.T) {
	h, database, apiKey, _ := setupWorkspaceWithDB(t)
	slug, etID := seedEventTypeHTTP(t, h, apiKey)
	if _, err := database.Exec(`UPDATE event_types SET price_cents = 1000 WHERE id = ?`, etID); err != nil {
		t.Fatal(err)
	}
	h.SetPublicBaseURL("https://book.example.test")
	checkoutURLs := make(chan [2]string, 1)
	stripeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/checkout/sessions" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		checkoutURLs <- [2]string{r.Form.Get("success_url"), r.Form.Get("cancel_url")}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"cs_test_123","url":"https://checkout.stripe.test/session"}`)
	}))
	defer stripeServer.Close()
	sc, err := stripe.New("sk_test_x", "", "")
	if err != nil {
		t.Fatal(err)
	}
	sc.SetAPIBase(stripeServer.URL)
	h.SetStripe(sc)

	start := futureAt(10, 10, 0).Format(time.RFC3339)
	requestBody := fmt.Sprintf(`{"event_type_slug":%q,"start_at":%q,"name":"Test Attendee","email":"attendee@example.com","timezone":"UTC"}`, slug, start)
	req := httptest.NewRequest(http.MethodPost, "/v1/bookings", strings.NewReader(requestBody))
	rec := httptest.NewRecorder()
	h.CreateBooking(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("paid booking: %d - %s", rec.Code, rec.Body.String())
	}
	var booking struct {
		ID string `json:"booking_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &booking); err != nil || booking.ID == "" {
		t.Fatalf("paid booking response: %s (%v)", rec.Body.String(), err)
	}
	urls := <-checkoutURLs
	returnURL := "https://book.example.test/book/return/" + booking.ID
	if urls[0] != returnURL+"?paid=1&session_id={CHECKOUT_SESSION_ID}" || urls[1] != returnURL {
		t.Fatalf("Checkout return URLs are not stable: %q, %q", urls[0], urls[1])
	}

	const newSlug = "renamed-paid-meeting"
	if code, body := patchSlug(t, h, apiKey, slug, newSlug); code != http.StatusOK {
		t.Fatalf("rename while Checkout is open: %d - %v", code, body)
	}
	for _, tc := range []struct {
		query string
		want  string
	}{
		{"?paid=1&session_id=cs_test_123", "/book/" + newSlug + "?paid=1&session_id=cs_test_123"},
		{"", "/book/" + newSlug},
	} {
		req := httptest.NewRequest(http.MethodGet, "/book/return/"+booking.ID+tc.query, nil)
		req.SetPathValue("id", booking.ID)
		rec := httptest.NewRecorder()
		h.BookingReturn(rec, req)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != tc.want {
			t.Fatalf("return %q: %d to %q, want %q", tc.query, rec.Code, rec.Header().Get("Location"), tc.want)
		}
		pageReq := httptest.NewRequest(http.MethodGet, tc.want, nil)
		pageReq.SetPathValue("slug", newSlug)
		pageRec := httptest.NewRecorder()
		h.BookPage(pageRec, pageReq)
		if pageRec.Code != http.StatusOK {
			t.Fatalf("returned page: %d - %s", pageRec.Code, pageRec.Body.String())
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/book/return/no-such-booking", nil)
	req.SetPathValue("id", "no-such-booking")
	rec = httptest.NewRecorder()
	h.BookingReturn(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown booking return: %d; want 404", rec.Code)
	}
}

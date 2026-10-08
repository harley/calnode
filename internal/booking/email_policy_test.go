package booking_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/calnode/calnode/internal/booking"
)

func TestBlockedEmailDomains(t *testing.T) {
	domains, err := booking.NormalizeBlockedDomains([]string{" Gmail.COM ", "hotmail.com", "gmail.com"})
	if err != nil || !reflect.DeepEqual(domains, []string{"gmail.com", "hotmail.com"}) {
		t.Fatalf("normalized domains = %v, err = %v", domains, err)
	}
	for _, domain := range []string{"", "gmail", "@gmail.com", "https://gmail.com", "*.gmail.com", "gmail..com", "-gmail.com"} {
		if _, err := booking.NormalizeBlockedDomains([]string{domain}); err == nil {
			t.Errorf("accepted invalid domain %q", domain)
		}
	}
	for _, email := range []string{"a@gmail.com", "Name <a@GMAIL.COM>", "a@sub.gmail.com", "a@hotmail.com", `"a@company.com"@gmail.com`} {
		if err := booking.CheckEmailDomain(email, domains); !errors.Is(err, booking.ErrEmailDomainBlocked) {
			t.Errorf("%q: expected blocked domain, got %v", email, err)
		}
	}
	for _, email := range []string{"a@company.com", "gmail.com@company.com", "a@notgmail.com", "a@gmail.com.company.com", `"a@gmail.com"@company.com`} {
		if err := booking.CheckEmailDomain(email, domains); err != nil {
			t.Errorf("%q: unexpected rejection: %v", email, err)
		}
	}
	if err := booking.CheckEmailDomain("a@gmail.com", nil); err != nil {
		t.Fatal(err)
	}
}

func TestCreate_enforcesCurrentEmailPolicy(t *testing.T) {
	database := newTestDB(t)
	host := seedHost(t, database)
	event := seedEventType(t, database, host)
	if _, err := database.Exec(`UPDATE event_types SET blocked_email_domains = '["gmail.com"]' WHERE id = ?`, event); err != nil {
		t.Fatal(err)
	}
	params := booking.CreateParams{
		EventTypeID: event, HostIDs: []string{host}, StartAt: slot(9, 0), EndAt: slot(9, 30),
		Organizer: booking.Attendee{Name: "Test", Email: "a@gmail.com"},
	}
	svc := booking.New(database)
	if _, err := svc.Create(context.Background(), params); !errors.Is(err, booking.ErrEmailDomainBlocked) {
		t.Fatalf("expected domain rejection, got %v", err)
	}
	var count int
	if err := database.QueryRow("SELECT COUNT(*) FROM bookings").Scan(&count); err != nil || count != 0 {
		t.Fatalf("blocked booking persisted: count=%d, err=%v", count, err)
	}
	// The policy is editable; removing it immediately permits the same address.
	if _, err := database.Exec(`UPDATE event_types SET blocked_email_domains = '[]' WHERE id = ?`, event); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(context.Background(), params); err != nil {
		t.Fatalf("booking after policy removal: %v", err)
	}
}

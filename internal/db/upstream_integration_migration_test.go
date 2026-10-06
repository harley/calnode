package db

import (
	"testing"

	"github.com/pressly/goose/v3"
)

// The CoderPush database has already applied its own migrations 68-70. The
// upstream invite migrations must run after them without replacing that history.
func TestUpstreamInviteMigrationsAfterCoderPush70(t *testing.T) {
	database, err := Open("sqlite://:memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(database, "migrations", 70); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO users (id, email, name, iana_timezone)
		VALUES ('host', 'host@example.com', 'Host', 'UTC')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO event_types
		(id, user_id, slug, name, duration_minutes, blocked_email_domains)
		VALUES ('event', 'host', 'event', 'Event', 30, '["gmail.com"]')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO bookings
		(id, event_type_id, host_id, start_at, end_at, status)
		VALUES ('booking', 'event', 'host', '2026-10-01T10:00:00Z', '2026-10-01T10:30:00Z', 'cancelled')`); err != nil {
		t.Fatal(err)
	}

	if err := Migrate(database); err != nil {
		t.Fatalf("upgrade from CoderPush 70: %v", err)
	}
	version, err := AppliedVersion(t.Context(), database)
	if err != nil || version < 72 {
		t.Fatalf("version = %d, err = %v; want at least 72", version, err)
	}
	var domains, eventInvite, bookingInvite, organizer string
	if err := database.QueryRow(`SELECT blocked_email_domains, invite_delivery
		FROM event_types WHERE id = 'event'`).Scan(&domains, &eventInvite); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT invite_delivery, invite_organizer
		FROM bookings WHERE id = 'booking'`).Scan(&bookingInvite, &organizer); err != nil {
		t.Fatal(err)
	}
	if domains != `["gmail.com"]` || eventInvite != "calendar" || bookingInvite != "calendar" || organizer != "" {
		t.Fatalf("upgrade changed existing data: domains=%q event_invite=%q booking_invite=%q organizer=%q",
			domains, eventInvite, bookingInvite, organizer)
	}
	rows, err := database.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("upgrade left a foreign-key violation")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

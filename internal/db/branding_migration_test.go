package db_test

import (
	"testing"

	"github.com/calnode/calnode/internal/db"
	"github.com/pressly/goose/v3"
)

func TestBrandingUpgradePreservesCustomNames(t *testing.T) {
	for _, tc := range []struct {
		name, sender, business, wantSender, wantBusiness string
	}{
		{"upstream", "Calnode", "Calnode", "Book with CoderPush", "Book with CoderPush"},
		{"custom", "Sales team", "Acme", "Sales team", "Acme"},
		{"unconfigured", "Calnode", "", "Book with CoderPush", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, err := db.Open("sqlite://:memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			if err := db.Migrate(database); err != nil {
				t.Fatal(err)
			}
			if err := goose.DownTo(database, "migrations", 68); err != nil {
				t.Fatal(err)
			}
			if _, err := database.Exec(`UPDATE server_settings SET email_from_name = ?, business_name = ? WHERE id = 1`, tc.sender, tc.business); err != nil {
				t.Fatal(err)
			}
			if err := db.Migrate(database); err != nil {
				t.Fatal(err)
			}
			var sender, business string
			if err := database.QueryRow(`SELECT email_from_name, business_name FROM server_settings WHERE id = 1`).Scan(&sender, &business); err != nil {
				t.Fatal(err)
			}
			if sender != tc.wantSender || business != tc.wantBusiness {
				t.Fatalf("sender/business = %q/%q; want %q/%q", sender, business, tc.wantSender, tc.wantBusiness)
			}
		})
	}
}

package db

import (
	"github.com/pressly/goose/v3"
	"testing"
)

func TestGoogleSubjectIndexUpgrade(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy upgrade", true: "duplicate subjects fail safely"}[duplicate], func(t *testing.T) {
			database, err := Open("sqlite://:memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			goose.SetBaseFS(migrations)
			if err := goose.SetDialect("sqlite3"); err != nil {
				t.Fatal(err)
			}
			if err := goose.UpTo(database, "migrations", 69); err != nil {
				t.Fatal(err)
			}
			subject := ""
			if duplicate {
				subject = "duplicate-subject"
			}
			if _, err := database.Exec(`INSERT INTO users(id,email,name,provider,provider_id) VALUES ('one','one@example.com','One','google',?),('two','two@example.com','Two','google',?)`, subject, subject); err != nil {
				t.Fatal(err)
			}
			err = Migrate(database)
			if duplicate {
				if err == nil {
					t.Fatal("duplicate subjects accepted")
				}
				var count int
				database.QueryRow(`SELECT count(*) FROM users`).Scan(&count)
				if count != 2 {
					t.Fatal("failed migration discarded users")
				}
				version, err := AppliedVersion(t.Context(), database)
				if err != nil || version != 69 {
					t.Fatalf("failed upgrade version=%d err=%v", version, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var enabled bool
			var domains string
			if err := database.QueryRow(`SELECT google_signup_enabled,google_signup_domains FROM server_settings WHERE id=1`).Scan(&enabled, &domains); err != nil {
				t.Fatal(err)
			}
			if enabled || domains != "[]" {
				t.Fatal("upgrade enabled signup")
			}
			if _, err := database.Exec(`UPDATE users SET provider_id='stable-subject' WHERE id='one'`); err != nil {
				t.Fatal(err)
			}
			if _, err := database.Exec(`UPDATE users SET provider_id='stable-subject' WHERE id='two'`); !IsUniqueViolation(err) {
				t.Fatalf("duplicate subject not constrained: %v", err)
			}
		})
	}
}

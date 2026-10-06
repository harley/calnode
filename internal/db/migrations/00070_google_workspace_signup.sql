-- +goose Up
ALTER TABLE server_settings ADD COLUMN google_signup_enabled INTEGER NOT NULL DEFAULT 0 CHECK (google_signup_enabled IN (0, 1));
ALTER TABLE server_settings ADD COLUMN google_signup_domains TEXT NOT NULL DEFAULT '[]';
-- Fail rather than discard accounts if a prior operator wrote duplicate subjects.
CREATE UNIQUE INDEX users_google_subject ON users(provider_id)
WHERE provider = 'google' AND provider_id IS NOT NULL AND provider_id <> '';

-- +goose Down
DROP INDEX users_google_subject;
ALTER TABLE server_settings DROP COLUMN google_signup_domains;
ALTER TABLE server_settings DROP COLUMN google_signup_enabled;

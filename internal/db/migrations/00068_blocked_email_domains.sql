-- +goose Up
ALTER TABLE event_types ADD COLUMN blocked_email_domains TEXT NOT NULL DEFAULT '[]';

-- +goose Down
ALTER TABLE event_types DROP COLUMN blocked_email_domains;

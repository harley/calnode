-- +goose Up
UPDATE server_settings SET email_from_name = 'Book with CoderPush'
WHERE email_from_name = 'Calnode';
UPDATE server_settings SET business_name = 'Book with CoderPush'
WHERE business_name = 'Calnode';

-- +goose Down
-- Keep display names on rollback: reversing these values could overwrite a
-- name the operator explicitly selected after the upgrade.
SELECT 1;

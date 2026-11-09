-- +goose Up
-- Names the constraints 001 and 002 left to Postgres, or named apart from the
-- rest, so errors and ffserrors refer to them by names the schema chose.
ALTER TABLE identities RENAME CONSTRAINT identities_email_key TO identities_email_unique;
ALTER TABLE users RENAME CONSTRAINT users_email_key TO users_email_unique;
ALTER TABLE users RENAME CONSTRAINT fk_identities TO users_identity_fk;

-- +goose Down
ALTER TABLE users RENAME CONSTRAINT users_identity_fk TO fk_identities;
ALTER TABLE users RENAME CONSTRAINT users_email_unique TO users_email_key;
ALTER TABLE identities RENAME CONSTRAINT identities_email_unique TO identities_email_key;

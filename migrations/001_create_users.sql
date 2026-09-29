-- +goose Up
CREATE TABLE users (
    id UUID PRIMARY KEY,
    email TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('student', 'admin')),
    status TEXT NOT NULL CHECK (status IN ('active', 'disabled')),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    last_login_at TIMESTAMPTZ,
    CONSTRAINT users_email_normalized CHECK (email = lower(email)),
    CONSTRAINT users_email_not_blank CHECK (length(trim(email)) > 0),
    CONSTRAINT users_password_hash_not_blank CHECK (length(trim(password_hash)) > 0)
);

CREATE UNIQUE INDEX users_email_unique_idx ON users (email);
CREATE INDEX users_role_idx ON users (role);
CREATE INDEX users_status_idx ON users (status);

-- +goose Down
DROP TABLE users;
-- +goose Up
CREATE TABLE lab_sessions (
    id UUID PRIMARY KEY,
    user_id UUID NOT NULL REFERENCES users (id),
    lab_id TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('REQUESTED', 'STARTING', 'READY', 'STOPPING', 'STOPPED', 'FAILED')),
    runtime_namespace TEXT,
    runtime_pod_name TEXT,
    runtime_service_name TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    started_at TIMESTAMPTZ,
    stopped_at TIMESTAMPTZ,
    failure_code TEXT
);

CREATE INDEX lab_sessions_user_created_idx ON lab_sessions (user_id, created_at DESC);
CREATE INDEX lab_sessions_state_updated_idx ON lab_sessions (state, updated_at);
CREATE INDEX lab_sessions_runtime_pod_idx ON lab_sessions (runtime_namespace, runtime_pod_name);
CREATE INDEX lab_sessions_runtime_service_idx ON lab_sessions (runtime_namespace, runtime_service_name);

-- +goose Down
DROP TABLE lab_sessions;
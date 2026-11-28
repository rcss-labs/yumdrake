-- +goose Up
-- Runner registry and system counters, for the CloudWatch metrics and the
-- status screens. (00009 belongs to another branch.)

CREATE TABLE runners (
    id           text        PRIMARY KEY,
    version      text        NOT NULL,
    concurrency  int         NOT NULL,
    busy         int         NOT NULL,
    started_at   timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL
);
CREATE INDEX runners_last_seen_at_idx ON runners (last_seen_at);

CREATE TABLE system_events (
    id   bigserial   PRIMARY KEY,
    kind text        NOT NULL CHECK (kind IN ('lease_expired', 'log_persist_failed', 'sync_failed')),
    at   timestamptz NOT NULL
);
CREATE INDEX system_events_kind_at_idx ON system_events (kind, at);

-- When a message was given up on (state 'failed'); NULL for the others.
ALTER TABLE notifications ADD COLUMN failed_at timestamptz;

-- +goose Down
ALTER TABLE notifications DROP COLUMN failed_at;
DROP TABLE system_events;
DROP TABLE runners;

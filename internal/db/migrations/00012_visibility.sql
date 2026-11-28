-- +goose Up
-- Visibility for the System health page. Versions 00010 and 00011 belong to
-- other branches and are not part of this change; gaps are fine.
--
-- One row per connection (kind and ref): when it last worked and its last
-- error code. ref is the 12-digit account id for aws, the GitHub owner for
-- github, and the literal slack for Slack. Only the error code is kept, never
-- the message, so no credential or AWS message is stored here.
CREATE TABLE connection_checks (
    kind            text        NOT NULL CHECK (kind IN ('aws', 'github', 'slack')),
    ref             text        NOT NULL CHECK (ref <> ''),
    last_ok_at      timestamptz,
    last_error_at   timestamptz,
    last_error_code text        NOT NULL DEFAULT '',
    PRIMARY KEY (kind, ref)
);

-- +goose Down
DROP TABLE connection_checks;

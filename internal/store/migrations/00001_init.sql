-- +goose Up
CREATE TABLE sessions (
    id             TEXT    PRIMARY KEY,
    module         TEXT    NOT NULL,
    module_version INTEGER NOT NULL,
    tier           TEXT    NOT NULL,
    started_at     TEXT    NOT NULL,
    ended_at       TEXT
) STRICT;

CREATE TABLE turns (
    id         TEXT    PRIMARY KEY,
    session_id TEXT    NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    role       TEXT    NOT NULL CHECK (role IN ('user', 'assistant')),
    content    TEXT    NOT NULL,
    tokens_in  INTEGER NOT NULL DEFAULT 0,
    tokens_out INTEGER NOT NULL DEFAULT 0,
    model      TEXT    NOT NULL DEFAULT '',
    latency_ms INTEGER NOT NULL DEFAULT 0,
    created_at TEXT    NOT NULL
) STRICT;

CREATE INDEX turns_session_created ON turns (session_id, created_at);

-- +goose Down
DROP TABLE turns;
DROP TABLE sessions;

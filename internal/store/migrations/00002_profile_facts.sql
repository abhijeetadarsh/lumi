-- +goose Up
-- Durable facts the assistant carries between sessions. One row per fact,
-- rewritten rather than appended, so a correction replaces the old claim.
CREATE TABLE profile_facts (
    id         TEXT    PRIMARY KEY,
    fact       TEXT    NOT NULL,
    key        TEXT    NOT NULL DEFAULT '',
    confidence INTEGER NOT NULL DEFAULT 100,
    hits       INTEGER NOT NULL DEFAULT 0,
    source_session_id TEXT,
    created_at TEXT    NOT NULL,
    updated_at TEXT    NOT NULL
) STRICT;

-- Facts are keyed so a later session can supersede an earlier one instead of
-- accumulating contradictory rows ("works in Go" vs "switched to Rust").
CREATE UNIQUE INDEX profile_facts_key ON profile_facts (key) WHERE key != '';
CREATE INDEX profile_facts_updated ON profile_facts (updated_at);

-- One row per completed reflection, so a session is never reflected twice.
CREATE TABLE reflections (
    session_id TEXT PRIMARY KEY REFERENCES sessions (id) ON DELETE CASCADE,
    facts_kept INTEGER NOT NULL DEFAULT 0,
    created_at TEXT    NOT NULL
) STRICT;

-- +goose Down
DROP TABLE reflections;
DROP TABLE profile_facts;
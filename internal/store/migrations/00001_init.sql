-- +goose Up
CREATE TABLE config_dirs (
    path                TEXT PRIMARY KEY,
    label               TEXT NOT NULL,
    has_agentsmemory    INTEGER NOT NULL,
    has_quality_harness INTEGER NOT NULL,
    scanned_at          TEXT NOT NULL,
    qh_installed_at     TEXT NOT NULL
);

CREATE TABLE sessions (
    session_id      TEXT PRIMARY KEY,
    config_dir      TEXT NOT NULL,
    project_slug    TEXT NOT NULL,
    cwd             TEXT NOT NULL,
    transcript_path TEXT NOT NULL,
    claude_version  TEXT NOT NULL,
    started_at      TEXT NOT NULL,
    ended_at        TEXT NOT NULL,
    user_turns      INTEGER NOT NULL,
    tool_calls      INTEGER NOT NULL,
    subagents       INTEGER NOT NULL,
    am_calls        INTEGER NOT NULL,
    mrw_calls       INTEGER NOT NULL,
    qh_calls        INTEGER NOT NULL,
    cohort          TEXT NOT NULL
);

CREATE TABLE requests (
    request_key           TEXT PRIMARY KEY,
    session_id            TEXT NOT NULL,
    message_id            TEXT NOT NULL,
    iteration             INTEGER NOT NULL,
    request_id            TEXT NOT NULL,
    at                    TEXT NOT NULL,
    model                 TEXT NOT NULL,
    input_tokens          INTEGER NOT NULL,
    output_tokens         INTEGER NOT NULL,
    cache_read_tokens     INTEGER NOT NULL,
    cache_write_tokens    INTEGER NOT NULL,
    cache_write_5m_tokens INTEGER NOT NULL,
    cache_write_1h_tokens INTEGER NOT NULL,
    ttl_known             INTEGER NOT NULL,
    is_subagent           INTEGER NOT NULL,
    agent_id              TEXT NOT NULL,
    tool_uses             INTEGER NOT NULL
);
CREATE INDEX requests_by_session ON requests (session_id);

CREATE TABLE cost_references (
    config_dir         TEXT NOT NULL,
    cwd                TEXT NOT NULL,
    session_id         TEXT NOT NULL,
    last_cost_usd      REAL NOT NULL,
    input_tokens       INTEGER NOT NULL,
    output_tokens      INTEGER NOT NULL,
    cache_write_tokens INTEGER NOT NULL,
    cache_read_tokens  INTEGER NOT NULL,
    duration_ms        INTEGER NOT NULL,
    PRIMARY KEY (config_dir, cwd)
);

-- +goose Down
DROP TABLE cost_references;
DROP TABLE requests;
DROP TABLE sessions;
DROP TABLE config_dirs;

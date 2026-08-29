CREATE TABLE jobs (
    id                  TEXT PRIMARY KEY,
    idempotency_key     TEXT,
    target              TEXT NOT NULL,
    state               TEXT NOT NULL DEFAULT 'received' CHECK (state IN
                        ('received','resolving','forwarding','awaiting_target','completed','failed')),
    attempts            INTEGER NOT NULL DEFAULT 0,
    max_attempts        INTEGER NOT NULL,
    next_attempt_at     TEXT,               -- NULL = eligible now; doubles as poll schedule
    payload             BLOB NOT NULL,      -- original payload JSON, client api_key stripped
    resolved_payload    BLOB,
    target_job_id       TEXT,               -- id extracted from the target's accept response
    poll_deadline       TEXT,
    target_status       INTEGER,
    target_response     BLOB,               -- small JSON results inline
    result_path         TEXT,               -- large/binary results on disk
    result_content_type TEXT,
    error_code          TEXT,
    error_message       TEXT,
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL,
    completed_at        TEXT
);
CREATE UNIQUE INDEX jobs_idem_uq ON jobs(idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX jobs_queue_ix ON jobs(state, next_attempt_at);
CREATE INDEX jobs_created_ix ON jobs(created_at);

CREATE TABLE job_events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id     TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    from_state TEXT,
    to_state   TEXT NOT NULL,
    detail     TEXT,
    created_at TEXT NOT NULL
);
CREATE INDEX job_events_job_ix ON job_events(job_id);

CREATE TABLE series_cache (
    param_hash     TEXT PRIMARY KEY,
    resolvent_type TEXT NOT NULL,
    response       BLOB NOT NULL,
    created_at     TEXT NOT NULL,
    expires_at     TEXT NOT NULL
);
CREATE INDEX series_cache_exp_ix ON series_cache(expires_at);

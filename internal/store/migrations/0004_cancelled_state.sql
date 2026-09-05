-- tentacron:foreign_keys=off
-- A "cancelled" terminal state. SQLite cannot alter a CHECK constraint, so
-- the jobs table is rebuilt (the documented twelve-step procedure): the
-- directive above makes the runner disable foreign keys around this file,
-- or dropping the old table would cascade into job_events. rowid is copied
-- explicitly so insertion order — the tiebreaker behind list cursors and
-- claim ordering — survives.
CREATE TABLE jobs_new (
    id                  TEXT PRIMARY KEY,
    client              TEXT NOT NULL DEFAULT '',
    idempotency_key     TEXT,
    target              TEXT NOT NULL,
    state               TEXT NOT NULL DEFAULT 'received' CHECK (state IN
                        ('received','resolving','forwarding','awaiting_target','completed','failed','cancelled')),
    attempts            INTEGER NOT NULL DEFAULT 0,
    max_attempts        INTEGER NOT NULL,
    next_attempt_at     TEXT,
    payload             BLOB NOT NULL,
    resolved_payload    BLOB,
    target_job_id       TEXT,
    poll_deadline       TEXT,
    target_status       INTEGER,
    target_response     BLOB,
    result_path         TEXT,
    result_content_type TEXT,
    error_code          TEXT,
    error_message       TEXT,
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL,
    completed_at        TEXT,
    priority            INTEGER NOT NULL DEFAULT 0,
    options             TEXT NOT NULL DEFAULT '{}'
);
INSERT INTO jobs_new (rowid, id, client, idempotency_key, target, state, attempts, max_attempts, next_attempt_at,
    payload, resolved_payload, target_job_id, poll_deadline, target_status, target_response, result_path,
    result_content_type, error_code, error_message, created_at, updated_at, completed_at, priority, options)
SELECT rowid, id, client, idempotency_key, target, state, attempts, max_attempts, next_attempt_at,
    payload, resolved_payload, target_job_id, poll_deadline, target_status, target_response, result_path,
    result_content_type, error_code, error_message, created_at, updated_at, completed_at, priority, options
FROM jobs;
DROP TABLE jobs;
ALTER TABLE jobs_new RENAME TO jobs;
CREATE UNIQUE INDEX jobs_idem_uq ON jobs(client, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX jobs_queue_ix ON jobs(state, next_attempt_at);
CREATE INDEX jobs_created_ix ON jobs(created_at);
CREATE INDEX jobs_claim_ix ON jobs(state, priority, created_at);

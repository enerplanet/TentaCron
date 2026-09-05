-- Recurring runs: a schedule materialises into ordinary jobs, one per due
-- time, under the idempotency key schedule:<id>:<next_run_at>, so a
-- restart or a duplicate tick can never produce a second run.
CREATE TABLE schedules (
    id          TEXT PRIMARY KEY,
    client      TEXT NOT NULL,
    target      TEXT NOT NULL,
    payload     BLOB NOT NULL,
    cron        TEXT NOT NULL,
    timezone    TEXT NOT NULL DEFAULT 'UTC',
    priority    INTEGER NOT NULL DEFAULT 0,
    options     TEXT NOT NULL DEFAULT '{}',
    enabled     INTEGER NOT NULL DEFAULT 1,
    next_run_at TEXT,
    last_run_at TEXT,
    last_job_id TEXT,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);
CREATE INDEX schedules_due_ix ON schedules(enabled, next_run_at);
CREATE INDEX schedules_client_ix ON schedules(client, created_at);

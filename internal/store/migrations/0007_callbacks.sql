-- Completion callbacks: the URL a request asked to be notified at, and one
-- delivery row per terminal request, created in the terminal transition's
-- own transaction so a callback can never be lost or duplicated.
ALTER TABLE jobs ADD COLUMN callback_url TEXT;
CREATE TABLE callback_deliveries (
    job_id          TEXT PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
    url             TEXT NOT NULL,
    event           TEXT NOT NULL,
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TEXT,
    delivered_at    TEXT,
    last_status     INTEGER,
    last_error      TEXT,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);
CREATE INDEX callback_deliveries_due_ix ON callback_deliveries(delivered_at, next_attempt_at);

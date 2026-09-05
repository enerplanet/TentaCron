-- Scheduling: a priority per job (-10..10, default 0) and, per client, when
-- it was last handed a job. Claims order by priority, then least recently
-- served client, then age; the index serves the candidate query.
ALTER TABLE jobs ADD COLUMN priority INTEGER NOT NULL DEFAULT 0;
CREATE INDEX jobs_claim_ix ON jobs(state, priority, created_at);

CREATE TABLE client_claims (
    client          TEXT PRIMARY KEY,
    last_claimed_at TEXT NOT NULL
);

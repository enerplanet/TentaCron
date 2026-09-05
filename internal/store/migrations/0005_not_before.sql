-- Delayed runs: the earliest time a job may be claimed, echoed by the API
-- for the job's whole life. next_attempt_at seeds from it at insert and is
-- cleared on claim, which is why it cannot serve as the record.
ALTER TABLE jobs ADD COLUMN not_before TEXT;

-- Idempotent schedule creation: a client's Idempotency-Key names one
-- schedule, so a retried creation replays it instead of doubling the run
-- cadence. NULL for schedules created without a key.
ALTER TABLE schedules ADD COLUMN idempotency_key TEXT;
CREATE UNIQUE INDEX schedules_idem_uq ON schedules(client, idempotency_key) WHERE idempotency_key IS NOT NULL;

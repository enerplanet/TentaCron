-- Per-key queue caps count a client's non-terminal jobs on every submission;
-- the index makes that one range scan instead of a table walk under load.
CREATE INDEX jobs_client_state_ix ON jobs(client, state);

-- Retention scans select terminal jobs by completion time; without an index
-- every housekeeping sweep walked all terminal rows.
CREATE INDEX jobs_terminal_ix ON jobs(state, completed_at);

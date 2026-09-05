-- Per-job processing options as JSON (e.g. {"cache":"refresh"}); an empty
-- object means every default.
ALTER TABLE jobs ADD COLUMN options TEXT NOT NULL DEFAULT '{}';

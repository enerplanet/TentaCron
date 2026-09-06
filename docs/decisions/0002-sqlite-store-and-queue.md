# ADR-0002: SQLite is the store and the job queue; one instance

- **Status:** accepted
- **Date:** 2026-08-29

## Context

Every accepted request must survive a restart, be auditable afterwards, and
be processed exactly once by one worker. The expected load is tens to a few
thousand jobs per day from two clients, not a stream that needs horizontal
scaling. Adding Postgres or a message broker would double the operational
surface of a service whose whole point is to be simple to run.

## Decision

A single SQLite database holds the jobs table (which is also the queue), the
audit trail and the resolved-series cache. Claiming a job is a compare-and-
swap `UPDATE`; transactions open in immediate mode under WAL so concurrent
workers never deadlock. TentaCron runs as exactly one instance.

## Consequences

- No external infrastructure; a deployment is the binary, a config file and
  a volume. Backups are a file copy.
- Horizontal scaling and high availability are out of scope. Multi-instance
  would require a shared database with row locking and leader election for
  the sweeper; that is a separate decision, to be taken only when metrics
  show a single instance is the limit.
- Large-batch operations must respect SQLite's limits (bound parameters per
  statement, one writer at a time); housekeeping works in batches.
- The database file is the source of truth and is safe to inspect with the
  `sqlite3` CLI while the service runs.

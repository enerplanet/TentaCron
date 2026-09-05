# ADR-0006: Postgres is a gated option, not a roadmap item

- **Status:** accepted
- **Date:** 2026-09-06

## Context

ADR-0002 made SQLite the store and the queue and fixed tentacron at one
instance. Since then the service gained everything a second instance would
have to coordinate: fair claiming across clients, a scheduler that
materialises due runs, a callback deliverer, long-poll wake-ups through an
in-process hub, and housekeeping sweeps. Each of these relies on "there is
exactly one process" for its correctness, and each is tested against that
assumption.

The question "when do we move to Postgres" comes up whenever a client
imagines a busier future. Answering it with a date would commit effort to a
problem no measurement has shown. Answering it with "never" would be
dishonest: the single instance is a ceiling.

## Decision

Postgres is not scheduled. It is revisited only when one of these gates is
hit, measured on the production instance with the metrics that exist today:

1. **Throughput.** `tentacron_jobs_total` sustains more than one job per
   second over a day, or the queue depth (`tentacron_jobs_in_state`,
   `received`) grows for more than an hour while every worker is busy.
2. **Availability.** An operator requirement for no planned downtime — the
   current restart takes seconds and in-flight jobs resume, which has been
   sufficient.
3. **Multi-region or multi-tenant isolation** that needs more than one
   process for reasons other than load.

Until a gate is hit, single-instance tuning is preferred: more workers,
larger `resolvent_concurrency`, a faster disk, splitting deployments per
client. When a gate is hit, the shape of the change is already known and
recorded here so it is not redesigned under pressure:

- Extract a `store.Store` interface from the concrete type; the API, worker,
  scheduler and deliverer already depend on a narrow method set.
- Keep SQLite as the default implementation; add a pgx implementation whose
  claims use `SELECT … FOR UPDATE SKIP LOCKED` in place of the compare-and-
  set `UPDATE`, whose scheduler and sweeper run under an advisory lock
  (leader election), and whose schedule advance keeps its compare-and-set
  on `next_run_at`.
- Replace the in-process notify hub with `LISTEN`/`NOTIFY` so long polls and
  the deliverer wake on any instance.
- Per-instance result directories become a shared object store or a
  per-instance `href` prefix; the API must be able to serve a result stored
  by another instance.
- The golden suite runs unchanged against both implementations; the
  transcripts are the compatibility contract.

## Consequences

- No engineering time goes to a second store until a measured gate says so;
  the metrics that decide it are already exported.
- New features keep the single-process assumption explicit and local
  (compare-and-set on rows, one loop per concern), so the later extraction
  is a store change and not a redesign.
- Anyone proposing Postgres cites the gate and the measurement; anyone
  opposing it cites this record instead of re-arguing ADR-0002.
- Superseding this record means implementing the shape above and marking
  ADR-0002 superseded as well.

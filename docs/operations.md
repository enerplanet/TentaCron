# Operations

## Deployment

Releases are tagged `vX.Y.Z`. Every tag publishes static binaries with
checksums on the GitHub release and a multi-architecture image at
`ghcr.io/enerplanet/tentacron`, built from the root `Dockerfile`: a
distroless base, a non-root user, and `/data` as the only writable path.

- **Single instance.** SQLite doubles as the job queue, so run exactly one
  replica. Horizontal scaling would mean moving to Postgres; the measured
  gates that would trigger it, and its shape, are recorded in
  [ADR-0006](decisions/0006-postgres-decision-gate.md).
- **TLS at the proxy.** TentaCron listens on plain HTTP; put it behind a
  reverse proxy (Caddy, nginx, Traefik) for TLS and rate limiting.
- **Secrets via environment.** The YAML config references `${VARS}`; supply
  them through your orchestrator's secret mechanism. Startup fails fast if a
  referenced variable is unset, and the reference config needs
  `TENTACRON_KEY_FRONTEND`, `TENTACRON_KEY_BATCH`, `MEME_API_KEY`,
  `BUEM_API_KEY`, `PV1_API_KEY`, `WIND_API_KEY`, `WEATHER_API_KEY` and
  `IGNIS_API_KEY`.
- **Result sizes.** JSON answers from resources, targets and status polls
  are read into memory under `upstream.max_response_bytes` (10 MiB). A
  poll-mode result (a MEME bundle) streams straight into `results_dir`
  under `storage.max_result_bytes` (1 GiB) and is served with range support,
  so memory stays flat however large the bundle. The download has two
  bounds: the target's `timeout` between two reads, so a stalled connection
  fails as fast as any other call, and `response.poll.result_timeout` (10
  minutes by default) for the whole of it. A poll tick is not a processing
  attempt, so `job_timeout` never cuts a download off.
- **Persistent storage** for `storage.path` and `storage.results_dir`. In
  the image both belong under `/data` — the default relative paths
  (`./data/tentacron.db`, `./data/results`) resolve there — so mount a
  volume at `/data`. A bind-mounted host directory must be writable by
  uid 65532 (the distroless `nonroot` user).

```bash
# Container: pinned release image, read-only config, named volume for /data
docker run -d --name tentacron -p 8080:8080 \
  -v /etc/tentacron/config.yaml:/etc/tentacron/config.yaml:ro \
  -v tentacron-data:/data \
  --env-file /etc/tentacron/secrets.env \
  ghcr.io/enerplanet/tentacron:0.2.0-alpha

# Compose: the same service wired in environment/ (settings from .env.prod)
make -C environment run-release ENV=prod

# Host: a release binary from the GitHub release, or a local build
make build && ./bin/tentacron -config /etc/tentacron/config.yaml
```

The development image (`make -C environment run`) bind-mounts the sources,
compiles at start and runs as root; it is for local work, not deployment.

Before pointing production traffic at a config, validate it exactly as the
service would load it, then run one real request through it with the
env-gated live tier (`make live`, see
[test/README.md](https://github.com/enerplanet/tentacron/blob/main/test/README.md)):

```bash
tentacron validate -config /etc/tentacron/config.yaml   # or: make validate CONFIG=…
docker run --rm -v /etc/tentacron/config.yaml:/etc/tentacron/config.yaml:ro \
  --env-file /etc/tentacron/secrets.env ghcr.io/enerplanet/tentacron:0.2.0-alpha validate
```

`validate` interpolates `${VARS}`, applies defaults and runs every validation
rule, printing a summary of targets and resolvents (never credentials) on
success and every problem at once on failure. Exit codes: 0 valid, 1
invalid, 2 usage error. CI runs it against both reference configs.

## Command line

| Command | Purpose |
|---|---|
| `tentacron [serve] -config FILE` | Start the service; `serve` is the default, so `tentacron -config FILE` still works. |
| `tentacron validate -config FILE` | Load and validate a configuration without starting anything. |
| `tentacron backup -config FILE DEST` | Write a consistent, compacted copy of the database to `DEST` (safe while the service runs; refuses to overwrite). |
| `tentacron version` | Print the build version and Go version. |

## Health and readiness

- `GET /healthz` — process liveness.
- `GET /readyz` — database reachable and migrations applied. Wire this into
  your orchestrator's readiness probe.
- `GET /version` — which build answers (version, Go version, VCS revision).

## Logs

Structured JSON on stdout (`log/slog`), at `server.log_level` and above
(`info` by default; `debug` adds the per-tick poll status and dropped
duplicate outcomes). One line per HTTP request
(`request_id` echoed from/into `X-Request-ID`, `method`, `path`, `status`,
`duration_ms`; probes of `/healthz`, `/readyz` and `/version` at `debug`
so an orchestrator does not drown the log, a request that panicked with
its `500`) and one per job transition (`job_id`, `target`, `attempt`,
`client` — the name of the API key used — `error`). Substitution warnings
(e.g. a series without `"type": "time-series"`) are logged at `WARN` and do
not fail the job. API keys are never logged; upstream error bodies have every
configured downstream credential redacted and are then truncated to 512
bytes — a resource or target API echoing the request back in an error cannot
leak a key into logs, the audit store, or API responses.

## Reloading configuration

Send `SIGHUP` to re-read the configuration file without a restart:

```sh
kill -HUP "$(pidof tentacron)"          # bare process
docker kill --signal=HUP tentacron      # container
```

The file is parsed and validated first; a broken file is logged as
`configuration reload failed; keeping the running configuration` and the
service keeps running on what it had. A good file is swapped in atomically
and logged as `configuration reloaded` with the file's hash (also logged at
startup as `configuration loaded`), so the running configuration can always
be matched to a file revision.

Hot, effective for the next request or job: `auth` (keys, roles, limits),
`targets`, `resolvents` (including cache TTLs and ignore fields),
`callbacks`, `cache.default_ttl`, `upstream.max_response_bytes`,
`server.log_level`. A job already in
flight keeps the configuration it started with. Read once at startup, so a
change is logged under `restart_required` and needs a restart: `server`
(address, timeouts, body limit, CORS, metrics address), `storage`,
`worker.count`, `worker.poll_interval`, `worker.scheduler_interval`,
`cache.cleanup_interval`.

**Rotating an API key** without a gap: set the new value as `key`, move the
old one to `previous_key` next to it, reload; both are accepted while the
clients switch. Then remove `previous_key` and reload again. The audit trail
and logs keep naming the key by its `name`, which does not change.

## Metrics

Set `server.metrics_addr` (for example `127.0.0.1:9090`) and tentacron
serves Prometheus metrics on `/metrics` on that second listener and nothing
else there — the public API listener never exposes them. `GET /version` on
the API listener reports the build (version, Go version, VCS revision and
time) without authentication, and `/healthz` carries the version too.

| Metric | Labels | Meaning |
|---|---|---|
| `tentacron_jobs_total` | `target`, `outcome` | Jobs that reached `completed` or `failed`. |
| `tentacron_job_failures_total` | `target`, `code` | Failed jobs by job error code (`target_timeout`, `max_attempts_exceeded`, …). |
| `tentacron_jobs_in_state` | `state` | Queue depth per state, read from the store at scrape time. |
| `tentacron_upstream_requests_total` | `kind`, `name`, `class` | Outbound calls by kind (`resource`, `target`, `poll`, `result`), target or resolvent name, and status class (`2xx`…`5xx`, `error` for transport failures). |
| `tentacron_upstream_request_duration_seconds` | `kind`, `name` | Outbound call latency histogram. |
| `tentacron_series_cache_lookups_total` | `result` | Series cache `hit` / `miss`. |
| `tentacron_schedule_runs_total` | `target` | Runs materialised from schedules. |
| `tentacron_callback_deliveries_total` | `outcome` | Completion-callback attempts: `delivered`, `retry`, `failed`. |
| `tentacron_metrics_scrape_errors_total` | – | Scrapes on which the queue depth could not be read. |

Plus the standard Go runtime and process collectors. A minimal scrape
configuration and three alerts worth having:

```yaml
scrape_configs:
  - job_name: tentacron
    static_configs: [{ targets: ["tentacron.internal:9090"] }]
```

- **Failure rate:** `sum(rate(tentacron_jobs_total{outcome="failed"}[15m])) / sum(rate(tentacron_jobs_total[15m])) > 0.1`
- **Queue not draining:** `tentacron_jobs_in_state{state="received"} > 50` for 15 minutes
- **Targets timing out:** `increase(tentacron_job_failures_total{code="target_timeout"}[1h]) > 0`

## Inspecting state

The SQLite file is the source of truth and safe to read while the service
runs (WAL mode):

```bash
sqlite3 data/tentacron.db "select state, count(*) from jobs group by 1"
sqlite3 data/tentacron.db "select from_state, to_state, detail, created_at
                           from job_events where job_id = '<id>' order by id"
sqlite3 data/tentacron.db "select resolvent_type, count(*), min(expires_at)
                           from series_cache group by 1"
```

`jobs.payload` holds the request as accepted (client key stripped),
`jobs.resolved_payload` what was forwarded. The same audit trail is served
by `GET /v1/requests/{id}/events`, and `GET /v1/requests?state=failed` lists
recent failures, so most inspection needs no database access.

## Failure handling

| Situation | Behaviour / job error code |
|---|---|
| Resource/target 5xx, 429, timeout, network error | Retry with capped exponential backoff, up to `worker.max_attempts`; then `max_attempts_exceeded`. |
| `worker.job_timeout` (or the target's `job_timeout`) hit, or shutdown during an attempt | Treated as transient: requeued for a clean retry. |
| Forward cut off by a deadline on a target with `retry_on_timeout: false` | `target_timeout` after that single call; the target's work is never re-submitted. |
| Resource/target other non-2xx (4xx, redirects) or oversized response | Fail immediately (`resource_error` / `target_error`). |
| Resource response not a JSON object, `null`, or `response_path` missing | `invalid_resource_response`. |
| Unknown resolvent type | Fail before any HTTP call (`unknown_resolvent`). |
| Target URL `{field}` placeholder without a string/number payload field | `target_error`. |
| Target removed from the config while jobs are queued | `unknown_target`. |
| Accept response without a usable job id (poll mode) | `target_error`. |
| Status poll fails permanently (4xx, status path missing) | `target_error`. |
| Target job reports a `failed_values` status | `target_job_failed`. |
| Target job not done before `poll.timeout` (or status endpoint unreachable past it) | `target_timeout`. |
| Result fetch fails transiently | Retried next poll tick; `target_error` once the poll deadline has passed. |
| Panic or result file write failure | `internal`. |
| Client `DELETE /v1/requests/{id}` | `received` and `awaiting_target` requests end `cancelled` (the target is told to stop when it offers a `cancel_url_template`); in-flight and finished requests answer `409`. |
| Process restart | `resolving`/`forwarding` jobs are requeued and reprocessed from the original payload (series cache makes this cheap); `awaiting_target` jobs resume polling within one poll interval, spread over it, without re-submitting. |
| Shutdown (SIGTERM) | Long-polls answer at once with the current state, HTTP drains within `server.shutdown_grace`, in-flight jobs park back to `received`, pending target-cancel notifications are waited for. A second signal terminates the process immediately. |

Failed jobs stay queryable until `storage.retention` expires. To re-run a
failed request, resubmit it — identical resolvents within the cache TTL are
served from cache.

## Housekeeping

A background sweeper (every `cache.cleanup_interval`):

- removes expired series cache rows;
- rescues jobs stranded in `resolving`/`forwarding` without a schedule (a
  failed bookkeeping write) once they are untouched for twice their
  target's attempt deadline — the target's `job_timeout` when it sets one,
  `worker.job_timeout` otherwise — so a target whose attempts legitimately
  run long is never pulled from under a working worker;
- prunes terminal jobs older than `storage.retention` in batches of 1,000
  until the backlog is drained, deleting each batch's result files before
  its rows so a crash in between never orphans a file. A first sweep after a
  long downtime therefore takes several passes instead of one huge delete;
- compacts the database (incremental vacuum, WAL checkpoint) so the file
  shrinks after pruning.

## Schedules

The scheduler runs inside the worker process and wakes every
`worker.scheduler_interval` (default 30 s): a run is never created before
its due time and at most one interval after it. Time zones come from the
binary's embedded IANA database, so `Europe/Berlin` works in the distroless
image; daylight-saving transitions follow the zone. After downtime a
schedule runs once, not once per missed due time. The runs of a schedule
are ordinary requests: they age out under `storage.retention` like any
other, and `GET /v1/schedules/{id}/runs` shows what is left. Deleting a
schedule stops future runs and leaves existing ones. Runs are counted by
`tentacron_schedule_runs_total{target}`.

## Callbacks

The callback deliverer runs inside the worker process, wakes whenever a
request ends and, for retries, every `worker.poll_interval`. Each delivery
row is created in the terminal transition's own transaction, so a callback
is never lost or sent twice; a crash between the POST and recording its
outcome repeats that one attempt, which is why receivers should treat
`X-Tentacron-Request-Id` as an idempotency key. Delivery rows are deleted
with their request under `storage.retention`. Outcomes are counted by
`tentacron_callback_deliveries_total{outcome=delivered|retry|failed}` and
logged per attempt (`callback delivered`, `callback attempt failed`). A
reload that removes a host from `callbacks.allowed_hosts`, or empties the
list, does not strand pending deliveries: every attempt re-checks the
list, records the reason as `last_error`, and keeps the backoff schedule,
so restoring the host lets the delivery go out and the attempt budget
otherwise ends it as `failed`.

## Backups and storage growth

The SQLite file is the whole state: back it up like any other database.
`tentacron backup -config config.yaml /backups/tentacron-$(date +%F).db`
takes a consistent snapshot with `VACUUM INTO` while the service runs; the
sqlite3 CLI's `.backup` command or a continuous replicator such as
Litestream work as well. Never copy the `.db` file with `cp` while the
service is writing — the WAL sidecar would be missing from the copy.

Result files under `storage.results_dir` are not in the database; back up
the directory alongside it if results must survive a restore.

**Upgrading.** Back up before installing a new release, with the binary
you run now. Schema migrations apply on the new binary's first start and
readiness waits for them; they are additive, so the previous release can
still open a migrated database if a rollback is ever needed, but a backup
taken beforehand is what a restore falls back to. The suite rehearses this
upgrade over a database populated at the previous release's schema.

Databases created by tentacron use SQLite's incremental auto-vacuum, and
every housekeeping sweep hands pages freed by pruning back to the filesystem
and checkpoints the WAL, so the file tracks the live data rather than its
historical peak. A database created before this setting existed keeps
reusing freed pages internally but never shrinks; one manual
`sqlite3 data/tentacron.db 'PRAGMA auto_vacuum=INCREMENTAL; VACUUM;'`
while the service is stopped switches it over.


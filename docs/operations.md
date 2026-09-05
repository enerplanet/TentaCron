# Operations

## Deployment

Releases are tagged `vX.Y.Z`. Every tag publishes static binaries with
checksums on the GitHub release and a multi-architecture image at
`ghcr.io/enerplanet/tentacron`, built from the root `Dockerfile`: a
distroless base, a non-root user, and `/data` as the only writable path.

- **Single instance.** SQLite doubles as the job queue, so run exactly one
  replica. Horizontal scaling would require moving to Postgres/a broker —
  out of scope for v1.
- **TLS at the proxy.** Tentacron listens on plain HTTP; put it behind a
  reverse proxy (Caddy, nginx, Traefik) for TLS and rate limiting.
- **Secrets via environment.** The YAML config references `${VARS}`; supply
  them through your orchestrator's secret mechanism. Startup fails fast if a
  referenced variable is unset, and the reference config needs
  `TENTACRON_KEY_FRONTEND`, `TENTACRON_KEY_BATCH`, `MEME_API_KEY`,
  `BUEM_API_KEY`, `PV1_API_KEY`, `WIND_API_KEY`, `WEATHER_API_KEY` and
  `IGNIS_API_KEY`.
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
  ghcr.io/enerplanet/tentacron:0.1.0

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
  --env-file /etc/tentacron/secrets.env ghcr.io/enerplanet/tentacron:0.1.0 validate
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
| `tentacron version` | Print the build version and Go version. |

## Health and readiness

- `GET /healthz` — process liveness.
- `GET /readyz` — database reachable and migrations applied. Wire this into
  your orchestrator's readiness probe.

## Logs

Structured JSON on stdout (`log/slog`), at `server.log_level` and above
(`info` by default; `debug` adds the per-tick poll status and dropped
duplicate outcomes). One line per HTTP request
(`request_id` echoed from/into `X-Request-ID`, `method`, `path`, `status`,
`duration_ms`) and one per job transition (`job_id`, `target`, `attempt`,
`client` — the name of the API key used — `error`). Substitution warnings
(e.g. a series without `"type": "time-series"`) are logged at `WARN` and do
not fail the job. API keys are never logged; upstream error bodies have every
configured downstream credential redacted and are then truncated to 512
bytes — a resource or target API echoing the request back in an error cannot
leak a key into logs, the audit store, or API responses.

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
| Process restart | `resolving`/`forwarding` jobs are requeued and reprocessed from the original payload (series cache makes this cheap); `awaiting_target` jobs resume polling without re-submitting. |
| Shutdown (SIGTERM) | HTTP drains within `server.shutdown_grace`; in-flight jobs park back to `received`. |

Failed jobs stay queryable until `storage.retention` expires. To re-run a
failed request, resubmit it — identical resolvents within the cache TTL are
served from cache.

## Housekeeping

A background sweeper (every `cache.cleanup_interval`):

- removes expired series cache rows;
- rescues jobs stranded in `resolving`/`forwarding` without a schedule (a
  failed bookkeeping write) once they are untouched for twice
  `worker.job_timeout`;
- prunes terminal jobs older than `storage.retention` in batches of 1,000
  until the backlog is drained, deleting each batch's result files before
  its rows so a crash in between never orphans a file. A first sweep after a
  long downtime therefore takes several passes instead of one huge delete.

## Roadmap notes (v2)

- Prometheus `/metrics` (job states, upstream latencies, cache hit ratio).
- Per-type `cache_ignore_fields` so cosmetic fields (e.g. `name`) don't split
  cache entries.
- Config hot-reload / key rotation without restart.

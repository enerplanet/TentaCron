# Operations

## Deployment

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
- **Persistent storage** for `storage.path` and `storage.results_dir`
  (the containerized setup writes both under the bind-mounted repo's `data/`).

```bash
make -C environment build ENV=prod   # containerized (see environment/README.md)
make -C environment run   ENV=prod   # publishes on :80, settings from environment/.env.prod
# or directly on the host:
make build && ./bin/tentacron -config /etc/tentacron/config.yaml
```

Before pointing production traffic at a config, run one real request through
it with the env-gated live tier (`make live`, see
[test/README.md](https://github.com/enerplanet/tentacron/blob/main/test/README.md)).

## Health and readiness

- `GET /healthz` — process liveness.
- `GET /readyz` — database reachable and migrations applied. Wire this into
  your orchestrator's readiness probe.

## Logs

Structured JSON on stdout (`log/slog`). One line per HTTP request
(`request_id` echoed from/into `X-Request-ID`, `method`, `path`, `status`,
`duration_ms`) and one per job transition (`job_id`, `target`, `attempt`,
`client` — the name of the API key used — `error`). Substitution warnings
(e.g. a series without `"type": "time-series"`) are logged at `WARN` and do
not fail the job. API keys are never logged; upstream error bodies have every
configured downstream credential redacted and are then truncated to 512
bytes — a resource or target API echoing the request back in an error cannot
leak a key into logs, the audit store, or API responses.

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
`jobs.resolved_payload` what was forwarded. Or via the API:
`GET /v1/requests?state=failed`.

## Failure handling

| Situation | Behaviour / job error code |
|---|---|
| Resource/target 5xx, 429, timeout, network error | Retry with capped exponential backoff, up to `worker.max_attempts`; then `max_attempts_exceeded`. |
| `worker.job_timeout` hit or shutdown during an attempt | Treated as transient: requeued for a clean retry. |
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
- prunes terminal jobs older than `storage.retention`, deleting their result
  files before their rows so a crash in between never orphans a file.

## Roadmap notes (v2)

- Prometheus `/metrics` (job states, upstream latencies, cache hit ratio).
- Per-type `cache_ignore_fields` so cosmetic fields (e.g. `name`) don't split
  cache entries.
- Config hot-reload / key rotation without restart.

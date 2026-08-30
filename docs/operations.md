# Operations

## Deployment

- **Single instance.** SQLite doubles as the job queue, so run exactly one
  replica. Horizontal scaling would require moving to Postgres/a broker —
  out of scope for v1.
- **TLS at the proxy.** Tentacron listens on plain HTTP; put it behind a
  reverse proxy (Caddy, nginx, Traefik) for TLS and rate limiting.
- **Secrets via environment.** The YAML config references `${VARS}`; supply
  them through your orchestrator's secret mechanism. Startup fails fast if a
  referenced variable is unset.
- **Persistent volume** for `storage.path` and `storage.results_dir`
  (`/data` in the container image).

```bash
docker compose up --build          # local
# or
make build && ./bin/tentacron -config /etc/tentacron/config.yaml
```

## Health and readiness

- `GET /healthz` — process liveness.
- `GET /readyz` — database reachable and migrations applied. Wire this into
  your orchestrator's readiness probe.

## Logs

Structured JSON on stdout (`log/slog`). Useful fields: `request_id` (echoed
from/into `X-Request-ID`), `job_id`, `target`, `attempt`, `client` (the name
of the API key used), `error`. API keys are never logged; upstream error
bodies are truncated to 512 bytes.

## Inspecting state

The SQLite file is the source of truth and safe to read while the service
runs (WAL mode):

```bash
sqlite3 data/tentacron.db "select state, count(*) from jobs group by 1"
sqlite3 data/tentacron.db "select from_state, to_state, detail, created_at
                           from job_events where job_id = '<id>' order by id"
```

Or via the API: `GET /v1/requests?state=failed`.

## Failure handling

| Situation | Behaviour |
|---|---|
| Resource/target 5xx, 429, timeout, network error | Retry with capped exponential backoff, up to `worker.max_attempts`. |
| Resource/target 4xx | Fail immediately (`resource_error` / `target_error`). |
| Unknown resolvent type | Fail before any HTTP call (`unknown_resolvent`). |
| Target job reports failure | `target_job_failed`. |
| Target job not done before `poll.timeout` | `target_timeout`. |
| Process restart | `resolving`/`forwarding` jobs are requeued and reprocessed from the original payload (series cache makes this cheap); `awaiting_target` jobs resume polling without re-submitting. |
| Shutdown (SIGTERM) | HTTP drains within `server.shutdown_grace`; in-flight jobs park back to `received`. |

Failed jobs stay queryable until `storage.retention` expires. To re-run a
failed request, resubmit it — identical resolvents within the cache TTL are
served from cache.

## Housekeeping

A background sweeper (every `cache.cleanup_interval`) removes expired series
cache rows and prunes terminal jobs older than `storage.retention`, including
their result files.

## Roadmap notes (v2)

- Prometheus `/metrics` (job states, upstream latencies, cache hit ratio).
- Per-type `cache_ignore_fields` so cosmetic fields (e.g. `name`) don't split
  cache entries.
- Config hot-reload / key rotation without restart.

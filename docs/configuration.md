# Configuration

Tentacron reads one YAML file (`-config` flag). `${VAR}` references anywhere
in the file are interpolated from the environment at startup; referencing an
unset variable aborts startup so the service never runs with empty
credentials. `$$` escapes a literal `$`. Unknown keys are rejected.

See [`config.example.yaml`](https://github.com/enerplanet/tentacron/blob/main/config.example.yaml)
for a complete annotated example.

## server

| Key | Default | Description |
|---|---|---|
| `addr` | `:8080` | Listen address. |
| `read_timeout` | `10s` | HTTP read/read-header timeout. |
| `write_timeout` | `30s` | HTTP write timeout. |
| `shutdown_grace` | `20s` | Drain window on SIGTERM/SIGINT. |
| `max_body_bytes` | `10485760` | Caps inbound request bodies **and** upstream response bodies. |

## auth

```yaml
auth:
  api_keys:
    - name: frontend            # names make keys individually revocable
      key: "${TENTACRON_KEY_FRONTEND}"
```

At least one key is required. `POST` authenticates via the body's `api_key`;
`GET` endpoints via the `X-API-Key` header.

## storage

| Key | Default | Description |
|---|---|---|
| `path` | `./data/tentacron.db` | SQLite database file. |
| `results_dir` | `./data/results` | Large/binary target results. |
| `retention` | `720h` | Completed/failed jobs (and their result files) are pruned after this. |

## worker

| Key | Default | Description |
|---|---|---|
| `count` | `4` | Concurrent jobs. |
| `resolvent_concurrency` | `4` | Per-job fan-out bound to resource APIs. |
| `poll_interval` | `2s` | Queue poll cadence (picks up scheduled retries). |
| `max_attempts` | `5` | Processing attempts before `max_attempts_exceeded`. |
| `backoff_base` / `backoff_max` | `2s` / `60s` | Exponential backoff bounds (±20 % jitter). |
| `job_timeout` | `5m` | Deadline per processing attempt (resolution + forwarding). |

## cache

| Key | Default | Description |
|---|---|---|
| `default_ttl` | `6h` | Reuse window for resolved series without a per-type TTL. |
| `cleanup_interval` | `15m` | Sweep cadence for expired cache rows and retention pruning. |

Identical resolvent objects (same type + parameters, order-independent) share
one cache entry — repeated requests don't re-hit the resource APIs.

## targets

One entry per downstream workflow; the request's `target` field selects it.

```yaml
targets:
  meme:
    url: "https://meme.example.com/simulate?target=all"
    method: POST                        # GET | POST | PUT | PATCH
    api_key: "${MEME_API_KEY}"
    api_key_inject: body_field          # none | body_field | header
    api_key_field: api_key              # for body_field
    # api_key_header: X-API-Key         # for header
    timeout: 60s
    timeseries_path: "model.timeseries" # where resolvents live in the payload
    response:
      mode: poll                        # direct | poll
      poll:
        id_json_path: "job_id"          # job id in the accept response
        url_template: "https://meme.example.com/jobs/{id}"
        # result_url_template defaults to url_template
        status_json_path: "status"
        done_values: [done, completed]
        failed_values: [failed, error]
        interval: 10s
        timeout: 30m                    # poll deadline -> target_timeout
```

Notes:

- `timeseries_path` is a dot-separated path into the payload
  (default `time-series`). MEME spells its registry `model.timeseries`.
- `response.mode: direct` completes the job with the target's immediate
  response. `poll` extracts the target's job id, polls until a
  `done_values`/`failed_values` status appears, then fetches and stores the
  result.
- The target's `api_key` is injected into outbound requests only — stored
  payloads stay credential-free.

## resolvents

One entry per resolvent type. Keys must start with `resolvent-`.

```yaml
resolvents:
  resolvent-pv1:
    url: "https://pvsim.example.com/v1/generate"
    method: POST
    api_key: "${PV1_API_KEY}"
    api_key_header: X-API-Key
    timeout: 30s
    cache_ttl: 24h                      # falls back to cache.default_ttl
```

The whole resolvent object is sent as the request body to this URL; the
response must be a JSON object and is used verbatim as the time series.

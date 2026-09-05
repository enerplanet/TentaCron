# Configuration

Tentacron reads one YAML file (`-config` flag, default `config.yaml`).
`${VAR}` references anywhere in the file are interpolated from the
environment at startup; referencing an unset variable aborts startup so the
service never runs with empty credentials. `$$` escapes a literal `$`.
Unknown keys are rejected, and every validation problem is reported at once.
Durations must be positive (`30s`, `10m`, `24h`), counts and byte limits
positive integers; `worker.backoff_base` may not exceed `worker.backoff_max`
and a poll `interval` must be shorter than its `timeout`. A key that is left
out or set to zero takes its documented default.

See [`config.example.yaml`](https://github.com/enerplanet/tentacron/blob/main/config.example.yaml)
for a complete annotated example covering every integration below.

## server

| Key | Default | Description |
|---|---|---|
| `addr` | `:8080` | Listen address. |
| `read_timeout` | `10s` | HTTP read/read-header timeout. |
| `write_timeout` | `30s` | HTTP write timeout. |
| `shutdown_grace` | `20s` | Drain window on SIGTERM/SIGINT. |
| `max_body_bytes` | `10485760` | Caps inbound request bodies. Upstream responses have their own limit under `upstream`. |
| `metrics_addr` | – (off) | `host:port` of a second listener that serves Prometheus metrics on `/metrics` only. Keep it off the public network; see [Operations → Metrics](operations.md#metrics). |
| `log_level` | `info` | Minimum level written to the structured log: `debug`, `info`, `warn` or `error`. |
| `cors.allowed_origins` | `[]` (off) | Browser origins allowed to call the API directly, matched exactly (`https://app.example.org`); no wildcards. Preflights are answered and `X-Request-ID`, `Content-Disposition` and the range headers are exposed. A key held in a browser is visible to its users; prefer a backend-for-frontend that keeps the key server-side where that matters. |

## auth

```yaml
auth:
  api_keys:
    - name: frontend            # names make keys individually revocable
      key: "${TENTACRON_KEY_FRONTEND}"
    - name: batch-runner
      key: "${TENTACRON_KEY_BATCH}"
      max_concurrent: 2         # at most two of its jobs in flight at once
      max_priority: 0           # may not jump the queue
    - name: ops
      key: "${TENTACRON_KEY_OPS}"
      role: admin               # reads every client's requests
```

At least one key is required; names must be unique. Every endpoint reads
the key from the `X-API-Key` header (`POST` still accepts the deprecated
body field `api_key`). `role` is `client` (default: sees only its own
requests) or `admin` (sees all). The name appears in logs, scopes the
client's idempotency keys and is stored on each request.

**Scheduling per key.** Workers claim jobs by priority first, then
round-robin across clients (every client's first due job before any
client's second, the least recently served client first), then age, so one
client's batch never starves another's interactive requests. `max_concurrent` caps how many of a key's jobs may be
in flight (resolving or forwarding) at once — further jobs wait while other
clients' work proceeds; `0` (default) means no ceiling. `max_priority` caps
the `priority` a key may request (`-10`..`10`, default: the full range).

## storage

| Key | Default | Description |
|---|---|---|
| `path` | `./data/tentacron.db` | SQLite database file (parent directory is created). |
| `results_dir` | `./data/results` | Large/binary target results. |
| `retention` | `720h` | Completed/failed jobs (and their result files) are pruned after this. |
| `max_result_bytes` | `1073741824` | Cap for a poll-mode result download (a MEME bundle). Results stream to `results_dir` and never sit in memory; a larger result fails the job with `target_error`. |

## worker

| Key | Default | Description |
|---|---|---|
| `count` | `4` | Concurrent jobs. |
| `resolvent_concurrency` | `4` | Per-job fan-out bound to resolvent backends. |
| `poll_interval` | `2s` | Queue poll cadence (picks up scheduled retries and poll ticks). |
| `max_attempts` | `5` | Processing attempts before `max_attempts_exceeded`. |
| `backoff_base` / `backoff_max` | `2s` / `60s` | Exponential backoff bounds (±20 % jitter). |
| `job_timeout` | `5m` | Deadline per processing attempt (resolution + forwarding); hitting it requeues the job. Must cover every target's `timeout` plus the longest resolvent timeout (see the timeout budget note under targets); a target may override it. |

## upstream

| Key | Default | Description |
|---|---|---|
| `max_response_bytes` | `10485760` | Caps the JSON responses of resource, target and status-poll calls, which are read into memory. Larger responses fail the call permanently. Result downloads are governed by `storage.max_result_bytes` instead. |

## cache

| Key | Default | Description |
|---|---|---|
| `default_ttl` | `6h` | Reuse window for resolved series without a per-type TTL. |
| `cleanup_interval` | `15m` | Sweep cadence for expired cache rows, stuck-job rescue and retention pruning. |

Identical resolvent objects (same type + parameters, key order irrelevant)
share one cache entry — repeated requests don't re-hit the backends. The hash
covers the whole object, so cosmetic fields such as `name` split entries.

## targets

One entry per downstream workflow; the request's `target` field selects it.

| Key | Default | Description |
|---|---|---|
| `url` | required | http(s) URL. May contain `{field}` placeholders (see notes). |
| `method` | `POST` | `GET`, `POST`, `PUT` or `PATCH`. |
| `api_key` | – | Credential for the target; required unless `api_key_inject` is `none`. |
| `api_key_inject` | `header` if `api_key` is set, else `none` | `none`, `body_field` or `header`. |
| `api_key_field` | `api_key` | Top-level field added to the forwarded JSON (`body_field`). |
| `api_key_header` | `X-API-Key` | Header carrying the key (`header`). |
| `timeout` | `60s` | Per outbound call: the forward, each status poll, the result fetch. |
| `job_timeout` | `worker.job_timeout` | Deadline for one processing attempt of this target's jobs. Must cover `timeout` plus the longest resolvent timeout. |
| `max_attempts` | `worker.max_attempts` | Processing attempts for this target's jobs before `max_attempts_exceeded`. |
| `retry_on_timeout` | `true` | What a deadline hit on the forward means: `true` requeues like any transient failure, `false` fails the job with `target_timeout`. |
| `timeseries_path` | `time-series` | Dot-separated path to the resolvent container (or to a single resolvent object); `"."` scans the whole payload. |
| `attach_resolvent` | `true` | Keep the original resolvent object under the substituted series' `resolvent` key. |
| `proxy` | `false` | Hand the payload through unresolved. |
| `response.mode` | `direct` | `direct` or `poll`. |
| `response.poll.id_json_path` | required for `poll` | Dot path to the job id in the accept response. |
| `response.poll.url_template` | required for `poll` | Status URL; must contain `{id}`. |
| `response.poll.result_url_template` | `url_template` | Result URL once the job is done. |
| `response.poll.status_json_path` | required for `poll` | Dot path to the status value in the status response. |
| `response.poll.done_values` | required for `poll` | Status values meaning success. |
| `response.poll.failed_values` | – | Status values meaning failure; must not overlap `done_values`. |
| `response.poll.interval` | `10s` | Time between status polls. |
| `response.poll.timeout` | `30m` | Poll deadline → `target_timeout`. |

An asynchronous target — the verified [meme](https://github.com/enerplanet/meme)
contract (`202` + `{"id": …, "state": "queued"}`, states
`queued|running|succeeded|failed`, a zip bundle as result):

```yaml
targets:
  meme:
    url: "https://meme.example.com/simulate?target=all"
    method: POST
    api_key: "${MEME_API_KEY}"
    api_key_inject: body_field          # meme expects a top-level api_key in the body
    api_key_field: api_key
    timeout: 60s
    timeseries_path: "model.timeseries" # where resolvents live in the payload
    response:
      mode: poll
      poll:
        id_json_path: "id"
        url_template: "https://meme.example.com/jobs/{id}/status"
        status_json_path: "state"
        done_values: [succeeded]
        failed_values: [failed]
        result_url_template: "https://meme.example.com/jobs/{id}"
        interval: 10s
        timeout: 30m
```

A synchronous target — the real BuEM integration via
[buem-gateway](https://github.com/enerplanet/buem-gateway):

```yaml
  buem:
    url: "https://buem-gateway.example.com/api/v1/buem/buildings"
    method: POST
    api_key: "${BUEM_API_KEY}"
    api_key_inject: header
    api_key_header: X-Api-Key           # checked by buem-reverse-proxy
    timeout: 300s                       # the batch simulates in one call
    timeseries_path: "."                # weather sits at the payload root
    attach_resolvent: false             # BuEM's schema gets weather unchanged
    response:
      mode: direct
```

A proxy target — the payload handed through unresolved
([ignis](https://github.com/THD-Spatial-AI/ignis)'s calculate endpoint):

```yaml
  ignis-calculate:
    url: "https://ignis.example.com/api/v1/calculate/{code}"
    method: POST
    api_key: "${IGNIS_API_KEY}"
    api_key_inject: header
    api_key_header: X-Api-Key
    timeout: 60s
    proxy: true
    response:
      mode: direct
```

Notes:

- `timeseries_path` is a dot-separated path into the payload. MEME spells
  its registry `model.timeseries`; the container may be an array or a
  name→object registry. The special value `"."` scans the whole payload —
  for contracts like buem-gateway whose time series (the shared `weather`
  block) sits at the payload root rather than inside a named container.
- `attach_resolvent: false` is for targets whose schema validation rejects
  unknown keys; tentacron's audit store (original payload, resolved payload,
  events) keeps full traceability either way.
- `proxy: true` skips resolution entirely: tentacron contributes auth,
  persistence, the audit trail and retries, and forwards the payload
  untouched — byte-exact when the URL has no placeholders and the key is
  not body-injected. `timeseries_path`/`attach_resolvent` are rejected on a
  proxy target.
- **URL templating** works on any target: `{field}` placeholders
  (`[A-Za-z0-9_]+`) are filled with the path-escaped value of the payload's
  top-level field of that name, which must be a string or number. Consumed
  fields are stripped from the forwarded body, since they address the call
  rather than belong to it; a missing field fails the job with
  `target_error`.
- **Timeout budget.** One processing attempt covers resolution and the
  forward, bounded by `job_timeout` (the target's own, else
  `worker.job_timeout`). Validation refuses a budget shorter than the
  target's `timeout` plus the longest configured resolvent timeout — a
  target-backed resolvent counts with its backing target's `timeout` — because
  a forward that is merely slow would otherwise be cut off by the job
  deadline, classified transient, and the target's work submitted again.
  For synchronous targets whose work is expensive or not idempotent (BuEM's
  batch simulation) set `retry_on_timeout: false`: a deadline hit on the
  forward then fails the job with `target_timeout` after a single call,
  while 5xx and network errors still retry. `max_attempts` per target caps
  every kind of retry for that target.
- `response.mode: direct` completes the job with the target's immediate
  response. `poll` extracts the target's job id from the accept response
  (strings and numbers accepted; the id must match `[A-Za-z0-9._~-]{1,256}`),
  polls `url_template` with `GET` until a `done_values`/`failed_values`
  status appears, then fetches `result_url_template` and stores the result.
  A status outside both sets means "still running".
- The target's `api_key` is injected into outbound requests only — stored
  payloads stay credential-free. Header injection applies to the forward,
  the status polls and the result fetch; `body_field` injection can only
  reach the forward (the polls are bodiless `GET`s), so a status endpoint
  that itself requires a credential needs `header` injection.

## resolvents

One entry per resolvent type. Keys must start with `resolvent-`.

| Key | Default | Description |
|---|---|---|
| `url` | required unless `target` is set | http(s) URL of the resource API; `{field}` placeholders for `GET`. |
| `method` | `POST` | `GET` maps the object onto the URL; `POST`/`PUT`/`PATCH` send it as JSON body. |
| `api_key` | – | Optional credential, sent in `api_key_header`. |
| `api_key_header` | `X-API-Key` | Header carrying the key. |
| `timeout` | `30s` | Per resource call. |
| `cache_ttl` | `cache.default_ttl` | Reuse window for series of this type. |
| `target` | – | A configured **direct-mode** target as backend instead of `url`. |
| `payload_field` | – (whole object) | Resolvent field whose value is sent as the call's payload. |
| `response_path` | – (whole response) | Dot path extracting the series from the response; numeric segments index arrays. |

`method`, `api_key_header` and `timeout` are only defaulted for URL-backed
resolvents; a target-backed resolvent inherits transport settings from its
target. The substituted series should carry `"type": "time-series"` — any
other value is logged as a warning, not treated as an error.

### POST resolvents

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

The whole resolvent object (including `type` and any `name`) — or the object
under `payload_field` — is sent as the JSON request body; the response must
be a JSON object and is used verbatim as the series. `null`, arrays or
malformed bodies fail the job with `invalid_resource_response`.

### GET resolvents (query/path mapping)

With `method: GET` the resolvent object maps onto the request URL instead of
a body — the contract of the verified
[weather](https://github.com/enerplanet/weather),
[city2tabula](https://github.com/THD-Spatial-AI/city2tabula) and
[ignis](https://github.com/THD-Spatial-AI/ignis) APIs:

```yaml
  resolvent-weather:
    url: "https://weather.example.com/v1/weather/point?format=json"
    method: GET
    api_key: "${WEATHER_API_KEY}"
    timeout: 60s
    cache_ttl: 24h

  resolvent-city2tabula:
    url: "https://city2tabula.example.com/api/v1/buildings"
    method: GET                         # no API key per its spec
    response_path: "0"                  # the API returns a list
    cache_ttl: 24h

  resolvent-ignis:
    url: "https://ignis.example.com/api/v1/data/{code}"
    method: GET
    api_key: "${IGNIS_API_KEY}"
    api_key_header: X-Api-Key
    cache_ttl: 168h
```

- `{field}` placeholders in the configured URL are filled from the resolvent
  object (path-escaped) and consumed — e.g. ignis's
  `/api/v1/data/{code}` from `"code": "DE.N.SFH.04.Gen.ReEx.001.001"`;
- **every remaining field becomes a query parameter**, appended to any query
  fixed in the URL (e.g. `?format=json`); parameters are emitted in sorted
  order, so outbound URLs are deterministic. Keep GET resolvent objects to
  the API's parameters — a `name` or comment field would be sent too;
- arrays of scalars join comma-separated (`variables=T,GHI`,
  `osm_ids=123,456` — the convention of those APIs); the `type` field is
  tentacron's marker and never sent; nested objects are an authoring error
  (nest complex data under a POST resolvent instead);
- `response_path` accepts numeric segments to index array responses —
  city2tabula's building list resolves one building via `response_path: "0"`.

### Target-backed resolvents (composition)

A resolvent can name a configured **target** as its backend instead of a raw
URL — tentacron composing its own targets, e.g. a BuEM simulation producing
the heat-demand series of a MEME model:

```yaml
resolvents:
  resolvent-buem:
    target: buem-building                # a configured direct-mode target
    payload_field: payload               # which resolvent field is the nested payload
    response_path: "buem.thermal_load_profile.timeseries"
    cache_ttl: 24h
```

The resolvent object then carries the nested request under `payload_field`:

```json
{ "type": "resolvent-buem", "payload": { "id": "b-1", "buem": { "building": …, "weather": … } } }
```

Rules and semantics:

- `url` and `target` are mutually exclusive; only **direct-mode** targets can
  back a resolvent (a poll-mode backend would make resolution asynchronous).
  `method`/`api_key`/`timeout` belong to the backing target and are rejected
  on the resolvent.
- The nested payload is forwarded **as-is** through the target's URL, auth
  and timeout (including the target's own `{field}` URL templating) — it is
  never itself scanned for resolvents, so resolvent loops are impossible by
  construction. The backing target's `timeseries_path`/`attach_resolvent`
  play no role; the *outer* target decides whether the substituted series
  keeps its `resolvent` marker.
- `payload_field` selects the resolvent field sent as the nested payload
  (empty = the whole resolvent object); it must hold an object.
  `response_path` extracts the series-shaped sub-object from the response
  (empty = whole response). Both also work for URL-backed resolvents.
- Caching applies as usual: the hash covers the whole resolvent object, so
  identical nested simulations are served from the series cache.

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

Notes:

- `timeseries_path` is a dot-separated path into the payload
  (default `time-series`). MEME spells its registry `model.timeseries`.
  The special value `"."` scans the whole payload — for contracts like
  buem-gateway whose time series (the shared `weather` block) sits at the
  payload root rather than inside a named container.
- `attach_resolvent` (default `true`) controls whether the substituted series
  keeps the original resolvent object under its `resolvent` key. Set `false`
  for targets whose schema validation rejects unknown keys; tentacron's audit
  store (original payload, resolved payload, events) keeps full traceability
  either way.
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

POST-style resolvents send the whole resolvent object as the request body;
the response must be a JSON object and is used verbatim as the series.

### GET resolvents (query/path mapping)

With `method: GET` the resolvent object maps onto the request URL instead of
a body — the contract of the verified weather, city2tabula and ignis APIs:

- `{field}` placeholders in the configured URL are filled from the resolvent
  object (path-escaped) and consumed — e.g. ignis's
  `/api/v1/data/{code}`;
- every remaining field becomes a query parameter, appended to any query
  fixed in the URL (e.g. `?format=json`); parameters are emitted in sorted
  order, so outbound URLs are deterministic;
- arrays of scalars join comma-separated (`variables=T,GHI`,
  `osm_ids=123,456` — the convention of those APIs); the `type` field is
  tentacron's marker and never sent; nested objects are an authoring error;
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
- The nested payload is forwarded **as-is** — it is never itself scanned for
  resolvents, so resolvent loops are impossible by construction.
- `payload_field` selects the resolvent field sent as the nested payload
  (empty = the whole resolvent object). `response_path` extracts the
  series-shaped sub-object from the response (empty = whole response). Both
  also work for URL-backed resolvents.
- Caching applies as usual: the hash covers the whole resolvent object, so
  identical nested simulations are served from the series cache.

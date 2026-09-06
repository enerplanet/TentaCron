# Integration Contract

What a calling service sends tentacron and what it gets back. For the YAML
that wires each backend, see [Configuration](configuration.md); for the HTTP
envelope and error codes, [API Reference](api.md).

## Request envelope

```json
{
  "api_key": "<client key from auth.api_keys>",
  "target": "<target name>",
  "payload": { }
}
```

`POST /v1/requests` returns `202` + `{ "id", "state", "links" }`. Poll
`GET /v1/requests/{id}` until `state` is `completed` or `failed`. Inline JSON
results up to 256 KiB arrive as `result.target_response`; larger or binary
results as `result.href`. Optional `Idempotency-Key` header, scoped per
client.

The caller composes `payload`. Any business logic that decides *which* target
to call, *which* resolvents to embed, or *whether* to call tentacron at all
stays in the caller.

## Resolvent placeholders

A resolvent is an object inside the payload with a `type` of `resolvent-*`.
tentacron finds each one, calls the configured backend with its parameters,
and replaces it in place with the returned object. The original is kept under
the new object's `resolvent` key unless the target sets
`attach_resolvent: false`.

Where tentacron looks for them is the target's `timeseries_path`. Genuine
`{"type": "time-series", ...}` objects are left untouched. Identical
resolvents (same type and parameters) resolve once and are cached for the
type's `cache_ttl`.

| Type | Backend | Method | Parameters | Returns |
|---|---|---|---|---|
| `resolvent-pv1` | PV profile API | POST | `location {lat, lon}`, `capacity_kw`, `azimuth`, `tilt` | hourly capacity-factor / generation series |
| `resolvent-wind` | wind profile API | POST | `location {lat, lon}`, `hub_height_m`, `rotor_diameter_m` | hourly generation series |
| `resolvent-weather` | [weather](https://github.com/enerplanet/weather) | GET | `provider`, `lat`, `lon`, `year`, `use_case` | standardised hourly weather series |
| `resolvent-city2tabula` | [city2tabula](https://github.com/THD-Spatial-AI/city2tabula) | GET | `country`, `osm_ids` (array) | building attribute record (`response_path: "0"` selects the first) |
| `resolvent-ignis` | [ignis](https://github.com/THD-Spatial-AI/ignis) `GET /api/v1/data/{code}` | GET | `code` (TABULA variant code) | TABULA parameter set — see below |
| `resolvent-buem` | `buem-building` target | target | `payload` (one BuEM building request) | thermal load profile (`response_path` extracts `buem.thermal_load_profile.timeseries`) |

For GET-backed resolvents every non-`type` field is sent: `{field}` fills a
URL path segment (and is consumed), the rest become sorted query parameters.
Keep the object to the API's real parameters — a stray `name` would be sent
too. Scalar arrays join comma-separated (`osm_ids=123,456`).

The substituted object should carry `"type": "time-series"`; any other value
is logged as a warning, not an error, so parameter-record backends such as
`resolvent-ignis` work but log once per job.

### Example

```json
{
  "api_key": "…",
  "target": "meme",
  "payload": {
    "model": {
      "timeseries": {
        "pv_cf": { "type": "resolvent-pv1", "location": { "lat": 48.83, "lon": 12.95 }, "capacity_kw": 950 },
        "demand": { "type": "time-series", "unit": "MW", "values": [5.1, 4.8, 4.4] }
      }
    },
    "experiment": { "mode": "plan", "solver": { "name": "highs" } }
  }
}
```

## Targets

### meme

Async. `payload` is a full MEME model (`{ "model": …, "experiment": … }`);
resolvents live under `model.timeseries`. tentacron forwards, reads the job
id, polls to completion, stores the result bundle (a zip, served as
`result.href`).

### buem

Synchronous. `payload` is a buem-gateway building batch with the shared
`weather` block at the payload root (`timeseries_path: "."`,
`attach_resolvent: false`). The response (a per-building summary, no inline
8760-point series) is stored as `result.target_response`. A retried batch
re-runs every building — no downstream idempotency.

### ignis-calculate

Synchronous proxy. `payload` addresses and bodies one
`POST /api/v1/calculate/{code}` call:

```json
{ "api_key": "…", "target": "ignis-calculate", "payload": { "code": "DE.N.SFH.01.Gen.ReEx.001.001" } }
```

`code` fills the URL path and is stripped from the forwarded body. Any
remaining fields are forwarded as the ignis override body; `{}` (just `code`)
uses TABULA defaults. Unknown override fields → ignis `400`.

Response, stored verbatim as `result.target_response`:

```json
{ "variant_code": "DE.N.SFH.01.Gen", "q_h_nd": 123.45, "unit": "kWh/(m2.a)" }
```

## ignis contract detail (v0.5.0)

| Route | Via tentacron | Notes |
|---|---|---|
| `POST /api/v1/calculate/{code}` | `ignis-calculate` target | annual heat demand |
| `GET /api/v1/data/{code}` | `resolvent-ignis`, or a proxy target for a standalone lookup | full TABULA parameter set; changes only on an ignis reseed, cache with a long TTL and bust on version change |
| `GET /api/v1/variants/{cc}/match` | not routed | stable; the caller keeps calling this directly for variant resolution |

`GET /api/v1/data/{code}` returns:

```json
{
  "country": "germany",
  "variant_code": "DE.N.SFH.01.Gen",
  "expected_q_h_nd": 282.7,
  "tabula_data": { "BasicParameters": { }, "AdvancedParameters": { } }
}
```

`tabula_data` holds ~200 physical fields (areas, U-values, climate, solar
gains) under those two groups; field metadata is at `GET /api/v1/fields`.
Set `response_path: "tabula_data"` on the resolvent to substitute just the
parameters.

ignis constraints tentacron must respect in config:

- Request body cap 64 KB (→ `413`), per-request handler timeout 5 s (a slow
  call surfaces as `500`). Set the tentacron `timeout` low (10 s is ample);
  ignis has no rate limiting.
- ignis has no auth of its own. `X-Api-Key` is enforced by the Caddy proxy in
  front of it. Send the key only if tentacron reaches ignis through that
  proxy; a same-network call to the container needs no key (and ignis must
  then not be otherwise reachable). This depends on where tentacron sits in
  the deployment — confirm with the topology owner.
- Errors are `{ "error": "<message>" }` with `400` / `404` / `500`, plus
  `403` from the proxy for a bad key. tentacron maps a `404`/`400` to
  `target_error` (permanent) and `500` to a retry.

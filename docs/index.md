![TentaCron banner](assets/logos/tentacron-banner-dark.png#only-dark)
![TentaCron banner](assets/logos/tentacron-banner-light.png#only-light)

# TentaCron

TentaCron is an orchestration and *resolvent* API for renewable-energy
modelling workflows. It accepts model payloads that still contain **resolvent
objects** — placeholders such as `"type": "resolvent-pv1"` describing a PV
plant, a wind turbine or a weather query — resolves each into a real time
series via configured resource APIs, and forwards the completed payload to a
target service such as [MEME](https://github.com/enerplanet/meme) or
[BuEM](https://github.com/enerplanet/buem-gateway), polling async targets
until their job finishes.

## The core idea

A client shouldn't need to gather PV and wind generation profiles before
submitting an energy-model run. Instead it submits the model with
placeholders, authenticated by its `X-API-Key` header:

```json
{
  "target": "meme",
  "payload": {
    "model": {
      "timeseries": {
        "pv_cf":  { "type": "resolvent-pv1", "location": {"lat": 48.83, "lon": 12.95}, "capacity_kw": 12.5 },
        "demand": { "type": "time-series", "values": [5.1, 4.8, 4.4] }
      }
    }
  }
}
```

TentaCron resolves `pv_cf` by calling the PV resource API with the resolvent's
own properties, replaces the placeholder with the returned series (keeping the
original object under its `resolvent` key for traceability), leaves genuine
`time-series` entries untouched, and forwards the completed model to MEME.

## Highlights

- **Async job API** — `POST /v1/requests` returns `202` + id immediately;
  read `GET /v1/requests/{id}` for state and result, long-poll it with
  `?wait=`, or receive a signed callback when the request ends. Submit up to
  a hundred requests at once, dry-run a payload first, cancel a request
  that has not started.
- **Schedules** — a cron expression in a time zone turns the same
  submission into a recurring run; every due time becomes an ordinary
  request, never twice.
- **Resolvent chaining** — a resolvent's field may reference a sibling's
  result (`{"$from": "building", "path": "tabula_variant_code"}`), so a
  lookup feeds a calculation feeds a simulation in one request.
- **Third-party APIs without a shim** — `query_map` renames fields onto an
  API's parameters and `response_map` reshapes its answer into a series;
  PVGIS is wired that way.
- **Fair by default** — priorities, per-client round-robin and concurrency
  ceilings keep one client's batch from starving another's interactive
  requests.
- **Three resolvent backends** — POST resource APIs (the object is the
  body), GET resource APIs (fields become query parameters and `{field}` path
  segments — the weather, city2tabula and ignis contracts), and configured
  targets (a BuEM simulation feeding a MEME model).
- **Proxy targets** — hand a payload through unresolved and still get auth,
  persistence, the audit trail and retries; `{field}` URL templating fills
  path parameters such as ignis's `/calculate/{code}` from the payload.
- **Durable & auditable** — every request, state transition and result is
  persisted in SQLite and survives a process crash; interrupted work
  resumes after a restart (what a power loss can cost is stated under
  Operations).
- **Series cache** — identical resolvents within a TTL are served from cache
  instead of re-hitting resource APIs.
- **Config-driven** — targets, resolvent types, credentials, poll behaviour
  and search paths all live in one YAML file with `${ENV}` interpolation.
- **Retries done right** — capped exponential backoff with jitter for
  transient faults; fast, explicit failures for permanent ones.
- **Number fidelity** — payloads and series round-trip without float
  conversion, so integer ids above 2^53 reach the target byte-exact.
- **Operable** — Prometheus metrics on their own listener, structured
  logs, a `SIGHUP` reload that rotates keys without a gap, a `backup`
  subcommand, and an OpenAPI description served by the running build.

## Where to go next

- [Architecture](architecture.md) — components, state machine, queue semantics
- [API Reference](api.md) — endpoints, error codes, examples
- [Configuration](configuration.md) — every YAML key explained
- [Operations](operations.md) — deployment, logs, failure handling
- [Browser clients](browser-clients.md) — calling the API from a page: origins, long-polls, downloads, the key
- [OpenAPI reference](openapi/index.html) — the contract, rendered
- [Decisions](decisions/README.md) — why the service looks the way it does

For the quickstart and the list of wired integrations, see the
[README](https://github.com/enerplanet/tentacron#quickstart).

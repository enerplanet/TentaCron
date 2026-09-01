# Tentacron

Tentacron is an orchestration and *resolvent* API for renewable-energy
modelling workflows. It accepts model payloads that still contain **resolvent
objects** — placeholders such as `"type": "resolvent-pv1"` describing a PV
plant, a wind turbine or a weather query — resolves each into a real time
series via configured resource APIs, and forwards the completed payload to a
target service such as [MEME](https://github.com/enerplanet/meme) or
[BuEM](https://github.com/enerplanet/buem-gateway), polling async targets
until their job finishes.

## The core idea

A client shouldn't need to gather PV and wind generation profiles before
submitting an energy-model run. Instead it submits the model with placeholders:

```json
{
  "api_key": "…",
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

Tentacron resolves `pv_cf` by calling the PV resource API with the resolvent's
own properties, replaces the placeholder with the returned series (keeping the
original object under its `resolvent` key for traceability), leaves genuine
`time-series` entries untouched, and forwards the completed model to MEME.

## Highlights

- **Async job API** — `POST /v1/requests` returns `202` + id immediately;
  poll `GET /v1/requests/{id}` for state and result.
- **Three resolvent backends** — POST resource APIs (the object is the
  body), GET resource APIs (fields become query parameters and `{field}` path
  segments — the weather, city2tabula and ignis contracts), and configured
  targets (a BuEM simulation feeding a MEME model).
- **Proxy targets** — hand a payload through unresolved and still get auth,
  persistence, the audit trail and retries; `{field}` URL templating fills
  path parameters such as ignis's `/calculate/{code}` from the payload.
- **Durable & auditable** — every request, state transition and result is
  persisted in SQLite; interrupted work resumes after a restart.
- **Series cache** — identical resolvents within a TTL are served from cache
  instead of re-hitting resource APIs.
- **Config-driven** — targets, resolvent types, credentials, poll behaviour
  and search paths all live in one YAML file with `${ENV}` interpolation.
- **Retries done right** — capped exponential backoff with jitter for
  transient faults; fast, explicit failures for permanent ones.
- **Number fidelity** — payloads and series round-trip without float
  conversion, so integer ids above 2^53 reach the target byte-exact.

## Where to go next

- [Architecture](architecture.md) — components, state machine, queue semantics
- [API Reference](api.md) — endpoints, error codes, examples
- [Configuration](configuration.md) — every YAML key explained
- [Operations](operations.md) — deployment, logs, failure handling

For the quickstart and the list of wired integrations, see the
[README](https://github.com/enerplanet/tentacron#quickstart).

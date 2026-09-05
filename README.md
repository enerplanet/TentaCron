# Tentacron

Tentacron is an orchestration and *resolvent* API for renewable-energy modelling
workflows. It accepts a model payload that still contains **resolvent objects**
(placeholders such as `"type": "resolvent-pv1"` describing a PV plant, a wind
turbine or a weather query), resolves each of them into a real time series by
calling the configured resource APIs, and forwards the completed payload to a
target service such as [MEME](https://github.com/enerplanet/meme) or
[BuEM](https://github.com/enerplanet/buem-gateway) — polling async targets
until their job finishes and storing the final result.

```
Client ──POST /v1/requests──▶ tentacron ──▶ resource APIs (PV, wind, weather, …)
        ◀──202 {id}──────────    │  ▲              resolvent → time series
Client ──GET /v1/requests/id─    ▼  │
        ◀──state / result────  target API (meme, buem, ignis, …) ── poll until done
```

## How it works

1. `POST /v1/requests` with `{ "target": "meme", "payload": … }` and the
   client's `X-API-Key` header returns `202 Accepted` and a request id; the
   request is persisted (SQLite).
2. A worker finds every object with a `type` starting `resolvent-` inside the
   payload's time-series container (location configurable per target, e.g.
   `model.timeseries` for MEME, the payload root for BuEM's weather block).
3. Each resolvent object is resolved through its configured backend; the
   returned series **replaces the resolvent in place**, with the original
   object preserved under the new series' `resolvent` key (switchable off for
   schema-strict targets). A backend is a POST resource API (the object is
   the request body), a GET resource API (the object's fields become query
   parameters and `{field}` path segments — the weather, city2tabula and
   ignis contracts), or another configured target (a BuEM simulation feeding
   a MEME model). A resolvent's field may **reference** a sibling's result
   (`{"$from": "building", "path": "tabula_variant_code"}`), so a lookup can
   feed a calculation can feed a simulation in one request; resolvents are
   resolved level by level. Identical resolvents are served from a TTL cache
   instead of re-hitting the backend.
4. The resolved payload is forwarded to the target. For async targets
   tentacron extracts the target's job id, polls until it reports done or
   failed, and stores the final result. A **proxy target** skips steps 2–3 and
   hands the payload through untouched — tentacron still contributes auth,
   persistence, the audit trail and retries.
5. `GET /v1/requests/{id}` reports the state machine
   (`received → resolving → forwarding → awaiting_target → completed|failed`),
   the result (inline JSON or a downloadable file), and any error. Every
   transition is kept as an audit event.

Failed upstream calls retry with exponential backoff; interrupted jobs are
recovered on restart; polling resumes without re-submitting the target job.

## Integrations

[`config.example.yaml`](config.example.yaml) wires the services below. The
verified entries are grounded in the upstream sources/OpenAPI and exercised
by the golden E2E suite; replace the example hosts with your deployments.

| Config entry | Kind | Service |
|---|---|---|
| `targets.meme` | async target (poll `/jobs/{id}/status`, zip bundle result) | [meme](https://github.com/enerplanet/meme) `POST /simulate` |
| `targets.buem` | direct target, weather resolved at the payload root | [buem-gateway](https://github.com/enerplanet/buem-gateway) `POST /api/v1/buem/buildings` |
| `targets.buem-building` | direct target, also backs `resolvent-buem` | buem-gateway `POST /api/v1/buem/building` |
| `targets.ignis-calculate` | proxy target, `{code}` templated into the URL | [ignis](https://github.com/THD-Spatial-AI/ignis) `POST /api/v1/calculate/{code}` |
| `targets.demo` | direct target for the generic examples | illustrative |
| `resolvents.resolvent-weather` | GET resolvent (point query → `{index, variables}`) | [weather](https://github.com/enerplanet/weather) |
| `resolvents.resolvent-city2tabula` | GET resolvent, list response indexed via `response_path: "0"` | [city2tabula](https://github.com/THD-Spatial-AI/city2tabula) |
| `resolvents.resolvent-ignis` | GET resolvent, `{code}` templated into the path | ignis `GET /api/v1/data/{code}` |
| `resolvents.resolvent-buem` | target-backed resolvent (BuEM run feeding another model) | buem-gateway via `buem-building` |
| `resolvents.resolvent-pvgis` | GET resolvent adapted with `query_map` + `response_map` | [PVGIS](https://re.jrc.ec.europa.eu/pvg_tools/en/) `seriescalc`, hourly PV production (public, no key) |
| `resolvents.resolvent-pv1`, `resolvent-wind` | POST resolvents (object as body) | illustrative — the shape of an in-house profile service |

## Quickstart

```bash
# 1. Build (Go >= 1.26)
make build

# 2. Configure — copy the reference config and export the referenced secrets
cp config.example.yaml config.yaml
export TENTACRON_KEY_FRONTEND=dev-key TENTACRON_KEY_BATCH=dev-key2 \
       MEME_API_KEY=… BUEM_API_KEY=… PV1_API_KEY=… WIND_API_KEY=… \
       WEATHER_API_KEY=… IGNIS_API_KEY=…

# 3. Check the configuration, then run
./bin/tentacron validate -config config.yaml   # prints every target and resolvent, or every problem
./bin/tentacron -config config.yaml
```

Submit a request:

```bash
curl -s -X POST localhost:8080/v1/requests \
  -H 'Content-Type: application/json' -H 'X-API-Key: dev-key' \
  -d '{
    "target": "demo",
    "payload": {
      "scenario": "rooftop-expansion-2030",
      "time-series": [
        { "name": "pv_south_roof", "type": "resolvent-pv1",
          "location": {"lat": 48.831, "lon": 12.957},
          "capacity_kw": 12.5, "azimuth": 180, "tilt": 35 },
        { "name": "household_load", "type": "time-series",
          "unit": "kW", "values": [0.42, 0.38, 0.35] }
      ]
    }
  }'
# → {"id":"6f1c9be2…","state":"received","links":{"self":"/v1/requests/6f1c9be2…"}}

curl -s localhost:8080/v1/requests/6f1c9be2… -H 'X-API-Key: dev-key'
```

Or containerized — [environment/](environment/) carries a single image with
the full toolchain, meme-style (`ENV=dev|prod` selects `environment/.env.*`):

```bash
make -C environment build ENV=dev   # one-time image build
make -C environment run   ENV=dev   # API on http://localhost:8080
make -C environment test  ENV=dev   # Go suite inside the container
make -C environment shell ENV=dev   # interactive shell (go / make / sqlite3)
```

Deployments use the published release image instead — distroless, non-root,
built from the root [`Dockerfile`](Dockerfile) on every `v*` tag:

```bash
docker run -d -p 8080:8080 \
  -v "$PWD/config.yaml:/etc/tentacron/config.yaml:ro" -v tentacron-data:/data \
  --env-file secrets.env ghcr.io/enerplanet/tentacron:0.1.0
```

## API

| Endpoint | Description |
|---|---|
| `POST /v1/requests` | Submit `{target, payload}`; returns `202` + id. Supports an `Idempotency-Key` header. |
| `POST /v1/requests/batch` | Submit up to 100 requests at once; one result per item. |
| `POST /v1/requests/validate` | Dry run: which resolvents the payload contains (paths, cache state) and what would fail, without submitting. |
| `GET /v1/targets`, `GET /v1/resolvents` | Discovery: configured targets and resolvent types, no URLs or credentials. |
| `GET /v1/requests/{id}` | State, attempts, result (inline JSON or `result.href`), error. `?wait=25s` long-polls until terminal. |
| `GET /v1/requests/{id}/result` | Streams a stored result (inline JSON or a result file such as a MEME bundle). |
| `GET /v1/requests/{id}/events` | The request's audit trail: every state transition with its detail. |
| `POST /v1/schedules`, `GET /v1/schedules[/{id}]`, `DELETE /v1/schedules/{id}`, `GET /v1/schedules/{id}/runs` | Recurring submissions on a cron expression in a time zone; every due time becomes an ordinary request. |
| `GET /v1/requests?state=failed&target=meme&limit=50` | List the caller's requests, newest first, with cursor pagination (every client's for an `admin` key). |
| `GET /healthz`, `GET /readyz`, `GET /version` | Liveness, readiness, build. |
| `GET /openapi.yaml` | The OpenAPI 3.1 description of the running build. |

All `/v1` endpoints authenticate with the `X-API-Key` header; a key's `role`
(`client` or `admin`) decides whether it sees only its own requests or all.

See [docs/api.md](docs/api.md) for the full reference (also as
[OpenAPI](docs/openapi.md)),
[docs/configuration.md](docs/configuration.md) for every config key,
[docs/architecture.md](docs/architecture.md) for the design and
[docs/operations.md](docs/operations.md) for deployment and failure handling.

## Development

```bash
make test           # unit + integration + golden E2E tests
make test-race      # with race detector, shuffled order (CI mode)
make cover          # coverage summary
make e2e            # golden end-to-end corpus only, verbose
make golden-update  # accept an intended behavior change
make fuzz           # every fuzz target for 20s (FUZZTIME=…); seeds run in make test
make stress         # race detector, shuffled, repeated (STRESS_COUNT=…)
make live           # one real request through real upstreams (env-gated)
make lint           # go vet + golangci-lint
make run            # build and run with config.example.yaml
make validate       # build and validate config.example.yaml (CONFIG=… for another file)
```

The test suite spins up fake resource/target services in-process — no network
or external services required. Ready-to-send request payloads live in
[examples/](examples/) (each one is executed by the golden E2E suite), and
the test pyramid is documented in [test/README.md](test/README.md).

## Security

Report vulnerabilities privately through GitHub's advisory form; see
[SECURITY.md](SECURITY.md) for scope, response times and what is already in
place. Design decisions are recorded under [docs/decisions/](docs/decisions/).

## License

See [LICENSE](LICENSE).

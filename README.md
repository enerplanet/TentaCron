# Tentacron

Tentacron is an orchestration and *resolvent* API for renewable-energy modelling
workflows. It accepts a model payload that still contains **resolvent objects**
(placeholders such as `"type": "resolvent-pv1"` describing a PV plant or wind
turbine), resolves each of them into a real time series by calling the
configured resource APIs, and forwards the completed payload to a target
service such as [MEME](https://github.com/enerplanet/meme) or [BuEM](https://github.com/enerplanet/buem-gateway) — polling
async targets until their job finishes and storing the final result.

```
Client ──POST /v1/requests──▶ tentacron ──▶ resource APIs (PV, wind, …)
        ◀──202 {id}──────────    │  ▲              resolvent → time series
Client ──GET /v1/requests/id─    ▼  │
        ◀──state / result────  target API (meme, buem, …) ── poll until done
```

## How it works

1. `POST /v1/requests` with `{ "api_key": …, "target": "meme", "payload": … }`
   returns `202 Accepted` and a request id; the request is persisted (SQLite).
2. A worker finds every object with a `type` starting `resolvent-` inside the
   payload's time-series container (location configurable per target, e.g.
   `model.timeseries` for MEME, the payload root for BuEM's weather block).
3. Each resolvent object is sent to its resource API (from `config.yaml`); the
   returned time series **replaces the resolvent in place**, with the original
   object preserved under the new series' `resolvent` key. Identical resolvents
   are served from a TTL cache instead of re-hitting the resource API. A
   resolvent can also be backed by another configured target (e.g. a BuEM
   simulation feeding a MEME model) — see
   [docs/configuration.md](docs/configuration.md#target-backed-resolvents-composition).
4. The resolved payload is forwarded to the target. For async targets
   tentacron extracts the target's job id, polls until it reports done or
   failed, and stores the final result.
5. `GET /v1/requests/{id}` reports the state machine
   (`received → resolving → forwarding → awaiting_target → completed|failed`),
   the result (inline JSON or a downloadable file), and any error. Every
   transition is kept as an audit event.

Failed upstream calls retry with exponential backoff; interrupted jobs are
recovered on restart; polling resumes without re-submitting the target job.

## Quickstart

```bash
# 1. Build (Go >= 1.25)
make build

# 2. Configure — copy the reference config and export the referenced secrets
cp config.example.yaml config.yaml
export TENTACRON_KEY_FRONTEND=dev-key TENTACRON_KEY_BATCH=dev-key2 \
       MEME_API_KEY=… BUEM_API_KEY=… PV1_API_KEY=… WIND_API_KEY=… \
       WEATHER_API_KEY=… IGNIS_API_KEY=…

# 3. Run
./bin/tentacron -config config.yaml
```

Submit a request:

```bash
curl -s -X POST localhost:8080/v1/requests \
  -H 'Content-Type: application/json' \
  -d '{
    "api_key": "dev-key",
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

## API

| Endpoint | Description |
|---|---|
| `POST /v1/requests` | Submit `{api_key, target, payload}`; returns `202` + id. Supports an `Idempotency-Key` header. |
| `GET /v1/requests/{id}` | State, attempts, result (inline JSON or `result.href`), error. Auth: `X-API-Key`. |
| `GET /v1/requests/{id}/result` | Streams a stored result file (e.g. a MEME bundle). |
| `GET /v1/requests?state=failed&limit=50` | List recent requests. |
| `GET /healthz`, `GET /readyz` | Liveness / readiness. |

See [docs/api.md](docs/api.md) for the full reference,
[docs/configuration.md](docs/configuration.md) for every config key, and
[docs/architecture.md](docs/architecture.md) for the design.

## Development

```bash
make test           # unit + integration + golden E2E tests
make test-race      # with race detector (CI mode)
make e2e            # golden end-to-end corpus only, verbose
make golden-update  # accept an intended behavior change
make lint           # go vet + golangci-lint
make run            # build and run with config.example.yaml
```

The test suite spins up fake resource/target services in-process — no network
or external services required. Ready-to-send request payloads live in
[examples/](examples/) (each one is executed by the golden E2E suite), and
the test pyramid is documented in [test/README.md](test/README.md).

## License

See [LICENSE](LICENSE).

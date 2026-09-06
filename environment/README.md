# `environment/` — containerized build & test

A single image that carries everything needed to **build**, **test**, and
**run** the tentacron service end-to-end:

| Layer | What |
|---|---|
| Go 1.26 toolchain | build the service, run `go test ./...` |
| GNU make | the container runs the same Make targets as a local checkout |
| sqlite3 CLI | inspect the job store and audit trail (see [operations](../docs/operations.md)) |

The repo root is bind-mounted at `/src`, so source edits are picked up without
rebuilding the image. Only the toolchain layer is baked in; the Go build and
module caches persist in named volumes across runs — the first containerized
`test` run compiles everything, later runs are incremental. Rebuild the image
only when `go.mod`/`go.sum` or the Dockerfile change.

## Setup

Prerequisites: **Docker with compose v2** and GNU `make` — everything else
lives inside the image. From the repo root (or inside `environment/`, dropping
the `-C environment`):

```bash
make -C environment build ENV=dev   # one-time image build
make -C environment run   ENV=dev   # API on http://localhost:8080
```

## Usage

The targets live in this folder's [`Makefile`](Makefile): run them **inside
`environment/`** as plain `make <target>`, or from the repo root as
`make -C environment <target>`. There are deliberately no `docker-*` aliases
in the root Makefile:

```bash
# from the repo root:
make -C environment build ENV=dev   # image (Go toolchain + tooling)
make -C environment test  ENV=dev   # full Go suite inside the container
make -C environment validate ENV=dev # validate config.yaml with that env file
make -C environment run   ENV=dev   # API service
make -C environment shell ENV=dev   # go / make / sqlite3
```

## Per-environment settings (dev / prod)

Ports, the config file, the image tag and every credential are read from an
env file rather than hard-coded in the compose file. The variables use the
**same names** that [`config.yaml`](config.yaml) interpolates via `${...}`,
so one vocabulary works whether you run via compose or invoke the binary
directly (source the env file first).

| Variable | `.env.dev` | `.env.prod` | Meaning | Used by |
|---|---|---|---|---|
| `PORT` | 8080 | 8080 | port the API listens on inside the container | compose + config |
| `HOST_PORT` | 8080 | 80 | port published on your machine | compose |
| `CONFIG` | `environment/config.yaml` | `environment/config.yaml` | config file the API loads | compose → root `make run` |
| `IMAGE_TAG` | `tentacron-env:dev` | `tentacron-env:prod` | tag of the development image | compose |
| `RELEASE_IMAGE` | `ghcr.io/enerplanet/tentacron:latest` | pin a version | published release image for `run-release` | compose |
| `TENTACRON_KEY_*` | dev placeholders | **change-me** | client keys tentacron accepts | config |
| `MEME_API_KEY`, `BUEM_API_KEY`, `PV1_API_KEY`, `WIND_API_KEY`, `WEATHER_API_KEY`, `IGNIS_API_KEY` | dev placeholders | **change-me** | credentials tentacron presents to the downstream services (meme, buem-gateway, the PV/wind profile services, weather, ignis) | config |

Select one with `ENV=` on any of this folder's Make targets (defaults to
`dev`):

```bash
# from the repo root:
make -C environment run ENV=dev     # publishes on :8080
make -C environment run ENV=prod    # publishes on :80
```

Or drive compose directly with `--env-file`:

```bash
docker compose --env-file environment/.env.prod \
  -f environment/docker-compose.yml up api
```

Copy either file to add more environments (e.g. `.env.staging`) and select it
with `ENV=staging`.

## Production: the release image

Deployments use the image the release workflow publishes from the root
[`Dockerfile`](../Dockerfile) — distroless, non-root, nothing but the
binary — instead of the development image above. The `api-release` compose
service wires it with this folder's config mounted read-only and a named
volume at `/data` for the SQLite store and result files:

```bash
make -C environment run-release ENV=prod   # RELEASE_IMAGE from .env.prod
```

A frontend served from your machine, say Vite on port 5173, calls the
compose service across origins: list it under `server.cors.allowed_origins`
in `config.yaml` (`["http://localhost:5173"]`) and restart or send
`SIGHUP` to the container (`docker compose -f environment/docker-compose.yml
kill -s HUP api`). The [browser client example](../examples/browser/) is a
page to try it with.

Pin `RELEASE_IMAGE` to a version tag (`ghcr.io/enerplanet/tentacron:0.4.0-alpha`)
rather than `latest` in production, and keep `environment/config.yaml`'s
storage paths under `/data` (the defaults already resolve there). See
[operations](../docs/operations.md#deployment) for the plain `docker run`
form.

## Notes

- Unlike an `API_KEY`-optional service, tentacron **refuses to start** when a
  `${VAR}` referenced by its config is unset or when a required credential is
  empty — the compose file marks those variables as required, so a missing
  value fails fast with a clear message instead of booting an open service.
- [`config.yaml`](config.yaml) in this folder mirrors
  [`config.example.yaml`](../config.example.yaml) but takes its listen port
  and all credentials from the env file. The target/resource URLs still point
  at example hosts — replace them with your real meme, buem-gateway, weather,
  city2tabula, ignis and profile-service endpoints. The compose file marks
  every credential variable as required, so a new `${VAR}` reference in the
  config needs a matching line in both env files and in
  [`docker-compose.yml`](docker-compose.yml).
- [`config.yaml`](config.yaml) serves Prometheus metrics on `:9090`
  (`server.metrics_addr`) inside the compose network only; publish that
  port deliberately if you scrape from the host.
- The `api` service writes its SQLite database and result files to
  `/src/data` (the bind mount), which is gitignored. The development
  container runs as root, so `data/` contents it creates are root-owned on
  the host; the release image runs as `nonroot` and writes to its `/data`
  volume instead.
- `make -C environment test` runs the host suite (unit, integration, golden
  E2E) inside the container; the env-gated live tier is a host-side
  `make live` with `TENTACRON_LIVE_CONFIG`/`TENTACRON_LIVE_REQUEST` set
  (see [test/README.md](../test/README.md)).

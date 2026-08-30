# `environment/` — containerized build & test

A single image that carries everything needed to **build**, **test**, and
**run** the tentacron service end-to-end:

| Layer | What |
|---|---|
| Go 1.25 toolchain | build the service, run `go test ./...` |
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
| `IMAGE_TAG` | `tentacron-env:dev` | `tentacron-env:prod` | image tag | compose |
| `TENTACRON_KEY_*` | dev placeholders | **change-me** | client keys tentacron accepts | config |
| `MEME_API_KEY` … `WIND_API_KEY` | dev placeholders | **change-me** | credentials for downstream services | config |

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

## Notes

- Unlike an `API_KEY`-optional service, tentacron **refuses to start** when a
  `${VAR}` referenced by its config is unset or when a required credential is
  empty — the compose file marks those variables as required, so a missing
  value fails fast with a clear message instead of booting an open service.
- [`config.yaml`](config.yaml) in this folder mirrors
  [`config.example.yaml`](../config.example.yaml) but takes its listen port
  and all credentials from the env file. The target/resource URLs still point
  at example hosts — replace them with your real MEME/buem/resource endpoints.
- The `api` service writes its SQLite database and result files to
  `/src/data` (the bind mount), which is gitignored. The container runs as
  root, so `data/` contents created via docker are root-owned on the host.

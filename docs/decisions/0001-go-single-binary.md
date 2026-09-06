# ADR-0001: Implement tentacron in Go as one static binary

- **Status:** accepted, amended 2026-09-06
- **Date:** 2026-08-29

## Context

TentaCron is an orchestration service: it accepts requests, fans out to
resource APIs, forwards to targets and polls them, and must keep doing so
across restarts. It runs next to research services written in Python and is
operated by a small team without a platform group. Concurrency, a durable
worker loop and a small deployment footprint matter more than a rich
scientific ecosystem, which tentacron does not need — it moves JSON.

## Decision

TentaCron is written in Go using the standard library for HTTP, JSON and
logging, with two direct dependencies: a pure-Go SQLite driver and a YAML
parser. It builds with `CGO_ENABLED=0` into one static binary, which is
also what the release image ships.

## Consequences

- One artifact to deploy, cross-compiled for every platform in CI; the
  container image is a distroless base plus the binary.
- Goroutines and contexts give the worker pool, per-job deadlines and
  bounded fan-out without a task-queue framework.
- Contributors from the Python services need Go; the codebase stays small
  and idiomatic (short functions, table-driven tests) to keep that cost low.
- JSON number fidelity had to be handled deliberately (`json.Number`
  everywhere a payload is decoded) because Go's default decoding would round
  large integers through float64.

## Amendments

**2026-09-06.** The binary now has four direct runtime dependencies rather
than two: the pure-Go SQLite driver (`modernc.org/sqlite`), the YAML parser
(`gopkg.in/yaml.v3`), the Prometheus client
(`github.com/prometheus/client_golang`, for the metrics listener) and the
cron parser (`github.com/robfig/cron/v3`, for schedules).
`github.com/pb33f/libopenapi` and `libopenapi-validator` are imported by
tests only and do not reach the binary. The static `CGO_ENABLED=0` build
and the single artifact are unchanged; [`ATTRIBUTIONS.md`](https://github.com/enerplanet/tentacron/blob/main/ATTRIBUTIONS.md)
is the current list.

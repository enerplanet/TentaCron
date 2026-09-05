# Changelog

All notable changes to tentacron are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versioning will
follow [SemVer](https://semver.org/) once the first release is tagged.

## [Unreleased]

Initial development of the tentacron orchestration and resolvent API.

### Changed

- Build: Go 1.26 is now required. `go.mod` pins `go 1.26.0` with
  `toolchain go1.26.8`, so builds no longer depend on whatever Go the host
  happens to have; Go 1.25 has left the upstream support window and stopped
  receiving security fixes.
- CI reads the Go version from `go.mod`, pins golangci-lint instead of
  tracking `latest`, and runs `govulncheck` on every push; Dependabot keeps
  Go modules, GitHub Actions and the container base image current.

### Fixed

- Target URL templating: a `null` payload field no longer fills a `{field}`
  placeholder with an empty path segment, and a placeholder used twice in
  one URL is filled both times (the first substitution used to consume the
  field before the second lookup).

### Added

- Async job API: `POST /v1/requests` (202 + id, idempotency keys scoped per
  client with conflict detection), status/list/result endpoints, health and
  readiness probes, API-key auth with constant-time comparison.
- Resolution engine: resolvent objects (`type: resolvent-*`) found in a
  per-target container (`timeseries_path`, `"."` for the payload root) are
  replaced in place by their resolved series, with the original preserved
  under `resolvent` (per-target `attach_resolvent` opt-out for
  schema-strict targets); deterministic traversal order and number fidelity
  end to end (`json.Number`, no float64 round-trips).
- Resolvent backends: POST resource APIs (object as body), GET resource APIs
  (fields mapped to query parameters and `{field}` URL templates, array
  responses indexed via `response_path`), and configured direct-mode targets
  as backends (`target` + `payload_field` — target composition, e.g. a BuEM
  run feeding a MEME model), all with TTL series caching keyed by canonical
  parameter hash.
- Proxy targets (`proxy: true`): the payload is handed through unresolved —
  byte-exact without URL templating — while tentacron contributes auth,
  persistence, audit and retries; target URLs support `{field}` placeholders
  filled from (and stripped out of) the payload's top level.
- Verified integrations, grounded in upstream sources/OpenAPI: meme
  (async poll: `/jobs/{id}/status`, `queued|running|succeeded|failed`, zip
  bundle), buem-gateway (batch and single-building endpoints, per-building
  error entries as result data), weather (point query), city2tabula
  (building attributes), ignis (TABULA parameters).
- Durable SQLite job queue with audit trail (`job_events`), immediate-mode
  transactions, crash recovery, stuck-job rescue, retention pruning, and
  atomic claim semantics; exponential backoff with jitter and per-claim
  attempt ceilings; graceful shutdown that parks in-flight jobs.
- Hardened outbound client: transient/permanent error classification with
  deterministic multi-failure attribution, refused redirects, credential
  redaction in error excerpts, response size caps, SSRF-safe config-only
  URLs, validated target job ids.
- Test pyramid: unit and integration suites with per-package edge-case
  files, Go-native fuzz targets for every parser facing untrusted input
  (`make fuzz`, CI smoke job), a 45-scenario deterministic golden
  end-to-end corpus with tested example requests (`examples/`), a
  race/shuffle stress entry point (`make stress`), and an env-gated live
  tier (`make live`) for real-upstream verification.
- Containerized build/test environment (`environment/`, meme-style) and
  mkdocs documentation (architecture, API, configuration, operations).

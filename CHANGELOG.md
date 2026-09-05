# Changelog

All notable changes to tentacron are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versioning
follows [SemVer](https://semver.org/). Before 1.0, a minor release may
tighten configuration validation or extend API responses; both are listed
under "Changed" with the keys or fields concerned.

## [Unreleased]

### Added

- `GET /v1/requests/{id}/events` serves a request's audit trail (every
  state transition with its detail and a millisecond timestamp) under the
  same read scoping as the request.
- `X-API-Key` header authentication on `POST /v1/requests`, matching the
  read endpoints; a header wins over the body field. Keys carry a `role`:
  `client` (default) sees only the requests it submitted, `admin` sees
  every request. `Idempotency-Key` is capped at 255 bytes and an
  `X-Request-ID` longer than 128 characters is replaced; the request log
  line names the authenticated client.
- Prometheus metrics on a dedicated listener (`server.metrics_addr`, off by
  default): job outcomes and failure codes per target, queue depth per
  state, upstream call counts and latency by kind, name and status class,
  and series-cache hits and misses. `server.log_level` selects the minimum
  log level. `GET /version` reports the build (version, Go version, VCS
  revision and time) and `/healthz` now carries the version.
- `tentacron validate -config FILE` loads a configuration exactly as `serve`
  would — `${ENV}` interpolation, defaults, every validation rule — and
  prints a summary of targets and resolvents (never credentials) or every
  problem at once; exit code 1 on an invalid config, 2 on usage errors.
  `tentacron version` prints build information. `serve` is the default
  command, so the existing `tentacron -config FILE` keeps working.
  `make validate`, a compose `validate` service and a CI step against both
  reference configs use it.
- Per-target `job_timeout`, `max_attempts` and `retry_on_timeout`. A target
  can bound its own processing attempts and cap its retries; with
  `retry_on_timeout: false` a forward cut off by a deadline fails the job
  with `target_timeout` after that single call instead of being requeued,
  so an expensive or non-idempotent synchronous simulation is never
  submitted twice. The reference configs set it on the BuEM targets.
- Project hygiene for contributors: `SECURITY.md` with private disclosure
  and supported versions, a pull request template mirroring the
  contribution checklist, `CODEOWNERS`, architecture decision records under
  `docs/decisions/` for the choices already taken (Go, SQLite as store and
  queue, the asynchronous API, polling targets to completion, per-target
  resolvent location), and an API stability statement in `docs/api.md`.

### Changed

- Reads are scoped to the submitting client: `GET /v1/requests/{id}` and
  `/result` answer `404` for another client's request (as for an unknown
  id) and `GET /v1/requests` lists only the caller's requests. Deployments
  where one key must see everything give it `role: admin`.

### Deprecated

- The `api_key` field in the `POST /v1/requests` body. Send the
  `X-API-Key` header instead; the field stays accepted until 1.0.

- Configuration validation requires each target's attempt deadline
  (`job_timeout`, or `worker.job_timeout`) to cover the target's `timeout`
  plus the longest configured resolvent timeout (target-backed resolvents
  count with their backing target's `timeout`; proxy targets need only their
  own). Configs where a merely slow forward would have been cut off by the
  job deadline and re-submitted no longer load; the error names the key and
  the required value. The reference configs raise `worker.job_timeout` from
  `5m` to `15m` for that reason.
- A known route called with an unsupported method answers
  `405 method_not_allowed` with an `Allow` header, in the JSON error
  envelope; it used to answer `404 not_found`. Unknown routes still answer
  404.
- Configuration validation rejects non-positive numeric settings and
  durations: `worker.count`, `worker.resolvent_concurrency`,
  `worker.max_attempts`, `server.max_body_bytes`, every timeout, interval,
  retention and cache TTL, plus `worker.backoff_base` above
  `worker.backoff_max` and a poll `interval` not shorter than its `timeout`.
  Such values previously passed silently (a negative worker count started no
  workers at all). Zero still selects the default.

### Fixed

- A shutdown signal arriving in the instant after a worker's claim committed
  left that job orphaned in `resolving` (unclaimable until restart recovery)
  instead of parking it: the claimed job is now read back with a context
  that survives cancellation and always handed to the worker. This was the
  intermittent CI failure of the shutdown-parking test.
- Result files are written through uniquely named temp files, so two
  overlapping poll ticks completing the same job can never interleave
  writes into one shared `.tmp` path.
- A `timeseries_path` that addresses a single resolvent object (rather than
  a container of them) now resolves that object and replaces its slot; it
  used to scan the object's own fields and resolve nothing.
- Terminal states are strictly final: a second completion or failure of an
  already completed or failed job is refused, so an overlapping duplicate
  completion (a slower poll tick finishing after a faster one) can no longer
  replace the stored result body or file. Repeating the same terminal state
  used to be accepted.
- Upstream error excerpts are truncated on a rune boundary, so a stored or
  logged excerpt can no longer end in invalid UTF-8, and surrounding
  whitespace (the newline `http.Error` appends) is trimmed.
- The validation error for a malformed poll `url_template` quotes the
  template as written instead of the `{id}`-substituted probe URL.
- Retention pruning could stop working for good: the delete bound one SQL
  parameter per job id and SQLite refuses statements with more than 32,766
  of them, so once a sweep selected a larger backlog every sweep failed and
  the database grew without bound (result files were already unlinked before
  the failing delete). Rows are now deleted in chunks of 500 and each sweep
  works through the backlog in passes of 1,000 jobs.

## [0.1.0] - 2026-09-05

Initial release of the tentacron orchestration and resolvent API.

### Changed

- Build: Go 1.26 is now required. `go.mod` pins `go 1.26.0` with
  `toolchain go1.26.8`, so builds no longer depend on whatever Go the host
  happens to have; Go 1.25 has left the upstream support window and stopped
  receiving security fixes.
- CI reads the Go version from `go.mod`, pins golangci-lint instead of
  tracking `latest`, and runs `govulncheck` on every push; Dependabot keeps
  Go modules, GitHub Actions and the container base image current.

### Removed

- The organisation-template workflows `sync-legal.yml` and
  `sync-branch-rules.yml` (and their scripts): they acted on every
  repository of two GitHub organisations from this service repository. The
  template-only documentation pages (open-source checklist, repository
  naming, documentation setup, legal sync, branch rules) left the docs site;
  the branch-naming and commit-convention pages stay, reworded for this
  project. The docs deploy now builds in strict mode.

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

[Unreleased]: https://github.com/enerplanet/tentacron/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/enerplanet/tentacron/releases/tag/v0.1.0

# Changelog

All notable changes to tentacron are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versioning
follows [SemVer](https://semver.org/). Before 1.0, a minor release may
tighten configuration validation or extend API responses; both are listed
under "Changed" with the keys or fields concerned.

## [Unreleased]

### Added

- `auth.api_keys[].max_schedules` caps how many schedules a key may hold
  at once, 100 by default, the size of one schedule listing, so a key can
  always list everything it holds; `0` lets a key create none.
- `response.poll.result_timeout` (default 10 minutes, never below the
  target's `timeout`) bounds one result download as a whole, while the
  target's `timeout` bounds the wait between two reads of it. A stalled
  download fails within `timeout`; a slow but moving bundle may take up to
  `result_timeout`.

### Changed

- `POST /v1/schedules` answers `409 schedule_limit` when the key already
  holds as many schedules as it may; previously schedules were unbounded
  while the listing stopped at 100.

### Fixed

- A reload neither applied a changed `upstream.max_response_bytes` nor
  reported it as needing a restart; the cap now follows the reload.
- A shutdown was held open by long-polls: a wait of up to 25 seconds
  outlasted the 20-second grace window, and a second interrupt did nothing
  while the drain hung. Long-polls now answer with the current state the
  moment the drain begins, requests in flight still complete, target-cancel
  notifications are waited for, and a second signal terminates the process
  at once.
- A poll tick was claimed by pushing the next poll forward by one interval
  only, so a tick that outlasted the interval — a result download, a slow
  status call — was claimed again by another worker and the download ran
  twice. A claimed tick is now leased for the time a status call and a
  result download may take, and the worker reschedules to the real cadence
  when its tick ends; after a restart every waiting job polls within one
  interval, spread at random over it.
- A client at its `max_concurrent` ceiling whose queued jobs outranked the
  others filled the claim's candidate batch, every candidate was skipped,
  and the worker reported no work while other clients' jobs were due. A
  claim now reads the in-flight counts once, leaves capped clients out of
  the candidates, and re-queries after a full batch that yielded nothing
  (which also covers a batch of jobs failed for exhausted attempts).
- CORS preflights allowed GET, HEAD, POST and OPTIONS only, so a browser
  frontend on an allowed origin could neither cancel a request nor delete
  a schedule; DELETE is allowed now.
- A result download ran under the target's per-call `timeout`, 60 seconds
  by default, and under the job's attempt deadline on top, so a bundle
  anywhere near the size cap could not finish: the failure counted as
  transient and every tick retried it until the poll deadline ended the
  request as `target_error` although the target's work had succeeded. Poll
  ticks no longer run under `job_timeout`, and the download has its own
  bounds.
- A result that arrived after its request had been cancelled, or after an
  overlapping poll tick had completed it under another name, was dropped
  but its file stayed in the results directory forever, since no row
  referenced it and retention never saw it. The file is discarded with the
  outcome; the stored result's own file is never touched.
- The stuck-job rescue measured every in-flight job against twice
  `worker.job_timeout`, although a target may set a longer `job_timeout`
  and nothing refreshes a job while its attempt runs: a legitimate attempt
  on such a target was requeued and forwarded a second time by another
  worker. Each job is now measured against twice its own target's deadline.

## [0.2.0-alpha] - 2026-09-06

### Added

- The TentaCron identity: the banner on the README and the docs landing
  page (light and dark), the mark as the docs logo and the icon as favicon;
  the artwork lives under `docs/assets/logos/`.
- CI builds the release image on every push and runs `validate` inside it
  against the reference configuration, so a build-context or image
  regression is caught before a tag, not by the release.
- ADR-0007 records why the API contract is a hand-written OpenAPI 3.1
  document kept true by tests — rather than generated from annotations
  like the EnerPlanET backend's, or 3.0.3 like the sibling services' — and
  what would make the choice revisited.
- Every request and response in the OpenAPI description carries an example:
  the request examples are the shipped `examples/` files, the response
  examples come from the golden transcripts, and the lint checks each
  against its schema. The repeated 413, 415 and 422 error responses are
  shared components. A test validates every file under `examples/` against
  the `CreateRequest` schema, so an example the description does not
  describe fails the build.
- The golden end-to-end corpus is validated against the OpenAPI
  description: every recorded response must use a documented status code
  and media type and conform to the schema (format assertions on), and a
  probe of an unknown route must answer `404 not_found` in the error shape.
  With the route test this makes the description a checked contract; the
  document itself is validated by the same test, which replaces the
  structural check CI used to run separately.
- ADR-0006 records when a Postgres store would be reconsidered (measured
  throughput, availability or isolation gates) and the shape it would take,
  so the single-instance decision of ADR-0002 is revisited on evidence.
- Configuration reload on `SIGHUP`: keys, targets, resolvents, callbacks,
  cache TTLs and the log level take effect without a restart; an invalid
  file is rejected and the running configuration kept; startup-only
  settings are logged as `restart_required`. `auth.api_keys[].previous_key`
  is accepted alongside `key` during a rotation. The loaded file's hash is
  logged.
- Completion callbacks: `callback_url` on a submission or batch item
  receives the job document once the request is terminal, signed with
  HMAC-SHA256 (`X-Tentacron-Signature`) and retried with the worker
  backoff. Only https URLs on hosts listed in the new `callbacks` config
  section are accepted (`422 callback_not_allowed`); `GET /v1/requests/{id}`
  reports the delivery under `callback`. New metric
  `tentacron_callback_deliveries_total`.
- Recurring runs: `POST /v1/schedules` (with `GET`, `DELETE` and
  `GET /v1/schedules/{id}/runs`) takes a target, payload, cron expression
  (five fields or `@daily`-style descriptors) and IANA time zone. A
  scheduler loop materialises every due time into an ordinary request under
  the idempotency key `schedule:<id>:<due time>`, so restarts and duplicate
  ticks never double a run; runs default to `cache: refresh`. New setting
  `worker.scheduler_interval` (default 30s), new metric
  `tentacron_schedule_runs_total`.
- Delayed runs: `not_before` (RFC 3339, at most 30 days ahead) on a
  submission or batch item keeps the request in `received` until then; GET
  echoes it for the job's whole life and the audit trail marks the delay.
- Resolvent chaining: a field inside a resolvent object may reference a
  sibling's resolved series (`{"$from": "<name>", "path": "<path>"}`). The
  plan orders resolvents into dependency levels, rejects unknown or
  ambiguous names, self references, cycles and chains deeper than eight
  levels as `invalid_payload`, and the worker fills references level by
  level, hashing the filled input so cache keys reflect the parameters sent.
  The dry run lists `depends_on` per resolvent; `examples/buildings-chain.json`
  runs city2tabula, ignis and BuEM in one request.
- `POST /v1/requests/batch` submits up to 100 requests in one call, each
  validated and stored independently, with a per-item idempotency key and
  one result per item in the answer.
- `GET /v1/requests/{id}?wait=25s` long-polls: the call returns the moment
  the request reaches a terminal state (workers and cancellations wake it
  through an in-process notifier) or when the wait elapses, clamped below
  the server's write timeout.
- `DELETE /v1/requests/{id}` cancels a queued request or one awaiting its
  target (the target is told to stop its job when it configures
  `response.poll.cancel_url_template`); requests being processed or already
  finished answer `409 not_cancellable`. `cancelled` is a new terminal
  state, listable and pruned like the others. Schema migration 0004 rebuilds
  the jobs table for it; the migration runner learned to disable foreign
  keys around table rebuilds and to verify integrity before committing.
- Cache control: `options.cache` on a request selects `use` (default),
  `refresh` (fetch fresh, rewrite the cache — for reruns that must see new
  data) or `bypass` (fetch fresh, touch nothing), echoed on the job; a
  resolvent's `cache_ignore_fields` keeps label fields out of the cache key,
  `name` by default, so resolvents differing only in their label share one
  fetch. Job options live in a JSON column added by migration 0003.
- Response adapters: a resolvent's `response_map` builds the series object
  from a backend that does not speak the series contract (`".path"`
  selections with a `[*]` projection, `{path, scale}` rescaling, literals),
  and `query_map` renames resolvent fields onto a GET API's parameter
  names. Both are validated at startup and fuzzed. The reference configs
  wire PVGIS `seriescalc` as `resolvent-pvgis` — hourly PV production from
  the public API, verified against it — with an example request executed by
  the golden suite.
- `POST /v1/requests/validate` dry-runs a submission: the resolvents the
  payload contains (JSON pointers, names, cache state) and the problems that
  would fail the job, without persisting anything or calling any upstream.
  `GET /v1/targets` and `GET /v1/resolvents` list what the deployment can
  do — routing knobs only, never URLs or credentials.
- `server.cors.allowed_origins` lets browser frontends on exactly those
  origins call the API directly: preflights are answered and the request id,
  download and range headers are exposed. Off by default; no wildcards.
- An OpenAPI 3.1 description of the API, served by the running service at
  `GET /openapi.yaml`, rendered on the docs site and validated in CI; a test
  keeps it in lockstep with the registered routes.
- `tentacron backup -config FILE DEST` writes a consistent, compacted
  snapshot of the database with `VACUUM INTO` while the service runs. New
  databases use incremental auto-vacuum and every housekeeping sweep hands
  freed pages back to the filesystem and checkpoints the WAL, so the file
  shrinks after pruning; existing databases adopt the mode with one manual
  `VACUUM` (documented under Operations).
- Fair scheduling: workers claim jobs by `priority` (a new optional
  `-10`..`10` field on `POST /v1/requests`, echoed on the job), then
  round-robin across clients so a batch never starves interactive
  requests, then age. Keys gain `max_concurrent` (in-flight ceiling,
  enforced inside the claim transaction) and `max_priority` (the highest
  priority a key may request). Schema migration 0002 adds the column.
- Poll-mode results stream straight into the results directory under a new
  `storage.max_result_bytes` cap (1 GiB) instead of being read into memory,
  so a MEME bundle of any realistic size completes with flat memory use; a
  larger result fails the job with `target_error`.
- `GET /v1/requests` filters by `target`, `since`/`until` (RFC 3339 bounds
  on creation time) and, for admin keys, `client`, and pages through an
  opaque `cursor`: a full page returns `next_cursor`, the last page none.
- Result downloads carry `Content-Length`; file results add
  `Content-Disposition` with a filename and `Accept-Ranges`, honour `Range`
  (`206`) and conditional requests, and answer `HEAD`, so large bundles can
  be sized and resumed.
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

- The docs site is published through the GitHub Pages actions — a build
  job that lints the OpenAPI description and builds the site strictly, then
  a deploy from the artifact — on pushes that touch `docs/` or `mkdocs.yml`
  and on demand, as the sibling services do; `mkdocs gh-deploy` no longer
  pushes a `gh-pages` branch. The repository's Pages source must be set to
  *GitHub Actions* once.
- CI lints the OpenAPI description with Redocly under its recommended
  ruleset, the linter the sibling services use, in place of the structural
  check by openapi-spec-validator; the docs workflow runs the same lint
  before building the site, `make lint-openapi` runs it locally, and the
  deliberate exceptions (the local development server, no client error on
  the four unauthenticated system endpoints) are listed with reasons in
  `.redocly.lint-ignore.yaml`. The completion-callback webhook carries an
  `operationId` like every operation.
- The OpenAPI description lives at `docs/openapi/openapi.yaml`, the layout
  the sibling services use, next to a standalone Swagger UI page
  (`docs/openapi/index.html`) that renders it by relative path and so works
  on the docs site, from a checkout and as a raw file alike; the former docs
  page loaded the file from GitHub and stayed blank on in-site navigation.
  The binary embeds and serves the same file at `GET /openapi.yaml` as
  before. The description now lists the local development server and spells
  out that `info.version` is the contract version, not the release.
- The series-cache key leaves out a resolvent's `name` by default (set
  `cache_ignore_fields: []` on a type to restore the old behaviour); entries
  cached under the old keys expire with their TTL.
- GET resolvents no longer send the object's `name` field as a query
  parameter (it is tentacron's label, like `type`).
- `server.max_body_bytes` caps inbound request bodies only. The JSON
  responses of resource, target and status-poll calls are capped by the new
  `upstream.max_response_bytes` (same 10 MiB default); a deployment that had
  raised `max_body_bytes` to admit large upstream responses sets the new key
  instead.
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

- The OpenAPI description omitted three things the service answers with:
  the error codes `callback_not_allowed` (a refused `callback_url`) and the
  dry-run problem codes `invalid_payload`, `unknown_resolvent` and
  `target_error` (a schedule whose every run would fail), and the recorded
  content type of a file result download, such as `application/zip` for a
  MEME bundle.
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

[Unreleased]: https://github.com/enerplanet/tentacron/compare/v0.2.0-alpha...HEAD
[0.2.0-alpha]: https://github.com/enerplanet/tentacron/compare/v0.1.0...v0.2.0-alpha
[0.1.0]: https://github.com/enerplanet/tentacron/releases/tag/v0.1.0

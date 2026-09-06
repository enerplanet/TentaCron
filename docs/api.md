# API Reference

All requests and responses are JSON. The interactive reference lives in its
own standalone page, [`openapi/index.html`](openapi/index.html), so it can
be opened directly without running `mkdocs serve`. It renders
[`openapi/openapi.yaml`](openapi/openapi.yaml), the OpenAPI 3.1 document
the running service also serves at `GET /openapi.yaml`; download that file
to generate a client or import it into Postman (see
[OpenAPI description](#openapi-description)). Errors always use one shape:

```json
{ "error": { "code": "unknown_target", "message": "target \"buem2\" is not configured" } }
```

## Stability

The `/v1` API changes only additively. Fields are never removed or retyped;
new fields and endpoints may appear in any minor release, so clients must
ignore fields they do not know. A deprecation is announced in the changelog
at least one minor release before the removal, and nothing is removed
before 1.0. Configuration keys follow the same rule; a validation rule that
becomes stricter is listed under "Changed" in the changelog together with
the keys it concerns.

## Authentication

Clients authenticate with a named key from `auth.api_keys`, sent in the
`X-API-Key` header on every endpoint under `/v1`:

- `POST /v1/requests` reads the header first. The body's `api_key` field is
  still accepted as a fallback but **deprecated** (announced here; removal
  not before 1.0); when both are present the header wins. The key is
  compared in constant time, never stored and never forwarded.
- Every other `/v1` endpoint — reads, cancellation, the dry run, batch,
  discovery and schedules — requires the header and answers
  `401 unauthorized` when it is missing or unknown.
- Health and build endpoints are unauthenticated.

**Roles and visibility.** Each key has a `role`: `client` (default) or
`admin`. A client sees only the requests it submitted — another client's
request id answers `404 not_found` exactly like an unknown id, so ids cannot
be probed across clients, and lists contain only its own requests. An admin
reads every request. The key's name identifies the client in logs
(`client`), scopes its idempotency keys and is stored on each request.

**Header limits.** `Idempotency-Key` may be at most 255 bytes (longer
answers `400 invalid_parameter`); an `X-Request-ID` longer than 128
characters is replaced by a generated id.

**Browsers.** A frontend may call the API directly from the origins listed
under `server.cors.allowed_origins` (exact matches, no wildcards): preflights
are answered, `X-Request-ID`, `Content-Disposition` and the range headers are
exposed. Any other origin receives no CORS headers. A key embedded in a
browser is visible to its users; a backend-for-frontend keeps it server-side
where that matters.

Every response carries an `X-Request-ID` header — echoed from the request
when supplied, generated otherwise — which is also the `request_id` field of
the corresponding log line.

## POST /v1/requests

Submit a request for orchestration.

**Body**

| Field | Type | Description |
|---|---|---|
| `api_key` | string | Deprecated fallback for the `X-API-Key` header; never stored or forwarded. |
| `target` | string | Target workflow name from the `targets` config, e.g. `meme`. |
| `payload` | object | The body to resolve and forward to the target. Must be a JSON object. |
| `priority` | integer | Optional, `-10`..`10`, default `0`. Higher priorities are claimed first; each key may be capped by `max_priority` in its configuration. |
| `options.cache` | string | Optional: `use` (default) reads and writes the series cache, `refresh` fetches every resolvent fresh and rewrites its cache entry, `bypass` fetches fresh and leaves the cache alone. |
| `not_before` | string | Optional RFC 3339 time, at most 30 days ahead: the request is accepted at once but stays `received` until then (a delayed run). It can be cancelled meanwhile; a time in the past runs at once. |
| `callback_url` | string | Optional https URL that receives the job document once the request is terminal — see [Callbacks](#callbacks). The host must be listed in `callbacks.allowed_hosts`, otherwise `422 callback_not_allowed`. |

Inside the payload, a field of a resolvent object may reference a sibling
resolvent's resolved series instead of holding a literal:
`{"$from": "<name>", "path": "<path into the series>"}` — `name` is the
sibling's `name` field or registry key, `path` optional (whole series when
absent). Resolvents are then resolved level by level; unknown or ambiguous
names, self references, cycles and chains deeper than eight levels fail the
job as `invalid_payload` before any call. See
[Resolution order](architecture.md#resolution-order).

**Headers**

- `X-API-Key` (preferred over the body field)
- `Content-Type: application/json` (required)
- `Idempotency-Key` (optional) — scoped to the authenticated client.
  Resubmitting the identical request (same target and payload) with the same
  key returns `202` with the original request id and its *current* state
  (possibly already `completed`) instead of creating a duplicate; reusing
  the key with a *different* target or payload is rejected with `409`.
  Another client may use the same key value independently.

**Responses**

- `202 Accepted` — `{ "id": "…", "state": "received", "links": { "self": "/v1/requests/…" } }`
- `400 invalid_json` — body is not valid JSON, has trailing data after the
  JSON object, or `payload` is not a JSON object
- `400 missing_field` — no API key (neither header nor field), or `target`
  or `payload` absent (or `null`)
- `400 invalid_parameter` — `Idempotency-Key` longer than 255 bytes, a
  `priority` outside `-10`..`10` or above the key's `max_priority`, an
  unknown `options.cache`, or a `not_before` that is malformed or more than
  30 days ahead
- `401 unauthorized` — unknown API key
- `409 idempotency_conflict` — `Idempotency-Key` already used with a
  different target or payload
- `413 payload_too_large` — body exceeds `server.max_body_bytes`
- `415 unsupported_media_type` — `Content-Type: application/json` is required
- `422 unknown_target` — target not configured
- `422 callback_not_allowed` — `callback_url` is not https, carries
  credentials, or names a host outside `callbacks.allowed_hosts` (or
  callbacks are not enabled)

Validation happens in that order: an unknown target is only reported once
the key has been accepted.

## POST /v1/requests/validate

A dry run. The body is the same as for `POST /v1/requests` and goes through
the same decoding, authentication, target and priority checks (same `400`,
`401`, `415`, `422`), but nothing is persisted and no upstream is called:
the payload is inspected exactly as the worker would start it.

```json
{
  "ok": true,
  "target": "demo",
  "resolvents": [
    { "type": "resolvent-pv1", "path": "/time-series/0", "name": "pv_south_roof", "cached": true },
    { "type": "resolvent-wind", "path": "/time-series/1", "name": "wind_ridge", "cached": false }
  ],
  "problems": []
}
```

- `resolvents` lists every resolvent object in document order with its JSON
  pointer, its `name` field or registry key, and whether the series cache
  already holds it (a cached resolvent costs no upstream call).
- `problems` lists what would fail the job before the first call, with the
  job error codes `invalid_payload`, `unknown_resolvent` (one entry per
  unknown type) or, for proxy targets, `target_error` when a URL placeholder
  has no payload field. `ok` is `false` whenever there are problems.
- The answer is `200` whenever the request itself is well-formed; a
  frontend checks `ok`, not the status.

## POST /v1/requests/batch

Submit up to 100 requests in one call. Each item has the fields of a single
submission (`target`, `payload`, `priority`, `options`) plus an optional
`idempotency_key`, scoped to the client like the header on the single
endpoint. Items are validated and stored independently; the answer lists
one result per item, in order:

```json
{ "items": [
  { "id": "…", "state": "received", "links": { "self": "/v1/requests/…" } },
  { "error": { "code": "unknown_target", "message": "target \"hydra\" is not configured" } }
] }
```

`202` when at least one item was accepted (or replayed), `400` with the
same `items` when none was; an empty list or more than 100 items answer
`400 invalid_parameter` without items. Item errors use the single
endpoint's codes (`invalid_json`, `missing_field`, `unknown_target`,
`invalid_parameter`, `idempotency_conflict`).

## GET /v1/targets and GET /v1/resolvents

Discovery for frontends: what this deployment can do, without reading its
configuration. Both require a key; neither reveals URLs or credentials.

```json
{ "items": [
  { "name": "buem", "response_mode": "direct", "proxy": false, "timeseries_path": ".", "attach_resolvent": false },
  { "name": "ignis-calculate", "response_mode": "direct", "proxy": true },
  { "name": "meme", "response_mode": "poll", "proxy": false, "timeseries_path": "model.timeseries", "attach_resolvent": true }
] }
```

```json
{ "items": [
  { "type": "resolvent-buem", "backend": "target", "target": "buem-building", "cache_ttl": "24h0m0s" },
  { "type": "resolvent-pv1", "backend": "post", "cache_ttl": "24h0m0s" },
  { "type": "resolvent-weather", "backend": "get", "cache_ttl": "24h0m0s" }
] }
```

## GET /v1/requests/{id}

Add `?wait=25s` to long-poll: the call blocks until the request reaches a
terminal state or the wait elapses, then answers with the freshest state —
one call instead of a polling loop. The wait is clamped to
`server.write_timeout` minus five seconds (a minute when no write timeout
is configured); an invalid or negative value answers `400 invalid_parameter`.
A request that is already terminal answers at once. Workers wake waiting
calls the moment a request ends, so a completion arrives within
milliseconds; the wait is bounded, so a client loops on `wait` until the
state is terminal.

```json
{
  "id": "6f1c9be2…",
  "target": "meme",
  "state": "completed",
  "attempts": 1,
  "target_job_id": "meme-8842",
  "created_at": "2026-09-03T10:15:04Z",
  "updated_at": "2026-09-03T10:16:09Z",
  "completed_at": "2026-09-03T10:16:09Z",
  "result": { "target_status": 200, "target_response": { "objective": 1234.5 } },
  "error": null
}
```

- `state` is one of `received`, `resolving`, `forwarding`, `awaiting_target`,
  `completed`, `failed`, `cancelled`.
- `attempts` counts processing attempts (resolution + forwarding); it is
  incremented when a worker claims the job, so a crash mid-attempt still
  counts.
- `priority` appears when it is not the default `0`; `options` when any
  option differs from its default (`{ "cache": "refresh" }`).
- `target_job_id` appears once a poll-mode target accepted the job;
  `completed_at` once the job is terminal (completed, failed or cancelled);
  `not_before` for a delayed run, for the job's whole life.
- `callback` appears when the request named a `callback_url`:
  `{ "url", "state": pending|delivered|failed, "attempts", "last_status",
  "last_error" }` — pending until the request is terminal and the delivery
  succeeded.
- `result` is `null` until the job is `completed`. JSON results up to
  256 KiB are embedded as `result.target_response`; larger or non-JSON
  results (e.g. a MEME zip bundle) are stored as files and referenced as
  `result.href` + `result.content_type`. `target_status` is the HTTP status
  of the target's final response (`200` for a fetched poll result).
- Failed jobs carry `error.code`/`error.message`; job-level codes are
  `invalid_payload`, `unknown_target`, `unknown_resolvent`, `resource_error`,
  `invalid_resource_response`, `target_error`, `target_job_failed`,
  `target_timeout`, `max_attempts_exceeded`, `internal`. Messages that quote
  an upstream response have every configured downstream credential redacted
  and are truncated to 512 bytes. See
  [Operations → Failure handling](operations.md#failure-handling) for which
  situation produces which code.

`404 not_found` for unknown ids and for another client's ids (admins
excepted).

## DELETE /v1/requests/{id}

Cancels a request. A queued request (`received`) is cancelled at once and
never reaches the target. A request awaiting its target
(`awaiting_target`) is cancelled, polling stops, and when the target has a
`cancel_url_template` it is told to stop its job (best effort, in the
background — the request is `cancelled` either way). The answer is the
request envelope with `state: cancelled`, `completed_at` set and no
`result` or `error`.

- `409 not_cancellable` while a worker is processing the request
  (`resolving`, `forwarding`): retry once it is queued again or awaiting its
  target; the request keeps running.
- `409 not_cancellable` for a finished request (`completed`, `failed`,
  `cancelled`).
- `404 not_found` for unknown ids and for another client's requests.

`cancelled` is a terminal state: it appears in `state=cancelled` listings
and is pruned by retention like the others.

## GET /v1/requests/{id}/result

Serves the stored result once the request is `completed`: an inline JSON
result as `application/json`, a result file streamed with its recorded
content type (e.g. `application/zip`). Responses carry `Content-Length`;
file results also carry `Content-Disposition: attachment; filename=<id>.zip`
and `Accept-Ranges: bytes`, honour `Range` (`206 Partial Content`) and
`If-Modified-Since`, and answer `HEAD` with the headers alone — so a large
MEME bundle can be sized and resumed. `404 not_found` while the job is not
completed, when it completed without a result body, or after the result was
pruned by retention.

## GET /v1/requests/{id}/events

The request's audit trail, oldest first — every state transition with the
detail the worker recorded (claims, resolution counts, target acceptance,
retries with their backoff, the terminal outcome). Same scoping as the
request itself.

```json
{ "items": [
  { "to_state": "received", "detail": "job accepted", "created_at": "2026-09-03T10:15:04.120Z" },
  { "from_state": "received", "to_state": "resolving", "detail": "claimed by worker", "created_at": "2026-09-03T10:15:04.131Z" },
  { "from_state": "resolving", "to_state": "forwarding", "detail": "resolved 2 resolvent(s), 1 from cache", "created_at": "2026-09-03T10:15:05.802Z" }
] }
```

Timestamps carry millisecond precision. Poll ticks are deliberately not
recorded (they would flood the trail); their outcome is the final
transition.

## GET /v1/requests

List requests, newest first — the caller's own requests, or every client's
for an `admin` key. Items have the same shape as `GET /v1/requests/{id}`.

| Parameter | Description |
|---|---|
| `state` | One of the job states. |
| `target` | A target name. |
| `client` | Admin keys only: list one client's requests (a client may name itself). |
| `since` / `until` | RFC 3339 bounds on `created_at`; `since` inclusive, `until` exclusive. |
| `limit` | Page size, default 50, max 200 (larger values are capped). |
| `cursor` | The `next_cursor` of the previous page. |

```json
{ "items": [ { "id": "…", "state": "failed", "error": { "code": "target_timeout", "message": "…" } } ],
  "next_cursor": "MTc1Njk4…" }
```

`next_cursor` is present only when more items match; pass it back as
`cursor` with the same filters to fetch the next page. Cursors are opaque
tokens tied to the listing order, so pages never skip or repeat a request
even when many were created in the same millisecond. Invalid values (unknown
state, non-positive limit, malformed timestamp or cursor, a client filter
without an admin key) answer `400 invalid_parameter`. No total count is
returned.

## Schedules

A schedule is a recurring submission: the same target and payload, run at
every time its cron expression names. Each due time materialises into an
ordinary request — visible under `GET /v1/requests`, cancellable, with its
audit trail — created with the idempotency key `schedule:<id>:<due time>`,
so a restart or a duplicate scheduler tick can never produce a second run.
Runs default to `options.cache: refresh` (a recurring run exists to pick up
fresh inputs); pass `options` to change that. A run is never created before
its due time and at most `worker.scheduler_interval` after it. Due times
missed while the service was down collapse into one run; the next due time
is then computed from the current time.

| Method and path | Description |
|---|---|
| `POST /v1/schedules` | Create. Body: `target`, `payload`, `cron`, optional `timezone` (IANA name, default `UTC`), `priority`, `options`. `201` with the schedule and its `next_run_at`. |
| `GET /v1/schedules` | The caller's schedules, oldest first, at most 100 (`items`). An admin key lists every client's, or one client's with `?client=`. |
| `GET /v1/schedules/{id}` | One schedule: `cron`, `timezone`, `next_run_at`, `last_run_at`, `last_job_id`, `links.runs`. |
| `DELETE /v1/schedules/{id}` | `204`; runs already created stay. |
| `GET /v1/schedules/{id}/runs` | The runs, newest first, with the request list's `state`, `limit` and `cursor` parameters. |

`cron` takes five fields (`minute hour day-of-month month day-of-week`,
e.g. `30 6 * * 1-5`) or a descriptor: `@hourly`, `@daily`, `@weekly`,
`@monthly`, `@yearly`, `@every <duration>` (e.g. `@every 6h`). The
expression is evaluated in `timezone`, so `30 6 * * *` with
`Europe/Berlin` follows daylight-saving time. The payload is inspected as
`POST /v1/requests/validate` would; a schedule whose every run would fail
(`invalid_payload`, `unknown_resolvent`, `target_error`) is refused with
`400`, an unknown target with `422`, an invalid `cron` or `timezone` with
`400 invalid_parameter`. Another client's schedule answers `404`.

```json
{
  "id": "3c4d…", "target": "buem", "cron": "30 6 * * *", "timezone": "Europe/Berlin",
  "next_run_at": "2026-09-06T04:30:00Z",
  "last_run_at": "2026-09-05T04:30:00Z", "last_job_id": "9a1f…",
  "created_at": "2026-09-01T12:00:00Z", "updated_at": "2026-09-05T04:30:00Z",
  "links": { "self": "/v1/schedules/3c4d…", "runs": "/v1/schedules/3c4d…/runs" }
}
```

## Callbacks

A request may name a `callback_url`. Once it is terminal, tentacron POSTs
the job document — exactly what `GET /v1/requests/{id}` answers, without
the `callback` field — to that URL, once per request:

| Header | Value |
|---|---|
| `Content-Type` | `application/json` |
| `X-Tentacron-Event` | `request.completed`, `request.failed` or `request.cancelled` |
| `X-Tentacron-Request-Id` | the request id |
| `X-Tentacron-Attempt` | `1`, `2`, … |
| `X-Tentacron-Signature` | `sha256=<hex HMAC-SHA256 over the raw body, keyed with callbacks.signing_secret>` |

Any `2xx` acknowledges the delivery. A `4xx` other than `408`/`429`, or a
redirect (never followed), ends delivery as `failed`; any other outcome is
retried with the worker backoff up to `callbacks.max_attempts`. One
attempt is bounded by `callbacks.timeout` and reads at most 1 KiB of the
answer. Request data never supplies an outbound URL anywhere else; the
callback is the one deliberate exception, which is why the URL must be
https and its host must match `callbacks.allowed_hosts` exactly
(`host:port` when the URL carries a port).

Verify the signature before trusting a delivery — over the raw bytes, with
a constant-time compare:

```python
import hmac, hashlib
def verify(secret: bytes, body: bytes, header: str) -> bool:
    expected = "sha256=" + hmac.new(secret, body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, header)
```

```sh
printf 'sha256=%s' "$(openssl dgst -sha256 -hmac "$SECRET" -binary < body.json | xxd -p -c 256)"
```

## Health and build

- `GET /healthz` — liveness, always `200` while the process runs;
  `{ "status": "ok", "version": "v0.2.0-alpha" }`.
- `GET /readyz` — `200` once migrations ran and the database answers,
  `503` otherwise.
- `GET /version` — the running build: `version`, `go`, and `revision` /
  `built` when the binary was built inside the repository. Unauthenticated,
  reveals no configuration.
- `GET /openapi.yaml` — the OpenAPI description of this build.

Prometheus metrics are not on this listener; see
[Operations → Metrics](operations.md#metrics).

Any other route answers `404 not_found` in the standard error shape; a known
route with an unsupported method answers `405 method_not_allowed` with an
`Allow` header listing the accepted methods.

## OpenAPI description

[`openapi/openapi.yaml`](openapi/openapi.yaml) is the single source of
truth for this contract: the standalone
[`openapi/index.html`](openapi/index.html) renders it, the binary embeds
the same file and serves it at `GET /openapi.yaml`, and a test fails when
the document and the registered routes disagree, so the description can
neither describe a route the build does not serve nor miss one it does.
The golden end-to-end corpus is validated against it as well: every
recorded response must use a documented status code and media type and
conform to the schema, so a response field or error code missing from the
description fails the build. Every request and response carries an
example: the request examples are the shipped files under `examples/`
(which a test validates against the `CreateRequest` schema), the response
examples are taken from the golden transcripts, and the lint checks every
example against its schema. Why the document is hand-written OpenAPI 3.1
rather than generated, or the siblings' 3.0.3, is recorded in
[ADR-0007](decisions/0007-openapi-contract.md).
CI lints it with [Redocly](https://redocly.com/docs/cli/) under the
recommended ruleset (`make lint-openapi` runs the same check locally); the
few deliberate exceptions are listed with their reasons in
`.redocly.lint-ignore.yaml`.

Generate a client with any OpenAPI 3.1 tool, from the running service or
from the file in the repository:

```bash
curl -s http://localhost:8080/openapi.yaml -o tentacron.yaml
openapi-generator-cli generate -i tentacron.yaml -g python -o ./tentacron-client
```

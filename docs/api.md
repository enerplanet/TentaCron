# API Reference

All requests and responses are JSON. The same contract is available as an
[OpenAPI 3.1 document](openapi.md), served by the running service at
`GET /openapi.yaml`. Errors always use one shape:

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
- Every `GET /v1/requests…` endpoint requires the header and answers
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
- `400 invalid_parameter` — `Idempotency-Key` longer than 255 bytes, or a
  `priority` outside `-10`..`10` or above the key's `max_priority`
- `401 unauthorized` — unknown API key
- `409 idempotency_conflict` — `Idempotency-Key` already used with a
  different target or payload
- `413 payload_too_large` — body exceeds `server.max_body_bytes`
- `415 unsupported_media_type` — `Content-Type: application/json` is required
- `422 unknown_target` — target not configured

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
  `completed`, `failed`.
- `attempts` counts processing attempts (resolution + forwarding); it is
  incremented when a worker claims the job, so a crash mid-attempt still
  counts.
- `priority` appears when it is not the default `0`.
- `target_job_id` appears once a poll-mode target accepted the job;
  `completed_at` once the job is terminal (completed *or* failed).
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

## Health and build

- `GET /healthz` — liveness, always `200` while the process runs;
  `{ "status": "ok", "version": "v0.2.0" }`.
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

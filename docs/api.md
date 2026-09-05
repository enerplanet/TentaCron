# API Reference

All requests and responses are JSON. Errors always use one shape:

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
- `400 invalid_parameter` — `Idempotency-Key` longer than 255 bytes
- `401 unauthorized` — unknown API key
- `409 idempotency_conflict` — `Idempotency-Key` already used with a
  different target or payload
- `413 payload_too_large` — body exceeds `server.max_body_bytes`
- `415 unsupported_media_type` — `Content-Type: application/json` is required
- `422 unknown_target` — target not configured

Validation happens in that order: an unknown target is only reported once
the key has been accepted.

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
content type (e.g. `application/zip`). `404 not_found` while the job is not
completed, when it completed without a result body, or after the result was
pruned by retention.

## GET /v1/requests

List recent requests, newest first — a client's own requests, or every
request for an admin key. Query parameters: `state` (filter by job
state) and `limit` (default 50, max 200 — larger values are capped); invalid
values answer `400 invalid_parameter` (unknown state filter, or limit not a
positive integer). Items have the same shape as `GET /v1/requests/{id}`.

```json
{ "items": [ { "id": "…", "state": "failed", "error": { "code": "target_timeout", "message": "…" } } ] }
```

## Health and build

- `GET /healthz` — liveness, always `200` while the process runs;
  `{ "status": "ok", "version": "v0.2.0" }`.
- `GET /readyz` — `200` once migrations ran and the database answers,
  `503` otherwise.
- `GET /version` — the running build: `version`, `go`, and `revision` /
  `built` when the binary was built inside the repository. Unauthenticated,
  reveals no configuration.

Prometheus metrics are not on this listener; see
[Operations → Metrics](operations.md#metrics).

Any other route answers `404 not_found` in the standard error shape; a known
route with an unsupported method answers `405 method_not_allowed` with an
`Allow` header listing the accepted methods.

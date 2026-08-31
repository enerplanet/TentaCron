# API Reference

All requests and responses are JSON. Errors always use one shape:

```json
{ "error": { "code": "unknown_target", "message": "target \"buem2\" is not configured" } }
```

## POST /v1/requests

Submit a request for orchestration.

**Body**

| Field | Type | Description |
|---|---|---|
| `api_key` | string | A client key from `auth.api_keys`. Never stored or forwarded. |
| `target` | string | Target workflow name from the `targets` config, e.g. `meme`. |
| `payload` | object | The body to resolve and forward to the target. |

**Headers**

- `Content-Type: application/json` (required)
- `Idempotency-Key` (optional) — scoped to the authenticated client.
  Resubmitting the identical request (same target and payload) with the same
  key returns the original request id instead of creating a duplicate;
  reusing the key with a *different* request is rejected with `409`.

**Responses**

- `202 Accepted` — `{ "id": "…", "state": "received", "links": { "self": "/v1/requests/…" } }`
- `400 invalid_json` / `missing_field` — malformed body (including trailing
  data after the JSON object) or missing field
- `401 unauthorized` — unknown api key
- `409 idempotency_conflict` — `Idempotency-Key` already used with a
  different target or payload
- `413 payload_too_large` — body exceeds `server.max_body_bytes`
- `415 unsupported_media_type` — `Content-Type: application/json` is required
- `422 unknown_target` — target not configured

## GET /v1/requests/{id}

Authenticated via the `X-API-Key` header.

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
- Small JSON results are embedded as `result.target_response`; large or binary
  results (e.g. a MEME bundle) are referenced as `result.href` +
  `result.content_type`.
- Failed jobs carry `error.code`/`error.message`; job-level codes are
  `invalid_payload`, `unknown_target`, `unknown_resolvent`, `resource_error`,
  `invalid_resource_response`, `target_error`, `target_job_failed`,
  `target_timeout`, `max_attempts_exceeded`, `internal`.

`404 not_found` for unknown ids.

## GET /v1/requests/{id}/result

Streams the stored result with its recorded content type. Available once the
request is `completed`; `404` otherwise.

## GET /v1/requests

List recent requests, newest first. Authenticated via the `X-API-Key` header,
exactly like `GET /v1/requests/{id}` — the same applies to
`GET /v1/requests/{id}/result`. Query parameters: `state` (filter by job
state) and `limit` (default 50, max 200); invalid values answer
`400 invalid_parameter` (unknown state filter, or limit not a positive
integer).

```json
{ "items": [ { "id": "…", "state": "failed", "error": { "code": "target_timeout", "message": "…" } } ] }
```

## Health

- `GET /healthz` — liveness, always `200` while the process runs.
- `GET /readyz` — `200` once migrations ran and the database answers,
  `503` otherwise.

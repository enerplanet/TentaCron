# Architecture

Tentacron is a single Go binary. SQLite serves as both the audit store and the
durable job queue; a worker pool drives each request through resolution,
forwarding and target polling.

## Components

```mermaid
flowchart LR
    C[Client] -->|"POST /v1/requests"| A[API]
    C -->|"GET /v1/requests/{id}"| A
    A -->|persist job| DB[(SQLite)]
    A -.nudge.-> W[Worker pool]
    W -->|claim job| DB
    W -->|"resolvent object (POST body / GET query)"| R1[Resource API pv / weather / …]
    W -->|"nested payload (target-backed resolvent)"| T2[Target API buem-building]
    W -->|resolved or proxied payload| T[Target API meme / buem / ignis]
    W -->|"poll job {id}"| T
    W -->|series cache| DB
    S[Scheduler] -->|"due schedule → job"| DB
    D[Callback deliverer] -->|"signed job document"| CB[Client callback URL]
    W -.->|"job ended: wakes ?wait= long-polls"| A
    P[Prometheus] -->|"GET /metrics"| M[Metrics listener]
```

## Request lifecycle

```mermaid
stateDiagram-v2
    [*] --> received: POST accepted
    received --> resolving: worker claims job
    resolving --> forwarding: all resolvents substituted (or proxy: handed through)
    forwarding --> awaiting_target: target accepted (poll mode)
    forwarding --> completed: target responded (direct mode)
    awaiting_target --> completed: target job done, result stored
    resolving --> received: transient error, backoff requeue
    forwarding --> received: transient error, backoff requeue
    resolving --> failed: permanent error / attempts exhausted
    forwarding --> failed: permanent error / attempts exhausted
    awaiting_target --> failed: target job failed / poll deadline / permanent poll error
    received --> cancelled: client DELETE
    awaiting_target --> cancelled: client DELETE (target told to stop, best effort)
    completed --> [*]
    failed --> [*]
    cancelled --> [*]
```

Every transition is appended to the `job_events` table, giving a complete
audit trail per request. Terminal states are final: a late outcome from an
overlapping worker can never overwrite a result a client may already have
seen. A request a worker is processing right now (`resolving`,
`forwarding`) cannot be cancelled — the worker holds it and the upstream
call is in flight — so `DELETE` answers `409` there and the client retries
once the request is queued again or awaiting its target.

## Resolution

The payload's time-series container is located via the per-target
`timeseries_path` — `model.timeseries` for MEME, the default `time-series`
for the generic examples, or `"."` to scan the whole payload (buem-gateway,
whose `weather` block sits at the root). The container may be an array or a
name→object registry; nested arrays and objects below it are walked too. A
path that addresses a single resolvent object resolves that object itself,
replacing its slot in the parent.
Every object whose `type` starts with `resolvent-` is collected, in
deterministic order (array order, sorted keys). The walk does not descend
into resolvent objects (their fields are opaque backend parameters) nor into
genuine `time-series` objects (data). Unknown resolvent types fail the job
*before* any HTTP call. A missing container is not an error — there is
simply nothing to resolve.

Each distinct resolvent (deduplicated by a canonical SHA-256 hash of its type
and parameters) is fetched concurrently, bounded by
`worker.resolvent_concurrency` — from the series cache when an identical
resolvent was resolved within its TTL, otherwise through its backend:

| Backend | Configured by | Outbound call |
|---|---|---|
| POST resource API | `url` + `method: POST` (or PUT/PATCH) | the resolvent object (or its `payload_field`) as JSON body |
| GET resource API | `url` + `method: GET` | `{field}` placeholders filled from the object, every other field a query parameter |
| Configured target | `target: <direct-mode target>` | the nested payload forwarded through that target, with its URL, auth and timeout |

The response must be a JSON object (optionally narrowed via `response_path`,
which also indexes array responses). It replaces the resolvent object in
place; unless the target sets `attach_resolvent: false`, the original object
moves under the new series' `resolvent` key:

```json
{ "name": "pv", "type": "resolvent-pv1", "capacity_kw": 12.5 }
```

becomes

```json
{
  "type": "time-series", "unit": "kW", "values": [0.1, 0.2],
  "resolvent": { "name": "pv", "type": "resolvent-pv1", "capacity_kw": 12.5 }
}
```

A nested payload sent through a target-backed resolvent is never itself
scanned for resolvent objects; the only way one resolvent feeds another is
the explicit reference described next. Payloads and series are decoded with
number fidelity (`json.Number`), so integers above 2^53 survive the round
trip byte-exact.

### Resolution order

A field anywhere inside a resolvent object — including the nested payload of
a target-backed one — may be a **reference** to a sibling resolvent's
resolved series instead of a literal:

```json
"typology": { "type": "resolvent-ignis",
              "code": { "$from": "building", "path": "tabula_variant_code" } }
```

`$from` names the sibling by its `name` field or registry key; `path` is a
`response_map`-style path into that sibling's series (the leading dot is
optional, `[*]` projects over an array, absent means the whole series). The
plan orders the resolvents into **levels**: level 0 holds those without
references, every later level those whose references all point into earlier
levels. References to unknown or ambiguous names, self references, cycles
and chains deeper than eight levels fail the job as `invalid_payload` before
any call — the dry run reports the same problem, and lists each resolvent's
`depends_on`.

The worker fetches level by level. Before a level starts, every reference in
it is filled from the series already resolved; the filled object is what
goes upstream and what the cache key is computed from, so a chained
resolvent caches under the parameters it really sent and a literal twin of
it is a cache hit. A path that is missing from the series it points to fails
the job as `invalid_payload`. Failure attribution stays deterministic: the
first failing level, document order within it. The `resolvent` marker keeps
the original object with its `$from` references for traceability.

**Proxy targets** (`proxy: true`) skip resolution entirely: the payload is
stored as its own resolved form and forwarded untouched — byte-exact unless
the target URL carries `{field}` placeholders, which are filled from (and
stripped out of) the payload's top level.

## Queue semantics

SQLite *is* the queue — no external broker:

- Claiming a `received` job is an atomic compare-and-swap
  (`UPDATE … WHERE state = 'received'`); exactly one worker wins. The attempt
  counter is incremented at claim time, so an attempt cut short by a crash
  still counts, and a job that already reached `worker.max_attempts` is
  failed at claim instead of run again.
- Candidates are ordered by `priority` (`-10`..`10`), then round-robin
  across clients — a window function ranks each client's due jobs, every
  client's first comes before any client's second, and among those the
  client served least recently (tracked per claim) goes first — then age. A
  client at
  its `max_concurrent` ceiling is skipped for other clients' work; the
  ceiling is checked inside the claim's write transaction, so concurrent
  workers cannot overshoot it together.
- Poll ticks for `awaiting_target` jobs are claimed by pushing
  `next_attempt_at` forward atomically by the target's poll interval, so
  concurrent workers never poll the same job twice for the same tick.
- Transient failures (network errors, timeouts, HTTP 429/5xx, the job
  timeout) requeue the job with capped exponential backoff and ±20 % jitter;
  permanent failures (other non-2xx statuses including redirects, oversized
  responses, malformed resource bodies, unknown resolvent, target job failed)
  fail it immediately. A target configured with `retry_on_timeout: false`
  turns a deadline hit on its forward into a permanent `target_timeout`, so
  an expensive synchronous simulation is never submitted twice; per-target
  `job_timeout` and `max_attempts` override the worker defaults.
- Workers wake on a nudge from the API when a job is accepted and otherwise
  every `worker.poll_interval` to pick up scheduled retries and poll ticks.

## Schedules

A schedule row holds a target, payload, cron expression, time zone,
priority, options and its next due time. A scheduler loop in the worker
process wakes every `worker.scheduler_interval`, reads the due schedules and
creates one ordinary job per schedule with the idempotency key
`schedule:<id>:<due time>`, then advances `next_run_at` with a
compare-and-set on the due time. Two properties fall out of that:

- **No duplicated run.** A restart racing the old process, or two ticks
  looking at the same due time, both create "the" run — the idempotency key
  makes the second creation a replay of the first, and only one of them wins
  the compare-and-set that moves the schedule on.
- **No catch-up storm.** After downtime the next due time is computed from
  now, so a schedule that missed a hundred runs runs once.

A run is never created before its due time, and `not_before` on ordinary
requests reuses the same queue mechanism (`next_attempt_at`) for one-off
delays. Runs default to `cache: refresh` so a daily rerun sees today's
inputs; the run job carries the schedule's priority and client, and is read,
listed and cancelled like any request.

## Completion callbacks

A request may name a `callback_url`. The terminal transition (completed,
failed or cancelled) inserts a delivery row in its own transaction; a
deliverer loop POSTs the job document with an HMAC-SHA256 signature,
retrying on transport errors and 5xx with the worker backoff, giving up on
other 4xx, redirects or an exhausted attempt budget. The API reports the
delivery under `callback` on the request. Callback URLs are the one place
request data names an outbound destination, so they are fenced: https only,
host allow-listed in configuration, redirects never followed, the receiver's
answer read only up to 1 KiB.

## Restart safety

- On startup, jobs stuck in `resolving`/`forwarding` are requeued. Processing
  restarts from the stored original payload; the series cache makes the redo
  cheap.
- Jobs in `awaiting_target` keep their state and resume polling — the target
  job is never submitted twice.
- The housekeeping sweeper additionally rescues jobs left in
  `resolving`/`forwarding` without a schedule while the process kept running
  (a failed bookkeeping write) once they have been untouched for twice
  `worker.job_timeout`.
- On shutdown, the HTTP server drains first, then workers abort their
  upstream calls and park in-flight jobs back to `received`.

## Trust and security

- Client API keys are checked in constant time and never persisted: the key is
  stripped before the payload is stored.
- Target/resource credentials live only in the YAML config (via environment
  variables) and are injected into outbound requests at send time.
- All outbound URLs come from configuration — request data can only select
  dictionary entries, never supply a URL. The one exception, a request's
  `callback_url`, is accepted only for https and a host listed in
  `callbacks.allowed_hosts`. Values that do reach a URL are
  path-escaped: target-supplied job ids are additionally validated against a
  strict charset before they are substituted into poll/result URL templates,
  and `{field}` placeholders accept only string or number values.
- Outbound calls never follow redirects (Go's default policy would forward
  the injected API-key headers to cross-origin redirect targets), and
  upstream error excerpts have all configured credentials redacted before
  they are stored or logged.
- Inbound bodies are capped by `server.max_body_bytes`, upstream JSON
  responses by `upstream.max_response_bytes` and result downloads by
  `storage.max_result_bytes`; TLS termination is expected at a reverse
  proxy.

## Operability

- **Configuration** is loaded once into a provider and re-read on `SIGHUP`:
  a file that fails validation is rejected and the running configuration
  kept, a good one is swapped in atomically. The API reads the current
  configuration per request and a worker per job, so a reload never changes
  a request midway; settings read once at startup (listeners, storage,
  worker counts) are logged as `restart_required`. A key may carry a
  `previous_key` during a rotation.
- **Long-polls** (`GET /v1/requests/{id}?wait=`) park on an in-process
  notifier that workers and cancellations signal on every terminal
  transition, with a periodic re-read as the fallback.
- **Metrics** are served on a second listener (`server.metrics_addr`),
  never on the API's; `GET /version` and `/healthz` report the build.
  Structured logs and the `job_events` audit trail cover the rest.
- **Backups** are `VACUUM INTO` snapshots taken by `tentacron backup` while
  the service runs; the housekeeping sweep compacts the database file.
- **The API contract** is the OpenAPI document under `docs/openapi`,
  embedded into the binary and served at `GET /openapi.yaml`; tests keep it
  true to the routes and to the golden corpus
  ([ADR-0007](decisions/0007-openapi-contract.md)).

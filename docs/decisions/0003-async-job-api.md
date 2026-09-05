# ADR-0003: The client API is asynchronous: 202 plus polling

- **Status:** accepted
- **Date:** 2026-08-29

## Context

Resolving a payload can take minutes: resource APIs run simulations, and
targets such as MEME queue their own jobs for up to half an hour. A
synchronous HTTP request that long is fragile behind proxies and load
balancers, blocks the client, and cannot be retried without side effects.

## Decision

`POST /v1/requests` persists the request and answers `202 Accepted` with
an id at once. Clients read progress and the result from
`GET /v1/requests/{id}` (and the result body from `/result`). Every state
transition is recorded, and an `Idempotency-Key` lets a client resubmit
safely.

## Consequences

- Clients must poll; the response carries the state machine so a client can
  show progress. Callbacks and long-polling are additive extensions on top
  of this contract, not replacements.
- Work happens in a worker pool decoupled from the HTTP request, which is
  what makes retries, backoff and restart recovery possible.
- Results outlive the request and are pruned by a retention window; the
  client is responsible for fetching them in time.

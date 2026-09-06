# ADR-0008: TLS, rate limiting and connection limits belong to the reverse proxy

- **Status:** accepted
- **Date:** 2026-09-07

## Context

TentaCron listens on plain HTTP, limits nothing but the size of a request
body, and has no rate limiting or connection cap. Every deployment the
project describes — the Compose files, the container image, the sibling
services buem-gateway and weather — puts a reverse proxy in front of the
service: buem-gateway even leaves its API-key check to the proxy. The
documentation mentioned TLS at the proxy in passing; whether the absence
of rate limiting and connection limits was a decision or an omission was
not written down, so the question returned with every new deployment and
every review.

Two kinds of limit exist. Some need knowledge only the service has: which
key a request carries, how many of that key's jobs are queued or in
flight, what priority it may ask for. Others need none of that: how many
requests a second arrive from one address or one key, how many
connections are open, whether the transport is encrypted, which networks
may reach the service at all.

## Decision

TLS termination, rate limiting per address or per key, connection limits,
network allow-lists and any request-size limit beyond the body cap are the
reverse proxy's job. TentaCron keeps what needs its own knowledge:
authentication, the per-key ceilings (`max_concurrent`, `max_priority`,
`max_schedules`, `max_queued`), the body and response caps, and the poll
and result timeouts.

The operations page carries working proxy configurations for Caddy and
nginx, including the one rule a proxy must follow for this service: its
upstream read timeout must exceed `server.write_timeout`, or long-polls are
cut off by the proxy before they answer.

## Consequences

- A deployment without a proxy is unsupported and the documentation says
  so; the metrics listener stays on a private address for the same reason.
- Rate-limit policy lives in the proxy's configuration, close to the
  deployment that needs it, and changes without a release of TentaCron.
- Abuse that a proxy cannot see — one key flooding the queue — is answered
  inside the service by the per-key ceilings, which is why those exist.
- The decision is revisited if TentaCron is ever exposed at an edge without
  a proxy, or if a limit turns out to need queue knowledge the proxy does
  not have; then that limit, and only that limit, moves into the service.

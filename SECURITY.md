# Security Policy

## Supported versions

| Version | Supported |
|---|---|
| latest minor release (`0.x`) | yes |
| earlier releases | no — upgrade first |

Fixes ship in a new minor or patch release of the latest line; there are no
backports before 1.0.

## Reporting a vulnerability

Please do not open a public issue for a security problem. Use GitHub's
private vulnerability reporting for this repository:

<https://github.com/enerplanet/tentacron/security/advisories/new>

Include the tentacron version or commit, the relevant parts of your
configuration with credentials removed, and a reproduction or a description
of the impact. You will get an acknowledgement within five working days. We
aim to publish a fix or a mitigation within thirty days of confirming the
report and will credit you in the release notes unless you prefer otherwise.

## Scope

In scope: the tentacron service itself — the HTTP API, authentication, the
worker pipeline, configuration handling, the outbound client, the container
image and the release artifacts.

Out of scope: vulnerabilities in the upstream services tentacron talks to
(meme, buem-gateway, weather, city2tabula, ignis, or your own resource
APIs). Report those to their maintainers; if tentacron's handling of an
upstream response makes an issue worse, that part is in scope.

## What is already in place

- Client API keys are compared in constant time and never persisted or
  forwarded; upstream credentials live only in configuration and are
  redacted from every stored or logged error excerpt.
- API keys can be rotated without a gap (`previous_key` plus a `SIGHUP`
  reload), so a leaked key is replaced without downtime.
- All outbound URLs come from configuration. Request data can only select
  configured entries; values that reach a URL are path-escaped and
  target-supplied job ids are validated against a strict character set.
  The one exception is a request's `callback_url`, which is accepted only
  for https and a host listed in `callbacks.allowed_hosts`.
- Outbound calls never follow redirects, and request and response bodies
  are size-capped.
- `govulncheck` runs in CI on every push and Dependabot keeps the module
  graph, the Actions pins and the base image current.
- The release image is distroless and runs as a non-root user.

See [docs/architecture.md](docs/architecture.md#trust-and-security) for the
threat model behind these choices and
[docs/operations.md](docs/operations.md#deployment) for the deployment
assumptions (TLS at the reverse proxy, secrets via environment).

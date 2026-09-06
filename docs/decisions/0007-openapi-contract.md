# ADR-0007: The API contract is a hand-written OpenAPI 3.1 document kept true by tests

- **Status:** accepted
- **Date:** 2026-09-06

## Context

Two ways of describing an HTTP API exist in the enerplanet organisation.
The EnerPlanET backend generates a Swagger 2.0 document from swaggo
annotations on its gin handlers; the generation is a manual `swag init`,
nothing in CI checks that the document still matches the code. The
service repositories buem-gateway and weather hand-write an OpenAPI 3.0.3
document at `docs/openapi/openapi.yaml`, render it with a standalone
Swagger UI page next to it, and lint it with Redocly in the docs
workflow; their consumers in the backend are hand-written Go clients, not
generated ones. Nothing consumes tentacron's description yet.

Tentacron's contract contains things OpenAPI 3.0.3 cannot express: the
completion callback is a webhook, `result` and `error` are `null` or an
object (3.0 has only a `nullable` flag), and the health statuses are
constants. The server registers its routes from one table, and the golden
end-to-end corpus freezes every response the service answers, so the
material to check a description against the code exists already.

## Decision

The API is described by one hand-written OpenAPI 3.1 document,
`docs/openapi/openapi.yaml`, in the layout the sibling services use. It
is the single source of truth: the standalone `docs/openapi/index.html`
renders it, a small `docs/openapi` Go package embeds it, and the binary
serves it at `GET /openapi.yaml`, so a running service describes its own
build.

The document is not generated from the code; it is kept true by tests
instead. The registered routes must match its paths exactly
(`internal/api`), every response the golden corpus records must use a
documented status code and media type and validate against the schema
with format assertions on, every file under `examples/` must validate
against `CreateRequest` (`test/e2e`, with pb33f's libopenapi-validator),
and Redocly's recommended ruleset lints it in CI, with the few deliberate
exceptions listed with reasons in `.redocly.lint-ignore.yaml`.

Version 3.1 rather than the siblings' 3.0.3, because webhooks, `null` as
a type and `const` describe the contract as it is; Swagger UI 5 and
Redocly support it fully. swaggo was not adopted: it emits Swagger 2.0,
cannot express those constructs, annotates handlers tentacron does not
have (net/http, not gin), and would move the source of truth into
comments that nothing checks.

## Consequences

The description is a checked contract, not prose: an undocumented route,
an unlisted error code, a response field the schema does not know, or an
example the schema rejects fails the build rather than surfacing in a
client later. The viewer, the lint and the file layout are the same as in
the sibling repositories, so a reader of one finds their way in the
other.

Every API change edits the document by hand; the tests say when it has
been forgotten. The validation adds two MIT-licensed, test-only modules
(`libopenapi`, `libopenapi-validator`). Tools that parse with kin-openapi
read 3.0 only; a consumer generating a client needs a 3.1-capable
generator, which openapi-generator and Redocly are.

The decision is revisited if a consumer needs a 3.0.3 document — the
downgrade is mechanical: `nullable` flags, `enum` for the constants, the
webhook moved into prose — or if the organisation standardises on one
client generator, in which case the description is its input and this
record only changes in the tooling paragraph.

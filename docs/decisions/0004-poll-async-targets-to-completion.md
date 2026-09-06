# ADR-0004: TentaCron polls asynchronous targets to completion

- **Status:** accepted
- **Date:** 2026-08-29

## Context

Some targets are themselves asynchronous: MEME answers `202` with a job id
and exposes status and result endpoints. TentaCron could hand that id back
to the client and stop, or it could follow the target job itself. Clients
should not need to know each target's polling protocol, credentials or
result format.

## Decision

For a target configured with `response.mode: poll`, tentacron extracts the
target's job id, polls the configured status URL on the target's interval
until a done or failed value appears (or the poll deadline passes), fetches
the result, and stores it as the request's result. Poll ticks are claimed
atomically so concurrent workers never poll the same job twice, and polling
resumes after a restart without resubmitting the target job.

## Consequences

- The client sees one state machine and one result location regardless of
  whether the target was synchronous or asynchronous.
- Target credentials never leave tentacron; the client never talks to the
  target.
- TentaCron holds state for the target job's lifetime, so poll deadlines,
  cancellation and result size limits are tentacron's responsibility.
- The poll protocol is described entirely in configuration (id path, URL
  templates, status path, done and failed values), so a new asynchronous
  target needs no code.

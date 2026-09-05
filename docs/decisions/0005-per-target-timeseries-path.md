# ADR-0005: Resolvent location and marker policy are per target

- **Status:** accepted
- **Date:** 2026-08-29

## Context

Targets disagree about where time series live in a payload: MEME keeps a
registry under `model.timeseries`, the generic examples use a top-level
`time-series` array, and buem-gateway expects the weather block at the
payload root. Scanning the whole payload for every target would resolve
objects in places a target never reads, and some targets validate their
schema strictly, rejecting tentacron's traceability marker.

## Decision

Each target configures `timeseries_path`, the dot-separated container in
which resolvent objects are searched (`"."` for the payload root), and
`attach_resolvent`, whether the original resolvent object is kept under the
substituted series' `resolvent` key. The default is the `time-series`
container with the marker attached.

## Consequences

- The same resolvent types serve every target; only the container and the
  marker policy differ, and both are visible in the configuration.
- A payload whose container is missing has nothing to resolve and is
  forwarded as-is, which is not an error.
- When the marker is switched off, traceability lives in tentacron's audit
  store (original payload, resolved payload, events) rather than in the
  forwarded document.
- Proxy targets opt out of resolution entirely and reject both keys, so a
  configuration cannot suggest resolution that never happens.

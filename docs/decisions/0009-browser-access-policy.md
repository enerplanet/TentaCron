# ADR-0009: The browser access policy lives in the service, with meme's semantics

- **Status:** accepted
- **Date:** 2026-09-06

## Context

Two sibling services answer browsers: meme, a stateless simulation API,
and TentaCron, the job queue in front of it. A frontend that talks to both
met two policies with different edges. TentaCron's, from 0.2.0, allowed
exact origins only, by a deliberate decision: a key embedded in a page
should be usable from that page's origin and nowhere else. meme allows the
`*` origin and subdomain wildcards, credentials, Chrome's private-network
preflights and custom header lists, and documents a curl check.

Since 0.3.0 a key carries ceilings of its own, `max_queued`,
`max_concurrent`, `max_priority` and `max_schedules`, has a role that
scopes what it reads, and can be revoked on its own. What a leaked key
can do is bounded by configuration, which is the argument the 0.2.0
decision lacked. TentaCron also has needs a stateless service does not: a
page must read `Retry-After` on a `429`, an added frontend origin must not
cost a restart that drains the queue, and an operator must see in the log
why a page was refused.

ADR-0008 assigns TLS, rate limiting and connection limits to the reverse
proxy and keeps what needs the service's own knowledge in the service.
Which headers the handlers set and read is exactly such knowledge.

## Decision

The browser policy is implemented in the service, in `internal/cors`,
with the origin semantics of meme's `internal/api/cors.go`: exact origins,
the `*` origin, subdomain wildcards matching any depth below the host and
never the host itself, the literal `null` only when listed, matched
case-insensitively; preflights answered before the mux and before
authentication for every path, a denied one with a bare `204` and the full
`Vary` set; credentials, preflight max-age and private-network preflights
opt-in under the same names, validated by the Fetch specification's rules
(no `*` origin, exposed header or method with credentials; a credentialed
response always echoes the specific origin).

Where the two services differ, TentaCron keeps its own:

- The allowed and exposed header sets are derived from what the handlers
  read and set, kept true by a test that scans the handlers, and the
  configuration's `allowed_headers` and `expose_headers` are additions to
  those sets rather than replacements. meme's lists replace its defaults;
  a replacement here would let a configuration break the page.
- The allowed methods are every method the API has and are not
  configurable; a proxy cannot add methods.
- The policy is swapped on `SIGHUP`, since a restart drains the queue.
- The request log names a browser's origin and says when the policy
  refused it; preflights log at debug.

The `*` origin is allowed and warned about by `tentacron validate` and the
start-up log. The reverse proxy passes `OPTIONS` and `Origin` through
untouched and adds no `Access-Control-*` header of its own.

## Consequences

- A key in a page is a supported deployment with named limits: a `client`
  key of its own, its ceilings, revocation, and the backend-for-frontend
  pattern where the key must stay secret. The Browser clients page states
  this in plain words.
- The 0.2.0 exact-origin rule is lifted; an operator who sets `*` for a
  key with the admin role exposes every client's requests to any page
  that learns the key. The warning names it; nothing prevents it, because
  the same operator can revoke the key.
- A denied preflight answers `204` where it answered `405`; the change is
  recorded in the changelog and in the browser-preflight golden.
- Two implementations exist, one per service, and stay comparable by
  sharing test names rather than code.

## Alternatives considered

- **CORS at the reverse proxy.** Rejected: the proxy does not know which
  headers the handlers set and read, so its header sets would drift from
  the API's, and a preflight answered by the proxy could not be validated
  by the service's tests.
- **Exact origins only, as in 0.2.0.** Rejected: preview deployments and
  public read-only frontends need patterns, the ceilings now bound a
  leaked key, and parity with meme is worth more to a frontend developer
  than the narrower rule.
- **A Go module shared with meme.** Rejected: the two APIs expose
  different headers and methods, meme configures from flags and TentaCron
  from YAML with reload, and a shared module would make both repositories
  release together. The test matrix shares names with meme's instead, so
  the two stay comparable by reading.

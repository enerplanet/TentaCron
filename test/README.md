# `test/` — the test pyramid

Everything about testing tentacron lives behind [`test/Makefile`](Makefile);
the root Makefile aliases the E2E targets, so `make e2e` and
`make -C test e2e` are the same thing.

```bash
make test           # vet + full suite: unit, integration, golden E2E (seconds)
make test-race      # race detector, shuffled order
make e2e            # golden end-to-end corpus only, verbose
make golden-update  # accept an INTENDED behavior change
make fuzz           # every fuzz target for FUZZTIME (default 20s) each
make stress         # race detector, shuffled, repeated STRESS_COUNT times
make live           # one real request through real upstreams (env-gated)
```

Every tier except `live` runs on the host in seconds — no Docker, no
network, no external services — and is part of plain `go test ./...`, so CI
always runs all of it (the live tier self-skips without its env vars).

## The layers, bottom to top

### 1. Unit and integration tests (`internal/…`, milliseconds)

Live next to their packages:

| Area | What is proven |
|---|---|
| `internal/config` | YAML loading, `${ENV}` interpolation, every validation rule |
| `internal/resolver` | traversal/substitution engine, canonical hashing, number fidelity, null rejection |
| `internal/store` | migrations, claim CAS races, terminal-state guard, idempotency scoping/conflict, backoff scheduling, retention, the SQLITE_BUSY regression |
| `internal/upstream` | error classification, redirect refusal, credential redaction, job-id validation, size caps |
| `internal/api` | every 4xx path, idempotency replay, result serving |
| `internal/worker` | full pipeline against `httptest` fakes: retries, cache, recovery, shutdown parking, poll deadlines |

Each package also carries an `*_edge_test.go` file: the boundary and
failure cases that the happy paths above do not reach — config validation
corners, URL-mapping edge cases, poll-mode failure branches, claim and list
semantics, API input boundaries, and worker concurrency bounds.

### 1b. Fuzz targets (`internal/resolver`, `internal/upstream`)

Go-native fuzzers for the parsers that face untrusted input: the payload
walker and substitution engine, the GET query/path mapper, target URL
templating, dot-path navigation, job-id extraction, and the error-excerpt
redaction. Their seed corpora run as ordinary tests in `make test`; `make
fuzz` (and a CI smoke job) mutate them for `FUZZTIME` each. A crash found
by fuzzing is written to `testdata/fuzz/<Target>/` — commit it, it becomes a
regression test.

### 2. Golden end-to-end corpus ([`test/e2e`](e2e), the regression tripwire)

Each scenario boots the **complete service in-process** — HTTP API, SQLite
store, worker pool, outbound client — against scripted upstream fakes, then
freezes everything observable into one transcript per scenario under
[`e2e/testdata/golden/`](e2e/testdata/golden):

- every API response (status, body, error envelope),
- the payload the target actually received (substitution, pass-through,
  key injection, number fidelity),
- the job's audit trail (`job_events`),
- upstream call counts (dedup and cache reuse are visible as counts).

Volatile values are normalized (`«job-N»`, `«ts»`, `retrying in «dur»`), so
goldens compare byte-for-byte and stay deterministic — the suite is verified
over repeated and race-detector runs.

The corpus covers the edge cases end to end: resolvent dedup and cache reuse,
retry/backoff for transient resource *and* target failures, attempt
exhaustion, permanent resource/target 4xx, malformed/null resource bodies,
target errors with credential redaction, transient status-poll and
result-fetch flakes, poll deadlines (exceeded and straddled), the mid-flight
`awaiting_target` state, malformed/unsafe target job ids, binary and
number-heavy results, both API-key injection modes (header and body-field,
pinned via the captured upstream requests), the real buem-gateway contract
(root-level weather resolvent substituted marker-free via
`timeseries_path: "."` + `attach_resolvent: false`, per-building error
entries inside a 200 completing the job with the errors as result data,
never as a job failure, and target composition — a `resolvent-buem` backed
by the buem-building target feeding a MEME model), proxy targets (payload
handed through unresolved, URL `{field}` templating with body stripping —
the ignis calculate exemplar), GET resolvents against
the verified weather/city2tabula/ignis contracts (query-parameter and
path-template mapping frozen as exact request lines, array responses indexed
via `response_path`), the full validation-error surface, idempotency
replay/conflict/cross-client scoping, cross-client read visibility, and list
ordering. Every counts step also freezes an
`unexpected_upstream_requests` tripwire (always empty), so any unscripted
outbound request becomes a golden diff, and a corpus gate fails on stale
golden files whose scenario no longer exists.
[`e2e/examples_test.go`](e2e/examples_test.go) additionally runs every
payload in [`examples/`](../examples) through the stack — the drift gate
that keeps published examples true to the contract. Adding an example means
adding the file, a scenario there, and running `make golden-update` once to
freeze its transcript.
[`e2e/openapi_test.go`](e2e/openapi_test.go) validates every response the
corpus records against the OpenAPI description
([`docs/openapi/openapi.yaml`](../docs/openapi/openapi.yaml)): status
code documented for the operation, media type declared, body conforming
to the schema, format assertions on. Together with the route test in
`internal/api` this makes the description a checked contract rather than
prose — a response field or error code the description does not list is
a failing test, not a documentation issue found later.

**Workflow:** change behavior → `make test` fails with a readable transcript
diff → if the change is intended, `make golden-update` and review the golden
diff in `git diff` like any other code change. Read a regenerated golden
before accepting it: a scenario whose comment promises one outcome while
its transcript freezes another is a silent gap, not a passing test.

**Flakiness hunt:** `make stress` runs everything under the race detector,
shuffled, `STRESS_COUNT` times; the goldens and the timing-sensitive worker
tests are expected to survive it unchanged.

### 3. Live tier ([`test/live`](live), env-gated)

One real request through **real upstreams**: boots the full stack from an
operator-supplied config (real URLs, real credentials via `${ENV}`
interpolation; the job store lives in a temp dir), sends one request file,
and follows the job to a terminal state, logging every state transition, the
audit trail, and the result. The go-to answer for "does my config actually
work against the deployed meme/buem/weather?":

```bash
TENTACRON_LIVE_CONFIG=./config.yaml \
TENTACRON_LIVE_REQUEST=./examples/buem-buildings.json \
make live                              # TENTACRON_LIVE_TIMEOUT=30m for slow runs
```

Without both variables the tier skips instantly, so it never touches CI.

## Why there is no meme container tier

meme's own E2E runs real solvers in a container because meme's job *is* the
solvers. Tentacron's job is orchestration: its outbound contract is the
generic accept/status/result HTTP protocol described entirely by its YAML
config, and the golden corpus exercises that protocol — including MEME-style
async polling — with deterministic fakes. A real meme container would test
meme's solver stack, not tentacron, and would make golden transcripts
non-reproducible. The env-gated [live tier](#3-live-tier-testlive-env-gated)
covers the "does the real deployment answer" question with one real request,
without ever entering CI or the golden corpus.

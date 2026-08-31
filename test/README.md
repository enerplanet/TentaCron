# `test/` — the test pyramid

Everything about testing tentacron lives behind [`test/Makefile`](Makefile);
the root Makefile aliases the E2E targets, so `make e2e` and
`make -C test e2e` are the same thing.

```bash
make test           # vet + full suite: unit, integration, golden E2E (seconds)
make test-race      # race detector, shuffled order
make e2e            # golden end-to-end corpus only, verbose
make golden-update  # accept an INTENDED behavior change
```

Every tier runs on the host in seconds — no Docker, no network, no external
services. The whole suite is part of plain `go test ./...`, so CI always runs
all of it.

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
`timeseries_path: "."` + `attach_resolvent: false`), the full validation-
error surface, idempotency replay/conflict/cross-client scoping, cross-client
read visibility, and list ordering. Every counts step also freezes an
`unexpected_upstream_requests` tripwire (always empty), so any unscripted
outbound request becomes a golden diff, and a corpus gate fails on stale
golden files whose scenario no longer exists.
[`e2e/examples_test.go`](e2e/examples_test.go) additionally runs every
payload in [`examples/`](../examples) through the stack — the drift gate
that keeps published examples true to the contract.

**Workflow:** change behavior → `make test` fails with a readable transcript
diff → if the change is intended, `make golden-update` and review the golden
diff in `git diff` like any other code change.

## Why there is no meme container tier

meme's own E2E runs real solvers in a container because meme's job *is* the
solvers. Tentacron's job is orchestration: its outbound contract is the
generic accept/status/result HTTP protocol described entirely by its YAML
config, and the golden corpus exercises that protocol — including MEME-style
async polling — with deterministic fakes. A real meme container would test
meme's solver stack, not tentacron, and would make golden transcripts
non-reproducible. If a live integration check against a running meme is ever
wanted, add an env-gated test (`TENTACRON_LIVE_TARGET=…`) pointing a config
at the real service — no code change needed, the poll protocol is
config-driven.

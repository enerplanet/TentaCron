# Architecture decisions

Decisions that shape tentacron and are not derivable from the code alone
are recorded here as lightweight architecture decision records (ADRs). Each
record states the context, the decision and its consequences, so a later
reader knows why the service looks the way it does before proposing to
change it.

| ADR | Decision | Status |
|---|---|---|
| [0001](0001-go-single-binary.md) | Implement tentacron in Go as one static binary | accepted |
| [0002](0002-sqlite-store-and-queue.md) | SQLite is the store and the job queue; one instance | accepted |
| [0003](0003-async-job-api.md) | The client API is asynchronous: 202 plus polling | accepted |
| [0004](0004-poll-async-targets-to-completion.md) | Tentacron polls asynchronous targets to completion | accepted |
| [0005](0005-per-target-timeseries-path.md) | Resolvent location and marker policy are per target | accepted |

## Adding a record

Copy [0000-template.md](0000-template.md) to the next free number, fill it
in, add a row above, and link it from the pull request. A record is never
edited once accepted; a later decision that replaces it gets a new number
and marks the old one *superseded*.

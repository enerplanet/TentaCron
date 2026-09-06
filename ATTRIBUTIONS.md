# Attributions

TentaCron is licensed under the [MIT License](LICENSE). It builds on the Go
standard library and the following direct third-party modules (see
[`go.mod`](go.mod) for versions):

| Module | Purpose | License |
|---|---|---|
| [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) | pure-Go SQLite driver behind the job store, audit trail and series cache | BSD-3-Clause (SQLite itself is public domain) |
| [gopkg.in/yaml.v3](https://github.com/go-yaml/yaml) | configuration parsing | MIT and Apache-2.0 |
| [github.com/prometheus/client_golang](https://github.com/prometheus/client_golang) | Prometheus metrics registry and exposition (`/metrics`) | Apache-2.0 |
| [github.com/pb33f/libopenapi](https://github.com/pb33f/libopenapi) and [libopenapi-validator](https://github.com/pb33f/libopenapi-validator) | tests only: load the OpenAPI 3.1 description and validate the golden corpus and the examples against it | MIT |

The documentation site uses [MkDocs](https://www.mkdocs.org/) with the
[Material for MkDocs](https://squidfunk.github.io/mkdocs-material/) theme
(both MIT), pinned in [`docs/requirements.txt`](docs/requirements.txt).

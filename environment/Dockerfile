# syntax=docker/dockerfile:1
#
# Build-and-test environment for the tentacron service.
#
# One image that carries everything needed end-to-end:
#   * Go 1.26 toolchain -> build the service and run `go test ./...`
#   * make              -> the container runs the same targets as a local checkout
#   * sqlite3 CLI       -> inspect the job store/audit trail (docs/operations.md)
#
# The repo root is bind-mounted at /src by docker-compose, so source edits are
# picked up without a rebuild — only the toolchain layer is baked in.

FROM golang:1.26-bookworm
ENV GOTOOLCHAIN=local

RUN apt-get update && apt-get install -y --no-install-recommends \
        make \
        sqlite3 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src

# Pre-warm the Go module cache so the first containerized build is quick.
# NOTE: paths are relative to the build context, which is the repo root
# (compose sets context: ..).
COPY go.mod go.sum ./
RUN go mod download

EXPOSE 8080

# Default: build then run the API via the project Makefile so the container
# runs the same commands as a local checkout. CONFIG is supplied by compose
# (environment/config.yaml). Override the command in compose for `make test`.
CMD ["make", "run"]

# syntax=docker/dockerfile:1
#
# Production image for tentacron: a static binary on a distroless base,
# running as the unprivileged nonroot user. Built and published by
# .github/workflows/release.yml on every v* tag as
# ghcr.io/enerplanet/tentacron. The development toolchain image (go, make,
# sqlite3, bind-mounted sources) lives in environment/Dockerfile.
#
#   docker build --build-arg VERSION=$(git describe --tags --always) -t tentacron .
#   docker run --rm -p 8080:8080 \
#     -v "$PWD/config.yaml:/etc/tentacron/config.yaml:ro" \
#     -v tentacron-data:/data \
#     -e TENTACRON_KEY_FRONTEND=... -e MEME_API_KEY=... tentacron
#
# The config should point storage.path and storage.results_dir under /data,
# the only writable location in the image.

ARG GO_VERSION=1.26

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/tentacron ./cmd/tentacron \
 && mkdir /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/tentacron /usr/local/bin/tentacron
# An empty, nonroot-owned /data so the default relative storage paths and a
# fresh named volume are writable without a chown step.
COPY --from=build --chown=nonroot:nonroot /out/data /data
WORKDIR /
VOLUME ["/data"]
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/tentacron"]
CMD ["-config", "/etc/tentacron/config.yaml"]

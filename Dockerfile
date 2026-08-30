# Build stage
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=${VERSION}" \
    -o /out/tentacron ./cmd/tentacron

# Runtime stage: static binary, no shell, non-root
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/tentacron /usr/bin/tentacron
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/usr/bin/tentacron"]
CMD ["-config", "/etc/tentacron/config.yaml"]

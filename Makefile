BINARY   := tentacron
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -X main.version=$(VERSION)
CONFIG   ?= config.example.yaml

# Containerized build/test/run lives in environment/ (make -C environment help).

.PHONY: build test test-race cover lint run tidy clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/$(BINARY)

test:
	go test ./...

test-race:
	go test -race -shuffle=on ./...

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

lint:
	go vet ./...
	golangci-lint run

run: build
	./bin/$(BINARY) -config $(CONFIG)

tidy:
	go mod tidy

clean:
	rm -rf bin coverage.out

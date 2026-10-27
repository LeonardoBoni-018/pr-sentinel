BIN     := bin/pr-sentinel
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test cover lint run

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/pr-sentinel

test:
	go test -race ./...

cover:
	go test -race -coverprofile=coverage.out ./internal/...
	go tool cover -func=coverage.out | tail -n 1

lint:
	golangci-lint run ./...

run: build
	$(BIN) server

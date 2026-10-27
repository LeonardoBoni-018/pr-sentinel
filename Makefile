BIN       := bin/pr-sentinel
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -s -w -X main.version=$(VERSION)
MIN_COVER ?= 70

.PHONY: build test cover cover-check lint run docker

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/pr-sentinel

test:
	go test -race ./...

cover:
	go test -race -coverprofile=coverage.out ./internal/...
	go tool cover -func=coverage.out | tail -n 1

# Falha se a cobertura total do núcleo (internal/...) ficar abaixo de MIN_COVER%.
cover-check:
	@total=$$(go tool cover -func=coverage.out | awk '/^total:/ {gsub("%","",$$3); print $$3}'); \
	echo "cobertura total: $$total% (mínimo $(MIN_COVER)%)"; \
	awk -v t="$$total" -v m="$(MIN_COVER)" 'BEGIN { exit (t+0 >= m+0) ? 0 : 1 }'

lint:
	golangci-lint run ./...

# Requer ANTHROPIC_API_KEY, GITHUB_TOKEN e WEBHOOK_SECRET no ambiente.
run: build
	$(BIN) server

docker:
	docker build --build-arg VERSION=$(VERSION) -t pr-sentinel:$(VERSION) .

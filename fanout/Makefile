PREFIX ?= $(HOME)/.local
BIN := fanout

.PHONY: build test cover mutation lint install install-skill e2e

build:
	go build -o $(BIN) ./cmd/fanout

test:
	go test -race ./...

cover:
	go test -cover ./internal/...

mutation:
	./mutation.sh

lint:
	go vet ./... && golangci-lint run ./...

install: build
	install -m 0755 $(BIN) $(PREFIX)/bin/$(BIN)

install-skill: build
	./$(BIN) install-skill

# Real agents through real hc (costs a few cents of model time).
e2e: build
	./e2e.sh

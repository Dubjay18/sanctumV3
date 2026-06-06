# Project-wide Makefile for sanctum

GO ?= go
BINDIR ?= bin
IMAGE ?= sanctum:latest

.PHONY: help deps fmt lint test build server client run-server run-client dev air docker-build clean

help:
	@echo "Makefile targets:"
	@echo "  help           Show this help"
	@echo "  deps           Download Go module dependencies"
	@echo "  fmt            Run gofmt on repository"
	@echo "  lint           Run static checks (golangci-lint if available, else go vet)"
	@echo "  test           Run unit tests"
	@echo "  build          Build both server and client into $(BINDIR)/"
	@echo "  server         Build server binary"
	@echo "  client         Build client binary"
	@echo "  run-server     Run the server (using go run)"
	@echo "  run-client     Run the client (using go run)"
	@echo "  docker-build   Build Docker image named $(IMAGE)"
	@echo "  air            Run live-reload dev server using air (alias for air-server)"
	@echo "  dev            Alias for 'air-server' target"
	@echo "  air-server     Run server with air using .air.server.toml"
	@echo "  air-client     Run client with air using .air.client.toml"
	@echo "  clean          Remove build artifacts"

deps:
	@echo "Downloading module dependencies..."
	$(GO) mod download

fmt:
	@echo "Formatting Go sources..."
	@gofmt -s -w .

lint:
	@echo "Running linters..."
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		$(GO) vet ./...; \
	fi

test:
	@echo "Running tests..."
	$(GO) test ./...

build: server client
	@echo "Built all binaries in $(BINDIR)/"

server:
	@mkdir -p $(BINDIR)
	$(GO) build -o $(BINDIR)/server ./cmd/server

client:
	@mkdir -p $(BINDIR)
	$(GO) build -o $(BINDIR)/client ./cmd/client

run-server:
	$(GO) run ./cmd/server

run-client:
	$(GO) run ./cmd/client

air:
	@echo "Starting live-reload with air..."
	@if ! command -v air >/dev/null 2>&1; then \
		echo "\nair is not installed. Install with:\n  curl -sSfL https://raw.githubusercontent.com/cosmtrek/air/master/install.sh | sh\n"; exit 1; \
	fi
	air -c .air.server.toml

dev: air-server

air-server:
	@echo "Starting server live-reload (air -c .air.server.toml)..."
	@if ! command -v air >/dev/null 2>&1; then \
		echo "\nair is not installed. Install with:\n  curl -sSfL https://raw.githubusercontent.com/cosmtrek/air/master/install.sh | sh\n"; exit 1; \
	fi
	go tool air -c .air.server.toml

air-client:
	@echo "Starting client live-reload (air -c .air.client.toml)..."
	@if ! command -v air >/dev/null 2>&1; then \
		echo "\nair is not installed. Install with:\n  curl -sSfL https://raw.githubusercontent.com/cosmtrek/air/master/install.sh | sh\n"; exit 1; \
	fi
	go tool air -c .air.client.toml


docker-build:
	@echo "Building Docker image $(IMAGE)..."
	docker build -t $(IMAGE) .

clean:
	@echo "Cleaning build artifacts..."
	@rm -rf $(BINDIR)


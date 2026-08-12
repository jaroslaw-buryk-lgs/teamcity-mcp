# TeamCity MCP Server Makefile

# Variables
BINARY_NAME=teamcity-mcp
IMAGE_NAME=teamcity-mcp
VERSION ?= $(shell git describe --tags --always --dirty)
LDFLAGS=-ldflags "-X main.appVersion=$(VERSION)"

# Go related variables
GOBASE=$(shell pwd)
GOBIN=$(GOBASE)/bin

# Build flags
BUILD_FLAGS=-v $(LDFLAGS)

.PHONY: help build test test-unit clean docker run dev deps lint format \
        install-service uninstall-service service-status service-logs

## help: Show this help message
help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@awk '/^##/ { printf "  %-15s %s\n", $$2, substr($$0, index($$0, $$3)) }' $(MAKEFILE_LIST)

## build: Build the binary
build:
	@echo "Building $(BINARY_NAME)..."
	@go build $(BUILD_FLAGS) -o $(GOBIN)/$(BINARY_NAME) ./cmd/server

## test: Run tests
test:
	@echo "Running tests..."
	@go test -v -race -coverprofile=coverage.out ./...
	@go tool cover -html=coverage.out -o coverage.html

## test-unit: Run unit tests only (no coverage report)
test-unit:
	@echo "Running unit tests..."
	@go test -race ./internal/... ./tests/unit/...

## test-integration: Run integration tests with Docker
test-integration:
	@echo "Running integration tests..."
	@docker-compose -f docker-compose.test.yml up --build --abort-on-container-exit --exit-code-from test

## test-load: Run load tests
test-load:
	@echo "Running load tests..."
	@k6 run tests/load/test.js

## clean: Clean build artifacts
clean:
	@echo "Cleaning..."
	@go clean
	@rm -rf $(GOBIN)
	@rm -f coverage.out coverage.html

## deps: Download dependencies
deps:
	@echo "Downloading dependencies..."
	@go mod download
	@go mod tidy

## lint: Run linters
lint:
	@echo "Running linters..."
	@golangci-lint run ./...

## format: Format code
format:
	@echo "Formatting code..."
	@go fmt ./...
	@goimports -w .

## docker: Build Docker image
docker:
	@echo "Building Docker image..."
	@docker build -t $(IMAGE_NAME):$(VERSION) .
	@docker tag $(IMAGE_NAME):$(VERSION) $(IMAGE_NAME):latest

## docker-push: Push Docker image
docker-push: docker
	@echo "Pushing Docker image..."
	@docker push $(IMAGE_NAME):$(VERSION)
	@docker push $(IMAGE_NAME):latest

## run: Run the application
run: build
	@echo "Running $(BINARY_NAME)..."
	@$(GOBIN)/$(BINARY_NAME)

## run-stdio: Run in STDIO mode
run-stdio: build
	@echo "Running $(BINARY_NAME) in STDIO mode..."
	@$(GOBIN)/$(BINARY_NAME) -transport stdio

## install-service: Install as a systemd service that starts at boot (needs sudo)
install-service: build
	@sudo ./scripts/install-service.sh

## uninstall-service: Remove the systemd service (needs sudo)
uninstall-service:
	@sudo ./scripts/uninstall-service.sh

## service-status: Show service status and recent logs
service-status:
	@systemctl --no-pager --full status teamcity-mcp || true
	@echo ""
	@sudo journalctl -u teamcity-mcp -n 20 --no-pager || true

## service-logs: Follow the service logs
service-logs:
	@sudo journalctl -u teamcity-mcp -f

## dev: Run in development mode with hot reload
dev:
	@echo "Running in development mode..."
	@air -c .air.toml

## compose-up: Start services with Docker Compose
compose-up:
	@echo "Starting services..."
	@docker-compose up -d

## compose-down: Stop services
compose-down:
	@echo "Stopping services..."
	@docker-compose down

## compose-logs: Show logs
compose-logs:
	@docker-compose logs -f

## install-tools: Install development tools
install-tools:
	@echo "Installing development tools..."
	@go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
	@go install golang.org/x/tools/cmd/goimports@latest
	@go install github.com/cosmtrek/air@latest
	@go install github.com/goreleaser/goreleaser@latest

## release-snapshot: Build snapshot release with GoReleaser
release-snapshot:
	@echo "Building snapshot release..."
	@goreleaser release --snapshot --clean

## release-check: Check GoReleaser configuration
release-check:
	@echo "Checking GoReleaser configuration..."
	@goreleaser check

## ci: Run CI checks (same as check but more verbose)
ci: deps lint test build
	@echo "CI checks completed successfully!"

## check: Run all checks (lint, test, build)
check: lint test build
	@echo "All checks passed!"

# Default target
all: deps lint test build 
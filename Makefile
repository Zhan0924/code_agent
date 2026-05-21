.PHONY: build run test test-short test-cover lint clean docker-build docker-up docker-down migrate tidy generate openapi

APP_NAME := code-agent
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "v0.1.0")
BUILD_TIME := $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
LDFLAGS := -ldflags "-X main.Version=$(VERSION) -X main.BuildTime=$(BUILD_TIME)"
COVERAGE_OUT := coverage.out

# ─── Build & Run ───────────────────────────────────────────
build:
	@echo "==> Building $(APP_NAME)..."
	go build $(LDFLAGS) -o bin/$(APP_NAME) ./cmd/agent

run: build
	@echo "==> Running $(APP_NAME)..."
	./bin/$(APP_NAME)

# ─── Testing ───────────────────────────────────────────────
test:
	@echo "==> Running tests..."
	go test -race -cover ./...

test-short:
	@echo "==> Running short tests (no external deps)..."
	go test -short -race ./...

test-cover:
	@echo "==> Running tests with coverage..."
	go test -race -coverprofile=$(COVERAGE_OUT) -covermode=atomic ./...
	go tool cover -func=$(COVERAGE_OUT) | tail -1
	@echo "==> HTML coverage report: go tool cover -html=$(COVERAGE_OUT)"

# ─── Linting ──────────────────────────────────────────────
lint:
	@echo "==> Running linter..."
	golangci-lint run ./...

# ─── Database ─────────────────────────────────────────────
migrate:
	@echo "==> Running database migrations..."
	go run ./cmd/agent -config configs/config.yaml 2>&1 | head -5
	@echo "Migrations are auto-applied on startup. Use docker-up to start PostgreSQL."

# ─── Docker ───────────────────────────────────────────────
docker-build:
	docker build -t $(APP_NAME):$(VERSION) .

docker-up:
	docker compose up -d

docker-down:
	docker compose down -v

# ─── Maintenance ──────────────────────────────────────────
clean:
	@echo "==> Cleaning..."
	rm -rf bin/ $(COVERAGE_OUT)
	go clean

tidy:
	go mod tidy

generate:
	go generate ./...

# ─── API Documentation ───────────────────────────────────
openapi:
	@echo "==> OpenAPI spec at api/openapi.yaml"
	@cat api/openapi.yaml | head -5

# ─── Help ─────────────────────────────────────────────────
help:
	@echo "Available targets:"
	@echo "  build        - Build the binary"
	@echo "  run          - Build and run"
	@echo "  test         - Run all tests with race detector"
	@echo "  test-short   - Run short tests (no external deps)"
	@echo "  test-cover   - Run tests with coverage report"
	@echo "  lint         - Run golangci-lint"
	@echo "  migrate      - Run database migrations"
	@echo "  docker-build - Build Docker image"
	@echo "  docker-up    - Start all services (docker-compose)"
	@echo "  docker-down  - Stop all services"
	@echo "  clean        - Remove build artifacts"
	@echo "  tidy         - go mod tidy"
	@echo "  openapi      - Show OpenAPI spec location"

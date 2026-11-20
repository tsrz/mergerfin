export PATH := /usr/local/go/bin:/usr/local/bin:$(PATH)

BINARY_NAME := mergerfin
BUILD_DIR := bin
IMAGE_TAG ?= mergerfin:latest
COMPOSE_DEV := docker compose -f docker-compose.dev.yml

.PHONY: build test lint dev-up dev-seed dev-down docker-build

build:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/mergerfin

test:
	@if command -v gcc >/dev/null 2>&1; then \
		CGO_ENABLED=1 go test -v -race ./...; \
	else \
		CGO_ENABLED=0 go test -v -count=1 ./...; \
	fi

lint:
	golangci-lint run

dev-up:
	@mkdir -p data/dev-mergerfin jellyfin-config-a jellyfin-config-b test-media/upstream-a test-media/upstream-b
	@chmod 777 data/dev-mergerfin
	$(COMPOSE_DEV) up -d --build

dev-seed:
	./scripts/seed_upstreams.sh

dev-down:
	$(COMPOSE_DEV) down

docker-build:
	docker build -t $(IMAGE_TAG) .

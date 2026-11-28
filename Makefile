SHELL := /bin/bash
.DEFAULT_GOAL := help

MODULE := github.com/rcss-labs/yamdrake
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -X $(MODULE)/internal/version.Version=$(VERSION) -X $(MODULE)/internal/version.Commit=$(COMMIT)
BINARY_PACKAGES := orchestrator runner

# Compose builds read VERSION and COMMIT from the environment (see compose.yaml).
export VERSION COMMIT

.PHONY: help build lint test dev-up dev-down test-integration test-floci floci-clean test-e2e images smoke tf-check web-install web-lint web-test web-build web-e2e

help: ## List available targets
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk -F ':.*## ' '{printf "  %-18s %s\n", $$1, $$2}'

build: web-build ## Build yumdrake-orchestrator, yumdrake-runner and the yumdrake CLI into bin/ (with the web application embedded)
	@mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/yumdrake ./cmd/yumdrake
	@for pkg in $(BINARY_PACKAGES); do \
		if [ -d "cmd/$$pkg" ]; then \
			go build -ldflags "$(LDFLAGS)" -o "bin/yumdrake-$$pkg" "./cmd/$$pkg" || exit 1; \
		else \
			echo "skipping cmd/$$pkg (directory does not exist yet)"; \
		fi; \
	done

lint: ## Check gofmt, go vet and golangci-lint
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint is not on PATH. Install it: brew install golangci-lint (https://golangci-lint.run/welcome/install/)" >&2; \
		exit 2; }
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt reports unformatted files (run gofmt -w on them):" >&2; \
		echo "$$unformatted" >&2; \
		exit 1; \
	fi
	go vet ./...
	golangci-lint run

test: ## Run unit tests with the race detector
	go test -race ./...

dev-up: ## Start local PostgreSQL, Valkey and Floci (local AWS) and wait until healthy
	docker compose up -d --wait postgres valkey floci

dev-down: ## Stop local services and delete their volumes
	docker compose down -v

# Defaults point at the dev stack; CI and others may set these in the environment.
YUMDRAKE_TEST_DATABASE_URL ?= postgres://yumdrake:yumdrake@localhost:5432/yumdrake?sslmode=disable
YUMDRAKE_TEST_VALKEY_ADDR ?= localhost:6379
# Exported (not written into the recipe) so make never echoes the URL.
export YUMDRAKE_TEST_DATABASE_URL YUMDRAKE_TEST_VALKEY_ADDR

test-integration: ## Run integration tests against the local services (see dev-up)
	go test -race -tags integration -timeout 25m ./...

# Floci (the local AWS emulator) is published on 4577; 4566 is deliberately unused.
YUMDRAKE_TEST_AWS_ENDPOINT ?= http://localhost:4577
export YUMDRAKE_TEST_AWS_ENDPOINT
ifeq ($(origin YUMDRAKE_TEST_AWS_ENDPOINT),file)
E2E_FLOCI_ENDPOINT :=
else
E2E_FLOCI_ENDPOINT := $(strip $(YUMDRAKE_TEST_AWS_ENDPOINT))
endif

test-floci: ## Run the integration and Floci tests (see dev-up; needs Docker for ECS and ECR)
	go test -race -tags "integration floci" -timeout 25m ./...

floci-clean: ## Remove the floci-aws-* containers that our Floci created (never another Floci's)
	./scripts/floci-clean.sh

# Floci planning tests run only when YUMDRAKE_TEST_AWS_ENDPOINT is set in the environment or on the command line; the default above does not enable them.
test-e2e: ## Run the end-to-end tests (in-process orchestrator, fake GitHub; with Floci when YUMDRAKE_TEST_AWS_ENDPOINT is set) against the local services
ifeq ($(E2E_FLOCI_ENDPOINT),)
	go test -race -tags e2e -timeout 15m ./test/e2e/...
else
	go test -race -tags "e2e floci" -timeout 30m ./test/e2e/...
endif

images: ## Build the yumdrake-orchestrator and yumdrake-runner images (tag :dev)
	@for svc in orchestrator runner; do \
		docker build -f deploy/docker/$$svc.Dockerfile \
			--build-arg VERSION="$(VERSION)" --build-arg COMMIT="$(COMMIT)" \
			-t yumdrake-$$svc:dev . || exit 1; \
	done

TF_DIRS := deploy/terraform/example deploy/terraform/example/target-account

tf-check: ## Check the Terraform example: fmt and validate only, never plan or apply, no AWS credentials needed
	@set -euo pipefail; \
	command -v terraform >/dev/null 2>&1 || { echo "terraform is not on PATH (see deploy/terraform/example/versions.tf for the supported range)" >&2; exit 2; }; \
	for dir in $(TF_DIRS); do \
		[ -d "$$dir" ] || { echo "missing directory: $$dir" >&2; exit 1; }; \
	done; \
	terraform fmt -check -recursive deploy/terraform; \
	for dir in $(TF_DIRS); do \
		echo "== $$dir"; \
		(cd "$$dir" && terraform init -backend=false -input=false >/dev/null && terraform validate); \
	done

smoke: ## Build and start the app profile in Compose and check it is healthy
	./scripts/smoke.sh

web-install: ## Install the web application's dependencies from the lockfile
	npm --prefix web ci

web-lint: ## Lint and type-check the web application
	npm --prefix web run lint

web-test: ## Run the web application's unit tests once
	npm --prefix web run test

web-build: ## Build the web application and copy it into internal/webui/dist for embedding
	npm --prefix web run build
	find internal/webui/dist -mindepth 1 ! -name .gitkeep -delete
	cp -R web/dist/. internal/webui/dist/

web-e2e: build ## Run the browser test (Chromium) against the real orchestrator binary, the Compose services and a fake GitHub (see dev-up)
	npm --prefix web run test:e2e

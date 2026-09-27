# intel-payment — metering, credits & payments
COMPOSE := docker compose -f deploy/docker-compose.yml
MODULE  := github.com/truongpx396/intel-payment

.DEFAULT_GOAL := help
.PHONY: help build test test-integration lint arch-lint verify-portability \
        up up-jetstream down down-volumes logs migrate migrate-down seed proto fmt vuln secrets ci clean

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
	  awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-22s\033[0m %s\n",$$1,$$2}'

## ---- build & test ---------------------------------------------------------
build: ## Build all binaries
	go build -trimpath ./...

test: ## Unit tests + every conformance suite
	go test -race -count=1 ./...

test-integration: ## Integration tests (Testcontainers: real Redis + Postgres)
	go test -race -count=1 -tags=integration ./...

fmt: ## Format
	gofmt -l -w . && go mod tidy

## ---- the boundary ---------------------------------------------------------
lint: arch-lint ## golangci-lint (incl. depguard) + architecture graph
	golangci-lint run ./...

arch-lint: ## Enforce the hexagonal dependency graph
	go-arch-lint check --project-path .

verify-portability: ## Assert metering/ stands alone (SC-006)
	@echo "==> metering/ must not depend on billing/, entitlement/ or cmd/"
	@if go list -deps ./metering/... 2>/dev/null | \
	    grep -E '^$(MODULE)/(billing|entitlement|cmd)(/|$$)'; then \
	  echo "FAIL: metering/ reached outside itself — the engine is no longer portable"; exit 1; \
	fi
	@echo "OK"

vuln: ## govulncheck
	govulncheck ./...

secrets: ## gitleaks
	gitleaks detect --no-banner

ci: build test lint verify-portability vuln ## Everything CI runs

## ---- run ------------------------------------------------------------------
up: ## Start the stack (postgres + redis + migrate + paymentd + worker)
	$(COMPOSE) up -d --build
	@echo "REST :8080  gRPC :9090   → curl -s localhost:8080/readyz"
	@echo "bus: Redis Streams (default). For JetStream: make up-jetstream"

up-jetstream: ## Start the stack with NATS JetStream as the bus instead of Redis Streams
	PAYMENT_BUS=nats_jetstream PAYMENT_BUS_URL=nats://nats:4222 \
	  $(COMPOSE) --profile jetstream up -d --build

down: ## Stop and remove the stack
	$(COMPOSE) down

down-volumes: ## Stop and DELETE all data (ledger included)
	$(COMPOSE) down -v

logs: ## Tail service logs
	$(COMPOSE) logs -f paymentd payment-worker

migrate: ## Run migrations to head
	$(COMPOSE) run --rm migrate

migrate-down: ## Roll back the last migration (TO=0001 rolls back to that version)
	$(COMPOSE) run --rm --entrypoint /usr/local/bin/payment-migrate migrate down $(TO)

## ---- data -----------------------------------------------------------------
REALM ?= default
seed: ## Seed a realm: rate card, limits, plans, free tier (REALM=my-product)
	$(COMPOSE) exec -T postgres psql -U payment -d payment \
	  -v realm='$(REALM)' -f /dev/stdin < scripts/seed.sql

proto: ## Regenerate protobuf
	buf generate

clean:
	rm -rf bin dist coverage.out

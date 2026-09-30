# intel-payment — metering, credits & payments
COMPOSE := docker compose -f deploy/docker-compose.yml
MODULE  := github.com/truongpx396/intel-payment

.DEFAULT_GOAL := help
.PHONY: help build test test-integration fuzz mutation mutation-diff verify-red-green verify-tdd-gates lint arch-lint verify-portability \
        verify-boundary verify-schema verify-hot-path verify-deploy e2e e2e-install up up-jetstream down down-volumes logs \
        migrate seed proto fmt vuln secrets ci clean

help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | \
	  awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-22s\033[0m %s\n",$$1,$$2}'

## ---- build & test ---------------------------------------------------------
build: ## Build all binaries
	go build -trimpath ./...

# Tests are parallel (t.Parallel — enforced by the paralleltest linter) and shuffled, so a test that
# depends on another's side effects fails here rather than in a rebase. goleak guards packages that
# start goroutines (TestMain).
test: ## Unit tests + every conformance suite (parallel, shuffled, race detector)
	go test -race -count=1 -shuffle=on ./...

test-integration: ## Integration tests (Testcontainers: real Redis + Postgres; needs Docker)
	go test -race -count=1 -shuffle=on -tags=integration ./...

# Test-driven development, with the evidence checkable (docs/testing.md).
fuzz: ## Search every fuzz target beyond its seeds (FUZZTIME=20s each); commit a failing input from testdata/fuzz with its fix
	scripts/fuzz.sh

mutation: ## Mutation-test every target against its thresholds — do the tests fail when the code is wrong? (minutes)
	scripts/mutation.sh

mutation-diff: ## Mutation-test only the lines changed since REF, committed or not (REF=origin/main) — what a pull request is held to
	scripts/mutation.sh --diff $(or $(REF),origin/main)

verify-red-green: ## Prove the branch was test-driven: a failing tests-only commit precedes the change (REF=origin/main)
	scripts/verify-red-green.sh $(or $(REF),origin/main)

verify-tdd-gates: ## Show the TDD gates refuse what they should (red-green cases, a planted mutation survivor); needs gremlins
	scripts/verify-tdd-gates.sh

fmt: ## Format
	gofmt -l -w . && go mod tidy

## ---- the boundary ---------------------------------------------------------
lint: arch-lint ## golangci-lint (incl. depguard) + architecture graph
	golangci-lint run ./...

arch-lint: ## Enforce the hexagonal dependency graph
	go-arch-lint check --project-path .

verify-portability: ## Assert metering/ stands alone (SC-006)
	@echo "==> metering/ must not depend on billing/, entitlement/, events/ or cmd/"
	@if go list -deps ./metering/... 2>/dev/null | \
	    grep -E '^$(MODULE)/(billing|entitlement|events|cmd)(/|$$)'; then \
	  echo "FAIL: metering/ reached outside itself — the engine is no longer portable"; exit 1; \
	fi
	@echo "OK"

verify-boundary: ## Plant one violation per boundary rule and assert the gates reject it
	scripts/verify-boundary.sh

vuln: ## govulncheck
	govulncheck ./...

secrets: ## gitleaks
	gitleaks detect --no-banner

## ---- the design's own verifications (run without any Go code) ----------
PSQL ?= psql $(or $(PAYMENT_LEDGER_DSN),postgres://payment:payment@localhost:5432/payment)
verify-schema: ## Apply migrations + seed + the Phase 2 draft to an EMPTY Postgres and assert behaviour
	PSQL='$(PSQL)' scripts/verify-schema.sh

verify-hot-path: ## Run the reference Redis Functions against Redis in cluster mode (needs Docker)
	scripts/verify-hot-path.sh

verify-deploy: ## Build the image; migrate an empty Postgres; prove the Redis ACL (needs Docker)
	scripts/verify-deploy.sh

## ---- end-to-end (Playwright against a running stack) -----------------------
e2e-install: ## Install the e2e dependencies
	cd e2e && npm ci

e2e: ## Run the e2e suite against E2E_BASE_URL (default: the local `make up` stack)
	cd e2e && E2E_BASE_URL=$${E2E_BASE_URL:-http://localhost:8080} npx playwright test

# verify-red-green and mutation-diff read the branch's history, so they run on a branch, not here: run
# `make verify-red-green mutation-diff` before opening a pull request. verify-schema needs an empty DB.
ci: build test fuzz test-integration lint verify-portability verify-boundary verify-tdd-gates vuln verify-hot-path verify-deploy ## Everything CI runs on every change

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

# There is deliberately no down-migration target: migrations are forward-only (D27). In development,
# `make down-volumes && make up` rebuilds the schema from scratch.

## ---- data -----------------------------------------------------------------
REALM ?= default
seed: ## Seed a realm: pools, rate card, limits, plans, entitlements, free tier (REALM=my-product)
	$(COMPOSE) exec -T postgres psql -U payment -d payment \
	  -v realm='$(REALM)' -f /dev/stdin < scripts/seed.sql

proto: ## Regenerate protobuf
	buf generate

clean:
	rm -rf bin dist coverage.out

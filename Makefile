# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: MIT

GO ?= go

.PHONY: build build-stubs check check-all clean down fmt hooks openapi run run-down test-e2e test-store up

# The whole bar. Every gate lives in latere.ai/x/ci-gate, pinned as a tool
# in go.mod and configured in .lateregate.yaml, so this target is a name for
# `go tool lateregate` and nothing else. One gate at a time: `go tool
# lateregate cover`. The plan: `go tool lateregate list`.
check:
	@$(GO) tool lateregate

.DEFAULT_GOAL := check

OUT_DIR := out
SERVICE := arcad
MODULE := $(shell $(GO) list -m)

# Build metadata, deferred so the git and date calls run only for a build. A
# dirty tree marks the commit, because a binary built from uncommitted
# changes cannot be reproduced from its commit.
VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DIRTY = $(shell test -n "$$(git status --porcelain 2>/dev/null)" && echo -dirty)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
VERSION_PKG = $(MODULE)/internal/version
LDFLAGS = -X $(VERSION_PKG).Version=$(VERSION) \
          -X $(VERSION_PKG).Commit=$(COMMIT)$(DIRTY) \
          -X $(VERSION_PKG).Date=$(BUILD_DATE)

build:
	@mkdir -p $(OUT_DIR)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' \
		-o $(OUT_DIR)/$(SERVICE) ./cmd/$(SERVICE)
	@echo "built $(OUT_DIR)/$(SERVICE)"

# The stub binary of spec 014: the issuer and the authorizer `make run`
# starts beside the stack, and the image of spec 016 publishes.
STUBS := arca-stubs
build-stubs:
	@mkdir -p $(OUT_DIR)
	CGO_ENABLED=0 $(GO) build -trimpath -o $(OUT_DIR)/$(STUBS) ./test/stubs/cmd/$(STUBS)
	@echo "built $(OUT_DIR)/$(STUBS)"

# ── the stack (spec 014) ────────────────────────────────────────────────
#
# compose.yaml is the only definition of Postgres and MinIO. The project
# name and every port derive from the checkout's directory name, so two
# clones run side by side, and the values below are what a tier and
# `make run` read the stack through. Docker by default; another engine
# that speaks the same command line is selected with DEV_ENGINE.
DEV_ENGINE ?= $(shell command -v docker >/dev/null 2>&1 && echo docker || echo podman)
DEV_PROJECT ?= $(notdir $(CURDIR))
COMPOSE = $(DEV_ENGINE) compose -f compose.yaml -p $(DEV_PROJECT)
DEV_PORT_BASE ?= $(shell printf '%s' '$(DEV_PROJECT)' | cksum | awk '{print 20000 + ($$1 % 300) * 10}')
DEV_PUBLIC_PORT ?= $(DEV_PORT_BASE)
DEV_INTERNAL_PORT ?= $(shell expr $(DEV_PORT_BASE) + 1)
DEV_S3_PORT ?= $(shell expr $(DEV_PORT_BASE) + 2)
DEV_S3_CONSOLE_PORT ?= $(shell expr $(DEV_PORT_BASE) + 3)
DEV_DB_PORT ?= $(shell expr $(DEV_PORT_BASE) + 4)
DEV_ISSUER_PORT ?= $(shell expr $(DEV_PORT_BASE) + 5)
DEV_AUTHORIZER_PORT ?= $(shell expr $(DEV_PORT_BASE) + 6)
DEV_S3_KEY ?= minioadmin
DEV_S3_SECRET ?= minioadmin
DEV_S3_BUCKET ?= arca-test
DEV_DB_NAME ?= arca
DEV_DB_USER ?= arca
DEV_DB_PASSWORD ?= arca
DEV_S3_ENDPOINT ?= http://127.0.0.1:$(DEV_S3_PORT)
DEV_DATABASE_URL ?= postgres://$(DEV_DB_USER):$(DEV_DB_PASSWORD)@127.0.0.1:$(DEV_DB_PORT)/$(DEV_DB_NAME)?sslmode=disable
DEV_COMPOSE_ENV = DEV_PROJECT=$(DEV_PROJECT) DEV_S3_PORT=$(DEV_S3_PORT) \
                  DEV_S3_CONSOLE_PORT=$(DEV_S3_CONSOLE_PORT) DEV_DB_PORT=$(DEV_DB_PORT) \
                  DEV_S3_KEY=$(DEV_S3_KEY) DEV_S3_SECRET=$(DEV_S3_SECRET) \
                  DEV_S3_BUCKET=$(DEV_S3_BUCKET) DEV_DB_NAME=$(DEV_DB_NAME) \
                  DEV_DB_USER=$(DEV_DB_USER) DEV_DB_PASSWORD=$(DEV_DB_PASSWORD)

# The variables a tier reads. They are not ARCA_*: no server reads them,
# and a tier builds the ARCA_* values it starts a server with from them
# (spec 014).
TIER_ENV = E2E_DATABASE_URL="$(DEV_DATABASE_URL)" E2E_S3_ENDPOINT="$(DEV_S3_ENDPOINT)" \
           E2E_S3_KEY=$(DEV_S3_KEY) E2E_S3_SECRET=$(DEV_S3_SECRET) E2E_S3_BUCKET=$(DEV_S3_BUCKET)

# stack-up starts the stack and waits for facts rather than for a
# duration: MinIO answers its health endpoint, the one-shot container
# that makes the bucket has exited cleanly, and Postgres accepts
# connections. The waiting is here rather than in `compose up --wait`
# because only one of the two engines has that flag, and the container
# name filter carries both engines' separators.
define stack-up
	$(DEV_COMPOSE_ENV) $(COMPOSE) up -d >/dev/null
	for i in $$(seq 1 90); do \
		if curl -sf -o /dev/null "$(DEV_S3_ENDPOINT)/minio/health/live" \
		   && $(DEV_ENGINE) ps -a --filter 'name=$(DEV_PROJECT)[-_]minio-init' --format '{{.Status}}' 2>/dev/null | grep -qi 'exited (0)' \
		   && $(DEV_ENGINE) exec $$($(DEV_ENGINE) ps -q --filter 'name=$(DEV_PROJECT)[-_]postgres' | head -n1) \
		        pg_isready -U $(DEV_DB_USER) -d $(DEV_DB_NAME) >/dev/null 2>&1; then \
			echo "Postgres is at $(DEV_DATABASE_URL)"; \
			echo "MinIO is at $(DEV_S3_ENDPOINT) with the bucket $(DEV_S3_BUCKET)"; \
			break; \
		fi; \
		if [ $$i = 90 ]; then echo "the stack did not become ready"; $(DEV_COMPOSE_ENV) $(COMPOSE) ps; exit 1; fi; \
		sleep 1; \
	done
endef

up:
	@$(stack-up)

# down stops the stack and keeps its volumes, so a failed run is
# debuggable. `make clean` is what removes them.
down:
	-@$(DEV_COMPOSE_ENV) $(COMPOSE) down --remove-orphans 2>/dev/null

# The store tier: internal/blob and internal/store against the real
# MinIO and the real Postgres. Without the stack's variables every test
# in it skips itself with the remediation in its message.
test-store: up
	$(TIER_ENV) $(GO) test -tags=tiers -race -count=1 -run '^TestStore' ./internal/blob/... ./internal/store/... ./internal/files/... ./internal/uploads/...

# The e2e tier: arcad as a process against the stack and the stubs.
test-e2e: up build build-stubs
	$(TIER_ENV) ARCA_BINARY="$(CURDIR)/$(OUT_DIR)/$(SERVICE)" \
		$(GO) test -tags=tiers -race -count=1 -run '^TestE2E' ./test/e2e/...

# The whole bar plus both service tiers, which is what to run before
# pushing something that touches a store.
check-all: check test-store test-e2e

# ── make run (spec 014) ─────────────────────────────────────────────────
#
# A clean clone to a serving installation. The bucket has to exist before
# readiness passes and the database has to hold the schema before the
# server starts, so the order is the stack, the stubs, the migration, and
# then the server in the foreground.
DEV_DIR = $(CURDIR)/$(OUT_DIR)/dev/$(DEV_PROJECT)
DEV_STUBS_PID = $(DEV_DIR)/stubs.pid
DEV_STUBS_LOG = $(DEV_DIR)/stubs.log
DEV_ISSUER_URL = http://localhost:$(DEV_ISSUER_PORT)

# The server reads the same variables locally as in production; only the
# values differ. The identity rows are set here already, and the server
# begins reading them with spec 006.
DEV_SERVICE_ENV = ARCA_PUBLIC_ADDR=127.0.0.1:$(DEV_PUBLIC_PORT) \
                  ARCA_INTERNAL_ADDR=127.0.0.1:$(DEV_INTERNAL_PORT) \
                  ARCA_PUBLIC_URL=http://localhost:$(DEV_PUBLIC_PORT) \
                  ARCA_DATABASE_URL="$(DEV_DATABASE_URL)" \
                  ARCA_BUCKET=$(DEV_S3_BUCKET) ARCA_BUCKET_ENDPOINT=$(DEV_S3_ENDPOINT) \
                  ARCA_BUCKET_REGION=us-east-1 ARCA_BUCKET_PATH_STYLE=true \
                  ARCA_BUCKET_ACCESS_KEY=$(DEV_S3_KEY) ARCA_BUCKET_SECRET_KEY=$(DEV_S3_SECRET) \
                  ARCA_OIDC_ISSUERS=$(DEV_ISSUER_URL) ARCA_OIDC_INSECURE_ISSUERS=true \
                  ARCA_AUTHORIZER_URL=http://127.0.0.1:$(DEV_AUTHORIZER_PORT) \
                  ARCA_AUTHORIZER_TOKEN=stub-authorizer-token \
                  ARCA_ADMIN_SUBJECTS="$(DEV_ISSUER_URL)|dev"

# stubs-up starts arca-stubs in the background, once per checkout, and
# waits for the issuer's key set to answer.
define stubs-up
	mkdir -p $(DEV_DIR)
	if ! { [ -f $(DEV_STUBS_PID) ] && kill -0 $$(cat $(DEV_STUBS_PID)) 2>/dev/null; }; then \
		$(OUT_DIR)/$(STUBS) -issuer-listen 127.0.0.1:$(DEV_ISSUER_PORT) \
			-authorizer-listen 127.0.0.1:$(DEV_AUTHORIZER_PORT) -issuer-url $(DEV_ISSUER_URL) \
			>$(DEV_STUBS_LOG) 2>&1 & echo $$! >$(DEV_STUBS_PID); \
	fi
	for i in $$(seq 1 30); do \
		if curl -sf -o /dev/null "$(DEV_ISSUER_URL)/jwks"; then \
			echo "the stubs are ready: issuer $(DEV_ISSUER_URL), authorizer http://127.0.0.1:$(DEV_AUTHORIZER_PORT)"; break; \
		fi; \
		if [ $$i = 30 ]; then echo "arca-stubs did not become ready"; cat $(DEV_STUBS_LOG); exit 1; fi; \
		sleep 1; \
	done
endef

# token waits for the server, mints a token for the dev subject at the
# stub issuer, and prints what to export. The requests a token is for are
# spec 013's; until then the line is what a reader checks the probes with.
define token-line
	for i in $$(seq 1 60); do \
		if curl -sf -o /dev/null "http://127.0.0.1:$(DEV_INTERNAL_PORT)/readyz"; then break; fi; \
		if [ $$i = 60 ]; then echo "arcad did not become ready" >&2; exit 1; fi; \
		sleep 1; \
	done; \
	token=$$(curl -sf -X POST "$(DEV_ISSUER_URL)/mint" -d '{"sub":"dev"}' | sed 's/.*"token":"\([^"]*\)".*/\1/'); \
	echo; \
	echo "export ARCA_URL=http://localhost:$(DEV_PUBLIC_PORT) ARCA_TOKEN=$$token"; \
	echo 'curl -sS "$$ARCA_URL/readyz"'; \
	echo
endef

# One command from a clean clone to a serving installation: the stack,
# the stubs, the migrations, then arcad in the foreground.
run: build build-stubs up
	@$(stubs-up)
	$(DEV_SERVICE_ENV) $(OUT_DIR)/$(SERVICE) migrate
	@( $(token-line) ) &
	$(DEV_SERVICE_ENV) $(OUT_DIR)/$(SERVICE)

# run-down stops the server and the stubs and leaves the stack up, so a
# failed run is debuggable.
run-down:
	-@if [ -f $(DEV_STUBS_PID) ]; then kill $$(cat $(DEV_STUBS_PID)) 2>/dev/null; rm -f $(DEV_STUBS_PID); echo "the stubs stopped"; fi

# The committed OpenAPI description, written from the route table and the
# error table of internal/api (spec 013). A test fails when the file is not
# what a fresh run produces, so a route added without running this does not
# reach main.
openapi:
	@$(GO) run ./tools/apidoc

fmt:
	gofmt -w $$(git ls-files '*.go')

# Point git at the delegating hooks. Per clone, so it is a target.
hooks:
	chmod +x .githooks/*
	git config core.hooksPath .githooks

# Remove the stack with its volumes and the build output. Local state is
# disposable, and a stack that outlives the checkout is a surprise.
clean: run-down
	-@$(DEV_COMPOSE_ENV) $(COMPOSE) down --volumes --remove-orphans 2>/dev/null
	rm -rf $(OUT_DIR)

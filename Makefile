# Rigforge task runner. `make help` lists the targets.

SHELL := /bin/bash
.DEFAULT_GOAL := help

COMPOSE := docker compose

# The test suites reach the docker-compose services on these host ports.
# Go and Python tests use different Redis databases so they cannot collide.
TEST_DB := postgres://rigforge:rigforge@localhost:55432/rigforge
export RIGFORGE_TEST_DATABASE_URL ?= $(TEST_DB)
export RIGFORGE_TEST_S3_ENDPOINT ?= http://localhost:9000

POSE_MODEL := worker/models/pose_landmarker_heavy.task
POSE_MODEL_URL := https://storage.googleapis.com/mediapipe-models/pose_landmarker/pose_landmarker_heavy/float16/1/pose_landmarker_heavy.task
POSE_MODEL_SHA256 := 64437af838a65d18e5ba7a0d39b465540069bc8aae8308de3e318aad31fcbc7b

.PHONY: help
help: ## List the targets
	@grep -E '^[a-zA-Z0-9_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "} {printf "  \033[1m%-14s\033[0m %s\n", $$1, $$2}'

# ---- running ----------------------------------------------------------------

.PHONY: dev
dev: ## Build and run the whole stack with docker-compose (web: http://localhost:8081)
	$(COMPOSE) up --build

.PHONY: dev-r2
dev-r2: ## Same, but against Cloudflare R2 using the S3_* values in .env
	@test -f .env || { echo "create .env from .env.example first"; exit 1; }
	$(COMPOSE) --env-file .env up --build api worker web issuer postgres redis

.PHONY: infra
infra: ## Start only postgres, redis and minio (for tests and native development)
	$(COMPOSE) up -d --wait postgres redis minio

.PHONY: down
down: ## Stop the docker-compose stack (keeps volumes)
	$(COMPOSE) down

# ---- one-time setup -----------------------------------------------------------

.PHONY: setup
setup: models ## Install worker and web dependencies for native development
	cd worker && python3.11 -m venv .venv && .venv/bin/pip install -q -e ".[dev]"
	cd web && pnpm install

.PHONY: models
models: $(POSE_MODEL) ## Download the MediaPipe pose model (checksum verified)

$(POSE_MODEL):
	mkdir -p $(dir $@)
	curl -fsSL -o $@.tmp $(POSE_MODEL_URL)
	echo "$(POSE_MODEL_SHA256)  $@.tmp" | shasum -a 256 -c -
	mv $@.tmp $@

.PHONY: sample
sample: ## Rebuild docs/samples/power_jump.mp4 from its public-domain source
	scripts/make_sample_video.sh sample

.PHONY: character
character: ## Regenerate the default character (needs Blender, or `pip install bpy`)
	@if command -v blender >/dev/null; then \
		blender --background --python worker/rig/make_default_character.py; \
	else \
		python3.11 worker/rig/make_default_character.py; \
	fi
	node web/scripts/validate-gltf.mjs worker/rig/default_character.glb

# ---- tests ----------------------------------------------------------------------

.PHONY: test
test: infra test-api test-worker test-web ## Run every test suite (starts the infra containers)

.PHONY: test-api
test-api: ## Go: formatting, vet, unit and integration tests
	@cd api && test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }
	cd api && go vet ./...
	cd api && RIGFORGE_TEST_REDIS_URL=redis://localhost:56379/15 go test ./...

.PHONY: test-worker
test-worker: models ## Python: lint, queue tests on Redis, maths tests, pipeline smoke test
	cd worker && .venv/bin/ruff check . && .venv/bin/ruff format --check .
	cd worker && RIGFORGE_TEST_REDIS_URL=redis://localhost:56379/14 RIGFORGE_REQUIRE_SERVICES=1 .venv/bin/python -m pytest tests -q

.PHONY: test-web
test-web: ## Web: type check, unit and component tests
	cd web && pnpm typecheck && pnpm test

.PHONY: e2e
e2e: ## Playwright end-to-end tests against the full docker-compose stack
	$(COMPOSE) up -d --build --wait
	cd web && pnpm exec playwright install chromium && pnpm exec playwright test

.PHONY: resume-test
resume-test: ## The 2 GB resumable-upload test (needs the stack running: make dev)
	scripts/upload_resume_test.sh

.PHONY: validate
validate: ## Run the sample clip through the worker and validate the glTF output
	cd worker && .venv/bin/python -m rigforge_worker.main run ../docs/samples/power_jump.mp4 --out ../tmp/sample_out
	node web/scripts/validate-gltf.mjs tmp/sample_out/motion.glb

# ---- images and Kubernetes ----------------------------------------------------

.PHONY: build
build: ## Build the four images (tagged :dev)
	docker build -t rigforge-api:dev --target api api
	docker build -t rigforge-issuer:dev --target issuer api
	docker build -t rigforge-worker:dev worker
	docker build -t rigforge-web:dev web

.PHONY: deploy-kind
deploy-kind: ## Create a kind cluster, build and load the images, deploy, print the URL
	scripts/deploy_kind.sh

.PHONY: kind-status
kind-status: ## Show the pods of the kind deployment
	scripts/deploy_kind.sh status

.PHONY: kind-down
kind-down: ## Delete the kind cluster
	kind delete cluster --name rigforge

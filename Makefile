# VeriDB
#
# `make` with no target lists everything.

SHELL := bash
.DEFAULT_GOAL := help

# The viewer container runs as the host user so it can read the 0640 audit trail
# and write its SQLite store into the bind-mounted directories.
export VIEWER_UID := $(shell id -u)
export VIEWER_GID := $(shell id -g)

BIN        := bin
VERIDB     := $(BIN)/veridb
VIEWER     := $(BIN)/veridb-viewer
CONFIG     ?= configs/veridb.yaml
AUDIT_DIR  ?= tmp/audit
AUDIT_FILE ?= $(AUDIT_DIR)/veridb-audit.jsonl
VIEWER_DIR ?= tmp/viewer
GOFLAGS    ?= -trimpath

.PHONY: help
help: ## List available targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the MCP server and the audit viewer
	@mkdir -p $(BIN)
	go build $(GOFLAGS) -o $(VERIDB) ./cmd/veridb
	go build $(GOFLAGS) -o $(VIEWER) ./cmd/veridb-viewer

.PHONY: test
test: ## Run all tests
	go test ./...

.PHONY: test-race
test-race: ## Run tests with the race detector
	go test -race ./...

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: fmt
fmt: ## Format the Go sources
	gofmt -w cmd internal

.PHONY: check
check: fmt vet test ## Format, vet and test

.PHONY: verify
verify: check secrets ## Everything: format, vet, test, and scan for secrets

.PHONY: hooks
hooks: ## Enable the repository's git hooks (secret scanning on commit)
	git config core.hooksPath .githooks
	@echo "core.hooksPath = $$(git config --get core.hooksPath)"

.PHONY: secrets
secrets: ## Scan the working tree and all history for credentials
	@command -v gitleaks >/dev/null 2>&1 || { \
		echo "gitleaks is not installed; run this from the devenv shell"; exit 1; }
	@echo "scanning the working tree..."
	@gitleaks dir --no-banner --redact --config .gitleaks.toml .
	@echo "scanning all history..."
	@gitleaks git --no-banner --redact --config .gitleaks.toml .
	@echo "no credentials found"

.PHONY: run
run: ## Run the MCP server on stdio (interactive; Ctrl-D to stop)
	go run ./cmd/veridb -config $(CONFIG)

.PHONY: config-check
config-check: ## Validate the config and report which databases would connect
	go run ./cmd/veridb -config $(CONFIG) < /dev/null

.PHONY: audit-dir
audit-dir: ## Create the directories veridb and the viewer write into
	@mkdir -p $(AUDIT_DIR) $(VIEWER_DIR)
	@echo "audit trail: $(AUDIT_FILE)"
	@echo "viewer store: $(VIEWER_DIR)/audit.db"

.PHONY: viewer
viewer: ## Run the audit viewer locally against the JSONL trail
	AUDIT_FILE=$(AUDIT_FILE) AUDIT_DB=$(VIEWER_DIR)/audit.db go run ./cmd/veridb-viewer

.PHONY: viewer-up
viewer-up: audit-dir ## Start the viewer and ngrok tunnel in Docker
	docker compose up -d --build
	@echo "waiting for ngrok to publish a URL..."
	@for i in $$(seq 1 20); do \
		url=$$(curl -fsS localhost:$${NGROK_API_PORT:-4040}/api/tunnels 2>/dev/null \
			| python3 -c 'import sys,json;print(json.load(sys.stdin)["tunnels"][0]["public_url"])' 2>/dev/null); \
		if [ -n "$$url" ]; then echo "audit UI: $$url/?token=$${VIEWER_TOKEN}"; exit 0; fi; \
		sleep 1; \
	done; \
	echo "ngrok URL not published yet; check: docker compose logs ngrok"

.PHONY: viewer-url
viewer-url: ## Print the current public audit URL
	@curl -fsS localhost:$${NGROK_API_PORT:-4040}/api/tunnels \
		| python3 -c 'import sys,json;print(json.load(sys.stdin)["tunnels"][0]["public_url"])'

.PHONY: viewer-logs
viewer-logs: ## Follow the viewer and ngrok logs
	docker compose logs -f viewer ngrok

.PHONY: viewer-down
viewer-down: ## Stop the viewer stack
	docker compose down

.PHONY: clean
clean: ## Remove build output and local viewer state
	rm -rf $(BIN) $(VIEWER_DIR) tmp/veridb-pg

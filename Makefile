# Formae Plugin Makefile
#
# Targets:
#   build   - Build the plugin binary
#   test    - Run tests
#   lint    - Run linter
#   clean   - Remove build artifacts
#   install - Build and install plugin locally (binary + schema + manifest)

# Plugin metadata - extracted from formae-plugin.pkl
PLUGIN_NAME := $(shell pkl eval -x 'name' formae-plugin.pkl 2>/dev/null || echo "example")
PLUGIN_VERSION := $(shell pkl eval -x 'version' formae-plugin.pkl 2>/dev/null || echo "0.0.0")
PLUGIN_NAMESPACE := $(shell pkl eval -x 'namespace' formae-plugin.pkl 2>/dev/null || echo "EXAMPLE")

# Build settings
GO := go
GOFLAGS := -trimpath
BINARY := $(PLUGIN_NAME)

# Installation paths
# Plugin discovery expects lowercase directory names matching the plugin name
PLUGIN_BASE_DIR := $(HOME)/.pel/formae/plugins
INSTALL_DIR := $(PLUGIN_BASE_DIR)/$(PLUGIN_NAME)/v$(PLUGIN_VERSION)

.PHONY: all build test test-unit test-integration lint verify-schema clean install help clean-environment conformance-test conformance-test-crud conformance-test-discovery atlas-binary

all: build

## atlas-binary: Download a pinned atlas CLI binary into ./tools/atlas
## so integration + conformance tests can locate it via PATH. Stop-gap
## until formae core bundles atlas alongside the formae installer (see
## ~/dev/personal/engineering-notes/formae-mcp/2026-05-14-plugin-new-skill-gaps.md G-1).
## Override via ATLAS_VERSION=<ver>.
atlas-binary:
	@./scripts/install-atlas.sh

## build: Build the plugin binary and update manifest
build:
	@mkdir -p schema/pkl && echo "$(PLUGIN_VERSION)" > schema/pkl/VERSION
	$(GO) build $(GOFLAGS) -o bin/$(BINARY) .
	@SDK_MIN=$$($(GO) list -m -f '{{.Dir}}' github.com/platform-engineering-labs/formae/pkg/plugin 2>/dev/null | xargs -I{} grep 'MinFormaeVersion' {}/version.go 2>/dev/null | grep -oE '"[0-9]+\.[0-9]+\.[0-9]+"' | tr -d '"'); \
	DECLARED=$$(pkl eval -x minFormaeVersion formae-plugin.pkl 2>/dev/null); \
	EFFECTIVE=$$(printf '%s\n%s\n' "$$SDK_MIN" "$$DECLARED" | grep -E '^[0-9]+\.[0-9]+\.[0-9]+$$' | sort -t. -k1,1n -k2,2n -k3,3n | tail -1); \
	if [ -n "$$EFFECTIVE" ] && [ "$$EFFECTIVE" != "$$DECLARED" ]; then \
		echo "Raising minFormaeVersion to $$EFFECTIVE (sdk=$$SDK_MIN, declared=$$DECLARED)"; \
		if [ "$$(uname)" = "Darwin" ]; then \
			sed -i '' 's/^minFormaeVersion = .*/minFormaeVersion = "'"$$EFFECTIVE"'"/' formae-plugin.pkl; \
		else \
			sed -i 's/^minFormaeVersion = .*/minFormaeVersion = "'"$$EFFECTIVE"'"/' formae-plugin.pkl; \
		fi; \
	else \
		echo "Keeping declared minFormaeVersion=$$DECLARED (sdk=$$SDK_MIN, never downgrade below declared)"; \
	fi

## test: Run all tests
test:
	$(GO) test -v ./...

## test-unit: Run unit tests only (tests with //go:build unit tag)
test-unit:
	$(GO) test -v -tags=unit ./...

## test-integration: Run integration tests (real postgres + atlas binary)
## Spins up a postgres container via docker. Prepends ./tools to PATH so
## the atlas CLI installed by `make atlas-binary` is discoverable.
test-integration: atlas-binary
	PATH="$(CURDIR)/tools:$$PATH" $(GO) test -v -tags=integration ./...

## lint: Run golangci-lint
lint:
	golangci-lint run

## verify-schema: Validate PKL schema files
## Checks that schema files are well-formed and follow formae conventions.
verify-schema:
	@mkdir -p schema/pkl && echo "$(PLUGIN_VERSION)" > schema/pkl/VERSION
	$(GO) run github.com/platform-engineering-labs/formae/pkg/plugin/testutil/cmd/verify-schema --namespace $(PLUGIN_NAMESPACE) ./schema/pkl

## clean: Remove build artifacts
clean:
	rm -rf bin/ dist/

## install: Build and install plugin locally (binary + schema + manifest)
## Installs to ~/.pel/formae/plugins/<name>/v<version>/
## Removes any existing versions of the plugin first to ensure clean state.
install: build
	@echo "Installing $(PLUGIN_NAME) v$(PLUGIN_VERSION) (namespace: $(PLUGIN_NAMESPACE))..."
	@rm -rf $(PLUGIN_BASE_DIR)/$(PLUGIN_NAME)
	@mkdir -p $(INSTALL_DIR)/schema
	@cp bin/$(BINARY) $(INSTALL_DIR)/$(BINARY)
	@cp -r schema/* $(INSTALL_DIR)/schema/
	@cp formae-plugin.pkl $(INSTALL_DIR)/
	@echo "Installed to $(INSTALL_DIR)"
	@echo "  - Binary: $(INSTALL_DIR)/$(BINARY)"
	@echo "  - Schema: $(INSTALL_DIR)/schema/"
	@echo "  - Manifest: $(INSTALL_DIR)/formae-plugin.pkl"

## help: Show this help message
help:
	@echo "Available targets:"
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'

## clean-environment: Clean up test resources in cloud environment
## Called before and after conformance tests. Edit scripts/ci/clean-environment.sh
## to configure for your provider.
clean-environment:
	@./scripts/ci/clean-environment.sh

## conformance-test: Run all conformance tests (CRUD + discovery)
## Usage: make conformance-test [TEST=s3-bucket] [TIMEOUT=30m]
## Calls clean-environment before and after tests.
conformance-test: conformance-test-crud conformance-test-discovery

## conformance-test-crud: Run only CRUD lifecycle tests
## Usage: make conformance-test-crud [TEST=s3-bucket] [TIMEOUT=30m]
##
## ATLAS_DELETE_MODE=dropRevisions flips the plugin into destructive-
## Delete mode. The conformance harness's OOB-delete phase relies on
## Delete dropping atlas's bookkeeping so the next Read returns NotFound
## and the agent tombstones the resource from inventory. Production
## users leave the env var unset (default = retain).
conformance-test-crud: install atlas-binary
	@echo "Pre-test cleanup..."
	@./scripts/ci/clean-environment.sh || true
	@echo ""
	@echo "Running CRUD conformance tests..."
	@PATH="$(CURDIR)/tools:$$PATH" FORMAE_TEST_FILTER="$(TEST)" FORMAE_TEST_TYPE=crud \
		ATLAS_DELETE_MODE=dropRevisions \
		$(GO) test -tags=conformance -v -timeout $(or $(TIMEOUT),30m) ./...; \
	TEST_EXIT=$$?; \
	echo ""; \
	echo "Post-test cleanup..."; \
	./scripts/ci/clean-environment.sh || true; \
	exit $$TEST_EXIT

## conformance-test-discovery: Run only discovery tests
## Usage: make conformance-test-discovery [TEST=s3-bucket] [TIMEOUT=30m]
conformance-test-discovery: install atlas-binary
	@echo "Pre-test cleanup..."
	@./scripts/ci/clean-environment.sh || true
	@echo ""
	@echo "Running discovery conformance tests..."
	@PATH="$(CURDIR)/tools:$$PATH" FORMAE_TEST_FILTER="$(TEST)" FORMAE_TEST_TYPE=discovery \
		ATLAS_DELETE_MODE=dropRevisions \
		$(GO) test -tags=conformance -v -timeout $(or $(TIMEOUT),30m) ./...; \
	TEST_EXIT=$$?; \
	echo ""; \
	echo "Post-test cleanup..."; \
	./scripts/ci/clean-environment.sh || true; \
	exit $$TEST_EXIT

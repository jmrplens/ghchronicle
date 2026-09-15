# Makefile for ghchronicle. Running `make` with no arguments lists the targets.
#
# The project is one binary plus seven development utilities under cmd/, and the
# utilities are most of the reason this file exists: each wants a different
# combination of positional arguments, a live Grafana, a token or a local
# PostgreSQL, and reading that back out of the source every time is work nobody
# should repeat. Every target that needs something says so before it runs.

.DEFAULT_GOAL := help

.PHONY: all help version \
	build build-all install run clean \
	build-linux-amd64 build-linux-arm64 build-darwin-amd64 \
	build-darwin-arm64 build-windows-amd64 build-windows-arm64 \
	test test-short test-race test-e2e coverage cover-check \
	coverage-conditions coverage-mutants \
	test-e2e-docker test-e2e-docker-race e2e-docker-up e2e-docker-down e2e-docker-logs \
	fmt fmt-check vet tidy lint golangci-lint govulncheck analyze analyze-fix sonar \
	mdlint mdlint-fix check-doc-links docs check-docs \
	probe gen-dashboards check-dashboards check-dashboards-live \
	check-prometheus check-postgres publish-dashboard \
	gen-brand gen-brand-compose \
	site-install site-dev site-build site-check site-analyze site-preview \
	docker-build install-tools tools-versions release-check

# ─── Variables ──────────────────────────────────────────────────────────────

BINARY_NAME := ghchronicle
CMD_PATH    := ./cmd/$(BINARY_NAME)
BIN_DIR     := bin
DIST_DIR    := dist

# The package list is spelled out instead of written as ./..., because plan/ is
# git-ignored and holds a Go package of its own (plan/svg-concepts/gen). With
# ./... every command here would mean one thing on the machine that has plan/
# and another in a fresh clone or in CI. Spelled out, it is what ./... means in
# a fresh clone: the root package, which is version.go and nothing else, and
# the three trees under it.
PKGS := . ./cmd/... ./internal/... ./test/...

# The formatter takes paths rather than package patterns, and it reads "." as
# the whole directory tree, plan/ included. So the root package is named by its
# files here, and the three trees the same way as above.
FMT_PATHS := $(wildcard *.go) ./cmd/... ./internal/... ./test/...

# Every target that compiles runs the toolchain go.mod names, whatever the
# machine has installed. CI's setup-go reads the same line, so a local
# `make analyze` and the jobs that gate a merge analyze with the same compiler
# and the same standard library, which is most of what govulncheck reports on.
PROJECT_GO_VERSION := $(shell awk '/^go / {print $$2; exit}' go.mod)
GO_TOOLCHAIN ?= go$(PROJECT_GO_VERSION)
export GOTOOLCHAIN := $(GO_TOOLCHAIN)

# The containerized end-to-end suite: the stores themselves, in Docker, for
# the assertions that need a real one. It is gated behind a build tag so that
# `go test ./...` and every target above stay free of Docker, and behind the
# targets below so that the compose project name is never left to a default.
# That name is not decoration: this repository is developed on a machine that
# runs unrelated containers, and a compose command without -p can reach them.
E2E_DOCKER_DIR     := test/e2e/docker
E2E_DOCKER_TAG     := dockere2e
E2E_DOCKER_PROJECT := ghchronicle-e2e
E2E_DOCKER_COMPOSE := docker compose -p $(E2E_DOCKER_PROJECT) -f $(E2E_DOCKER_DIR)/docker-compose.yml
# Bringing nine containers up cold is minutes, not seconds, and Elasticsearch
# and InfluxDB are most of it.
E2E_DOCKER_TIMEOUT := 30m
# Extra `go test` flags for the containerized suite. Empty for the ordinary
# run; test-e2e-docker-race sets it to -race.
E2E_DOCKER_TESTFLAGS ?=

# Coverage is one profile that instruments every package under cmd/ and
# internal/, the utilities included: each of them is tested through a run
# function that takes its arguments and its output streams, so none of them is
# exempt from the floor. ./test/... is left out because its suites drive the
# built binary as a separate process, which the profile of the test binary
# cannot see: adding it changes the total by nothing and the run time by a lot.
# SonarCloud reads the same coverage.out, so the number it reports and the
# floor below are one measurement.
COVERAGE_MIN      := 90
COVERAGE_PKGS     := ./cmd/... ./internal/...
COVERAGE_COVERPKG := ./cmd/...,./internal/...

# `go test -timeout` bounds each package's test binary, not the run, so this
# covers the slowest single package under the detector with room for a slow
# runner. It is there to end a deadlock with every goroutine stack printed,
# not to say how long the suite should take. .github/workflows/race.yml runs
# `make test-race`, so the bound is the same there.
RACE_TIMEOUT ?= 60m

# markdownlint-cli2 is the release DavidAnson/markdownlint-cli2-action v24.2.0
# bundles, so `make mdlint` and the Markdown job in CI are the same linter.
# Bump the two together.
MARKDOWNLINT_CLI2_VERSION := 0.23.2
MDLINT_GLOBS := "**/*.{md,mdx}" "\#plan" "\#node_modules"

# Version from the VERSION file (single source of truth); commit and date from
# git. Use shell `cat` (portable to GNU Make 3.81 on macOS; `$(file ...)` needs
# Make 4+).
#
# The date is the commit's rather than this minute's, for the reason
# .goreleaser.yaml gives: building the same commit twice should produce the same
# binary. It also agrees with what an unstamped build reports, since the
# toolchain records the same commit time as vcs.time.
VERSION    := $(strip $(shell cat VERSION 2>/dev/null))
COMMIT     := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_DATE := $(shell git log -1 --format=%cI 2>/dev/null || date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(BUILD_DATE)

# GOLANGCI_LINT_VERSION is the release CI installs, read back from here by
# .github/workflows/ci.yml so the version lives in one place. Deliberately not
# a go.mod `tool` directive: golangci-lint advises against building it from
# source (slower, and results can vary with the compiling Go). It is the
# release gitlab-mcp-server runs, because the two repositories share one lint
# standard and a different linter release is a different standard.
GOLANGCI_LINT_VERSION := v2.13.1

# govulncheck is the opposite case: a `tool` directive in go.mod. `go install
# golang.org/x/vuln/cmd/govulncheck` without @version installs the release
# go.mod names, so the scanner moves only in a commit that runs `go get -tool
# golang.org/x/vuln/cmd/govulncheck@<version>`; Dependabot does not bump tool
# modules (see scripts/govulncheck.sh). Its dependencies sit in go.sum but in
# no package this module builds, so none reaches a binary. Read back here only
# for the tools-versions table; recursive (=) so go.mod is read only by the
# targets that ask.
GOVULNCHECK_VERSION = $(shell awk '$$1 == "golang.org/x/vuln" {print $$2; exit}' go.mod)

# Arguments the utility targets take. Each one is a variable rather than a
# positional argument so that a missing value is a named thing the target can
# complain about.
CONFIG ?= config.yaml
ARGS   ?=
REPO   ?=
STORE  ?= influxdb
DS     ?=
RANGE  ?=
DUMP   ?=
SCHEMA ?=
BRAND_DIR ?= brand

##@ General

help: ## List the targets
	@awk 'BEGIN {FS = ":.*## "} \
		/^##@ / {printf "\n\033[1m%s\033[0m\n", substr($$0, 5); next} \
		/^[a-zA-Z0-9_-]+:.*## / {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}' \
		$(MAKEFILE_LIST)
	@echo ""

all: analyze test build ## Run the static analysis suite, the tests and the build

version: build ## Print the version the built binary reports, and where it came from
	@$(BIN_DIR)/$(BINARY_NAME) -version
	@echo "stamped from VERSION=$(VERSION), commit $(COMMIT), dated $(BUILD_DATE)"

##@ Build

build: ## Build the collector into bin/
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY_NAME) $(CMD_PATH)

# The six legs match the goos/goarch matrix in .goreleaser.yaml. They are here
# to catch a compile failure on a platform this machine is not, not to produce
# what a release ships: a release comes from GoReleaser, which also builds the
# archives and the checksums.
build-all: build-linux-amd64 build-linux-arm64 build-darwin-amd64 build-darwin-arm64 build-windows-amd64 build-windows-arm64 ## Cross-compile the collector for every released platform

build-linux-amd64:
	@mkdir -p $(DIST_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY_NAME)-linux-amd64 $(CMD_PATH)

build-linux-arm64:
	@mkdir -p $(DIST_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY_NAME)-linux-arm64 $(CMD_PATH)

build-darwin-amd64:
	@mkdir -p $(DIST_DIR)
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY_NAME)-darwin-amd64 $(CMD_PATH)

build-darwin-arm64:
	@mkdir -p $(DIST_DIR)
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY_NAME)-darwin-arm64 $(CMD_PATH)

build-windows-amd64:
	@mkdir -p $(DIST_DIR)
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY_NAME)-windows-amd64.exe $(CMD_PATH)

build-windows-arm64:
	@mkdir -p $(DIST_DIR)
	CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY_NAME)-windows-arm64.exe $(CMD_PATH)

install: ## Install the collector into GOBIN, stamped like a build
	CGO_ENABLED=0 go install -trimpath -ldflags "$(LDFLAGS)" $(CMD_PATH)

run: ## Run the collector against CONFIG (default config.yaml); pass more flags in ARGS
	go run $(CMD_PATH) -config $(CONFIG) $(ARGS)

clean: ## Remove build, coverage and site artifacts
	rm -rf $(BIN_DIR) $(DIST_DIR) site/dist
	rm -f coverage.out coverage.html

##@ Test

test: ## Run every test with a coverage profile
	go test -count=1 -coverprofile=coverage.out $(PKGS)

test-short: ## Run every test once, without the coverage profile
	go test -count=1 $(PKGS)

# PKGS includes ./test/..., so the end-to-end suite runs under the detector too,
# and its harness builds the collector with -race through a build-tag seam
# (test/e2e/harness_race_test.go): a race inside the collector process fails
# the test that started it, with the report. -count=1 because that collector
# is built by the test at run time, out of sight of the test cache, which would
# otherwise replay a pass for a collector that has since changed.
test-race: ## Run every test under the race detector (what .github/workflows/race.yml runs)
	go test -count=1 -race -timeout $(RACE_TIMEOUT) $(PKGS)

# The end-to-end suite carries no build tag, so `make test` already runs it.
# This target exists to run it on its own, which is what you want while
# changing a sink: it builds the binary and drives it against a fake GitHub.
test-e2e: ## Run only the end-to-end suite against the fake GitHub
	go test -count=1 ./test/e2e/

# Up, test, down, and down even when the tests fail: a suite that leaves nine
# containers behind on a failure is a suite nobody runs twice. The status is
# captured rather than left to make, because a non-zero `go test` would
# otherwise stop the recipe before the teardown.
test-e2e-docker: ## Run the containerized suite against the real stores
	@command -v docker >/dev/null 2>&1 || { echo "make test-e2e-docker: docker is not installed"; exit 2; }
	@echo "=== Removing anything this project left behind ==="
	@$(E2E_DOCKER_COMPOSE) down --volumes --remove-orphans 2>/dev/null || true
	@echo "=== Preparing the directories the stack writes back to the host ==="
	rm -rf $(E2E_DOCKER_DIR)/out/prometheus-targets $(E2E_DOCKER_DIR)/out/otel $(E2E_DOCKER_DIR)/out/telegraf
	mkdir -p $(E2E_DOCKER_DIR)/out/prometheus-targets $(E2E_DOCKER_DIR)/out/otel $(E2E_DOCKER_DIR)/out/telegraf
	@echo "=== Starting the stores ==="
	$(E2E_DOCKER_COMPOSE) up -d --wait --wait-timeout 600
	@echo "=== Running the suite ==="
	@set +e; \
		go test -count=1 $(E2E_DOCKER_TESTFLAGS) -tags $(E2E_DOCKER_TAG) -timeout $(E2E_DOCKER_TIMEOUT) -v ./$(E2E_DOCKER_DIR)/; \
		echo $$? > $(E2E_DOCKER_DIR)/out/.status
	@echo "=== Tearing down ==="
	@status=$$(cat $(E2E_DOCKER_DIR)/out/.status); \
		teardown=0; \
		$(E2E_DOCKER_COMPOSE) down --volumes --remove-orphans || teardown=$$?; \
		rm -f $(E2E_DOCKER_DIR)/out/.status; \
		docker ps --filter "label=com.docker.compose.project=$(E2E_DOCKER_PROJECT)" --format '{{.Names}}' | grep . \
			&& { echo "FAIL: containers of $(E2E_DOCKER_PROJECT) are still running"; exit 1; } || true; \
		if [ "$$status" -ne 0 ]; then exit "$$status"; fi; \
		if [ "$$teardown" -ne 0 ]; then exit "$$teardown"; fi

# The containerized suite under the race detector. The unit and end-to-end
# suites cover the collector against fakes; this one drives it against the
# real stores, where the exporter answers scrapes while a sweep is still
# writing and the push sinks retry against a server that is slow for real,
# which is the timing no fake reproduces. The harness builds the collector
# with -race through the same seam (test/e2e/docker/harness_race_test.go) and
# fails the test that started it with the report. gitlab-mcp-server runs none
# of its container suites under the detector; this repository does because
# its containers are the sinks, the half of the program with the most
# concurrency.
test-e2e-docker-race: ## Run the containerized suite with the collector built under the race detector
	@$(MAKE) --no-print-directory test-e2e-docker E2E_DOCKER_TESTFLAGS=-race

# The two halves on their own, because the reason to fail an assertion is to
# go and look at the store, and a suite that tore the store down first cannot
# be looked at. The Go harness reuses a stack it finds already running and
# leaves it running.
e2e-docker-up: ## Start the containerized stores and leave them running
	@command -v docker >/dev/null 2>&1 || { echo "make e2e-docker-up: docker is not installed"; exit 2; }
	mkdir -p $(E2E_DOCKER_DIR)/out/prometheus-targets $(E2E_DOCKER_DIR)/out/otel $(E2E_DOCKER_DIR)/out/telegraf
	$(E2E_DOCKER_COMPOSE) up -d --wait --wait-timeout 600
	@$(E2E_DOCKER_COMPOSE) ps
	@echo
	@echo "Run the suite against it with:"
	@echo "  go test -count=1 -tags $(E2E_DOCKER_TAG) -timeout $(E2E_DOCKER_TIMEOUT) -v ./$(E2E_DOCKER_DIR)/"

e2e-docker-down: ## Stop the containerized stores and remove their volumes
	$(E2E_DOCKER_COMPOSE) down --volumes --remove-orphans
	@docker ps --filter "label=com.docker.compose.project=$(E2E_DOCKER_PROJECT)" --format '{{.Names}}' | grep . \
		&& { echo "FAIL: containers of $(E2E_DOCKER_PROJECT) are still running"; exit 1; } || echo "nothing of $(E2E_DOCKER_PROJECT) is left"

e2e-docker-logs: ## Show the containerized stores' logs (SERVICE=name for one)
	$(E2E_DOCKER_COMPOSE) logs --no-color --tail 200 $(SERVICE)

coverage: test ## Write the HTML coverage report to coverage.html
	go tool cover -html=coverage.out -o coverage.html

cover-check: ## Fail if coverage over cmd/ and internal/ is below COVERAGE_MIN
	go test -count=1 -coverpkg=$(COVERAGE_COVERPKG) -coverprofile=coverage.out $(COVERAGE_PKGS)
	@go tool cover -func=coverage.out | grep '^total:'
	@# The summary line is anchored: a plain "total" also matches any function
	@# whose name contains it, which yields two values and turns the comparison
	@# below into an awk syntax error that silently passes the gate.
	@COVERAGE=$$(go tool cover -func=coverage.out | grep '^total:' | awk '{print $$3}' | tr -d '%'); \
	if [ -z "$$COVERAGE" ]; then \
		echo "FAIL: no total coverage line in coverage.out"; exit 1; \
	fi; \
	if ! awk "BEGIN {exit !($$COVERAGE + 0 >= $(COVERAGE_MIN) + 0)}" 2>/dev/null; then \
		echo "FAIL: coverage $$COVERAGE% is below minimum $(COVERAGE_MIN)%"; exit 1; \
	fi; \
	echo "PASS: coverage $$COVERAGE% meets minimum $(COVERAGE_MIN)%"

# Two finer instruments than the percentage, for a package being changed. A
# line can be covered with only one outcome of its condition ever taken, which
# gobco reports, and a condition can be evaluated both ways by tests that pass
# whichever way it goes, which a surviving mutant reports.
coverage-conditions: ## Report the boolean conditions of PKG never evaluated both ways (gobco)
	@test -n "$(PKG)" || { echo "usage: make coverage-conditions PKG=./cmd/gen_dashboards"; exit 2; }
	cd $(PKG) && go run github.com/rillig/gobco@v1.3.4

coverage-mutants: ## Mutation-test PKG with gremlins, INVERT_LOGICAL on (gate: Lived 0, Not covered 0)
	@test -n "$(PKG)" || { echo "usage: make coverage-mutants PKG=./cmd/gen_dashboards"; exit 2; }
	go run github.com/go-gremlins/gremlins/cmd/gremlins@v0.6.0 unleash --invert-logical --workers 4 $(GREMLINS_FLAGS) $(PKG)

##@ Static analysis

lint: golangci-lint govulncheck ## Run golangci-lint and govulncheck

# The three commands CI's golangci-lint job runs, in its order. The build tag of
# the containerized suite is in .golangci.yml's run.build-tags, so this one run
# reaches test/e2e/docker as well as everything else.
golangci-lint: ## Verify the linter config, check formatting, then lint
	@echo "=== golangci-lint config verify ==="
	golangci-lint config verify
	@echo "=== golangci-lint fmt --diff ==="
	golangci-lint fmt --diff $(FMT_PATHS)
	@echo "=== golangci-lint run ==="
	golangci-lint run $(PKGS)

# With the build tag too, for the reason golangci-lint has it in its config: a
# vulnerable call made only from the containerized suite is still a call. The
# wrapper fails on any advisory our code reaches unless it is on its
# allowlist, which is empty (see scripts/govulncheck.sh).
govulncheck: ## Scan for known vulnerabilities in what the code actually reaches
	./scripts/govulncheck.sh -tags $(E2E_DOCKER_TAG) $(PKGS)

# Scoped to FMT_PATHS for the reason given where it is defined, and because the
# formatter is the one command that rewrites files: with no path argument
# golangci-lint formats the whole tree, and .golangci.yml excludes no path,
# exactly as gitlab-mcp-server's does. Unscoped, `make fmt` would rewrite
# git-ignored working notes and `make fmt-check` could report drift CI can
# never see.
fmt: ## Apply every formatter .golangci.yml configures
	golangci-lint fmt $(FMT_PATHS)

fmt-check: ## Report formatting drift without rewriting anything
	golangci-lint fmt --diff $(FMT_PATHS)

vet: ## Run go vet
	go vet $(PKGS)

tidy: ## Tidy go.mod and go.sum
	go mod tidy

# pnpm dlx fetches the pinned release into pnpm's cache and runs it, so nothing
# is installed into the repository or onto PATH. The rules are in
# .markdownlint-cli2.jsonc, which the CLI and the CI action both read.
mdlint: ## Lint every Markdown and MDX file (markdownlint-cli2, as CI runs it)
	@echo "=== markdownlint ==="
	pnpm dlx markdownlint-cli2@$(MARKDOWNLINT_CLI2_VERSION) $(MDLINT_GLOBS)

mdlint-fix: ## Apply markdownlint's automatic fixes (writes files)
	@echo "=== markdownlint --fix ==="
	pnpm dlx markdownlint-cli2@$(MARKDOWNLINT_CLI2_VERSION) --fix $(MDLINT_GLOBS)

check-doc-links: ## Check that every relative link in tracked Markdown and MDX resolves
	@echo "=== documentation local links ==="
	node scripts/check-doc-links.mjs

# docs/ is output, not source: every file in it is generated from the English
# pages of the site by site/scripts/gen-docs.mjs. Edit the page, run this.
# Both targets need the site's dependencies, which is the one thing they cannot
# get for themselves, so they say so rather than failing on a missing module.
docs: ## Regenerate docs/ from the site's English pages (site/scripts/gen-docs.mjs)
	@test -d site/node_modules || { echo "make docs: the site's dependencies are not installed. Run make site-install"; exit 2; }
	cd site && pnpm run docs

check-docs: ## Fail if docs/ no longer matches the pages it is generated from
	@echo "=== docs/ up to date ==="
	@test -d site/node_modules || { echo "make check-docs: the site's dependencies are not installed. Run make site-install"; exit 2; }
	cd site && pnpm run docs:check

# Unlike a prerequisite chain, every step runs even when an earlier one fails
# and the summary names each failure, so one pass tells you everything instead
# of one thing per pass. There is no separate gofmt or go vet step: gofumpt is
# gofmt with more rules, and golangci-lint's govet runs every analyzer go vet
# runs and more, so both would repeat a question the linter already answered.
# check-dashboards and check-docs are in the list because a dashboard that no
# longer matches its specification, or a file under docs/ that no longer
# matches the page it is generated from, is the same kind of defect as a lint
# finding: something committed that the source no longer produces.
analyze: ## Run the whole static-analysis suite and report every failure at once
	@analysis_status=0; \
	run_check() { \
		step="$$1"; \
		shift; \
		echo "$$step"; \
		output="$$( "$$@" 2>&1 )"; \
		status="$$?"; \
		if [ "$$status" -ne 0 ]; then \
			if [ -n "$$output" ]; then echo "$$output"; fi; \
			echo "FAIL (exit $$status)"; \
			analysis_status=1; \
		else \
			echo "OK"; \
		fi; \
		echo ""; \
	}; \
	echo "============================================================"; \
	echo " Static analysis suite - ghchronicle"; \
	echo "============================================================"; \
	echo "Go toolchain: $$GOTOOLCHAIN (go.mod: $(PROJECT_GO_VERSION))"; \
	echo "Go analysis packages: $(PKGS)"; \
	echo "Go analysis build tags: $(E2E_DOCKER_TAG)"; \
	echo ""; \
	run_check "[1/8] golangci-lint config verify" golangci-lint config verify; \
	run_check "[2/8] golangci-lint fmt" golangci-lint fmt --diff $(FMT_PATHS); \
	run_check "[3/8] golangci-lint run" golangci-lint run $(PKGS); \
	run_check "[4/8] govulncheck" $(MAKE) --no-print-directory govulncheck; \
	run_check "[5/8] markdownlint" $(MAKE) --no-print-directory mdlint; \
	run_check "[6/8] documentation local links" $(MAKE) --no-print-directory check-doc-links; \
	run_check "[7/8] dashboards up to date" $(MAKE) --no-print-directory check-dashboards; \
	run_check "[8/8] docs/ up to date" $(MAKE) --no-print-directory check-docs; \
	echo "============================================================"; \
	if [ "$$analysis_status" -ne 0 ]; then \
		echo "Analysis failed. Review the findings above."; \
		exit "$$analysis_status"; \
	fi; \
	echo "Analysis complete. Everything passed."

# The automatic fixes of the tools above, in the order that lets each start
# from the previous one's output: formatting first, then the linters' own
# fixes, then Markdown. A fix is still a change to review, and `make analyze`
# afterwards says what is left for a person.
analyze-fix: ## Apply every automatic fix the analysis tools offer (writes files)
	@echo "=== Applying automatic fixes ==="
	@echo "[1/3] golangci-lint fmt"
	golangci-lint fmt $(FMT_PATHS)
	@echo "[2/3] golangci-lint run --fix"
	-golangci-lint run --fix $(PKGS)
	@echo "[3/3] markdownlint --fix"
	-pnpm dlx markdownlint-cli2@$(MARKDOWNLINT_CLI2_VERSION) --fix $(MDLINT_GLOBS)
	@echo "=== Fixes applied. Run 'make analyze' to verify. ==="

sonar: ## Scan with SonarCloud locally (needs sonar-scanner and SONAR_TOKEN)
	@command -v sonar-scanner >/dev/null 2>&1 || { echo "make sonar: sonar-scanner is not installed"; exit 2; }
	@test -n "$$SONAR_TOKEN" || { echo "make sonar: SONAR_TOKEN is not set"; exit 2; }
	@# The scanner reads coverage.out, and the profile has to be the one the
	@# coverage floor measures, so it is regenerated here, floor included,
	@# rather than reusing whatever `make test` last left behind. A profile
	@# below the floor is not scanned: in CI the scan job needs the coverage job.
	$(MAKE) --no-print-directory cover-check
	sonar-scanner -Dsonar.host.url=https://sonarcloud.io

##@ Dashboards and brand

# Two different things are called checking a dashboard, and the names below keep
# them apart. check-dashboards is offline: it asks whether the committed JSON
# still matches the specification in cmd/internal/dashboards, so it belongs in
# CI. check-dashboards-live, check-prometheus and check-postgres ask whether the
# queries actually work, which needs a running Grafana or PostgreSQL and can
# therefore never run in CI.

gen-dashboards: ## Regenerate the five Grafana dashboards into dashboards/ (cmd/gen_dashboards)
	go run ./cmd/gen_dashboards

check-dashboards: ## Fail if the committed dashboards no longer match the specification (offline)
	go run ./cmd/gen_dashboards -check

check-dashboards-live: ## Run every panel of one dashboard through a live Grafana (STORE=, DS=, GRAFANA_TOKEN) (cmd/check_dashboards)
	@test -n "$(DS)" || { echo "make check-dashboards-live: set DS=<datasource-uid>, and STORE=<store> if not $(STORE). RANGE is optional, e.g. RANGE=now-90d"; exit 2; }
	@test -n "$$GRAFANA_TOKEN" || { echo "make check-dashboards-live: GRAFANA_TOKEN is not set. GRAFANA_URL may also need setting"; exit 2; }
	go run ./cmd/check_dashboards $(STORE) $(DS) $(RANGE)

check-prometheus: ## Check the Prometheus panels against a /metrics dump (DUMP=, optional DS= to also parse them) (cmd/check_prometheus)
	@test -n "$(DUMP)" || { echo "make check-prometheus: set DUMP=<file holding a dump of the exporter's /metrics>. DS=<datasource-uid> additionally posts each expression to Grafana"; exit 2; }
	@test -z "$(DS)" || test -n "$$GRAFANA_TOKEN" || { echo "make check-prometheus: DS is set but GRAFANA_TOKEN is not, so no expression could be posted"; exit 2; }
	go run ./cmd/check_prometheus $(DUMP) $(DS)

check-postgres: ## EXPLAIN every PostgreSQL panel query against a scratch database (SCHEMA=) (cmd/check_postgres)
	@test -n "$(SCHEMA)" || { echo "make check-postgres: set SCHEMA=<schema.json>. Fetch one first with: GRAFANA_TOKEN=... go run ./cmd/check_postgres --dump <influxdb-datasource-uid> schema.json"; exit 2; }
	@# The command declares the schema in a scratch database over `sudo -u
	@# postgres psql`, so it needs a local PostgreSQL and the right to reach it.
	@command -v psql >/dev/null 2>&1 || { echo "make check-postgres: psql is not installed, and the queries are planned by PostgreSQL itself"; exit 2; }
	go run ./cmd/check_postgres $(SCHEMA)

publish-dashboard: ## Overwrite one dashboard in a running Grafana (STORE=, DS=, GRAFANA_TOKEN) (cmd/publish_dashboard)
	@test -n "$(DS)" || { echo "make publish-dashboard: set DS=<datasource-uid>, and STORE=<store> if not $(STORE). This writes to a live server"; exit 2; }
	@test -n "$$GRAFANA_TOKEN" || { echo "make publish-dashboard: GRAFANA_TOKEN is not set. GRAFANA_URL may also need setting"; exit 2; }
	go run ./cmd/publish_dashboard $(STORE) $(DS)

gen-brand: ## Regenerate the mark and the favicons (cmd/gen_brand mark)
	go run ./cmd/gen_brand mark -out $(BRAND_DIR)

# Kept apart from gen-brand because only this half shells out to rsvg-convert:
# a machine without librsvg can still refresh the mark, which is pure text.
gen-brand-compose: ## Regenerate the banner, social and og images (cmd/gen_brand compose, needs rsvg-convert)
	@command -v rsvg-convert >/dev/null 2>&1 || { echo "make gen-brand-compose: rsvg-convert is not installed, and the PNGs that ship are rendered with it"; exit 2; }
	go run ./cmd/gen_brand compose -out $(BRAND_DIR)

probe: ## Run the collectors against one repository and print the line protocol, writing nothing (cmd/probe)
	@test -n "$$GITHUB_TOKEN" || echo "note: GITHUB_TOKEN is not set, so this runs against the unauthenticated rate limit"
	@# REPO is optional: the command has a default repository of its own, and
	@# GHC_DUMP=<family> narrows the output to one family.
	go run ./cmd/probe $(REPO)

##@ Documentation site

# The site is a separate pnpm workspace under site/, and pnpm is the house
# package manager: npm and yarn are not used here.
#
# Every command changes into site/ rather than passing --dir site. Corepack
# picks the pnpm release from the package.json of the directory it starts in,
# and the repository root has none, so from the root it starts its own default
# release, which refuses to run whenever it is not the one the packageManager
# field in site/package.json pins. From inside site/ it starts the pinned
# release, the one CI reads from the same field.

site-install: ## Install the site's dependencies and the browser mermaid renders with
	@command -v pnpm >/dev/null 2>&1 || { echo "make site-install: pnpm is not installed (corepack enable, then corepack prepare pnpm --activate)"; exit 2; }
	cd site && pnpm install --frozen-lockfile
	site/node_modules/.bin/playwright install --with-deps chromium

site-dev: ## Serve the documentation site with hot reload
	cd site && pnpm dev

site-build: ## Build the documentation site into site/dist
	cd site && pnpm build

# The gates CI's site-lint job runs, in its order: the ones that need no build.
site-check: ## Run the site's fast checks: astro check, contrast, i18n parity, the landing's counts, the figures, docs/, eslint and formatting
	cd site && pnpm run check
	cd site && pnpm run contrast:check
	cd site && pnpm run i18n:check
	cd site && pnpm run stats:check
	cd site && pnpm run figures:check
	cd site && pnpm run docs:check
	cd site && pnpm run eslint
	cd site && pnpm run format:check

site-analyze: ## Run every site check the deploy workflow runs, build included
	cd site && pnpm run analyze

site-preview: site-build ## Serve the built site
	cd site && pnpm preview

##@ Tools and release

install-tools: ## Install the pinned analysis tools and goreleaser into GOBIN
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	go install golang.org/x/vuln/cmd/govulncheck
	go install github.com/goreleaser/goreleaser/v2@latest
	@echo "Installed. Run 'make tools-versions' to see what PATH resolves."

# The second column is the tell when a stale binary earlier in PATH shadows the
# version just installed, which otherwise shows up as a lint finding that only
# one machine can reproduce.
tools-versions: ## Show the pinned tool versions beside what PATH actually resolves
	@# `cmd 2>/dev/null | filter || echo` cannot report a missing tool: the
	@# pipeline's status is the filter's, and a filter fed nothing still exits
	@# 0, so the line would end after "PATH: " with nothing at all. Ask
	@# `command -v` first instead.
	@printf "%-16s pin %-10s PATH: " golangci-lint $(GOLANGCI_LINT_VERSION); \
		if command -v golangci-lint >/dev/null 2>&1; then golangci-lint version 2>/dev/null | head -1; else echo "not installed"; fi
	@printf "%-16s pin %-10s PATH: " govulncheck $(GOVULNCHECK_VERSION); \
		if command -v govulncheck >/dev/null 2>&1; then govulncheck -version 2>/dev/null | sed -n 's/^Scanner: //p'; else echo "not installed"; fi
	@printf "%-16s pin %-10s PATH: " goreleaser latest; \
		if command -v goreleaser >/dev/null 2>&1; then goreleaser --version 2>/dev/null | sed -n 's/^GitVersion: *//p'; else echo "not installed"; fi

release-check: ## Validate .goreleaser.yaml without releasing anything
	@command -v goreleaser >/dev/null 2>&1 || { echo "make release-check: goreleaser is not installed (make install-tools)"; exit 2; }
	goreleaser check

docker-build: ## Build the container image locally, stamped with VERSION and COMMIT
	@command -v docker >/dev/null 2>&1 || { echo "make docker-build: docker is not installed"; exit 2; }
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		-t $(BINARY_NAME):$(VERSION) \
		-t $(BINARY_NAME):latest \
		.

# Lint and analysis tools are pinned as `tool` directives in internal/tools/go.mod,
# a module separate from the library's so the library's go.mod stays
# dependency-free. They are built from inside that module into ./bin, which is
# the only safe way to do it: running `go get`/`go mod tidy` against that go.mod
# from the repo root (via -modfile) makes the go command treat the whole repo as
# the tools module and pull the *published* library from the proxy. Never do that.

TOOLS_DIR     := internal/tools
BIN           := $(CURDIR)/bin
LINT_WORKFLOW := .github/workflows/lint.yml

# Tools are built with the Go on PATH, never an auto-downloaded one. If a tool
# bump (e.g. from Dependabot) raises the tools module's `go` directive past the
# toolchain CI installs, the build fails loudly instead of silently compiling
# the linters with a newer Go than the library supports.
GO_LOCAL   := GOTOOLCHAIN=local go
TOOLS      := $(notdir $(shell cd $(TOOLS_DIR) && $(GO_LOCAL) list tool))

# Stamp file named after the toolchain and platform, so upgrading Go rebuilds
# every tool. Linters compiled against an older toolchain's analysis packages
# can reject or silently skip code that uses newer syntax, and a checkout shared
# across OSes (a container mounting the host's working copy) must not reuse
# binaries built for the other platform.
GO_PLATFORM := $(shell cd $(TOOLS_DIR) && $(GO_LOCAL) env GOVERSION GOOS GOARCH | tr '\n' '-' | sed 's/-$$//')
GO_STAMP    := $(BIN)/.go-$(GO_PLATFORM)

.DEFAULT_GOAL := help

.PHONY: help tools tools-tidy clean fmt lint check-ci check-lint-expands $(addprefix lint-,$(TOOLS)) test

# If `go list tool` failed, TOOLS is empty, so `tools` and `lint` would have no
# prerequisites and make would report success having done nothing.
define require_tools
@test -n "$(TOOLS)" || { echo "error: no tools found; 'go list tool' failed in $(TOOLS_DIR)" >&2; exit 1; }
endef

## help: list available targets
help:
	@grep -hE '^## ' $(MAKEFILE_LIST) | sed 's/^## //'

## tools: build every tool from internal/tools/go.mod into ./bin
tools: $(addprefix $(BIN)/,$(TOOLS))
	$(require_tools)

# One binary per tool, rebuilt when the pins or the toolchain change. `go build`
# hits the build cache, so a rebuild with unchanged inputs is near-instant.
$(BIN)/%: $(TOOLS_DIR)/go.mod $(TOOLS_DIR)/go.sum $(GO_STAMP)
	@pkg=$$(cd $(TOOLS_DIR) && $(GO_LOCAL) list tool | grep -E '(^|/)$*$$'); \
	 n=$$(printf '%s' "$$pkg" | grep -c . || true); \
	 test "$$n" -eq 1 || { \
	   echo "error: expected exactly one tool directive matching '$*' in $(TOOLS_DIR)/go.mod, found $$n" >&2; \
	   test "$$n" -eq 0 || echo "$$pkg" >&2; \
	   exit 1; \
	 }; \
	 echo "building $* from $$pkg"; \
	 cd $(TOOLS_DIR) && $(GO_LOCAL) build -o $@ $$pkg

$(GO_STAMP):
	@mkdir -p $(BIN)
	@rm -f $(BIN)/.go-*
	@touch $@

## clean: remove built tool binaries
clean:
	rm -f $(BIN)/* $(BIN)/.go-*
	@rmdir $(BIN) 2>/dev/null || true

## tools-tidy: tidy the tools module (use this, never `go mod tidy -modfile=...`)
tools-tidy:
	cd $(TOOLS_DIR) && go mod tidy

## lint: run every check CI runs
# Runs under -k so one failing tool does not hide the others, matching CI's
# fail-fast: false matrix.
lint:
	$(require_tools)
	@$(MAKE) -k check-ci $(addprefix lint-,$(TOOLS))

## check-ci: fail if the tool pins, the lint targets and the CI matrix disagree
# Three places name the tools: `tool` directives in the tools module, the
# lint-* recipes below, and the CI matrix. A tool missing from the matrix is
# the dangerous drift: `make lint` would run it locally while CI never does.
# This target also asserts its own matrix entry exists, otherwise deleting that
# entry would disable the check without any signal.
# CI runs the individual lint-* targets, never the `lint` wrapper, so a broken
# wrapper would only surface on a contributor's machine. `-n` expands the whole
# graph, recursion included, without running a single tool. The MAKELEVEL guard
# stops the expansion from re-entering itself.
check-lint-expands:
	@test "$(MAKELEVEL)" -gt 0 || $(MAKE) -n lint >/dev/null || { \
	   echo "error: 'make lint' does not expand cleanly" >&2; exit 1; }

check-ci: check-lint-expands
	$(require_tools)
	@want=$$(printf '%s\n' $(addprefix lint-,$(TOOLS)) | sort -u); \
	 mk=$$(grep -oE '^lint-[a-z0-9-]+' Makefile | sort -u); \
	 ci=$$(grep -oE 'target: *lint-[a-z0-9-]+' $(LINT_WORKFLOW) | sed 's/.*: *//' | sort -u); \
	 status=0; \
	 [ "$$want" = "$$mk" ] || { status=1; \
	   echo "error: $(TOOLS_DIR)/go.mod tools and Makefile lint targets disagree" >&2; }; \
	 [ "$$want" = "$$ci" ] || { status=1; \
	   echo "error: $(TOOLS_DIR)/go.mod tools and the CI matrix in $(LINT_WORKFLOW) disagree" >&2; }; \
	 grep -qE 'target: *check-ci' $(LINT_WORKFLOW) || { status=1; \
	   echo "error: $(LINT_WORKFLOW) has no 'target: check-ci' entry, so CI would not run this check" >&2; }; \
	 if [ $$status -ne 0 ]; then \
	   echo "tools:  $$want" >&2; echo "make:   $$mk" >&2; echo "ci:     $$ci" >&2; \
	 fi; \
	 exit $$status

lint-golangci-lint: $(BIN)/golangci-lint
	$(BIN)/golangci-lint run ./...

# govulncheck downloads the vulnerability database on every run, so an offline
# machine gets a red `make lint` that looks like a finding. Say which it is.
lint-govulncheck: $(BIN)/govulncheck
	@out=$$($(BIN)/govulncheck ./... 2>&1); status=$$?; \
	 echo "$$out"; \
	 if [ $$status -ne 0 ] && echo "$$out" | grep -q 'fetching vulnerabilities'; then \
	   echo "note: could not reach the vulnerability database; this is a network failure, not a finding" >&2; \
	 fi; \
	 exit $$status

# This repo is a library: the only reachability roots are cmd/, examples/ and
# (via -test) the test files. Any exported FUNCTION OR METHOD no test or
# example calls is reported as dead. That is intentional: new exported funcs
# must ship with a test or an example. Exported types, constants and variables
# are NOT covered by this tool. See CONTRIBUTING.md.
#
# deadcode exits zero even when it finds something, so fail on any output. The
# -f template emits GitHub workflow commands so findings show up as inline
# annotations on the PR diff; locally they are still readable.
lint-deadcode: $(BIN)/deadcode
	@out="$$($(BIN)/deadcode -test \
		-f '{{range .Funcs}}::error file={{.Position.File}},line={{.Position.Line}},col={{.Position.Col}}::unreachable func: {{.Name}}{{"\n"}}{{end}}' \
		./...)"; \
	if [ -n "$$out" ]; then \
		n=$$(printf '%s\n' "$$out" | wc -l | tr -d ' '); \
		echo "Found $$n unreachable function(s) no test or example reaches."; \
		echo "GitHub shows at most 10 as annotations; the full list is below."; \
		echo "$$out"; \
		exit 1; \
	fi

## fmt: apply the configured formatters (gofumpt, goimports) in place
fmt: $(BIN)/golangci-lint
	$(BIN)/golangci-lint fmt ./...

## test: run the test suite with race detection (COVERPROFILE=file writes coverage)
COVERPROFILE ?=
test:
	go test -race -cover $(if $(COVERPROFILE),-coverprofile=$(COVERPROFILE)) ./...

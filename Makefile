# Lint and analysis tools are pinned as `tool` directives in internal/tools/go.mod,
# a module separate from the library's so the library's go.mod stays
# dependency-free. They are built from inside that module into ./bin, which is
# the only safe way to do it: running `go get`/`go mod tidy` against that go.mod
# from the repo root (via -modfile) makes the go command treat the whole repo as
# the tools module and pull the *published* library from the proxy. Never do that.

TOOLS_DIR := internal/tools
BIN       := $(CURDIR)/bin
TOOLS     := $(notdir $(shell cd $(TOOLS_DIR) && go list tool))

.PHONY: tools tools-tidy lint $(addprefix lint-,$(TOOLS)) test

## tools: build every tool from internal/tools/go.mod into ./bin
tools: $(addprefix $(BIN)/,$(TOOLS))

# One binary per tool, rebuilt only when the pins change. `go build` hits the
# build cache, so a rebuild with unchanged pins is near-instant.
$(BIN)/%: $(TOOLS_DIR)/go.mod $(TOOLS_DIR)/go.sum
	cd $(TOOLS_DIR) && go build -o $@ $$(go list tool | grep -E '(^|/)$*$$')

## tools-tidy: tidy the tools module (use this, never `go mod tidy -modfile=...`)
tools-tidy:
	cd $(TOOLS_DIR) && go mod tidy

## lint: run every check CI runs
lint: $(addprefix lint-,$(TOOLS))

lint-golangci-lint: $(BIN)/golangci-lint
	$(BIN)/golangci-lint run --timeout=5m ./...

lint-gofumpt: $(BIN)/gofumpt
	@out="$$($(BIN)/gofumpt -l .)"; \
	if [ -n "$$out" ]; then \
		echo "The following files are not gofumpt-formatted:"; \
		echo "$$out"; echo; echo "Diff:"; \
		$(BIN)/gofumpt -d .; \
		exit 1; \
	fi

lint-modernize: $(BIN)/modernize
	$(BIN)/modernize ./...

lint-govulncheck: $(BIN)/govulncheck
	$(BIN)/govulncheck ./...

# This repo is a library: the only reachability roots are cmd/, examples/ and
# (via -test) the test files. Any exported symbol no test or example calls is
# reported as dead. That is intentional: new public API must ship with a test
# or an example. See CONTRIBUTING.md.
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

## test: run the test suite with race detection
test:
	go test -race -cover ./...

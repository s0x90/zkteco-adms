# Contributing to ZKTeco ADMS

Thank you for considering a contribution! This document explains how to get
started.

## Prerequisites

- Go 1.26 or higher

- `make`

All lint and analysis tools (golangci-lint, gofumpt, modernize, govulncheck,
deadcode) are pinned in `internal/tools/go.mod` and built into `./bin` by the
Makefile, so nothing else needs to be installed. That module is separate from
the library's `go.mod`, which stays dependency-free.

## Getting Started

```bash
git clone https://github.com/s0x90/zkteco-adms.git
cd zkteco-adms
go test -race ./...
```

## Development Workflow

1. Fork the repository and create a feature branch from `master`.
2. Make your changes.
3. Run the full check suite before submitting:

```bash
make test
make lint
go build ./examples/basic ./examples/database
```

`make lint` runs exactly the checks CI runs; each is also available on its own
(`make lint-deadcode`, `make lint-golangci-lint`, ...).

To bump or add a tool, work inside the tools module and never point `go get`
or `go mod tidy` at it from the repo root with `-modfile`: that makes the go
command treat the whole repo as the tools module and pull the published
library from the proxy.

```bash
cd internal/tools
go get -tool mvdan.cc/gofumpt@vX.Y.Z   # or `go get -tool <pkg>@<version>` for a new tool
cd ../.. && make tools-tidy
```

4. Open a pull request against `master`.

## Code Style

- Follow standard Go conventions (`gofmt`, `goimports`).
- The project uses `golangci-lint` v2 with the config in `.golangci.yml`.
  Run it locally (see above) to catch issues before pushing.
- Keep the library at **zero external dependencies** (pure stdlib).
- Use US English spelling in comments and strings (enforced by `misspell`).

## Tests

All changes should include tests. This is enforced for public API by the
`deadcode` check: the library has no `main`, so its only reachability roots are
`cmd/`, `examples/` and the test files. An exported function that no test or
example calls is reported as dead and fails CI. If you add public API, add a
test or an example that exercises it in the same change.

Run the full suite with race detection:

```bash
go test -race -cover ./...
```

The project targets >90% coverage. You can generate an HTML coverage report:

```bash
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

## Commit Messages

Write clear commit messages with a summary line (imperative mood, ~72 chars)
and an optional body explaining the "why" behind the change.

## Reporting Issues

Open an issue on GitHub with steps to reproduce, expected behavior, and actual
behavior. Include your Go version and device model if relevant.

## License

By contributing, you agree that your contributions will be licensed under the
MIT License.

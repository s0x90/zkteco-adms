# Contributing to ZKTeco ADMS

Thank you for considering a contribution! This document explains how to get
started.

## Prerequisites

- Go 1.26 or higher

All lint and analysis tools (golangci-lint, gofumpt, modernize, govulncheck,
deadcode) are pinned in `internal/tools/go.mod` and run through `go tool`, so
nothing else needs to be installed. That module is separate from the library's
`go.mod`, which stays dependency-free.

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
go test -race -cover ./...
go tool -modfile=internal/tools/go.mod golangci-lint run ./...
go tool -modfile=internal/tools/go.mod gofumpt -l .
go tool -modfile=internal/tools/go.mod modernize ./...
go tool -modfile=internal/tools/go.mod govulncheck ./...
go tool -modfile=internal/tools/go.mod deadcode -test ./...
go build ./examples/basic ./examples/database
```

These are exactly the checks CI runs (`.github/workflows/lint.yml`).

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

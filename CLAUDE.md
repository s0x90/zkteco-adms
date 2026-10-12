# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`github.com/s0x90/zkteco-adms` (package `zkadms`): a Go library implementing the HTTP-based ZKTeco ADMS protocol that biometric attendance devices use to push data to a server and poll for commands. **Zero external dependencies, stdlib only; keep it that way.** The library is the root package; `cmd/probe` and `examples/basic|commands` (`commands` also serves an embedded web dashboard, `ui.go` + `web/index.html`, which polls `/ui/state` outside `logMiddleware` to avoid log spam). Per-device timezones there are server-side only (`PUT /api/devices/{sn}/timezone`, `-devices SN=Zone`, `-timezone`); `POST .../timezone/apply` sends `SET OPTION TimeZone=<whole hours>` to the device, but that key and format come from third-party PUSH docs, not from commands confirmed in this repo. The library's `CommandResult` drops the `Content=` field of device replies, so the example captures raw `/iclock/devicecmd` bodies itself (`captureDeviceReplies`) are `main` packages in the same module. **`examples/database` is its own module** (own `go.mod`, `replace` to `../..`) because it uses the pure-Go SQLite driver `modernc.org/sqlite`; this keeps the root `go.mod` dependency-free. `./...` from the root skips it, so `make test` runs its tests explicitly, but golangci-lint, deadcode and govulncheck do not cover it.

## Commands

```bash
go build ./...
make test                          # go test -race -cover ./...  (COVERPROFILE=coverage.out to write coverage)
go test -race -run TestName ./...  # single test (root package tests are in the repo root)
(cd examples/database && go test -race ./...)  # separate module, see below
make lint                          # everything CI's lint workflow runs, via -k so all failures show
make lint-golangci-lint            # or lint-govulncheck / lint-deadcode / check-ci individually
make fmt                           # gofumpt + goimports, applied in place via golangci-lint
```

- Go 1.26 required. `GOTOOLCHAIN=local` is used for building lint tools, so a too-old local Go fails loudly instead of auto-downloading.
- Lint tools (golangci-lint v2, govulncheck, deadcode) are pinned as `tool` directives in `internal/tools/go.mod`, a **separate module** so the library's `go.mod` stays dependency-free. They are built into `./bin` by the Makefile. **Never run `go get`/`go mod tidy` against the tools module from the repo root (e.g. with `-modfile`)**; it pulls the published library from the proxy. Use `cd internal/tools && go get -tool <pkg>@<ver>` then `make tools-tidy`.
- Adding or removing a lint tool means editing three places that `make check-ci` verifies agree: `internal/tools/go.mod`, the `lint-*` targets in the `Makefile`, and the matrix in `.github/workflows/lint.yml`.
- `govulncheck` needs network access; a "fetching vulnerabilities" failure is a network problem, not a finding.

### The `deadcode` gate

`lint-deadcode` fails on any exported **function or method** not reachable from `cmd/`, `examples/`, or a test file (`-test`). Any new exported func/method must be called by a test or an example in the same change. Exported types, constants, and vars are not checked. Project targets >90% coverage.

### Style enforced by lint

US English spelling (`misspell`), `gofumpt`/`goimports` formatting, `modernize`, `gocritic`, `revive`, `unparam`, `prealloc`, `gochecknoinits` (no `init()`). Writes to `http.ResponseWriter` via `fmt.Fprint*` intentionally ignore errors.

## Architecture

Everything is in the single `zkadms` package; one `ADMSServer` (in `adms.go`) holds all state, and the other files add methods to it:

- `adms.go`: `ADMSServer` struct, constructor, `Close`, the callback pipeline, and the stale-device eviction worker.
- `options.go`: functional options (`WithX` for the server, `WithDeviceX` for per-device `DeviceOption`), plus defaults.
- `handler.go`: `ServeHTTP` router (matches by path *suffix*) and the endpoint handlers `HandleCData`, `HandleRegistry`, `HandleGetRequest`, `HandleDeviceCmd`, `HandleInspect` (opt-in via `WithEnableInspect`, otherwise 404).
- `device.go`: device registry, online/activity tracking, timezone resolution.
- `command.go`: the command queue and the `SendXxxCommand` helpers.
- `parser.go`: parsing of ATTLOG, USERINFO, KV bodies, and devicecmd results, plus serial-number validation.
- `types.go`: public types, sentinel errors, constants (routes, query-param names, response strings, verify modes).

### Request flow

Every handler starts with `requireMethod` then `requireDevice`, which validates `?SN=` (1–64 chars of alphanumerics, hyphen, underscore), **auto-registers the device** (subject to `WithMaxDevices` → 503), and bumps `LastActivity`. Any device request therefore creates the device, and `QueueCommand` returns `ErrDeviceNotFound` for devices that have never contacted the server (or were evicted).

`HandleCData` branches on `?table=`: `ATTLOG` → attendance callback, `OPERLOG` → ack only, `USERINFO` → `WithOnQueryUsers`, anything else → treated as device info (POST body parsed as `key=value` lines) and then falls through to `writeCommandsOrOK`. `/iclock/registry` bodies are comma-separated `key=value` with `~` key prefixes stripped, and merged into `Device.Options`.

### Command lifecycle (spans command.go, handler.go, adms.go)

1. `QueueCommand` assigns a monotonically increasing ID from an atomic counter **at queue time** and appends to `commandQueue[sn]`. The ID is returned so callers can correlate it with the result later.
2. The device polls `/iclock/getrequest` (or `cdata`), and `DrainCommands` empties the queue, writes `C:<id>:<cmd>\n` lines, and moves each entry into `pendingCommands` (sent but unconfirmed).
3. The device POSTs `ID=..&Return=..&CMD=..` to `/iclock/devicecmd`. `HandleDeviceCmd` fills in `CommandResult.QueuedCommand` from `pendingCommands`, dispatches the callback, and **only deletes the pending entry after dispatch succeeds**, so a device retry after a 503 still correlates.
4. `WithMaxCommandsPerDevice` counts queued **plus** pending commands. Eviction removes both for stale devices.

`validateCommandField` rejects `\r`/`\n` in any command field because the wire format is line-based and would otherwise allow command injection. New `Send*` helpers must validate every interpolated field.

### Callbacks and concurrency

User callbacks never run on the HTTP goroutine. `dispatchCallback` pushes closures onto `callbackCh`, drained by one worker goroutine (so callbacks are sequential and ordered). If the channel is full it blocks up to `dispatchTimeout` and then drops. Handlers translate a drop into **HTTP 503** so the device retries. `safeCall` recovers callback panics. Callbacks receive `baseCtx`, which is cancelled only *after* `Close()` drains the queue.

Locking: `devicesMutex` and `queueMutex` are separate and must never be held together (eviction does device phase, then queue phase). `callbackMu` guards closing `callbackCh` against concurrent dispatchers. `GetDevice`/`ListDevices` return copies. Don't hand out pointers into `s.devices`.

### Timestamps

ATTLOG `YYYY-MM-DD HH:MM:SS` values are device-local and parsed in the effective timezone (per-device → `WithDefaultTimezone` → UTC); Unix-epoch values are UTC. Malformed lines are skipped rather than emitted with a zero timestamp.

## Protocol gotchas (verified on real hardware, SpeedFace-V5L-RFID / ZAM180-NF)

`datasheet/ADMS.md` has the vendor spec plus a "Real Device Protocol Findings" section. Where they differ, the real-device behavior wins. The README's "Confirmed Commands" and "Command Return Codes" tables are the source of truth for which commands actually work. In particular:

- Use `DATA UPDATE USERINFO` / `DATA DELETE USERINFO`, **not** the documented `USER ADD` / `USER DEL` (device returns `-1002`). `DATA DEL` also fails; the full word `DELETE` is required.
- `DATA QUERY ...` results arrive as a POST to `/iclock/cdata` (`table=USERINFO`), not in the devicecmd confirmation.
- devicecmd bodies come in two shapes (`&`-separated batched lines, and newline-separated multiline for `Shell` with `Content=`); `parseCommandResults` handles both by flushing the current result whenever a new `ID=` appears.

`cmd/probe` (`go run ./cmd/probe -addr :8080 -sn <SN>`) queues candidate commands against a real device to discover what a model supports; `-destructive` includes risky ones.

# Suggestions Not Yet Implemented

Code review suggestions PR #25 that have not been addressed yet.

## 1. Probe: correlate command results by ID instead of string matching

**File:** `cmd/probe/main.go:205`
**Severity:** Medium

Correlation in the probe is still based on `QueuedCommand` string matching
(plus FIFO fallback). Since `QueueCommand` now returns the assigned ID and
`CommandResult` includes `ID`, the probe can correlate results deterministically
by ID (e.g., keep an `id -> candidate` map when queuing). This avoids O(n)
scans per confirmation and prevents ambiguity if the same command string is
queued more than once or the device normalizes command formatting.

**Action:** Refactor the probe to maintain an `id -> candidate` map populated
when `QueueCommand` returns, and look up results by `CommandResult.ID` instead
of iterating `queued` by string comparison.

## 2. DrainCommands exports unexported type `commandEntry`

**File:** `adms.go:973`
**Severity:** High (breaking API)

`DrainCommands` is an exported method but returns `[]commandEntry`, where
`commandEntry` (and its fields) are unexported. This makes the API effectively
unusable for external callers and is also a breaking type change from `[]string`.

**Action:** Either:
- Export `commandEntry` (and its fields) as a public type (e.g., `CommandEntry`
  with `ID int64` and `Command string`), or
- Keep the public signature as `[]string` and add a separate exported
  method/type for ID+command pairs.

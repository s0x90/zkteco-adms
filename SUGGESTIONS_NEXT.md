# Suggested Next Improvements

## Priority 1: Prevent silent attendance loss

Problem:
- Attendance callbacks are dispatched through a fixed-size buffered channel.
- When the queue is full, events are dropped but the device still receives a success response.

Why this matters:
- Attendance ingestion is correctness-sensitive.
- Silent drops can create missing punch records that are hard to detect later.

Suggested changes:
- Replace fire-and-forget callback dispatch with one of these strategies:
  - block briefly with timeout before dropping
  - return backpressure and avoid acknowledging unprocessed data
  - persist records first, then acknowledge device requests
- Add metrics or counters for queued, processed, and dropped callbacks.
- Surface callback delivery failures in logs with device serial and record count.

Acceptance criteria:
- The server never reports successful attendance ingestion without a visible signal when records were dropped.
- Tests cover queue saturation behavior.

## Priority 2: Make shutdown deterministic

Problem:
- `Close()` can race with callback dispatch, which may lose callbacks during shutdown.

Why this matters:
- The current contract says pending callbacks are drained before exit.
- Shutdown races are rare but can lose real events.

Suggested changes:
- Rework callback lifecycle so shutdown stops new submissions before draining.
- Use a clearer ownership model such as:
  - closing the callback queue and draining it in the worker, or
  - tracking in-flight callbacks with `sync.WaitGroup`
- Ensure post-close dispatch is rejected in a deterministic way.

Acceptance criteria:
- `Close()` either drains all accepted callbacks or clearly rejects new ones before shutdown begins.
- Tests cover concurrent `Close()` plus active request handling.

## Priority 3: Harden callback API concurrency

Problem:
- `OnAttendance`, `OnDeviceInfo`, and `OnRegistry` are public mutable fields read without synchronization.

Why this matters:
- Users may change callbacks at runtime and accidentally introduce data races.
- The rest of the server appears goroutine-safe, so this edge is easy to miss.

Suggested changes:
- Document callbacks as configuration that must be set before serving, or
- Replace public fields with synchronized setter methods.
- If runtime mutation is supported, guard reads and writes with a mutex or atomic value.

Acceptance criteria:
- Callback lifecycle expectations are explicit in the API.
- Race tests pass when callbacks are configured as documented.

## Priority 4: Tighten attendance parsing and validation

Problem:
- Invalid timestamps or integer fields can degrade into zero values without rejection.

Why this matters:
- Bad device payloads can look like valid attendance records.
- Downstream systems may store corrupted timestamps or status codes.

Suggested changes:
- Return parse results with success and failure counts.
- Skip malformed lines and log structured parse errors.
- Consider exposing a validation hook or error callback for rejected rows.

Acceptance criteria:
- Malformed attendance rows are either rejected or explicitly flagged.
- Tests cover invalid timestamps, bad status codes, and partial rows.

## Priority 5: Fix example quality and safety

Problem:
- `examples/database_integration.go` manually builds JSON.
- Date filtering excludes boundary timestamps.

Why this matters:
- Examples are likely to be copied into production code.
- Invalid JSON and off-by-one date filters create avoidable bugs.

Suggested changes:
- Replace manual JSON assembly with `encoding/json` response structs.
- Use inclusive start boundaries for date filters.
- Optionally add build tags or separate example tests so examples stay compilable.

Acceptance criteria:
- Example endpoints always emit valid JSON.
- Daily summary includes records exactly at the start of the day.

## Priority 6: Align docs with actual behavior

Problem:
- Docs describe the library as a complete/full protocol implementation.
- Main examples do not consistently show `defer server.Close()`.
- Go version guidance is inconsistent across files.

Why this matters:
- Users may expect more protocol coverage than currently exists.
- Missing shutdown guidance can leak the worker goroutine or lose callbacks.

Suggested changes:
- Adjust wording in `README.md` to describe supported endpoints and current scope precisely.
- Add `defer server.Close()` to all setup snippets.
- Align `GETTING_STARTED.md`, `README.md`, CI, and `go.mod` around the same Go version policy.

Acceptance criteria:
- Docs accurately describe supported features and lifecycle requirements.
- Every quick-start sample matches current runtime behavior.

## Priority 7: Strengthen CI and test coverage

Problem:
- CI runs build, vet, and tests, but misses race detection and tagged example compilation.

Why this matters:
- The main remaining risks are concurrency-related.
- Example drift can go unnoticed.

Suggested changes:
- Add `go test -race ./...` to CI.
- Compile tagged examples explicitly in CI.
- Add tests for:
  - callback queue saturation
  - shutdown interleavings
  - invalid attendance rows
  - example JSON output

Acceptance criteria:
- CI catches concurrency regressions earlier.
- Public examples remain compilable and trustworthy.

## Recommended implementation order

1. Fix callback queue data-loss behavior.
2. Fix shutdown and callback lifecycle semantics.
3. Lock down callback API expectations.
4. Improve parsing validation.
5. Repair examples.
6. Update docs.
7. Expand CI and regression tests.

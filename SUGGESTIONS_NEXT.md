# Code Review Notes

Overall, the branch improves protocol handling and test coverage, but there are a few correctness and API consistency issues worth fixing before merging.

## Issues To Fix

### 1. Query-user commands are documented as supported, but the pushed result data is still discarded
- Severity: High
- Files: `README.md:344`, `README.md:354`, `datasheet/ADMS.md:366`, `adms.go:945`, `adms.go:958`
- Problem: Docs now say `DATA QUERY USERINFO` results are pushed back through `POST /iclock/cdata`, but the server currently only ACKs `OPERLOG` and otherwise treats non-table POST bodies as generic device info. That means a successful query can still lose the returned dataset.
- Why it matters: the new query command API appears usable, but callers cannot actually receive the queried user records.
- Suggested fix: either implement parsing/callbacks for query result payloads coming through `/iclock/cdata`, or tone down/remove the query command documentation until that data path is supported end-to-end.

### 2. Command results are not reliably correlatable for `CMD=DATA` confirmations
- Severity: High
- Files: `adms.go:397`, `adms.go:643`, `adms.go:760`, `cmd/probe/main.go:188`, `cmd/probe/main.go:192`
- Problem: command IDs are generated when responses are written to `/iclock/getrequest`, but the original command string is not stored anywhere by ID. Later, command confirmations only expose the echoed device `CMD`, which is often just `DATA` for multiple different commands.
- Why it matters: callers cannot tell whether a `DATA` confirmation belongs to add-user, delete-user, or query-user operations. The probe tool is already forced to infer this by FIFO ordering.
- Suggested fix: keep an `id -> original command` mapping and surface the original queued command in `CommandResult`, or return a handle/assigned ID from queueing APIs so callers can correlate confirmations safely.

### 3. Probe marks a device as online before it ever connects
- Severity: Medium
- Files: `cmd/probe/main.go:232`, `cmd/probe/main.go:261`, `adms.go:840`, `cmd/probe/main_test.go:558`
- Problem: the probe pre-registers the serial with `RegisterDevice`, and `RegisterDevice` sets `LastActivity` immediately. The later wait loop checks `IsDeviceOnline`, so the probe can announce the device as online even if no real device request has happened yet.
- Why it matters: this gives misleading diagnostics when users are trying to verify whether a device is actually reaching the server.
- Suggested fix: avoid pre-registering in the probe, or wait for a real callback/request from the device instead of relying on online status.

### 4. Commands example reports obsolete command names back to API clients
- Severity: Medium
- Files: `examples/commands/main.go:418`, `examples/commands/main.go:441`, `examples/commands/main_test.go:711`, `examples/commands/main_test.go:778`, `README.md:352`
- Problem: the example API queues `DATA UPDATE USERINFO` / `DATA DELETE USERINFO`, but the JSON response still says `USER ADD` / `USER DEL`.
- Why it matters: this conflicts with the updated protocol guidance and exposes inaccurate wire-command information to API consumers.
- Suggested fix: return the actual queued command string, or rename the response field so it represents an abstract operation instead of the on-wire ADMS command.

## Suggested Priority
1. Fix command-result correlation.
2. Either implement query-result ingestion or narrow the docs/API claims.
3. Correct probe online detection.
4. Align the example API response with the actual queued commands.

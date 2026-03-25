# Deferred Items for Next PR

The following high-severity architectural improvements were identified during
Copilot code review of PR #23 and are intentionally deferred to a follow-up PR.

## 1. Command-result correlation for `CMD=DATA` confirmations

- **Severity:** High
- **Problem:** Command IDs are generated when responses are written to
  `/iclock/getrequest`, but the original command string is not stored anywhere
  by ID. Later, command confirmations only expose the echoed device `CMD`,
  which is often just `DATA` for multiple different commands (add-user,
  delete-user, query-user). Callers cannot reliably correlate a `DATA`
  confirmation back to the operation that triggered it.
- **Current workaround:** The probe tool and callers infer correlation by FIFO
  ordering, which is fragile when commands fail or are reordered.
- **Planned fix:**
  - Assign command IDs at queue time (not at write time).
  - Maintain an `id -> original command` mapping internally.
  - Surface the original queued command string in `CommandResult`.
  - Optionally return the assigned ID from `QueueCommand` / helper methods
    so callers can correlate confirmations unambiguously.

## 2. Query-user result ingestion via `/iclock/cdata`

- **Severity:** High
- **Problem:** `SendQueryUsersCommand` queues a `DATA QUERY USERINFO` command
  and the device responds by pushing user data via `POST /iclock/cdata`. However,
  the current `HandleCData` implementation only acknowledges these pushes and
  does not parse, dispatch, or surface the returned user records via any callback.
  Query results are effectively silently discarded.
- **Current workaround:** Documentation and docstrings now note this limitation,
  advising callers to handle `/iclock/cdata` requests themselves.
- **Planned fix:**
  - Implement parsing for user-data query result payloads arriving on
    `/iclock/cdata`.
  - Expose the returned user records to callers via a new callback
    (e.g., `WithOnQueryResult` or similar).
  - If full ingestion is deferred further, narrow the API surface so it does
    not imply end-to-end support for returning user datasets.

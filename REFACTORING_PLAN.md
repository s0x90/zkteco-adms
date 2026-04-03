# Refactoring Plan: Split adms.go and adms_test.go

## Overview

Split the monolithic `adms.go` (originally ~2,000 lines) and `adms_test.go` (5,391 lines) into 7 logically organized source files and 7 matching test files. This is a pure refactor — no behavioral changes, all symbols stay in `package zkadms`.

## Source File Split (COMPLETED)

| File | Lines | Content |
|---|---|---|
| `adms.go` | 355 | Server struct, lifecycle (New, Close), dispatch internals |
| `types.go` | 259 | Constants, verify modes, sentinel errors, type definitions |
| `options.go` | 213 | `Option`/`DeviceOption` types, all `With*` functions |
| `device.go` | 142 | Device registration, online status, timezone, eviction |
| `command.go` | 221 | Command queuing, draining, pending counts, Send* methods |
| `handler.go` | 416 | HTTP handlers, request helpers (ServeHTTP, handleCData, etc.) |
| `parser.go` | 268 | Parsing functions for attendance, commands, KV pairs, users |

## Test File Split (COMPLETED)

| Test File | Tests | Content |
|---|---|---|
| `adms_test.go` | 19 tests | Server lifecycle, dispatch internals |
| `types_test.go` | 10 tests | Constants, verify modes, sentinel errors |
| `options_test.go` | 33 tests | Option/DeviceOption types, all With* functions |
| `device_test.go` | 24 tests | Device registration, online status, timezone, eviction |
| `command_test.go` | 37 tests | Command queuing, draining, pending counts, Send* methods |
| `handler_test.go` | 72 tests + 1 benchmark | HTTP handlers, request helpers |
| `parser_test.go` | 18 tests + 1 benchmark | Parsing functions for attendance, commands, KV pairs, users |

## Key Constraints

- All tests stay in `package zkadms` (not `_test`) because they access internal fields
- Each file gets only the imports it actually needs
- All new test files + rewritten adms_test.go must be created in one batch before compiling (to avoid duplicate symbol errors)
- No behavioral changes — pure file reorganization

## Test Assignment Details

### adms_test.go (KEEP these 19 tests + 2 benchmarks)

- `TestNewADMSServer`
- `TestCloseIdempotent`
- `TestDispatchAfterClose`
- `TestCallbackPanicRecovery`
- `BenchmarkHandleCData`
- `BenchmarkParseAttendanceRecords`
- `TestDispatchCallback_ReturnsFalseAfterClose`
- `TestClose_DrainsAllAcceptedCallbacks`
- `TestClose_ConcurrentDispatchAndClose`
- `TestNewADMSServer_Defaults`
- `TestDispatchAttendance_EmptyRecords`
- `TestDispatchAttendance_CallbackQueueFull`
- `TestDispatchCommandResult_NilCallback`
- `TestDispatchCallback_AfterClose`
- `TestConcurrentDispatchAndClose`
- `TestDispatchCallback_SlowPathSuccess`
- `TestDispatchCallback_CloseWhileWaiting`
- `TestDispatchQueryUsers_EmptySlice`
- `TestClose_DeliversLiveContextToCallbacks`

### command_test.go (35 tests)

- `TestQueueAndGetCommands`, `TestDrainCommandsDeletesKey`, `TestQueueCommand`, `TestSendUserAddCommand`, `TestSendUserDeleteCommand`, `TestSendInfoCommand`, `TestSendCheckCommand`, `TestSendGetOptionCommand`, `TestSendShellCommand`, `TestSendQueryUsersCommand`, `TestSendLogCommand`, `TestDrainCommands`, `TestDrainCommands_DeletesKey`, `TestPendingCommandsCount`, `TestSendUserAddCommand_ReturnsError`, `TestSendUserDeleteCommand_ReturnsError`, `TestSendInfoCommand_ReturnsError`, `TestSendCheckCommand_ReturnsError`, `TestSendGetOptionCommand_ReturnsError`, `TestSendShellCommand_ReturnsError`, `TestSendQueryUsersCommand_ReturnsError`, `TestSendLogCommand_ReturnsError`, `TestCommandIDIncrement`, `TestCommandIDIncrement_AcrossRequests`, `TestCommandIDIncrement_AcrossDevices`, `TestQueueCommand_ReturnsIncrementingIDs`, `TestSendQueryUsersCommand_ReturnsID`, `TestQueueCommand_ReturnsErrDeviceNotFound`, `TestSendInfoCommand_ReturnsErrDeviceNotFound`, `TestValidateCommandField_RejectsCRLF`, `TestValidateCommandField_AcceptsValid`, `TestQueueCommand_RejectsNewlineInCommand`, `TestSendUserAddCommand_RejectsNewlineInFields`, `TestSendShellCommand_RejectsNewline`, `TestPendingCommands_CountedTowardLimit`

### handler_test.go (~75 tests + 3 helpers)

Helper types: `errReader`, `errResponseWriter`, `errOnNthWriteResponseWriter`

Tests: All HTTP handler tests (handleCData, handleDeviceCmd, handleRegistry, handleInspect, ServeHTTP routing, readBody, registerOrReject, requireDevice, writeCommandsOrOK, commandResult, etc.)

### parser_test.go (17 tests)

- `TestParseKVPairs_Registry`, `TestParseAttendanceRecords`, `TestParseKVPairs_DeviceInfo`, `TestParseQueryParams`, `TestParseCommandResults`, `TestBodyPreview_Truncation`, `TestBodyPreview_Short`, `TestBodyPreview_ExactBoundary`, `TestParseQueryParams_InvalidURL`, `TestParseQueryParams_EmptyQueryValue`, `TestParseUserRecords`, `TestParseAttendance_DeviceTimezone`, `TestParseAttendance_DefaultTimezone`, `TestParseAttendance_NilTimezone`, `TestParseAttendance_UTC_Fallback`, `TestParseAttendance_UnixEpoch_IgnoresLocation`, `TestParseAttendance_WinterSummerTime`

## Status

- [x] Source files split (7 files)
- [x] types_test.go created (10 tests)
- [x] options_test.go created (33 tests)
- [x] device_test.go created (24 tests, duplicates removed)
- [x] command_test.go created (37 tests)
- [x] handler_test.go created (72 tests + 1 benchmark)
- [x] parser_test.go created (18 tests + 1 benchmark)
- [x] adms_test.go rewritten (19 tests)
- [x] Tests pass (213 tests, race-clean)
- [x] Linter passes (golangci-lint: 0 issues)
- [ ] Committed and pushed

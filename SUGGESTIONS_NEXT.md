# Suggestions for Next PR

Open-source readiness audit for the current tree.

The good news: `go test ./...`, `go test -race ./...`, and `golangci-lint run` all pass.
The bad news: passing checks does not cancel the failure modes below.

## ~~HIGH: `Close()` cancels callback context before draining queued work~~ DONE

**Location:** `adms.go:600-617`, `adms.go:823-826`, `adms.go:844-846`, `adms.go:866-867`, `adms.go:882-885`, `GETTING_STARTED.md:110`

`Close()` cancels `baseCtx` before waiting for queued callbacks to drain. That means the library promises
"drain pending callbacks" while simultaneously handing those callbacks an already-canceled context. Any
consumer doing the sane thing with that context (`db.ExecContext`, HTTP calls, etc.) can abort its own work
during shutdown and silently drop the very events `Close()` is supposed to flush.

This is the kind of shutdown behavior that looks fine in tests and loses the last few attendance records in
production.

**Suggested fix:**

- Either delay `baseCtxCancel()` until after `callbackDone`.
- Or document clearly that `Close()` drains callback execution, not successful callback completion.
- Better: pass a per-callback context derived from `baseCtx` at dispatch time and only cancel it if the event
  itself should be abandoned.

```go
func (s *ADMSServer) Close() {
	s.closeOnce.Do(func() {
		close(s.closeCh)
		<-s.evictionDone

		s.callbackMu.Lock()
		close(s.callbackCh)
		s.callbackMu.Unlock()

		<-s.callbackDone
		s.baseCtxCancel()
	})
}
```

## ~~HIGH: Several handlers silently drop data under backpressure and still return `200 OK`~~ DONE

**Location:** `adms.go:1131-1141`, `adms.go:1151-1161`, `adms.go:1233-1240`, `adms.go:1699-1705`

`ATTLOG` is handled correctly: if the callback queue is full, the handler returns `503` so the device can retry.
Everything else is worse:

- `table=USERINFO` records are dropped and still acknowledged with `200 OK`
- device info pushes are dropped and still acknowledged with `200 OK`
- registry payloads are dropped and still acknowledged with `200 OK`
- command results are dropped and still acknowledged with `200 OK`

That is silent data loss. The device thinks delivery succeeded. Your consumer never sees the data. Nobody gets
to debug this without packet captures and swearing.

**Suggested fix:**

- Pick one delivery contract and apply it consistently.
- If the callback is required for correctness, return `503` on enqueue failure.
- If best-effort is acceptable, make that explicit in the API docs and expose a metric/counter for dropped events.

```go
if !s.dispatchQueryUsers(serialNumber, users) {
	http.Error(w, "FAIL: callback queue full", http.StatusServiceUnavailable)
	return
}
```

## ~~HIGH: `pendingCommands` can grow without bound even when per-device queue limits are enabled~~ DONE

**Location:** `adms.go:1036-1047`, `adms.go:1052-1058`, `adms.go:1216-1220`, `adms.go:660-691`

`WithMaxCommandsPerDevice` only limits commands before they are polled by the device. After `DrainCommands`, the
entries are removed from `commandQueue` but stay alive in `pendingCommands` until the device confirms them via
`/iclock/devicecmd` or gets evicted.

So a device that keeps polling but never sends confirmations can accumulate an unbounded `pendingCommands` map.
That is a real memory-growth path, and it gets worse because those commands are now invisible to the per-device
queue limit.

**Suggested fix:**

- Track pending confirmations per device and cap them too.
- Add a confirmation TTL and reap stale pending IDs.
- Consider rejecting new commands when `queued + pending` exceeds the configured limit.

```go
type pendingEntry struct {
	sn       string
	cmd      string
	queuedAt time.Time
}

if s.maxCommandsPerDevice > 0 {
	pending := s.pendingCountLocked(serialNumber)
	queued := len(s.commandQueue[serialNumber])
	if queued+pending >= s.maxCommandsPerDevice {
		return 0, ErrCommandQueueFull
	}
}
```

## ~~HIGH: `QueueCommand` accepts unknown serial numbers, which creates an orphaned memory leak path~~ DONE

**Location:** `adms.go:1036-1047`, `adms.go:660-691`, `examples/basic/main.go:206-228`

`QueueCommand` does not require the device to exist. That means callers can enqueue commands for arbitrary serial
numbers, creating `commandQueue` and `pendingCommands` entries for devices the server has never seen. The eviction
worker only walks `devices`, so these orphaned command structures are never cleaned up.

The basic example exposes this directly through `/command`, so anyone hitting that endpoint can spray fake serials
and turn memory into a landfill.

**Suggested fix:**

- Make `QueueCommand` return `ErrDeviceNotFound` for unknown devices.
- If pre-registration is a required feature, add an explicit option like `WithAllowCommandsForUnknownDevices()`.
- At minimum, document the behavior and add cleanup for orphaned serials.

```go
s.devicesMutex.RLock()
_, exists := s.devices[serialNumber]
s.devicesMutex.RUnlock()
if !exists {
	return 0, ErrDeviceNotFound
}
```

## ~~HIGH: Command construction is injection-prone because user strings are written straight onto the ADMS wire~~ DONE

**Location:** `adms.go:117`, `adms.go:749-751`, `adms.go:1543-1545`, `adms.go:1559-1561`, `adms.go:1588-1590`, `adms.go:1607-1609`, `examples/commands/main.go:503-526`, `examples/commands/main.go:558-579`, `examples/basic/main.go:206-228`

The library builds wire commands with raw `fmt.Sprintf` and then emits them verbatim as `C:<id>:<cmd>\n`.
If `pin`, `name`, `key`, or shell text contains tabs, newlines, or delimiter characters, the caller can inject
extra fields or extra command lines.

For an open-source library, this is a footgun. For the example APIs that accept raw commands and shell commands,
it is a loaded footgun handed to the nearest bystander.

**Suggested fix:**

- Validate command fragments before formatting.
- Reject control characters (`\r`, `\n`, `\t`) in fields unless the protocol explicitly requires them.
- Provide a small escaping/validation helper used by all command builders.

```go
func validateCommandField(name, value string) error {
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("%s contains forbidden control characters", name)
	}
	return nil
}
```

## MEDIUM: One slow callback blocks every event type and turns the server into a head-of-line queue

**Location:** `adms.go:590-593`, `adms.go:620-638`, `adms.go:813-831`

There is exactly one callback worker goroutine. All attendance, device info, registry events, user query results,
and command confirmations are serialized through that single worker. Then `dispatchAttendance` processes an entire
batch inside one queued closure.

So one slow database write, one hanging webhook, or one oversized attendance batch stalls everything behind it.
Eventually the queue fills and now you are in the data-drop behavior described above.

This is not a race bug. It is a throughput ceiling with a built-in traffic jam.

**Suggested fix:**

- Document that callbacks must be fast and non-blocking.
- Consider configurable worker concurrency.
- Consider dispatching records individually or in bounded sub-batches.

```go
for i := 0; i < s.callbackWorkers; i++ {
	go s.callbackWorker()
}
```

## ~~MEDIUM: Public docs are out of sync with the actual API and current behavior~~ DONE

**Location:** `README.md:191-219`, `README.md:354`, `README.md:609-618`, `GETTING_STARTED.md:164-193`

The docs are lying in multiple places:

- examples still show `SendInfoCommand` and `QueueCommand` as if they return only `error`
- docs still describe `DrainCommands` as returning strings, not `[]CommandEntry`
- README still says `DATA QUERY` results are not parsed/surfaced, but the library now parses `USERINFO` and
  exposes `WithOnQueryUsers`

Shipping an OSS library with stale signatures and stale behavior docs is how you farm issue reports from annoyed
users who did exactly what your README told them to do.

**Suggested fix:**

- Update all command examples to use `(id, err)` or explicitly ignore the ID.
- Update API tables to reflect `[]CommandEntry`.
- Replace the stale `DATA QUERY` note with the actual `USERINFO` support story and its limits.

```go
_, err := server.SendInfoCommand("DEVICE001")
if err != nil {
	log.Printf("Failed to queue INFO command: %v", err)
}

cmds := server.DrainCommands("DEVICE001")
for _, cmd := range cmds {
	fmt.Println(cmd.ID, cmd.Command)
}
```

## LOW: Clarify `RegisterDevice` idempotency contract

**Location:** `adms.go:955-978`

`RegisterDevice` behaves like an upsert. Document explicitly that omitted options retain their existing values and
calling it without options on an existing device is a no-op.

```go
// RegisterDevice registers a new device or updates the options of an existing one.
// When called for an existing device, only the provided [DeviceOption] values are
// applied; omitted options retain their current values. Calling RegisterDevice
// without options on an existing device is a no-op.
```

## LOW: Document the timezone TOCTOU trade-off in ATTLOG parsing

**Location:** `adms.go:1100-1105`

The timezone is resolved under `RLock` and then used outside the lock. A concurrent `SetDeviceTimezone` call can
change the configured timezone while parsing is already in progress. That trade-off is probably fine, but it should
be documented instead of left as a surprise.

```go
// NOTE: The timezone is resolved at the start of ATTLOG processing.
// If the device's timezone changes concurrently, records already being
// parsed continue using the previously resolved timezone.
```

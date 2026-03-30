# Suggestions for Next PR

Carried over from the PR #28 code review.

## LOW: Clarify `RegisterDevice` idempotency contract

**Location:** `adms.go:953-975`

`RegisterDevice` changed from no-op-on-existing to upsert. Document explicitly that omitted
options retain their current values and calling without options is a no-op for existing devices.

```go
// RegisterDevice registers a new device or updates the options of an existing one.
// When called for an existing device, only the provided [DeviceOption] values are
// applied; omitted options retain their current values. Calling RegisterDevice
// without options on an existing device is a no-op.
```

## LOW: Document TOCTOU behavior in `HandleCData` timezone resolution

**Location:** `adms.go:1098-1103`

The timezone is resolved under `RLock` and then used outside the lock. A concurrent
`SetDeviceTimezone` call could change the timezone between resolution and parsing.
Add a comment documenting this acceptable trade-off.

```go
// NOTE: The timezone is resolved at the start of ATTLOG processing.
// If the device's timezone is changed concurrently, records already
// being parsed will use the previously resolved timezone.
```

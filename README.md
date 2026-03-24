# ZKTeco ADMS

[![Test](https://github.com/s0x90/zkteco-adms/actions/workflows/test.yml/badge.svg)](https://github.com/s0x90/zkteco-adms/actions/workflows/test.yml)
[![codecov](https://codecov.io/gh/s0x90/zkteco-adms/graph/badge.svg?token=4RWDG59BGM)](https://codecov.io/gh/s0x90/zkteco-adms)

A Go library implementing the ZKTeco ADMS protocol for ZKTeco biometric attendance devices.

## Overview

This library provides a complete implementation of the HTTP-based ADMS protocol used by ZKTeco devices to communicate with servers. It handles device registration, attendance data collection, and remote command execution.

Zero external dependencies — pure Go standard library.

## Features

- **Full ADMS Protocol Support**: Implements all standard endpoints (`/iclock/cdata`, `/iclock/getrequest`, `/iclock/devicecmd`) plus device registry and inspection endpoints
- **Functional Options API**: Clean, extensible configuration via `WithX` option functions
- **Structured Logging**: Uses `log/slog` for structured, leveled logging
- **Context Support**: Callbacks receive a `context.Context` tied to the server lifecycle
- **Attendance Data Processing**: Parses and processes attendance logs with multiple timestamp formats
- **Device Management**: Thread-safe device registration and tracking
- **Command Queuing**: Queue and send commands to devices remotely
- **Heartbeat & Online Status**: Tracks last activity per device with configurable online threshold
- **Concurrent-Safe**: Built with goroutine-safe data structures and async callback dispatch
- **Request Body Limits**: Configurable `MaxBytesReader` protection against oversized payloads
- **Serial Number Validation**: Rejects malformed device identifiers at the protocol boundary
- **Device & Command Limits**: Configurable caps on registered devices and per-device command queue depth
- **Opt-In Debug Endpoint**: `/iclock/inspect` is disabled by default, enabled via `WithEnableInspect()`
- **Graceful Shutdown**: `Close()` drains pending callbacks and cancels the base context

## Installation

Requires Go 1.26 or higher.

```bash
go get github.com/s0x90/zkteco-adms
```

## Quick Start

```go
package main

import (
    "context"
    "fmt"
    "log"
    "net/http"
    "time"

    zkadms "github.com/s0x90/zkteco-adms"
)

func main() {
    server := zkadms.NewADMSServer(
        zkadms.WithOnAttendance(func(ctx context.Context, record zkadms.AttendanceRecord) {
            fmt.Printf("User %s status: %s at %s from device %s\n",
                record.UserID,
                statusString(record.Status),
                record.Timestamp.Format(time.RFC3339),
                record.SerialNumber)
        }),
        zkadms.WithOnDeviceInfo(func(ctx context.Context, sn string, info map[string]string) {
            fmt.Printf("Device %s connected: %v\n", sn, info)
        }),
    )
    defer server.Close()

    http.Handle("/iclock/", server)
    log.Fatal(http.ListenAndServe(":8080", nil))
}

func statusString(status int) string {
    switch status {
    case 0:
        return "check-in"
    case 1:
        return "check-out"
    case 2:
        return "break-out"
    case 3:
        return "break-in"
    case 4:
        return "overtime-in"
    case 5:
        return "overtime-out"
    default:
        return "unknown"
    }
}
```

## Usage

### Creating a Server

```go
// Defaults: slog.Default() logger, 10 MB body limit, 256 callback buffer,
// 2-minute online threshold, 1-second dispatch timeout, 1000 max devices,
// 24h device eviction timeout.
server := zkadms.NewADMSServer()
defer server.Close()
```

### Functional Options

Configure the server at construction time:

```go
server := zkadms.NewADMSServer(
    zkadms.WithLogger(slog.New(slog.NewJSONHandler(os.Stderr, nil))),
    zkadms.WithMaxBodySize(5 << 20),              // 5 MB body limit
    zkadms.WithCallbackBufferSize(512),            // larger callback buffer
    zkadms.WithOnlineThreshold(5 * time.Minute),   // 5 min before "offline"
    zkadms.WithDispatchTimeout(2 * time.Second),   // callback dispatch timeout
    zkadms.WithBaseContext(ctx),                    // tie to parent context
    zkadms.WithOnAttendance(handleAttendance),
    zkadms.WithOnDeviceInfo(handleDeviceInfo),
    zkadms.WithOnRegistry(handleRegistry),
)
defer server.Close()
```

| Option | Default | Description |
|--------|---------|-------------|
| `WithLogger` | `slog.Default()` | Structured logger |
| `WithMaxBodySize` | 10 MB | Max request body size |
| `WithCallbackBufferSize` | 256 | Internal callback channel capacity |
| `WithOnlineThreshold` | 2 min | Duration before a device is considered offline |
| `WithDispatchTimeout` | 1 sec | Max block time when callback queue is full |
| `WithBaseContext` | `context.Background()` | Parent context for callbacks |
| `WithMaxDevices` | 1000 | Max registered devices; use `WithUnlimitedDevices()` to remove the cap |
| `WithUnlimitedDevices` | — | Remove the device registration limit (use with caution) |
| `WithMaxCommandsPerDevice` | 0 (unlimited) | Max queued commands per device; returns `ErrCommandQueueFull` |
| `WithDeviceEvictionInterval` | 5 min | How often the eviction worker checks for stale devices |
| `WithDeviceEvictionTimeout` | 24 hours | Inactivity duration before a device is automatically evicted |
| `WithEnableInspect` | disabled | Enable the `/iclock/inspect` debug endpoint |
| `WithOnAttendance` | nil | Attendance record callback |
| `WithOnDeviceInfo` | nil | Device info callback |
| `WithOnRegistry` | nil | Device registry callback |
| `WithOnCommandResult` | nil | Command confirmation callback (see [CommandResult](#commandresult)) |

### Handling Attendance Records

```go
zkadms.WithOnAttendance(func(ctx context.Context, record zkadms.AttendanceRecord) {
    // record.UserID       - Employee ID
    // record.Timestamp    - Time of attendance
    // record.Status       - 0=Check In, 1=Check Out, 2=Break Out, 3=Break In, 4=Overtime In, 5=Overtime Out
    // record.VerifyMode   - Verification method; use zkadms.VerifyModeName(record.VerifyMode) for label
    // record.WorkCode     - Optional work code
    // record.SerialNumber - Device serial number
    //
    // ctx is cancelled when server.Close() is called.
})
```

### Handling Device Information

```go
zkadms.WithOnDeviceInfo(func(ctx context.Context, sn string, info map[string]string) {
    // sn   - Device serial number
    // info - Map of device properties (firmware version, device name, etc.)
})
```

### Handling Device Registry

```go
zkadms.WithOnRegistry(func(ctx context.Context, sn string, info map[string]string) {
    // Called when a device registers or re-registers.
    // info contains parsed key=value pairs from the registry body.
})
```

### Handling Command Results

```go
zkadms.WithOnCommandResult(func(ctx context.Context, result zkadms.CommandResult) {
    // Called when a device reports the result of a command.
    // result.SerialNumber - Device that executed the command
    // result.ID           - Command ID assigned by the server
    // result.ReturnCode   - 0 = success, non-zero = error
    // result.Command      - Command type echoed back (e.g. "USER ADD")
})
```

### Sending Commands to Devices

```go
// Queue a custom command (returns ErrCommandQueueFull if limit reached)
err := server.QueueCommand("DEVICE001", "CHECK")

// Request device information
err = server.SendInfoCommand("DEVICE001")

// Add or update a user on the device
err = server.SendUserAddCommand("DEVICE001", "12345", "John Doe", 0, "")

// Delete a user from the device
err = server.SendUserDeleteCommand("DEVICE001", "12345")

// Drain all pending commands for a device
cmds := server.DrainCommands("DEVICE001")
```

### Listing Connected Devices

```go
devices := server.ListDevices()
for _, device := range devices {
    online := server.IsDeviceOnline(device.SerialNumber)
    fmt.Printf("Device: %s, Last Seen: %s, Online: %t\n",
        device.SerialNumber,
        device.LastActivity.Format(time.RFC3339),
        online)
}
```

### HTTP Routing

The server implements `http.Handler`, so you can use it directly:

```go
http.Handle("/iclock/", server)
```

Or register individual endpoints:

```go
http.HandleFunc("/iclock/cdata", server.HandleCData)
http.HandleFunc("/iclock/registry", server.HandleRegistry)
http.HandleFunc("/iclock/getrequest", server.HandleGetRequest)
http.HandleFunc("/iclock/devicecmd", server.HandleDeviceCmd)
http.HandleFunc("/iclock/inspect", server.HandleInspect) // opt-in: not routed by ServeHTTP unless WithEnableInspect is set
```

### Sentinel Errors

```go
var (
    zkadms.ErrServerClosed       // operation attempted on a closed server
    zkadms.ErrCallbackQueueFull  // callback queue full, dispatch timed out
    zkadms.ErrMaxDevicesReached  // device limit reached (WithMaxDevices)
    zkadms.ErrCommandQueueFull   // per-device command queue full (WithMaxCommandsPerDevice)
    zkadms.ErrInvalidSerialNumber // serial number failed validation
)
```

### Graceful Shutdown

```go
server := zkadms.NewADMSServer(/* ... */)
// ... use server ...

// Close drains pending callbacks, cancels the base context, and stops the worker.
// Safe to call multiple times.
server.Close()
```

## Protocol Details

### ADMS Protocol Endpoints

| Endpoint | Method | Purpose |
|----------|--------|---------|
| `/iclock/cdata` | GET/POST | Receives attendance logs (ATTLOG) and operation logs (OPERLOG). Also accepts device info POSTs |
| `/iclock/registry` | GET/POST | Device registration and capability payloads (key=value comma-separated) |
| `/iclock/getrequest` | GET | Device polls for pending commands |
| `/iclock/devicecmd` | POST | Device reports command execution results |
| `/iclock/inspect` | GET | Returns JSON summary of devices and their current status (opt-in via `WithEnableInspect`) |

### Command Wire Format

When a device polls `/iclock/getrequest`, pending commands are sent as:

```
C:<ID>:<CMD>\n
```

Where `<ID>` is a monotonically increasing integer assigned by the server. For example:

```
C:1:INFO
C:2:USER ADD PIN=1001	Name=John Doe	Privilege=0	Card=12345678
C:3:USER DEL PIN=1001
```

After executing a command, the device POSTs the result to `/iclock/devicecmd` with a body like:

```
ID=1&Return=0&CMD=INFO
```

A `Return` value of `0` indicates success. The parsed result is delivered to the callback registered via `WithOnCommandResult`.

### Registry Payload Parsing

Some ZKTeco devices POST a registry body containing comma-separated `key=value` pairs, e.g.:

```
DeviceType=acc,~DeviceName=SpeedFace-V5L-RFID[TI],FirmVer=ZAM180...,IPAddress=192.168.1.201
```

Notes:
- Keys can be prefixed with `~`. The tilde is stripped when parsed.
- Values are stored into `Device.Options` for subsequent inspection.
- The handler merges all parsed keys into the registered device.

### Heartbeat and Online Status

The server updates `Device.LastActivity` at each request from the device and marks the device online.

- A device is considered online if its last activity is within the online threshold (default: 2 minutes, configurable via `WithOnlineThreshold`).
- The `/iclock/inspect` endpoint reports for each device:
    - `serial`: device serial number
    - `lastActivity`: RFC3339 timestamp of last activity
    - `online`: boolean derived from last activity
    - `options`: the registry/options map

### Attendance Record Format

Devices send attendance data as tab-separated values:

```
UserID\tTimestamp\tStatus\tVerifyMode\tWorkCode
```

Example:
```
123	2024-01-01 08:00:00	0	1	0
```

Supported timestamp formats:
- `2006-01-02 15:04:05` (standard datetime)
- Unix timestamp (seconds since epoch)

### Status Codes

- `0` - Check In
- `1` - Check Out
- `2` - Break Out
- `3` - Break In
- `4` - Overtime In
- `5` - Overtime Out

### Verify Mode

These are the ADMS protocol verify mode values observed from real devices.
Use `zkadms.VerifyModeName(mode)` to resolve any value to a human-readable label.

| Value | Method |
|-------|--------|
| `0` | Password |
| `1` | Fingerprint |
| `2` | Card (legacy) |
| `3` | Password (alternative) |
| `4` | Card |
| `5` | Fingerprint+Card |
| `6` | Fingerprint+Password |
| `7` | Card+Password |
| `8` | Card+Fingerprint+Password |
| `9` | Other |
| `15` | Face |
| `25` | Palm |

> **Note:** Values may vary across device models and firmware versions.
> The constants `VerifyModePassword`, `VerifyModeFingerprint`, `VerifyModeCard`,
> `VerifyModeFace`, and `VerifyModePalm` are provided for the most common codes.

## Examples

See the [examples](./examples) directory for complete examples:

- **[basic](./examples/basic)** - Simple server with status endpoint
- **[commands](./examples/commands)** - Device command management via REST API
- **[database](./examples/database)** - Integration with database storage

### Running Examples

```bash
go run ./examples/basic
```

Then configure your ZKTeco device to connect to:
```
http://your-server:8080/iclock/
```

#### Commands Example

The commands example exposes a REST API for managing devices:

```bash
go run ./examples/commands -devices ABCD12345678
```

Endpoints:

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/devices` | List all connected devices |
| GET | `/api/devices/{sn}` | Device detail |
| POST | `/api/devices/{sn}/reboot` | Reboot device |
| POST | `/api/devices/{sn}/info` | Request device info |
| POST | `/api/devices/{sn}/sync-time` | Sync device clock |
| POST | `/api/devices/{sn}/clear-data` | Clear attendance data |
| POST | `/api/devices/{sn}/clear-log` | Clear operation log |
| POST | `/api/devices/{sn}/users` | Add/update user (JSON body) |
| POST | `/api/devices/{sn}/users/delete` | Delete user (JSON body) |
| POST | `/api/devices/{sn}/open-door` | Trigger door relay |
| POST | `/api/devices/{sn}/command` | Send raw command (JSON body) |

Example curl usage:

```bash
# Add a user
curl -X POST http://localhost:8080/api/devices/<SN>/users \
     -H 'Content-Type: application/json' \
     -d '{"pin":"1001","name":"John Doe","privilege":0,"card":"12345678"}'

# Delete a user
curl -X POST http://localhost:8080/api/devices/<SN>/users/delete \
     -H 'Content-Type: application/json' \
     -d '{"pin":"1001"}'

# Reboot device
curl -X POST http://localhost:8080/api/devices/<SN>/reboot

# Request device info
curl -X POST http://localhost:8080/api/devices/<SN>/info

# Send a raw command
curl -X POST http://localhost:8080/api/devices/<SN>/command \
     -H 'Content-Type: application/json' \
     -d '{"command":"CHECK"}'
```

## Testing

Run the test suite:

```bash
go test -v
```

Run with race detection and coverage:

```bash
go test -v -race -cover
```

Run benchmarks:

```bash
go test -bench=.
```

## API Reference

### Types

#### `Option`
A function that configures an `ADMSServer`. Obtained via `WithX` functions.

#### `ADMSServer`
Main server structure handling all protocol operations. Implements `http.Handler`.

#### `Device`
Represents a registered ZKTeco device with `SerialNumber`, `LastActivity`, and `Options` fields.

#### `AttendanceRecord`
Represents a single attendance transaction with `UserID`, `Timestamp`, `Status`, `VerifyMode`, `WorkCode`, and `SerialNumber` fields.

#### `DeviceSnapshot`
JSON representation of a device in the `/iclock/inspect` response.

#### `CommandResult`
Represents the result of a command execution reported by a device. Fields: `SerialNumber`, `ID` (int64), `ReturnCode` (int, 0 = success), and `Command` (string).

### Constructor

#### `NewADMSServer(opts ...Option) *ADMSServer`
Creates a new ADMS server instance configured with the given options.

### Option Functions

| Function | Description |
|----------|-------------|
| `WithLogger(*slog.Logger)` | Set structured logger |
| `WithMaxBodySize(int64)` | Set max request body size |
| `WithCallbackBufferSize(int)` | Set callback channel capacity |
| `WithOnlineThreshold(time.Duration)` | Set device online threshold |
| `WithDispatchTimeout(time.Duration)` | Set callback dispatch timeout |
| `WithBaseContext(context.Context)` | Set parent context |
| `WithMaxDevices(int)` | Set max registered devices (default 1000) |
| `WithUnlimitedDevices()` | Remove the device registration limit |
| `WithMaxCommandsPerDevice(int)` | Set max command queue depth per device |
| `WithDeviceEvictionInterval(time.Duration)` | Set stale-device check interval |
| `WithDeviceEvictionTimeout(time.Duration)` | Set inactivity threshold for eviction |
| `WithEnableInspect()` | Enable `/iclock/inspect` in ServeHTTP router |
| `WithOnAttendance(func(context.Context, AttendanceRecord))` | Set attendance callback |
| `WithOnDeviceInfo(func(context.Context, string, map[string]string))` | Set device info callback |
| `WithOnRegistry(func(context.Context, string, map[string]string))` | Set registry callback |
| `WithOnCommandResult(func(context.Context, CommandResult))` | Set command confirmation callback |

### Methods

| Method | Description |
|--------|-------------|
| `Close()` | Drain callbacks and stop the worker goroutine |
| `RegisterDevice(serialNumber string) error` | Register a device; validates SN, respects device limit |
| `GetDevice(serialNumber string) *Device` | Get device information (returns a copy) |
| `IsDeviceOnline(serialNumber string) bool` | Check if a device is online |
| `QueueCommand(serialNumber, command string) error` | Queue a command; respects per-device limit |
| `DrainCommands(serialNumber string) []string` | Drain and return all pending commands |
| `PendingCommandsCount(serialNumber string) int` | Return the number of queued commands without draining |
| `SendInfoCommand(serialNumber string) error` | Queue an INFO command |
| `SendUserAddCommand(serialNumber, pin, name string, privilege int, card string) error` | Queue a USER ADD command (ADMS protocol) |
| `SendUserDeleteCommand(serialNumber, pin string) error` | Queue a USER DEL command (ADMS protocol) |
| `ListDevices() []*Device` | List all registered devices (returns copies) |
| `ServeHTTP(w, r)` | `http.Handler` implementation — routes to endpoint handlers |
| `HandleCData(w, r)` | Handle `/iclock/cdata` requests |
| `HandleRegistry(w, r)` | Handle `/iclock/registry` requests |
| `HandleGetRequest(w, r)` | Handle `/iclock/getrequest` requests |
| `HandleDeviceCmd(w, r)` | Handle `/iclock/devicecmd` requests |
| `HandleInspect(w, r)` | Handle `/iclock/inspect` requests (JSON device snapshot) |

### Standalone Functions

#### `ParseQueryParams(urlStr string) (map[string]string, error)`
Parses URL query parameters into a map.

### Sentinel Errors

| Error | Description |
|-------|-------------|
| `ErrServerClosed` | Returned when an operation is attempted on a closed server |
| `ErrCallbackQueueFull` | Returned when the callback queue is full and dispatch timed out |
| `ErrMaxDevicesReached` | Returned by `RegisterDevice` when the device limit is reached |
| `ErrCommandQueueFull` | Returned by `QueueCommand` when the per-device command limit is reached |
| `ErrInvalidSerialNumber` | Returned when a serial number is empty, too long, or contains invalid characters |

### Deprecated

The following are retained for backward compatibility and will be removed in a future major version:

- `ADMSServer.OnAttendance` field — use `WithOnAttendance` instead
- `ADMSServer.OnDeviceInfo` field — use `WithOnDeviceInfo` instead
- `ADMSServer.OnRegistry` field — use `WithOnRegistry` instead
- `SendCommand()` method — use `QueueCommand` instead
- `GetCommands()` method — use `DrainCommands` instead

## Device Configuration

Configure your ZKTeco device to connect to your server:

1. Access device web interface or admin panel
2. Chose ADMS
3. Set server address: `http://your-server:8080`

## License

MIT License - see LICENSE file for details

## Contributing

Contributions are welcome! See [CONTRIBUTING.md](./CONTRIBUTING.md) for guidelines.

## Support

For issues and questions, please open an issue on GitHub.

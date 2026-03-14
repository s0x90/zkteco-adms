# ZK Device Sync

A Go library implementing the iClock (ZKTeco ADMS) protocol for ZKTeco biometric attendance devices.

## Overview

This library provides a complete implementation of the HTTP-based iClock protocol used by ZKTeco devices to communicate with servers. It handles device registration, attendance data collection, and remote command execution.

## Features

- **Full iClock Protocol Support**: Implements all standard endpoints (`/iclock/cdata`, `/iclock/getrequest`, `/iclock/devicecmd`) plus device registry and inspection endpoints
- **Attendance Data Processing**: Parses and processes attendance logs with multiple timestamp formats
- **Device Management**: Thread-safe device registration and tracking
- **Command Queuing**: Queue and send commands to devices remotely
- **Heartbeat & Online Status**: Tracks last activity per device and exposes online/offline status
- **Concurrent-Safe**: Built with goroutine-safe data structures
- **Extensible**: Easy-to-use callbacks for custom business logic

## Installation

```bash
go get github.com/s0x90/zk-device-sync
```

## Quick Start

```go
package main

import (
    "fmt"
    "log"
    "net/http"
    
    zkdevicesync "github.com/s0x90/zk-device-sync"
)

func main() {
    // Create a new iclock server
    server := zkdevicesync.NewIClockServer()
    
    // Set up callback for attendance records
    server.OnAttendance = func(record zkdevicesync.AttendanceRecord) {
        fmt.Printf("User %s checked %s at %s from device %s\n",
            record.UserID,
            getStatusString(record.Status),
            record.Timestamp,
            record.SerialNumber)
    }
    
    // Set up callback for device information
    server.OnDeviceInfo = func(sn string, info map[string]string) {
        fmt.Printf("Device %s connected: %v\n", sn, info)
    }
    
    // Start HTTP server
    http.Handle("/iclock/", server)
    log.Fatal(http.ListenAndServe(":8080", nil))
}

func getStatusString(status int) string {
    switch status {
    case 0:
        return "in"
    case 1:
        return "out"
    default:
        return "unknown"
    }
}
```

## Usage

### Creating a Server

```go
server := zkdevicesync.NewIClockServer()
```

### Handling Attendance Records

```go
server.OnAttendance = func(record zkdevicesync.AttendanceRecord) {
    // Process attendance record
    // record.UserID - Employee ID
    // record.Timestamp - Time of attendance
    // record.Status - 0=Check In, 1=Check Out, 2=Break Out, 3=Break In
    // record.VerifyMode - 0=Password, 1=Fingerprint, 2=Card
    // record.WorkCode - Optional work code
    // record.SerialNumber - Device serial number
}
```

### Handling Device Information

```go
server.OnDeviceInfo = func(sn string, info map[string]string) {
    // Process device information
    // sn - Device serial number
    // info - Map of device properties (firmware version, device name, etc.)
}
```

### Sending Commands to Devices

```go
// Send INFO command
server.SendInfoCommand("DEVICE001")

// Send custom command
server.SendCommand("DEVICE001", "CHECK")

// Send data query command
server.SendDataCommand("DEVICE001", "USER", "user data")
```

### Listing Connected Devices

```go
devices := server.ListDevices()
for _, device := range devices {
    fmt.Printf("Device: %s, Last Seen: %s\n", 
        device.SerialNumber, 
        device.LastActivity)
}
```

### HTTP Routing

The server implements `http.Handler`, so you can use it directly:

```go
http.Handle("/iclock/", server)
```

Or handle individual endpoints:

```go
http.HandleFunc("/iclock/cdata", server.HandleCData)
http.HandleFunc("/iclock/registry", server.HandleRegistry) // Device registry/capabilities
http.HandleFunc("/iclock/getrequest", server.HandleGetRequest)
http.HandleFunc("/iclock/devicecmd", server.HandleDeviceCmd)
http.HandleFunc("/iclock/inspect", server.HandleInspect)   // JSON snapshot of devices
```

## Protocol Details

### iClock Protocol Endpoints

| Endpoint | Method | Purpose |
|----------|--------|---------|
| `/iclock/cdata` | GET/POST | Receives attendance logs (ATTLOG) and operation logs (OPERLOG). Also accepts device info POSTs |
| `/iclock/registry` | GET/POST | Device registration and capability payloads (key=value comma-separated) |
| `/iclock/getrequest` | GET | Device polls for pending commands |
| `/iclock/devicecmd` | POST | Device reports command execution results |
| `/iclock/inspect` | GET | Returns JSON summary of devices and their current status |
### Registry Payload Parsing

Some ZKTeco devices POST a registry body containing comma-separated `key=value` pairs, e.g.:

```
DeviceType=acc,~DeviceName=SpeedFace-V5L-RFID[TI],FirmVer=ZAM180...,IPAddress=192.168.1.201
```

Notes:
- Keys can be prefixed with `~`. The tilde is stripped when parsed.
- Values are stored into `Device.Options` for subsequent inspection.
- The handler merges all parsed keys into the registered device.

Callback hook:

```go
server.OnRegistry = func(sn string, info map[string]string) {
        // handle registry info (subset of device capabilities/config)
}
```

### Heartbeat and Online Status

The server updates `Device.LastActivity` at each request from the device (`/iclock/cdata`, `/iclock/registry`, `/iclock/getrequest`, `/iclock/devicecmd`) and marks the device online.

- A device is considered online if its last activity is within the last 2 minutes.
- The `/iclock/inspect` endpoint reports for each device:
    - `serial`: device serial number
    - `lastActivity`: RFC3339 timestamp of last activity
    - `online`: boolean derived from last activity
    - `options`: the registry/options map

You can adjust the online threshold in code by changing the 2-minute window.


### Attendance Record Format

Devices send attendance data as tab-separated values:

```
UserID\tTimestamp\tStatus\tVerifyMode\tWorkCode
```

Example:
```
123\t2024-01-01 08:00:00\t0\t1\t0
```

Supported timestamp formats:
- `2006-01-02 15:04:05` (RFC3339-like)
- Unix timestamp (seconds since epoch)

### Status Codes

- `0` - Check In
- `1` - Check Out
- `2` - Break Out
- `3` - Break In
- `4` - Overtime In
- `5` - Overtime Out

### Verify Mode

- `0` - Password
- `1` - Fingerprint
- `2` - Card
- `3` - Face Recognition

## Examples

See the [examples](./examples) directory for complete examples:

- **[basic_server.go](./examples/basic_server.go)** - Simple server with status endpoint
- **[database_integration.go](./examples/database_integration.go)** - Integration with database storage

### Running Examples

```bash
go run -tags basic_server ./examples/basic_server.go
```

Then configure your ZKTeco device to connect to:
```
http://your-server:8080/iclock/
```

## Testing

Run the test suite:

```bash
go test -v
```

Run with coverage:

```bash
go test -v -cover
```

Run benchmarks:

```bash
go test -bench=.
```

## API Reference

### Types

#### `IClockServer`
Main server structure handling all protocol operations.

#### `Device`
Represents a registered ZKTeco device.

#### `AttendanceRecord`
Represents a single attendance transaction.

### Functions

#### `NewIClockServer() *IClockServer`
Creates a new iclock server instance.

#### `(*IClockServer) RegisterDevice(serialNumber string)`
Registers a device with the server.

#### `(*IClockServer) GetDevice(serialNumber string) *Device`
Retrieves device information.

#### `(*IClockServer) QueueCommand(serialNumber, command string)`
Queues a command for a device.

#### `(*IClockServer) SendCommand(serialNumber, command string)`
Alias for QueueCommand.

#### `(*IClockServer) SendInfoCommand(serialNumber string)`
Sends an INFO command to request device information.

#### `(*IClockServer) SendDataCommand(serialNumber, table, data string)`
Sends a DATA QUERY command.

#### `(*IClockServer) ListDevices() []*Device`
Returns all registered devices.

#### `ParseQueryParams(urlStr string) (map[string]string, error)`
Parses URL query parameters.

## Device Configuration

Configure your ZKTeco device to connect to your server:

1. Access device web interface or admin panel
2. Set server address: `http://your-server:8080/iclock/`
3. Set upload interval (recommended: 60 seconds)
4. Enable real-time upload for immediate attendance transmission

## License

MIT License - see LICENSE file for details

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## Support

For issues and questions, please open an issue on GitHub.

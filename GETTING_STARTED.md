# Getting Started with ZKTeco ADMS

This guide will help you get started with the ZKTeco ADMS library for integrating ZKTeco biometric devices with your Go applications.

## Prerequisites

- Go 1.26 or higher
- A ZKTeco biometric device (attendance machine)
- Network access between your server and the device

## Installation

```bash
go get github.com/s0x90/zkteco-adms
```

## Your First Server

Create a file `main.go`:

```go
package main

import (
    "context"
    "fmt"
    "log"
    "net/http"

    zkadms "github.com/s0x90/zkteco-adms"
)

func main() {
    // Create the server with a callback for attendance records
    server := zkadms.NewADMSServer(
        zkadms.WithOnAttendance(func(ctx context.Context, record zkadms.AttendanceRecord) {
            fmt.Printf("Attendance: User %s at %s\n",
                record.UserID,
                record.Timestamp)
        }),
    )
    defer server.Close()

    // Start the HTTP server
    http.Handle("/iclock/", server)
    log.Println("Server running on :8080")
    log.Fatal(http.ListenAndServe(":8080", nil))
}
```

Run it:

```bash
go run main.go
```

## Configuring Your ZKTeco Device

1. **Access the device admin interface** (usually via web browser or device menu)

2. **Set the server address:**
   - URL: `http://YOUR_SERVER_IP:8080/iclock/`
   - Example: `http://192.168.1.100:8080/iclock/`

3. **Configure upload settings:**
   - Upload interval: 60 seconds (recommended)
   - Enable real-time upload: Yes

4. **Save and test connection**
   - The device should appear in your server logs
   - Try clocking in/out to see attendance records

## Understanding Attendance Records

Each attendance record contains:

```go
type AttendanceRecord struct {
    UserID       string    // Employee ID (e.g., "123")
    Timestamp    time.Time // When the attendance occurred
    Status       int       // 0=Check In, 1=Check Out, 2=Break Out, 3=Break In, 4=Overtime In, 5=Overtime Out
    VerifyMode   int       // Verification method; see VerifyMode* constants and VerifyModeName(mode)
    WorkCode     string    // Optional work code
    SerialNumber string    // Device that recorded this
}
```

## Common Use Cases

### 1. Save to Database

```go
server := zkadms.NewADMSServer(
    zkadms.WithOnAttendance(func(ctx context.Context, record zkadms.AttendanceRecord) {
        query := `INSERT INTO attendance (user_id, timestamp, status, device)
                  VALUES (?, ?, ?, ?)`
        _, err := db.ExecContext(ctx, query,
            record.UserID,
            record.Timestamp,
            record.Status,
            record.SerialNumber)
        if err != nil {
            log.Printf("Error saving: %v", err)
        }
    }),
)
defer server.Close()
```

Note: the `ctx` passed to your callback is derived from the server's base context and will be cancelled after `server.Close()` finishes draining all queued callbacks. This means database calls and HTTP requests inside callbacks complete normally during graceful shutdown.

### 2. Send Notifications

```go
server := zkadms.NewADMSServer(
    zkadms.WithOnAttendance(func(ctx context.Context, record zkadms.AttendanceRecord) {
        switch record.Status {
        case 0: // Check In
            sendWelcomeNotification(ctx, record.UserID)
        case 1: // Check Out
            sendGoodbyeNotification(ctx, record.UserID)
        case 2: // Break Out
            sendBreakNotification(ctx, record.UserID, "started")
        case 3: // Break In
            sendBreakNotification(ctx, record.UserID, "ended")
        case 4: // Overtime In
            sendOvertimeNotification(ctx, record.UserID, "started")
        case 5: // Overtime Out
            sendOvertimeNotification(ctx, record.UserID, "ended")
        }
    }),
)
defer server.Close()
```

### 3. Real-time Dashboard

```go
var attendanceChannel = make(chan zkadms.AttendanceRecord, 100)

server := zkadms.NewADMSServer(
    zkadms.WithOnAttendance(func(ctx context.Context, record zkadms.AttendanceRecord) {
        select {
        case attendanceChannel <- record:
        case <-ctx.Done():
        }
    }),
)
defer server.Close()

// In another goroutine, process for dashboard
go func() {
    for record := range attendanceChannel {
        updateDashboard(record)
    }
}()
```

## Sending Commands to Devices

### Request Device Information

```go
if _, err := server.SendInfoCommand("DEVICE001"); err != nil {
    log.Printf("Failed to queue INFO command: %v", err)
}
```

The device will respond with its configuration, which you can handle:

```go
server := zkadms.NewADMSServer(
    zkadms.WithOnDeviceInfo(func(ctx context.Context, sn string, info map[string]string) {
        fmt.Printf("Device %s info:\n", sn)
        for key, value := range info {
            fmt.Printf("  %s: %s\n", key, value)
        }
    }),
)
defer server.Close()
```

### Custom Commands

```go
// Queue a command for the device (returns command ID and error)
if _, err := server.QueueCommand("DEVICE001", "CHECK"); err != nil {
    log.Printf("Failed to queue command: %v", err)
}

// Drain all pending commands (useful for custom polling logic)
cmds := server.DrainCommands("DEVICE001")
for _, cmd := range cmds {
    fmt.Println(cmd.ID, cmd.Command)
}
```

## Configuring the Server

Use functional options to customize behavior:

```go
server := zkadms.NewADMSServer(
    // Use a custom structured logger
    zkadms.WithLogger(slog.New(slog.NewJSONHandler(os.Stderr, nil))),

    // Limit request body size to 5 MB
    zkadms.WithMaxBodySize(5 << 20),

    // Consider devices offline after 5 minutes of inactivity
    zkadms.WithOnlineThreshold(5 * time.Minute),

    // Limit registered devices (0 = unlimited)
    zkadms.WithMaxDevices(100),

    // Limit queued commands per device (0 = unlimited)
    zkadms.WithMaxCommandsPerDevice(50),

    // Enable the /iclock/inspect debug endpoint (disabled by default)
    zkadms.WithEnableInspect(),

    // Tie the server to an application-level context
    zkadms.WithBaseContext(appCtx),

    // Register callbacks
    zkadms.WithOnAttendance(handleAttendance),
    zkadms.WithOnDeviceInfo(handleDeviceInfo),
    zkadms.WithOnRegistry(handleRegistry),
)
defer server.Close()
```

## Monitoring Connected Devices

The library provides a built-in JSON inspection endpoint. It is **disabled by default** in the `ServeHTTP` router for security. Enable it with `WithEnableInspect()`, or register the handler directly on your own mux:

```go
// Option 1: Enable in the default router
server := zkadms.NewADMSServer(
    zkadms.WithEnableInspect(),
)

// Option 2: Register on your own mux (always available)
http.HandleFunc("/iclock/inspect", server.HandleInspect)
```

This returns a JSON snapshot with `serial`, `lastActivity` (RFC3339), `online` flag, and parsed `options` (from registry) for each device.

You can also query devices programmatically:

```go
http.HandleFunc("/devices", func(w http.ResponseWriter, r *http.Request) {
    devices := server.ListDevices()
    for _, device := range devices {
        online := server.IsDeviceOnline(device.SerialNumber)
        fmt.Fprintf(w, "Device: %s, Last Seen: %s, Online: %t\n",
            device.SerialNumber,
            device.LastActivity.Format(time.RFC3339),
            online)
    }
})
```

## Testing Without a Physical Device

You can test the server by simulating device requests:

```bash
# Simulate device check-in
curl -X POST "http://localhost:8080/iclock/cdata?SN=TEST001&table=ATTLOG" \
  -d "123	2024-01-01 08:00:00	0	1	0"

# Check server for commands
curl "http://localhost:8080/iclock/getrequest?SN=TEST001"

# Simulate device registry payload (capabilities/options)
curl -X POST "http://localhost:8080/iclock/registry?SN=TEST001" \
    -d "DeviceType=acc,~DeviceName=SpeedFace,IPAddress=192.168.1.201"
```

## Troubleshooting

### Device Not Connecting

1. **Check network connectivity:**
   ```bash
   ping YOUR_DEVICE_IP
   ```

2. **Verify server is accessible:**
   ```bash
   curl http://YOUR_SERVER:8080/iclock/cdata?SN=TEST
   ```

3. **Check firewall rules** - ensure port 8080 is open

4. **Review device logs** - access device admin panel

### No Attendance Records Received

1. **Verify device is registered** — check the `/iclock/inspect` endpoint for device status

2. **Ensure a callback is set:**
   ```go
   server := zkadms.NewADMSServer(
       zkadms.WithOnAttendance(func(ctx context.Context, record zkadms.AttendanceRecord) {
           log.Printf("Got record: %+v", record)
       }),
   )
   ```

3. **Test with simulated data** (see above)

### Device Shows "Connection Failed"

- Ensure server URL is correct (must end with `/iclock/`)
- Check if server is running: `curl http://YOUR_SERVER:8080/iclock/getrequest?SN=TEST`
- Try HTTP instead of HTTPS (most devices don't support HTTPS)

## Next Steps

- Review the [examples](./examples) directory for complete applications
- Read the [API documentation](./README.md#api-reference)
- Integrate with your database using the [database example](./examples/database) as a template
- Add authentication/authorization if needed
- Set up HTTPS with a reverse proxy (nginx/caddy) for production

## Support

For issues or questions:
- Open an issue on GitHub
- Check existing issues for solutions
- Review the protocol documentation

## Security Considerations

**Built-in protections:**

- **Serial number validation** — all endpoints reject malformed device IDs (empty, too long, special characters)
- **Device registration limits** — `WithMaxDevices(n)` caps how many devices can register
- **Command queue limits** — `WithMaxCommandsPerDevice(n)` prevents unbounded queue growth
- **Request body limits** — `WithMaxBodySize(n)` prevents oversized payloads
- **Debug endpoint opt-in** — `/iclock/inspect` is disabled unless `WithEnableInspect()` is set

**Additional measures for production:**

1. **Add authentication** - the basic protocol doesn't include authentication
2. **Use HTTPS** - set up a reverse proxy with TLS
3. **Rate limiting** - prevent abuse with rate limiting middleware
4. **Access control** - restrict which IPs can connect
5. **Log monitoring** - monitor for suspicious activity

Example with basic authentication:

```go
func authMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        // Verify device token or IP whitelist
        token := r.Header.Get("X-Device-Token")
        if !isValidToken(token) {
            http.Error(w, "Unauthorized", http.StatusUnauthorized)
            return
        }
        next.ServeHTTP(w, r)
    })
}

http.Handle("/iclock/", authMiddleware(server))
```

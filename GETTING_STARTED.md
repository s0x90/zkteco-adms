# Getting Started with ZK Device Sync

This guide will help you get started with the ZK Device Sync library for integrating ZKTeco biometric devices with your Go applications.

## Prerequisites

- Go 1.16 or higher
- A ZKTeco biometric device (attendance machine)
- Network access between your server and the device

## Installation

```bash
go get github.com/s0x90/zk-device-sync
```

## Your First Server

Create a file `main.go`:

```go
package main

import (
    "fmt"
    "log"
    "net/http"
    
    zkdevicesync "github.com/s0x90/zk-device-sync"
)

func main() {
    // Create the server
    server := zkdevicesync.NewIClockServer()
    
    // Handle attendance records
    server.OnAttendance = func(record zkdevicesync.AttendanceRecord) {
        fmt.Printf("Attendance: User %s at %s\n", 
            record.UserID, 
            record.Timestamp)
    }
    
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
    Status       int       // 0=Check In, 1=Check Out
    VerifyMode   int       // 0=Password, 1=Fingerprint, 2=Card
    WorkCode     string    // Optional work code
    SerialNumber string    // Device that recorded this
}
```

## Common Use Cases

### 1. Save to Database

```go
server.OnAttendance = func(record zkdevicesync.AttendanceRecord) {
    query := `INSERT INTO attendance (user_id, timestamp, status, device) 
              VALUES (?, ?, ?, ?)`
    _, err := db.Exec(query, 
        record.UserID, 
        record.Timestamp, 
        record.Status,
        record.SerialNumber)
    if err != nil {
        log.Printf("Error saving: %v", err)
    }
}
```

### 2. Send Notifications

```go
server.OnAttendance = func(record zkdevicesync.AttendanceRecord) {
    if record.Status == 0 { // Check In
        sendWelcomeNotification(record.UserID)
    } else if record.Status == 1 { // Check Out
        sendGoodbyeNotification(record.UserID)
    }
}
```

### 3. Real-time Dashboard

```go
var attendanceChannel = make(chan zkdevicesync.AttendanceRecord, 100)

server.OnAttendance = func(record zkdevicesync.AttendanceRecord) {
    attendanceChannel <- record
}

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
server.SendInfoCommand("DEVICE001")
```

The device will respond with its configuration, which you can handle:

```go
server.OnDeviceInfo = func(sn string, info map[string]string) {
    fmt.Printf("Device %s info:\n", sn)
    for key, value := range info {
        fmt.Printf("  %s: %s\n", key, value)
    }
}
```

### Custom Commands

```go
// Restart device
server.SendCommand("DEVICE001", "CHECK")

// Clear admin privileges
server.SendCommand("DEVICE001", "CLEAR DATA")
```

## Monitoring Connected Devices

```go
http.HandleFunc("/devices", func(w http.ResponseWriter, r *http.Request) {
    devices := server.ListDevices()
    for _, device := range devices {
        fmt.Fprintf(w, "Device: %s, Last Seen: %s\n",
            device.SerialNumber,
            device.LastActivity)
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

1. **Verify device is registered:**
   ```bash
   curl http://localhost:8080/devices
   ```

2. **Check OnAttendance callback is set:**
   ```go
   if server.OnAttendance == nil {
       log.Fatal("OnAttendance callback not set!")
   }
   ```

3. **Test with simulated data** (see above)

### Device Shows "Connection Failed"

- Ensure server URL is correct (must end with `/iclock/`)
- Check if server is running: `curl http://YOUR_SERVER:8080/iclock/getrequest?SN=TEST`
- Try HTTP instead of HTTPS (most devices don't support HTTPS)

## Next Steps

- Review the [examples](./examples) directory for complete applications
- Read the [API documentation](./README.md#api-reference)
- Integrate with your database using [database_integration.go](./examples/database_integration.go) as a template
- Add authentication/authorization if needed
- Set up HTTPS with a reverse proxy (nginx/caddy) for production

## Support

For issues or questions:
- Open an issue on GitHub
- Check existing issues for solutions
- Review the protocol documentation

## Security Considerations

⚠️ **Important for Production:**

1. **Add authentication** - the basic protocol doesn't include authentication
2. **Use HTTPS** - set up a reverse proxy with TLS
3. **Validate input** - always validate device serial numbers and user IDs
4. **Rate limiting** - prevent abuse with rate limiting middleware
5. **Access control** - restrict which IPs can connect
6. **Log monitoring** - monitor for suspicious activity

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

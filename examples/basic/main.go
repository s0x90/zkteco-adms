// Command basic demonstrates a minimal ZKTeco ADMS server with device
// status and command-sending endpoints.
//
// Run with:
//
//	go run ./examples/basic
package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"time"

	zkadms "github.com/s0x90/zkteco-adms"
)

// statusRecorder wraps http.ResponseWriter to capture the status code.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// logMiddleware logs each HTTP request with method, path, remote address,
// response status code, and duration.
func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		defer func() {
			slog.Info("http request",
				"method", r.Method,
				"path", r.URL.Path,
				"remote", r.RemoteAddr,
				"status", rec.status,
				"duration", time.Since(start),
			)
		}()
		next.ServeHTTP(rec, r)
	})
}

func main() {
	// Create a new ADMS server with functional options
	server := zkadms.NewADMSServer(
		// Enable the /iclock/inspect debug endpoint.
		zkadms.WithEnableInspect(),

		// Callback for attendance records
		zkadms.WithOnAttendance(func(ctx context.Context, record zkadms.AttendanceRecord) {
			fmt.Printf("Attendance Record:\n")
			fmt.Printf("  Device: %s\n", record.SerialNumber)
			fmt.Printf("  User ID: %s\n", record.UserID)
			fmt.Printf("  Timestamp: %s\n", record.Timestamp.Format(time.RFC3339))
			fmt.Printf("  Status: %d (0=Check In, 1=Check Out)\n", record.Status)
			fmt.Printf("  Verify Mode: %s (%d)\n", zkadms.VerifyModeName(record.VerifyMode), record.VerifyMode)
			fmt.Printf("  Work Code: %s\n", record.WorkCode)
			fmt.Println("---")
		}),

		// Callback for device information
		zkadms.WithOnDeviceInfo(func(ctx context.Context, sn string, info map[string]string) {
			fmt.Printf("Device Info for %s:\n", sn)
			for key, value := range info {
				fmt.Printf("  %s: %s\n", key, value)
			}
			fmt.Println("---")
		}),

		// Callback for device registry/capabilities
		zkadms.WithOnRegistry(func(ctx context.Context, sn string, info map[string]string) {
			fmt.Printf("Registry Info for %s (partial):\n", sn)
			shown := 0
			for k, v := range info {
				fmt.Printf("  %s: %s\n", k, v)
				shown++
				if shown >= 8 { // avoid spamming console
					break
				}
			}
			fmt.Println("---")
		}),
	)
	defer server.Close()

	// Register some known devices (optional).
	// RegisterDevice returns an error for invalid serial numbers or
	// when the device limit (WithMaxDevices) has been reached.
	for _, sn := range []string{"DEVICE001", "DEVICE002"} {
		if err := server.RegisterDevice(sn); err != nil {
			log.Fatalf("failed to register device %s: %v", sn, err)
		}
	}

	// Set up HTTP routes
	http.Handle("/iclock/", logMiddleware(server))

	// Add a status endpoint to view connected devices
	http.Handle("/status", logMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		devices := server.ListDevices()
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "Connected Devices: %d\n\n", len(devices))
		for _, device := range devices {
			online := server.IsDeviceOnline(device.SerialNumber)
			fmt.Fprintf(w, "Serial Number: %s\n", device.SerialNumber)
			fmt.Fprintf(w, "Last Activity: %s\n", device.LastActivity.Format(time.RFC3339))
			fmt.Fprintf(w, "Online: %t\n", online)
			fmt.Fprintf(w, "Options: %v\n", device.Options)
			fmt.Fprintln(w, "---")
		}
	})))

	// Add a command endpoint to send commands to devices
	http.Handle("/command", logMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		sn := r.URL.Query().Get("sn")
		cmd := r.URL.Query().Get("cmd")

		if sn == "" || cmd == "" {
			http.Error(w, "Missing sn or cmd parameter", http.StatusBadRequest)
			return
		}

		if err := server.QueueCommand(sn, cmd); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(w, "Command queued for device %s: %s\n", sn, cmd)
	})))

	// Start the server
	addr := ":8080"
	fmt.Printf("ZKTeco iClock Server starting on %s\n", addr)
	fmt.Println("Endpoints:")
	fmt.Println("  /iclock/cdata      - Attendance data endpoint")
	fmt.Println("  /iclock/registry   - Device registry/capabilities endpoint")
	fmt.Println("  /iclock/getrequest - Device polling endpoint")
	fmt.Println("  /iclock/devicecmd  - Command confirmation endpoint")
	fmt.Println("  /iclock/inspect    - JSON device snapshot (opt-in via WithEnableInspect)")
	fmt.Println("  /status            - View connected devices")
	fmt.Println("  /command           - Send commands to devices (POST)")
	fmt.Println()

	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal(err)
	}
}

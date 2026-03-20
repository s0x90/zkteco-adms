// Command basic demonstrates a minimal ZKTeco ADMS server with device
// status and command-sending endpoints.
//
// SECURITY WARNING: This example does NOT include any authentication or
// authorization. All endpoints (/status, /command) are publicly accessible.
// In a production deployment you MUST add authentication middleware (e.g.
// API keys, OAuth2, mTLS) before exposing these routes to a network.
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
	"os/signal"
	"syscall"
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

// logMiddleware logs each HTTP request with method, path, query parameters,
// remote address, headers, response status code, and duration.
func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		defer func() {
			slog.Info("http request",
				"method", r.Method,
				"path", r.URL.Path,
				"query", r.URL.RawQuery,
				"remote", r.RemoteAddr,
				"headers", r.Header,
				"status", rec.status,
				"duration", time.Since(start),
			)
		}()
		next.ServeHTTP(rec, r)
	})
}

// statusHandler returns an http.Handler that lists registered devices and
// their online/offline status.
func statusHandler(server *zkadms.ADMSServer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
	})
}

// commandHandler returns an http.Handler that accepts POST requests to queue
// commands for a specific device identified by "sn" and "cmd" query params.
func commandHandler(server *zkadms.ADMSServer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	})
}

// newMux builds the HTTP mux with all routes wired up to the given ADMS server.
func newMux(server *zkadms.ADMSServer) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/iclock/", logMiddleware(server))
	// WARNING: These routes have no authentication. Add auth middleware
	// before deploying to production.
	mux.Handle("/status", logMiddleware(statusHandler(server)))
	mux.Handle("/command", logMiddleware(commandHandler(server)))
	return mux
}

// run creates the ADMS server, wires HTTP routes, and blocks until ctx is
// canceled. On cancellation the HTTP server is gracefully shut down and
// the ADMS callback queue is drained before returning.
func run(ctx context.Context, addr string) error {
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
			fmt.Printf("  Status: %d (0=Check In, 1=Check Out, 2=Break Out, 3=Break In, 4=Overtime In, 5=Overtime Out)\n", record.Status)
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
			return fmt.Errorf("register device %s: %w", sn, err)
		}
	}

	mux := newMux(server)

	srv := &http.Server{Addr: addr, Handler: mux}

	// Shutdown goroutine: wait for context cancellation, then gracefully
	// stop the HTTP server so in-flight requests can finish.
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("HTTP server shutdown error: %v", err)
		}
	}()

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

	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}

	log.Println("server stopped")
	return nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, ":8080"); err != nil {
		log.Fatal(err)
	}
}

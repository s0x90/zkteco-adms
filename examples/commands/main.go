// Command commands demonstrates a ZKTeco ADMS server with a REST API
// for sending management commands to devices (reboot, sync time, user
// management, door control, etc.).
//
// SECURITY WARNING: This example does NOT include any authentication or
// authorization. All API endpoints — including reboot, clear-data, open-door,
// and user management — are publicly accessible. In a production deployment
// you MUST add authentication middleware (e.g. API keys, OAuth2, mTLS)
// before exposing these routes to a network. Unauthenticated access to these
// endpoints can lead to data loss, unauthorized physical access, and device
// compromise.
//
// Run with:
//
//	go run ./examples/commands -devices ABCD12345678,EFGH87654321
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os/signal"
	"strings"
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
// remote address, response status code, and duration.
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
				"status", rec.status,
				"duration", time.Since(start),
			)
		}()
		next.ServeHTTP(rec, r)
	})
}

// ---------- JSON request/response types ----------

// apiError is the JSON structure for error responses.
type apiError struct {
	Error string `json:"error"`
}

// commandResponse is the JSON structure for successful command responses.
type commandResponse struct {
	Status  string `json:"status"`
	Device  string `json:"device"`
	Command string `json:"command"`
}

// deviceEntry is the JSON structure for a device in list/detail responses.
type deviceEntry struct {
	SerialNumber string            `json:"serial_number"`
	LastActivity string            `json:"last_activity"`
	Online       bool              `json:"online"`
	Options      map[string]string `json:"options,omitzero"`
	PendingCmds  int               `json:"pending_commands"`
}

// devicesResponse is the JSON structure for the device list endpoint.
type devicesResponse struct {
	Count   int           `json:"count"`
	Devices []deviceEntry `json:"devices"`
}

// userRequest is the JSON body for the add/update user endpoint.
type userRequest struct {
	PIN       string `json:"pin"`
	Name      string `json:"name"`
	Privilege int    `json:"privilege"`
	Card      string `json:"card"`
}

// deleteUserRequest is the JSON body for the delete user endpoint.
type deleteUserRequest struct {
	PIN string `json:"pin"`
}

// rawCommandRequest is the JSON body for the raw command endpoint.
type rawCommandRequest struct {
	Command string `json:"command"`
}

// ---------- JSON helpers ----------

// writeJSON encodes v as JSON and writes it to w with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("failed to write JSON response", "error", err)
	}
}

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, apiError{Error: msg})
}

// writeCommandOK writes a JSON success response for a queued command.
func writeCommandOK(w http.ResponseWriter, sn, cmd string) {
	writeJSON(w, http.StatusOK, commandResponse{
		Status:  "queued",
		Device:  sn,
		Command: cmd,
	})
}

// decodeJSON reads and decodes a JSON request body into dst.
// On failure it writes a 400 error response and returns false.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// ---------- device lookup helper ----------

// requireDevice validates the serial number from the path and checks that
// the device is registered. On failure it writes a JSON error and returns
// ("", false).
func requireDevice(w http.ResponseWriter, r *http.Request, server *zkadms.ADMSServer) (string, bool) {
	sn := r.PathValue("sn")
	if sn == "" {
		writeError(w, http.StatusBadRequest, "missing device serial number")
		return "", false
	}
	if server.GetDevice(sn) == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("device %s not found", sn))
		return "", false
	}
	return sn, true
}

// queueOrFail queues a command and writes a JSON error on failure.
func queueOrFail(w http.ResponseWriter, server *zkadms.ADMSServer, sn, cmd string) bool {
	if err := server.QueueCommand(sn, cmd); err != nil {
		if errors.Is(err, zkadms.ErrCommandQueueFull) {
			writeError(w, http.StatusServiceUnavailable, "command queue full for device "+sn)
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return false
	}
	return true
}

// ---------- command handlers ----------

// handleReboot queues a REBOOT command for the device.
func handleReboot(server *zkadms.ADMSServer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sn, ok := requireDevice(w, r, server)
		if !ok {
			return
		}
		if !queueOrFail(w, server, sn, "REBOOT") {
			return
		}
		writeCommandOK(w, sn, "REBOOT")
	})
}

// handleInfo queues an INFO command to request device configuration.
func handleInfo(server *zkadms.ADMSServer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sn, ok := requireDevice(w, r, server)
		if !ok {
			return
		}
		if err := server.SendInfoCommand(sn); err != nil {
			if errors.Is(err, zkadms.ErrCommandQueueFull) {
				writeError(w, http.StatusServiceUnavailable, "command queue full for device "+sn)
			} else {
				writeError(w, http.StatusInternalServerError, err.Error())
			}
			return
		}
		writeCommandOK(w, sn, "INFO")
	})
}

// handleSyncTime queues a command to synchronize the device clock to the
// server's current time.
func handleSyncTime(server *zkadms.ADMSServer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sn, ok := requireDevice(w, r, server)
		if !ok {
			return
		}
		now := time.Now().Format("2006-01-02 15:04:05")
		cmd := "SET OPTION ServerLocalTime=" + now
		if !queueOrFail(w, server, sn, cmd) {
			return
		}
		writeCommandOK(w, sn, cmd)
	})
}

// handleClearData queues a CLEAR DATA command to wipe attendance logs.
func handleClearData(server *zkadms.ADMSServer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sn, ok := requireDevice(w, r, server)
		if !ok {
			return
		}
		if !queueOrFail(w, server, sn, "CLEAR DATA") {
			return
		}
		writeCommandOK(w, sn, "CLEAR DATA")
	})
}

// handleClearLog queues a CLEAR LOG command to wipe operation logs.
func handleClearLog(server *zkadms.ADMSServer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sn, ok := requireDevice(w, r, server)
		if !ok {
			return
		}
		if !queueOrFail(w, server, sn, "CLEAR LOG") {
			return
		}
		writeCommandOK(w, sn, "CLEAR LOG")
	})
}

// handleAddUser queues a DATA UPDATE USERINFO command to add or update
// a user on the device. Expects a JSON body with pin, name, privilege,
// and card fields.
func handleAddUser(server *zkadms.ADMSServer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sn, ok := requireDevice(w, r, server)
		if !ok {
			return
		}
		var req userRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if req.PIN == "" {
			writeError(w, http.StatusBadRequest, "pin is required")
			return
		}
		data := fmt.Sprintf("PIN=%s\tName=%s\tPri=%d\tCard=%s",
			req.PIN, req.Name, req.Privilege, req.Card)
		if err := server.SendDataCommand(sn, "USERINFO", data); err != nil {
			if errors.Is(err, zkadms.ErrCommandQueueFull) {
				writeError(w, http.StatusServiceUnavailable, "command queue full for device "+sn)
			} else {
				writeError(w, http.StatusInternalServerError, err.Error())
			}
			return
		}
		writeCommandOK(w, sn, "DATA UPDATE USERINFO")
	})
}

// handleDeleteUser queues a DATA DEL_USER command to remove a user from
// the device. Expects a JSON body with a pin field.
func handleDeleteUser(server *zkadms.ADMSServer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sn, ok := requireDevice(w, r, server)
		if !ok {
			return
		}
		var req deleteUserRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if req.PIN == "" {
			writeError(w, http.StatusBadRequest, "pin is required")
			return
		}
		cmd := fmt.Sprintf("DATA DEL_USER PIN=%s", req.PIN)
		if !queueOrFail(w, server, sn, cmd) {
			return
		}
		writeCommandOK(w, sn, cmd)
	})
}

// handleOpenDoor queues a CONTROL DEVICE command to trigger the door relay.
func handleOpenDoor(server *zkadms.ADMSServer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sn, ok := requireDevice(w, r, server)
		if !ok {
			return
		}
		if !queueOrFail(w, server, sn, "CONTROL DEVICE 1 1") {
			return
		}
		writeCommandOK(w, sn, "CONTROL DEVICE 1 1")
	})
}

// handleRawCommand queues a caller-supplied command string. This is an
// escape hatch for commands not covered by the dedicated endpoints.
// Expects a JSON body with a command field.
func handleRawCommand(server *zkadms.ADMSServer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sn, ok := requireDevice(w, r, server)
		if !ok {
			return
		}
		var req rawCommandRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if req.Command == "" {
			writeError(w, http.StatusBadRequest, "command is required")
			return
		}
		if !queueOrFail(w, server, sn, req.Command) {
			return
		}
		writeCommandOK(w, sn, req.Command)
	})
}

// ---------- query handlers ----------

// handleListDevices returns all registered devices with their online status.
func handleListDevices(server *zkadms.ADMSServer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		devices := server.ListDevices()
		resp := devicesResponse{
			Count:   len(devices),
			Devices: make([]deviceEntry, len(devices)),
		}
		for i, d := range devices {
			resp.Devices[i] = deviceEntry{
				SerialNumber: d.SerialNumber,
				LastActivity: d.LastActivity.Format(time.RFC3339),
				Online:       server.IsDeviceOnline(d.SerialNumber),
				Options:      d.Options,
				PendingCmds:  server.PendingCommandsCount(d.SerialNumber),
			}
		}
		writeJSON(w, http.StatusOK, resp)
	})
}

// handleDeviceDetail returns details for a single device.
func handleDeviceDetail(server *zkadms.ADMSServer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sn, ok := requireDevice(w, r, server)
		if !ok {
			return
		}
		d := server.GetDevice(sn)
		entry := deviceEntry{
			SerialNumber: d.SerialNumber,
			LastActivity: d.LastActivity.Format(time.RFC3339),
			Online:       server.IsDeviceOnline(sn),
			Options:      d.Options,
			PendingCmds:  server.PendingCommandsCount(sn),
		}
		writeJSON(w, http.StatusOK, entry)
	})
}

// ---------- wiring ----------

// newMux builds the HTTP mux with all routes wired up to the given ADMS server.
func newMux(server *zkadms.ADMSServer) *http.ServeMux {
	mux := http.NewServeMux()

	// ADMS protocol endpoints (handled by the library).
	mux.Handle("/iclock/", logMiddleware(server))

	// Device listing / detail.
	mux.Handle("GET /api/devices", logMiddleware(handleListDevices(server)))
	mux.Handle("GET /api/devices/{sn}", logMiddleware(handleDeviceDetail(server)))

	// WARNING: All command endpoints below have NO authentication.
	// In production, wrap these with auth middleware to prevent unauthorized
	// reboot, data wipe, door unlock, and user management operations.
	mux.Handle("POST /api/devices/{sn}/reboot", logMiddleware(handleReboot(server)))
	mux.Handle("POST /api/devices/{sn}/info", logMiddleware(handleInfo(server)))
	mux.Handle("POST /api/devices/{sn}/sync-time", logMiddleware(handleSyncTime(server)))
	mux.Handle("POST /api/devices/{sn}/clear-data", logMiddleware(handleClearData(server)))
	mux.Handle("POST /api/devices/{sn}/clear-log", logMiddleware(handleClearLog(server)))
	mux.Handle("POST /api/devices/{sn}/users", logMiddleware(handleAddUser(server)))
	mux.Handle("POST /api/devices/{sn}/users/delete", logMiddleware(handleDeleteUser(server)))
	mux.Handle("POST /api/devices/{sn}/open-door", logMiddleware(handleOpenDoor(server)))
	mux.Handle("POST /api/devices/{sn}/command", logMiddleware(handleRawCommand(server)))

	return mux
}

// run creates the ADMS server, wires HTTP routes, and blocks until ctx is
// canceled. On cancellation the HTTP server is gracefully shut down and
// the ADMS callback queue is drained before returning.
func run(ctx context.Context, addr string, devices []string) error {
	server := zkadms.NewADMSServer(
		// Enable the /iclock/inspect debug endpoint.
		zkadms.WithEnableInspect(),

		// Limit queued commands per device to prevent unbounded growth.
		zkadms.WithMaxCommandsPerDevice(50),

		// Log device info responses (sent after INFO commands).
		zkadms.WithOnDeviceInfo(func(_ context.Context, sn string, info map[string]string) {
			fmt.Printf("Device %s info response:\n", sn)
			for key, value := range info {
				fmt.Printf("  %s: %s\n", key, value)
			}
			fmt.Println("---")
		}),

		// Log attendance records.
		zkadms.WithOnAttendance(func(_ context.Context, record zkadms.AttendanceRecord) {
			fmt.Printf("Attendance: User %s at %s (status=%d, verify=%s) from %s\n",
				record.UserID,
				record.Timestamp.Format(time.RFC3339),
				record.Status,
				zkadms.VerifyModeName(record.VerifyMode),
				record.SerialNumber)
		}),
	)
	defer server.Close()

	// Pre-register known devices so the API can send commands to them
	// before they first connect. Pass device serial numbers via -devices.
	for _, sn := range devices {
		sn = strings.TrimSpace(sn)
		if sn == "" {
			continue
		}
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

	fmt.Printf("ZKTeco Device Command Server starting on %s\n", addr)
	fmt.Println("Endpoints:")
	fmt.Println("  /iclock/                          - ZKTeco device ADMS endpoints")
	fmt.Println("  /iclock/inspect                   - JSON device snapshot (debug)")
	fmt.Println()
	fmt.Println("  GET  /api/devices                 - List all devices")
	fmt.Println("  GET  /api/devices/{sn}            - Device detail")
	fmt.Println()
	fmt.Println("  POST /api/devices/{sn}/reboot     - Reboot device")
	fmt.Println("  POST /api/devices/{sn}/info       - Request device info")
	fmt.Println("  POST /api/devices/{sn}/sync-time  - Sync device clock")
	fmt.Println("  POST /api/devices/{sn}/clear-data - Clear attendance data")
	fmt.Println("  POST /api/devices/{sn}/clear-log  - Clear operation log")
	fmt.Println("  POST /api/devices/{sn}/users      - Add/update user (JSON body)")
	fmt.Println("  POST /api/devices/{sn}/users/delete - Delete user (JSON body)")
	fmt.Println("  POST /api/devices/{sn}/open-door  - Trigger door relay")
	fmt.Println("  POST /api/devices/{sn}/command    - Send raw command (JSON body)")
	fmt.Println()
	fmt.Println("Example usage with curl:")
	fmt.Println(`  curl -X POST http://localhost:8081/api/devices/<SN>/reboot`)
	fmt.Println(`  curl -X POST http://localhost:8081/api/devices/<SN>/info`)
	fmt.Println(`  curl -X POST http://localhost:8081/api/devices/<SN>/users \`)
	fmt.Println(`       -H 'Content-Type: application/json' \`)
	fmt.Println(`       -d '{"pin":"1001","name":"John Doe","privilege":0,"card":"12345678"}'`)
	fmt.Println(`  curl -X POST http://localhost:8081/api/devices/<SN>/command \`)
	fmt.Println(`       -H 'Content-Type: application/json' \`)
	fmt.Println(`       -d '{"command":"ENROLL_FP PIN=1001"}'`)
	fmt.Println()

	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}

	log.Println("server stopped")
	return nil
}

func main() {
	addr := flag.String("addr", ":8081", "HTTP listen address")
	devicesFlag := flag.String("devices", "", "comma-separated device serial numbers to pre-register")
	flag.Parse()

	var devices []string
	if *devicesFlag != "" {
		devices = strings.Split(*devicesFlag, ",")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *addr, devices); err != nil {
		log.Fatal(err)
	}
}

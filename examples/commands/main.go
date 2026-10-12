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
//
// Devices send wall-clock times with no timezone, so the server must know
// where each one is. Set a default with -timezone and override per device with
// -devices SN=Zone or PUT /api/devices/{sn}/timezone:
//
//	go run ./examples/commands -timezone Asia/Jakarta -devices SN1,SN2=Asia/Makassar
package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // IANA zone names (e.g. Asia/Jakarta) work even where the OS has no zoneinfo

	zkadms "github.com/s0x90/zkteco-adms"
)

// maxLogBody is the maximum number of bytes logged for request/response bodies.
const maxLogBody = 1024

// now returns the current time. Tests replace it to get deterministic output.
var now = time.Now

// dumpWriter is the destination for multi-line HTTP dump output. Defaults to
// os.Stderr so that dump blocks render with real newlines in the console.
// Tests may reassign this to capture output.
var dumpWriter io.Writer = os.Stderr

// statusRecorder wraps http.ResponseWriter to capture the status code and
// response body for debug logging.
//
// PRODUCTION NOTE: This wrapper only forwards WriteHeader and Write. In
// production you should also implement Unwrap(), Flush() (http.Flusher),
// and Hijack() (http.Hijacker) so that wrapped handlers retain access to
// optional http.ResponseWriter capabilities (e.g. streaming, WebSocket
// upgrades, http.ResponseController).
type statusRecorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	if n > 0 && r.body.Len() < maxLogBody+1 {
		r.body.Write(b[:n])
	}
	return n, err
}

// truncateBody returns s as-is when it fits within maxLogBody, otherwise
// returns the first maxLogBody bytes with a truncation notice.
//
// PRODUCTION NOTE: The reported "total" is only the number of captured
// bytes, not the true payload size (the body is read through a
// LimitReader). Production code should track actual bytes written/read
// separately and report the real total so operators are not misled during
// incident debugging.
func truncateBody(s string, total int) string {
	if len(s) <= maxLogBody {
		return s
	}
	return s[:maxLogBody] + fmt.Sprintf("... (truncated, %d bytes total)", total)
}

// logMiddleware logs each HTTP request and response in a human-readable
// HTTP-style dump format that shows the full request line, headers,
// request body, response status, response headers, and response body.
//
// NOTE: This middleware logs all headers and bodies verbatim. In a
// production deployment you should redact sensitive headers (e.g.
// Authorization, Cookie) and consider gating body logging behind a
// debug flag.
func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()

		// Buffer request body (up to maxLogBody bytes) for logging while
		// preserving the full stream for downstream handlers.
		var reqBody []byte
		if r.Body != nil {
			reqBody, _ = io.ReadAll(io.LimitReader(r.Body, maxLogBody+1))
			// Reconstruct the body so downstream sees the complete payload:
			// the bytes we already consumed followed by whatever remains.
			//
			// PRODUCTION NOTE: io.NopCloser discards the original body's
			// Close method, which can degrade connection reuse under load.
			// Production code should wrap the MultiReader with a custom
			// ReadCloser that forwards Close() to the original r.Body.
			r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(reqBody), r.Body))
		}

		// Deferred logging ensures the exchange is recorded even when a
		// downstream handler panics.  After logging we re-panic so the
		// standard recovery behavior is preserved.
		var panicked bool
		var panicVal any
		defer func() {
			if v := recover(); v != nil {
				panicked = true
				panicVal = v
			}

			// PRODUCTION NOTE: This builds the full dump string on every
			// request even when the log level is above Info. Production
			// code should check slog.Default().Enabled(ctx, slog.LevelInfo)
			// before buffering bodies and building the dump to avoid
			// unnecessary CPU and allocation overhead under load.

			elapsed := time.Since(start)

			// --- Build request section ---
			var buf strings.Builder
			buf.WriteString("\n--- HTTP Request ---\n")
			fullPath := r.URL.Path
			if r.URL.RawQuery != "" {
				fullPath += "?" + r.URL.RawQuery
			}
			fmt.Fprintf(&buf, "%s %s %s\n", r.Method, fullPath, r.Proto)
			fmt.Fprintf(&buf, "Host: %s\n", r.Host)
			// PRODUCTION NOTE: Headers are dumped verbatim here. Production
			// code must redact sensitive headers (Authorization, Cookie,
			// Set-Cookie) and should gate body logging behind an explicit
			// debug flag to avoid leaking credentials or PII into logs.
			for name, vals := range r.Header {
				fmt.Fprintf(&buf, "%s: %s\n", name, strings.Join(vals, ", "))
			}
			if len(reqBody) > 0 {
				buf.WriteString("\n")
				buf.WriteString(truncateBody(string(reqBody), len(reqBody)))
				buf.WriteString("\n")
			}

			// --- Build response section ---
			fmt.Fprintf(&buf, "\n--- HTTP Response (%d %s, %s) ---\n",
				rec.status, http.StatusText(rec.status), elapsed)
			for name, vals := range rec.Header() {
				fmt.Fprintf(&buf, "%s: %s\n", name, strings.Join(vals, ", "))
			}
			if rec.body.Len() > 0 {
				buf.WriteString("\n")
				buf.WriteString(truncateBody(rec.body.String(), rec.body.Len()))
				buf.WriteString("\n")
			}

			if panicked {
				slog.Error("http exchange (panic)",
					"method", r.Method,
					"path", fullPath,
					"status", rec.status,
					"duration", elapsed,
					"panic", panicVal,
				)
				fmt.Fprint(dumpWriter, buf.String())
				panic(panicVal) // re-panic for upstream recovery
			}

			slog.Info("http exchange",
				"method", r.Method,
				"path", fullPath,
				"status", rec.status,
				"duration", elapsed,
			)
			fmt.Fprint(dumpWriter, buf.String())
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
	Timezone     string            `json:"timezone"`
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

// getOptionRequest is the JSON body for the get-option endpoint.
type getOptionRequest struct {
	Key string `json:"key"`
}

// shellRequest is the JSON body for the shell command endpoint.
type shellRequest struct {
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
	_, err := server.QueueCommand(sn, cmd)
	return sendOrFail(w, sn, err)
}

// sendOrFail checks err from a Send*/QueueCommand call and writes a JSON
// error response when non-nil.  Returns true when the command was queued
// successfully.
func sendOrFail(w http.ResponseWriter, sn string, err error) bool {
	if err != nil {
		if errors.Is(err, zkadms.ErrCommandQueueFull) {
			writeError(w, http.StatusServiceUnavailable, "command queue full for device "+sn)
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return false
	}
	return true
}

// ignoreID discards the command ID from a (int64, error) return pair,
// returning just the error for use with sendOrFail.
func ignoreID(_ int64, err error) error { return err }

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
		if !sendOrFail(w, sn, ignoreID(server.SendInfoCommand(sn))) {
			return
		}
		writeCommandOK(w, sn, "INFO")
	})
}

// handleSyncTime queues a command to set the device clock to the current time
// as it is in the device's own timezone (see handleSetTimezone), not the
// server's: a device in another timezone would otherwise end up hours off.
//
// Devices send and expect wall-clock time with no zone attached, so the zone
// configured here must match where the device physically is.
func handleSyncTime(server *zkadms.ADMSServer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sn, ok := requireDevice(w, r, server)
		if !ok {
			return
		}
		loc := server.GetDeviceTimezone(sn)
		if loc == nil { // device evicted since requireDevice
			loc = time.UTC
		}
		cmd := "SET OPTION ServerLocalTime=" + now().In(loc).Format("2006-01-02 15:04:05")
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

// handleAddUser queues a DATA UPDATE USERINFO command to add or update a user on
// the device. Expects a JSON body with pin, name, privilege, and card fields.
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
		if !sendOrFail(w, sn, ignoreID(server.SendUserAddCommand(sn, req.PIN, req.Name, req.Privilege, req.Card))) {
			return
		}
		writeCommandOK(w, sn, "DATA UPDATE USERINFO")
	})
}

// handleDeleteUser queues a DATA DELETE USERINFO command to remove a user from
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
		if !sendOrFail(w, sn, ignoreID(server.SendUserDeleteCommand(sn, req.PIN))) {
			return
		}
		writeCommandOK(w, sn, "DATA DELETE USERINFO")
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

// handleCheck queues a CHECK (heartbeat) command to verify device responsiveness.
func handleCheck(server *zkadms.ADMSServer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sn, ok := requireDevice(w, r, server)
		if !ok {
			return
		}
		if !sendOrFail(w, sn, ignoreID(server.SendCheckCommand(sn))) {
			return
		}
		writeCommandOK(w, sn, "CHECK")
	})
}

// handleGetOption queues a GET OPTION command to retrieve a device configuration
// value. Expects a JSON body with a key field. The option value is delivered
// asynchronously via the device info push, not in the command confirmation.
func handleGetOption(server *zkadms.ADMSServer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sn, ok := requireDevice(w, r, server)
		if !ok {
			return
		}
		var req getOptionRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if req.Key == "" {
			writeError(w, http.StatusBadRequest, "key is required")
			return
		}
		if !sendOrFail(w, sn, ignoreID(server.SendGetOptionCommand(sn, req.Key))) {
			return
		}
		writeCommandOK(w, sn, "GET OPTION FROM "+req.Key)
	})
}

// handleShell queues a Shell command for execution on the device's Linux OS.
// Expects a JSON body with a command field.
//
// WARNING: This executes arbitrary commands on the device. Incorrect commands
// can brick the device. Use with extreme caution.
func handleShell(server *zkadms.ADMSServer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sn, ok := requireDevice(w, r, server)
		if !ok {
			return
		}
		var req shellRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if req.Command == "" {
			writeError(w, http.StatusBadRequest, "command is required")
			return
		}
		if !sendOrFail(w, sn, ignoreID(server.SendShellCommand(sn, req.Command))) {
			return
		}
		writeCommandOK(w, sn, "Shell "+req.Command)
	})
}

// handleQueryUsers queues a DATA QUERY USERINFO command to request all user
// records from the device. The data is pushed asynchronously via /iclock/cdata.
func handleQueryUsers(server *zkadms.ADMSServer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sn, ok := requireDevice(w, r, server)
		if !ok {
			return
		}
		if !sendOrFail(w, sn, ignoreID(server.SendQueryUsersCommand(sn))) {
			return
		}
		writeCommandOK(w, sn, "DATA QUERY USERINFO")
	})
}

// handleLog queues a LOG command to request log data from the device.
func handleLog(server *zkadms.ADMSServer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sn, ok := requireDevice(w, r, server)
		if !ok {
			return
		}
		if !sendOrFail(w, sn, ignoreID(server.SendLogCommand(sn))) {
			return
		}
		writeCommandOK(w, sn, "LOG")
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

// ---------- timezone ----------

// timezoneRequest is the JSON body for the set-timezone endpoint.
type timezoneRequest struct {
	Timezone string `json:"timezone"`
}

// timezoneResponse reports the timezone now in effect for a device.
type timezoneResponse struct {
	Device   string `json:"device"`
	Timezone string `json:"timezone"`
	// Source is "device" when set for this device, or "default" when it falls
	// back to the server-wide default.
	Source string `json:"source"`
}

// loadTimezone resolves an IANA zone name such as "Asia/Jakarta". "Local" is
// rejected because it would silently mean whatever zone the server runs in.
func loadTimezone(name string) (*time.Location, error) {
	if name == "" || name == "Local" {
		return nil, fmt.Errorf("timezone %q is not allowed; use an IANA name such as Asia/Jakarta", name)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("unknown timezone %q; use an IANA name such as Asia/Jakarta", name)
	}
	return loc, nil
}

// handleSetTimezone sets the timezone the server uses for a device: to read
// its attendance timestamps and to compute the time sent by sync-time. An
// empty "timezone" resets the device to the server default. It does not change
// anything on the device itself.
//
// The setting is held in memory: it is lost on restart and when an idle device
// is evicted. Use -devices SN=Zone to set it at startup.
func handleSetTimezone(server *zkadms.ADMSServer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sn, ok := requireDevice(w, r, server)
		if !ok {
			return
		}
		var req timezoneRequest
		if !decodeJSON(w, r, &req) {
			return
		}

		var loc *time.Location // nil resets to the server default
		if req.Timezone != "" {
			var err error
			if loc, err = loadTimezone(req.Timezone); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		if err := server.SetDeviceTimezone(sn, loc); err != nil {
			if errors.Is(err, zkadms.ErrDeviceNotFound) {
				writeError(w, http.StatusNotFound, fmt.Sprintf("device %s not found", sn))
				return
			}
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		source := "device"
		if loc == nil {
			source = "default"
		}
		writeJSON(w, http.StatusOK, timezoneResponse{
			Device:   sn,
			Timezone: server.GetDeviceTimezone(sn).String(),
			Source:   source,
		})
	})
}

// applyTimezoneResponse reports the commands queued to change a device's own
// timezone setting.
type applyTimezoneResponse struct {
	Status      string   `json:"status"`
	Device      string   `json:"device"`
	Timezone    string   `json:"timezone"`
	OffsetHours int      `json:"offset_hours"`
	Commands    []string `json:"commands"`
}

// handleApplyTimezone sends the device's configured timezone (see
// handleSetTimezone) to the device itself, as its integer UTC offset in hours:
//
//	SET OPTION TimeZone=7
//
// It then queues GET OPTION FROM TimeZone so the value the device actually
// holds comes back and shows in the dashboard events.
//
// The key name and the whole-hour encoding come from third-party PUSH protocol
// documentation, not from this repository's confirmed commands, so firmware
// may differ: read the device-reply and command-result events to see whether
// the device accepted it. Offsets that are not a whole number of hours are
// refused rather than guessed. The offset is the one in effect now, so it is
// not updated automatically when a daylight saving change happens.
func handleApplyTimezone(server *zkadms.ADMSServer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sn, ok := requireDevice(w, r, server)
		if !ok {
			return
		}
		loc := server.GetDeviceTimezone(sn)
		if loc == nil { // device evicted since requireDevice
			loc = time.UTC
		}
		_, offset := now().In(loc).Zone()
		if offset%3600 != 0 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf(
				"%s is UTC%+.2f, not a whole number of hours; send the raw command (SET OPTION TimeZone=...) once you know the encoding your device expects",
				loc, float64(offset)/3600))
			return
		}
		hours := offset / 3600

		cmds := []string{
			fmt.Sprintf("SET OPTION TimeZone=%d", hours),
			"GET OPTION FROM TimeZone",
		}
		for _, cmd := range cmds {
			if !queueOrFail(w, server, sn, cmd) {
				return
			}
		}
		writeJSON(w, http.StatusOK, applyTimezoneResponse{
			Status:      "queued",
			Device:      sn,
			Timezone:    loc.String(),
			OffsetHours: hours,
			Commands:    cmds,
		})
	})
}

// parseDeviceSpec parses a -devices entry: "SN" or "SN=Zone".
func parseDeviceSpec(spec string) (sn string, loc *time.Location, err error) {
	sn, zone, hasZone := strings.Cut(strings.TrimSpace(spec), "=")
	sn = strings.TrimSpace(sn)
	if !hasZone {
		return sn, nil, nil
	}
	if loc, err = loadTimezone(strings.TrimSpace(zone)); err != nil {
		return "", nil, fmt.Errorf("device %s: %w", sn, err)
	}
	return sn, loc, nil
}

// ---------- query handlers ----------

// listDevicesResponse builds the device list with each device's online status.
func listDevicesResponse(server *zkadms.ADMSServer) devicesResponse {
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
			Timezone:     server.GetDeviceTimezone(d.SerialNumber).String(),
			PendingCmds:  server.PendingCommandsCount(d.SerialNumber),
		}
	}
	return resp
}

// handleListDevices returns all registered devices with their online status.
func handleListDevices(server *zkadms.ADMSServer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, listDevicesResponse(server))
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
			Timezone:     server.GetDeviceTimezone(sn).String(),
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
	mux.Handle("POST /api/devices/{sn}/check", logMiddleware(handleCheck(server)))
	mux.Handle("POST /api/devices/{sn}/sync-time", logMiddleware(handleSyncTime(server)))
	mux.Handle("PUT /api/devices/{sn}/timezone", logMiddleware(handleSetTimezone(server)))
	mux.Handle("POST /api/devices/{sn}/timezone/apply", logMiddleware(handleApplyTimezone(server)))
	mux.Handle("POST /api/devices/{sn}/clear-data", logMiddleware(handleClearData(server)))
	mux.Handle("POST /api/devices/{sn}/clear-log", logMiddleware(handleClearLog(server)))
	mux.Handle("POST /api/devices/{sn}/users", logMiddleware(handleAddUser(server)))
	mux.Handle("POST /api/devices/{sn}/users/delete", logMiddleware(handleDeleteUser(server)))
	mux.Handle("POST /api/devices/{sn}/users/query", logMiddleware(handleQueryUsers(server)))
	mux.Handle("POST /api/devices/{sn}/open-door", logMiddleware(handleOpenDoor(server)))
	mux.Handle("POST /api/devices/{sn}/get-option", logMiddleware(handleGetOption(server)))
	mux.Handle("POST /api/devices/{sn}/shell", logMiddleware(handleShell(server)))
	mux.Handle("POST /api/devices/{sn}/log", logMiddleware(handleLog(server)))
	mux.Handle("POST /api/devices/{sn}/command", logMiddleware(handleRawCommand(server)))

	return mux
}

// run creates the ADMS server, wires HTTP routes, and blocks until ctx is
// canceled. On cancellation the HTTP server is gracefully shut down and
// the ADMS callback queue is drained before returning.
//
// Each devices entry is a serial number, optionally with its timezone as
// "SN=Zone" (e.g. "ABC123=Asia/Jakarta"). serverOpts are applied after the
// example's own options, so they can add to or override them (main uses this
// for the default timezone).
func run(ctx context.Context, addr string, devices []string, serverOpts ...zkadms.Option) error {
	// Recent device events, shown in the web dashboard.
	events := newEventLog(maxEvents)

	opts := []zkadms.Option{
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
			events.add("device-info", sn, formatInfo(info))
		}),

		// Log attendance records.
		zkadms.WithOnAttendance(func(_ context.Context, record zkadms.AttendanceRecord) {
			fmt.Printf("Attendance: User %s at %s (status=%d, verify=%s) from %s\n",
				record.UserID,
				record.Timestamp.Format(time.RFC3339),
				record.Status,
				zkadms.VerifyModeName(record.VerifyMode),
				record.SerialNumber)
			events.add("attendance", record.SerialNumber,
				fmt.Sprintf("User %s at %s (status=%d, verify=%s)",
					record.UserID, record.Timestamp.Format(time.RFC3339),
					record.Status, zkadms.VerifyModeName(record.VerifyMode)))
		}),

		// Log command confirmations from devices.
		zkadms.WithOnCommandResult(func(_ context.Context, result zkadms.CommandResult) {
			status := "OK"
			if result.ReturnCode != 0 {
				status = fmt.Sprintf("FAIL (code %d)", result.ReturnCode)
			}
			fmt.Printf("Command result: device=%s id=%d cmd=%q queued=%q status=%s\n",
				result.SerialNumber, result.ID, result.Command, result.QueuedCommand, status)
			level := "ok"
			if result.ReturnCode != 0 {
				level = "error"
			}
			events.addLevel("command-result", level, result.SerialNumber,
				fmt.Sprintf("#%d %s: %s", result.ID, cmp.Or(result.QueuedCommand, result.Command),
					describeResult(result.ReturnCode)))
		}),

		// Log user records received from DATA QUERY USERINFO.
		zkadms.WithOnQueryUsers(func(_ context.Context, sn string, users []zkadms.UserRecord) {
			fmt.Printf("Query users result: device=%s count=%d\n", sn, len(users))
			for i, u := range users {
				fmt.Printf("  [%d] PIN=%s Name=%q Privilege=%d Card=%q Password=%q\n",
					i, u.PIN, u.Name, u.Privilege, u.Card, u.Password)
			}
			events.add("users", sn, formatUsers(users))
		}),
	}
	server := zkadms.NewADMSServer(slices.Concat(opts, serverOpts)...)
	defer server.Close()

	// Pre-register known devices so the API can send commands to them
	// before they first connect. Pass device serial numbers via -devices.
	for _, spec := range devices {
		sn, loc, err := parseDeviceSpec(spec)
		if err != nil {
			return err
		}
		if sn == "" {
			continue
		}
		if err := server.RegisterDevice(sn, zkadms.WithDeviceTimezone(loc)); err != nil {
			return fmt.Errorf("register device %s: %w", sn, err)
		}
	}

	mux := newMux(server)
	mountDashboard(mux, server, events)

	srv := &http.Server{Addr: addr, Handler: captureDeviceReplies(events, mux)}

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
	fmt.Printf("Web dashboard: http://localhost%s/\n", dashboardPort(addr))
	fmt.Println("Endpoints:")
	fmt.Println("  /iclock/                          - ZKTeco device ADMS endpoints")
	fmt.Println("  /iclock/inspect                   - JSON device snapshot (debug)")
	fmt.Println()
	fmt.Println("  GET  /api/devices                 - List all devices")
	fmt.Println("  GET  /api/devices/{sn}            - Device detail")
	fmt.Println()
	fmt.Println("  POST /api/devices/{sn}/reboot     - Reboot device")
	fmt.Println("  POST /api/devices/{sn}/info       - Request device info")
	fmt.Println("  POST /api/devices/{sn}/check      - Heartbeat / connectivity check")
	fmt.Println("  POST /api/devices/{sn}/sync-time  - Sync device clock (in the device's timezone)")
	fmt.Println("  PUT  /api/devices/{sn}/timezone   - Set device timezone (JSON body; empty resets to default)")
	fmt.Println("  POST /api/devices/{sn}/timezone/apply - Send that timezone to the device itself")
	fmt.Println("  POST /api/devices/{sn}/clear-data - Clear attendance data")
	fmt.Println("  POST /api/devices/{sn}/clear-log  - Clear operation log")
	fmt.Println("  POST /api/devices/{sn}/users      - Add/update user (JSON body)")
	fmt.Println("  POST /api/devices/{sn}/users/delete - Delete user (JSON body)")
	fmt.Println("  POST /api/devices/{sn}/users/query  - Query all users from device")
	fmt.Println("  POST /api/devices/{sn}/open-door  - Trigger door relay")
	fmt.Println("  POST /api/devices/{sn}/get-option - Get device option (JSON body)")
	fmt.Println("  POST /api/devices/{sn}/shell      - Execute shell command (JSON body)")
	fmt.Println("  POST /api/devices/{sn}/log        - Request log data")
	fmt.Println("  POST /api/devices/{sn}/command    - Send raw command (JSON body)")
	fmt.Println()
	fmt.Println("Example usage with curl:")
	fmt.Println(`  curl -X POST http://localhost:8080/api/devices/<SN>/info`)
	fmt.Println(`  curl -X POST http://localhost:8080/api/devices/<SN>/check`)
	fmt.Println(`  curl -X POST http://localhost:8080/api/devices/<SN>/reboot`)
	fmt.Println()
	fmt.Println(`  # Add a user`)
	fmt.Println(`  curl -X POST http://localhost:8080/api/devices/<SN>/users \`)
	fmt.Println(`       -H 'Content-Type: application/json' \`)
	fmt.Println(`       -d '{"pin":"1001","name":"John Doe","privilege":0,"card":"12345678"}'`)
	fmt.Println()
	fmt.Println(`  # Delete a user`)
	fmt.Println(`  curl -X POST http://localhost:8080/api/devices/<SN>/users/delete \`)
	fmt.Println(`       -H 'Content-Type: application/json' \`)
	fmt.Println(`       -d '{"pin":"1001"}'`)
	fmt.Println()
	fmt.Println(`  # Query all users from device (data pushed via /iclock/cdata)`)
	fmt.Println(`  curl -X POST http://localhost:8080/api/devices/<SN>/users/query`)
	fmt.Println()
	fmt.Println(`  # Get a device option`)
	fmt.Println(`  curl -X POST http://localhost:8080/api/devices/<SN>/get-option \`)
	fmt.Println(`       -H 'Content-Type: application/json' \`)
	fmt.Println(`       -d '{"key":"DeviceName"}'`)
	fmt.Println()
	fmt.Println(`  # Execute a shell command on the device (use with caution!)`)
	fmt.Println(`  curl -X POST http://localhost:8080/api/devices/<SN>/shell \`)
	fmt.Println(`       -H 'Content-Type: application/json' \`)
	fmt.Println(`       -d '{"command":"date"}'`)
	fmt.Println()
	fmt.Println(`  # Request log data`)
	fmt.Println(`  curl -X POST http://localhost:8080/api/devices/<SN>/log`)
	fmt.Println()
	fmt.Println(`  # Send a raw command`)
	fmt.Println(`  curl -X POST http://localhost:8080/api/devices/<SN>/command \`)
	fmt.Println(`       -H 'Content-Type: application/json' \`)
	fmt.Println(`       -d '{"command":"CHECK"}'`)
	fmt.Println()

	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}

	log.Println("server stopped")
	return nil
}

// describeResult explains a device return code. The codes are the ones
// confirmed on real hardware (see the README); anything else is reported as is.
func describeResult(code int) string {
	switch code {
	case 0:
		return "OK"
	case -1:
		return "FAILED (-1: command not supported, or no data)"
	case -2:
		return "FAILED (-2: file operation failed)"
	case -1002:
		return "FAILED (-1002: invalid command syntax)"
	case -1004:
		return "FAILED (-1004: table or feature not supported on this model)"
	default:
		return fmt.Sprintf("FAILED (code %d)", code)
	}
}

// formatInfo renders a device info map as sorted "key=value" pairs.
func formatInfo(info map[string]string) string {
	pairs := make([]string, 0, len(info))
	for _, k := range slices.Sorted(maps.Keys(info)) {
		pairs = append(pairs, k+"="+info[k])
	}
	return strings.Join(pairs, ", ")
}

// formatUsers summarizes a user query result. Passwords are left out: the
// dashboard has no authentication.
func formatUsers(users []zkadms.UserRecord) string {
	parts := make([]string, 0, len(users))
	for _, u := range users {
		parts = append(parts, fmt.Sprintf("%s (%s)", u.PIN, u.Name))
	}
	return fmt.Sprintf("%d user(s): %s", len(users), strings.Join(parts, ", "))
}

// dashboardPort returns the ":port" part of a listen address for display, or
// an empty string when the address has no usable port.
func dashboardPort(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil && port != "" {
		return ":" + port
	}
	return ""
}

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	devicesFlag := flag.String("devices", "",
		"comma-separated device serial numbers to pre-register; add =Zone to set a device's timezone (SN1=Asia/Jakarta,SN2)")
	tzFlag := flag.String("timezone", "UTC",
		"default IANA timezone for devices without their own (e.g. Asia/Jakarta)")
	flag.Parse()

	defaultLoc, err := loadTimezone(*tzFlag)
	if err != nil {
		log.Fatal(err)
	}

	var devices []string
	if *devicesFlag != "" {
		devices = strings.Split(*devicesFlag, ",")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *addr, devices, zkadms.WithDefaultTimezone(defaultLoc)); err != nil {
		log.Fatal(err)
	}
}

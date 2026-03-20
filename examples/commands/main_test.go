package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	zkadms "github.com/s0x90/zkteco-adms"
)

// ---------- statusRecorder tests ----------

func TestStatusRecorder_DefaultStatus(t *testing.T) {
	w := httptest.NewRecorder()
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

	rec.Write([]byte("hello"))

	if rec.status != http.StatusOK {
		t.Errorf("expected default status %d, got %d", http.StatusOK, rec.status)
	}
}

func TestStatusRecorder_CapturesWriteHeader(t *testing.T) {
	w := httptest.NewRecorder()
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

	rec.WriteHeader(http.StatusNotFound)

	if rec.status != http.StatusNotFound {
		t.Errorf("expected status %d, got %d", http.StatusNotFound, rec.status)
	}
	if w.Code != http.StatusNotFound {
		t.Errorf("underlying ResponseWriter should also receive %d, got %d",
			http.StatusNotFound, w.Code)
	}
}

// ---------- logMiddleware tests ----------

func TestLogMiddleware_LogsRequest(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})

	handler := logMiddleware(inner)
	req := httptest.NewRequest(http.MethodPost, "/test-path", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	logOutput := buf.String()
	for _, want := range []string{"http request", "method=POST", "path=/test-path", "status=201", "duration="} {
		if !strings.Contains(logOutput, want) {
			t.Errorf("log output missing %q; got: %s", want, logOutput)
		}
	}
}

func TestLogMiddleware_PreservesResponseBody(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})

	handler := logMiddleware(inner)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Body.String() != `{"ok":true}` {
		t.Errorf("expected body %q, got %q", `{"ok":true}`, w.Body.String())
	}
	if w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q",
			w.Header().Get("Content-Type"))
	}
}

// ---------- helpers ----------

func newTestServer(t *testing.T, devices ...string) *zkadms.ADMSServer {
	t.Helper()
	server := zkadms.NewADMSServer()
	t.Cleanup(func() { server.Close() })
	for _, sn := range devices {
		if err := server.RegisterDevice(sn); err != nil {
			t.Fatalf("RegisterDevice(%q) failed: %v", sn, err)
		}
	}
	return server
}

func newTestMux(t *testing.T, devices ...string) *http.ServeMux {
	t.Helper()
	server := newTestServer(t, devices...)
	return newMux(server)
}

// postJSON builds a POST request with a JSON body.
func postJSON(t *testing.T, url string, body any) *http.Request {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// decodeResponse decodes a JSON response body into dst.
func decodeResponse(t *testing.T, w *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), dst); err != nil {
		t.Fatalf("failed to decode response JSON: %v\nbody: %s", err, w.Body.String())
	}
}

// ---------- list/detail endpoint tests ----------

func TestListDevices_Empty(t *testing.T) {
	mux := newTestMux(t)

	req := httptest.NewRequest(http.MethodGet, "/api/devices", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp devicesResponse
	decodeResponse(t, w, &resp)
	if resp.Count != 0 {
		t.Errorf("expected 0 devices, got %d", resp.Count)
	}
	if len(resp.Devices) != 0 {
		t.Errorf("expected empty devices list, got %d", len(resp.Devices))
	}
}

func TestListDevices_WithDevices(t *testing.T) {
	mux := newTestMux(t, "DEV001", "DEV002")

	req := httptest.NewRequest(http.MethodGet, "/api/devices", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp devicesResponse
	decodeResponse(t, w, &resp)
	if resp.Count != 2 {
		t.Errorf("expected 2 devices, got %d", resp.Count)
	}

	serials := make(map[string]bool)
	for _, d := range resp.Devices {
		serials[d.SerialNumber] = true
	}
	for _, sn := range []string{"DEV001", "DEV002"} {
		if !serials[sn] {
			t.Errorf("expected device %s in response", sn)
		}
	}
}

func TestListDevices_ContentType(t *testing.T) {
	mux := newTestMux(t)

	req := httptest.NewRequest(http.MethodGet, "/api/devices", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}
}

func TestDeviceDetail_Found(t *testing.T) {
	mux := newTestMux(t, "DEV001")

	req := httptest.NewRequest(http.MethodGet, "/api/devices/DEV001", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var entry deviceEntry
	decodeResponse(t, w, &entry)
	if entry.SerialNumber != "DEV001" {
		t.Errorf("expected serial DEV001, got %q", entry.SerialNumber)
	}
	if entry.LastActivity == "" {
		t.Error("expected non-empty last_activity")
	}
}

func TestDeviceDetail_NotFound(t *testing.T) {
	mux := newTestMux(t)

	req := httptest.NewRequest(http.MethodGet, "/api/devices/NOSUCH", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}

	var resp apiError
	decodeResponse(t, w, &resp)
	if !strings.Contains(resp.Error, "NOSUCH") {
		t.Errorf("expected error mentioning NOSUCH; got: %s", resp.Error)
	}
}

// ---------- reboot tests ----------

func TestReboot_Success(t *testing.T) {
	mux := newTestMux(t, "DEV001")

	req := httptest.NewRequest(http.MethodPost, "/api/devices/DEV001/reboot", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp commandResponse
	decodeResponse(t, w, &resp)
	if resp.Status != "queued" {
		t.Errorf("expected status 'queued', got %q", resp.Status)
	}
	if resp.Device != "DEV001" {
		t.Errorf("expected device DEV001, got %q", resp.Device)
	}
	if resp.Command != "REBOOT" {
		t.Errorf("expected command REBOOT, got %q", resp.Command)
	}
}

func TestReboot_DeviceNotFound(t *testing.T) {
	mux := newTestMux(t)

	req := httptest.NewRequest(http.MethodPost, "/api/devices/NOSUCH/reboot", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestReboot_MethodNotAllowed(t *testing.T) {
	mux := newTestMux(t, "DEV001")

	req := httptest.NewRequest(http.MethodGet, "/api/devices/DEV001/reboot", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for GET, got %d", w.Code)
	}
}

// ---------- info tests ----------

func TestInfo_Success(t *testing.T) {
	mux := newTestMux(t, "DEV001")

	req := httptest.NewRequest(http.MethodPost, "/api/devices/DEV001/info", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp commandResponse
	decodeResponse(t, w, &resp)
	if resp.Command != "INFO" {
		t.Errorf("expected command INFO, got %q", resp.Command)
	}
}

// ---------- sync-time tests ----------

func TestSyncTime_Success(t *testing.T) {
	mux := newTestMux(t, "DEV001")

	req := httptest.NewRequest(http.MethodPost, "/api/devices/DEV001/sync-time", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp commandResponse
	decodeResponse(t, w, &resp)
	if !strings.HasPrefix(resp.Command, "SET OPTION ServerLocalTime=") {
		t.Errorf("expected SET OPTION ServerLocalTime= prefix, got %q", resp.Command)
	}
}

// ---------- clear-data tests ----------

func TestClearData_Success(t *testing.T) {
	mux := newTestMux(t, "DEV001")

	req := httptest.NewRequest(http.MethodPost, "/api/devices/DEV001/clear-data", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp commandResponse
	decodeResponse(t, w, &resp)
	if resp.Command != "CLEAR DATA" {
		t.Errorf("expected command CLEAR DATA, got %q", resp.Command)
	}
}

// ---------- clear-log tests ----------

func TestClearLog_Success(t *testing.T) {
	mux := newTestMux(t, "DEV001")

	req := httptest.NewRequest(http.MethodPost, "/api/devices/DEV001/clear-log", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp commandResponse
	decodeResponse(t, w, &resp)
	if resp.Command != "CLEAR LOG" {
		t.Errorf("expected command CLEAR LOG, got %q", resp.Command)
	}
}

// ---------- add user tests ----------

func TestAddUser_Success(t *testing.T) {
	mux := newTestMux(t, "DEV001")

	body := userRequest{PIN: "1001", Name: "John Doe", Privilege: 0, Card: "12345678"}
	req := postJSON(t, "/api/devices/DEV001/users", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp commandResponse
	decodeResponse(t, w, &resp)
	if resp.Command != "DATA UPDATE USERINFO" {
		t.Errorf("expected command DATA UPDATE USERINFO, got %q", resp.Command)
	}
}

func TestAddUser_MissingPIN(t *testing.T) {
	mux := newTestMux(t, "DEV001")

	body := userRequest{Name: "No PIN"}
	req := postJSON(t, "/api/devices/DEV001/users", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}

	var resp apiError
	decodeResponse(t, w, &resp)
	if !strings.Contains(resp.Error, "pin") {
		t.Errorf("expected error about pin; got: %s", resp.Error)
	}
}

func TestAddUser_InvalidJSON(t *testing.T) {
	mux := newTestMux(t, "DEV001")

	req := httptest.NewRequest(http.MethodPost, "/api/devices/DEV001/users",
		strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid JSON, got %d", w.Code)
	}
}

func TestAddUser_DeviceNotFound(t *testing.T) {
	mux := newTestMux(t)

	body := userRequest{PIN: "1001", Name: "John"}
	req := postJSON(t, "/api/devices/NOSUCH/users", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// ---------- delete user tests ----------

func TestDeleteUser_Success(t *testing.T) {
	mux := newTestMux(t, "DEV001")

	body := deleteUserRequest{PIN: "1001"}
	req := postJSON(t, "/api/devices/DEV001/users/delete", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp commandResponse
	decodeResponse(t, w, &resp)
	if !strings.Contains(resp.Command, "DATA DEL_USER PIN=1001") {
		t.Errorf("expected DEL_USER command with PIN; got: %q", resp.Command)
	}
}

func TestDeleteUser_MissingPIN(t *testing.T) {
	mux := newTestMux(t, "DEV001")

	body := deleteUserRequest{}
	req := postJSON(t, "/api/devices/DEV001/users/delete", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// ---------- open-door tests ----------

func TestOpenDoor_Success(t *testing.T) {
	mux := newTestMux(t, "DEV001")

	req := httptest.NewRequest(http.MethodPost, "/api/devices/DEV001/open-door", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp commandResponse
	decodeResponse(t, w, &resp)
	if resp.Command != "CONTROL DEVICE 1 1" {
		t.Errorf("expected command CONTROL DEVICE 1 1, got %q", resp.Command)
	}
}

// ---------- raw command tests ----------

func TestRawCommand_Success(t *testing.T) {
	mux := newTestMux(t, "DEV001")

	body := rawCommandRequest{Command: "ENROLL_FP PIN=1001"}
	req := postJSON(t, "/api/devices/DEV001/command", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp commandResponse
	decodeResponse(t, w, &resp)
	if resp.Command != "ENROLL_FP PIN=1001" {
		t.Errorf("expected command verbatim; got %q", resp.Command)
	}
}

func TestRawCommand_EmptyCommand(t *testing.T) {
	mux := newTestMux(t, "DEV001")

	body := rawCommandRequest{}
	req := postJSON(t, "/api/devices/DEV001/command", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty command, got %d", w.Code)
	}
}

func TestRawCommand_InvalidJSON(t *testing.T) {
	mux := newTestMux(t, "DEV001")

	req := httptest.NewRequest(http.MethodPost, "/api/devices/DEV001/command",
		strings.NewReader("{bad"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// ---------- queue full tests ----------

func TestReboot_QueueFull(t *testing.T) {
	server := zkadms.NewADMSServer(zkadms.WithMaxCommandsPerDevice(1))
	t.Cleanup(func() { server.Close() })
	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatal(err)
	}

	mux := newMux(server)

	// Fill the queue with one command.
	req := httptest.NewRequest(http.MethodPost, "/api/devices/DEV001/reboot", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first command should succeed, got %d: %s", w.Code, w.Body.String())
	}

	// Second command should fail.
	req = httptest.NewRequest(http.MethodPost, "/api/devices/DEV001/reboot", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when queue full, got %d: %s", w.Code, w.Body.String())
	}

	var resp apiError
	decodeResponse(t, w, &resp)
	if !strings.Contains(resp.Error, "queue full") {
		t.Errorf("expected queue full error; got: %s", resp.Error)
	}
}

func TestAddUser_QueueFull(t *testing.T) {
	server := zkadms.NewADMSServer(zkadms.WithMaxCommandsPerDevice(1))
	t.Cleanup(func() { server.Close() })
	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatal(err)
	}

	mux := newMux(server)

	// Fill with first user.
	body := userRequest{PIN: "1001", Name: "First"}
	req := postJSON(t, "/api/devices/DEV001/users", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first command should succeed, got %d", w.Code)
	}

	// Second should fail.
	body = userRequest{PIN: "1002", Name: "Second"}
	req = postJSON(t, "/api/devices/DEV001/users", body)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestInfo_QueueFull(t *testing.T) {
	server := zkadms.NewADMSServer(zkadms.WithMaxCommandsPerDevice(1))
	t.Cleanup(func() { server.Close() })
	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatal(err)
	}

	mux := newMux(server)

	// Fill the queue.
	req := httptest.NewRequest(http.MethodPost, "/api/devices/DEV001/info", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first should succeed, got %d", w.Code)
	}

	// Second should fail.
	req = httptest.NewRequest(http.MethodPost, "/api/devices/DEV001/info", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------- method not allowed tests ----------

func TestCommandEndpoints_MethodNotAllowed(t *testing.T) {
	mux := newTestMux(t, "DEV001")

	postOnlyPaths := []string{
		"/api/devices/DEV001/reboot",
		"/api/devices/DEV001/info",
		"/api/devices/DEV001/sync-time",
		"/api/devices/DEV001/clear-data",
		"/api/devices/DEV001/clear-log",
		"/api/devices/DEV001/users",
		"/api/devices/DEV001/users/delete",
		"/api/devices/DEV001/open-door",
		"/api/devices/DEV001/command",
	}

	for _, path := range postOnlyPaths {
		t.Run("GET "+path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("expected 405 for GET %s, got %d", path, w.Code)
			}
		})
	}
}

// ---------- iclock route test ----------

func TestNewMux_IClockRoute(t *testing.T) {
	mux := newTestMux(t)

	req := httptest.NewRequest(http.MethodGet, "/iclock/cdata", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	// The route should be handled by the ADMS server, not 404.
	if w.Code == http.StatusNotFound {
		t.Error("/iclock/cdata should be handled by ADMS server, not 404")
	}
}

// ---------- unknown route test ----------

func TestNewMux_UnknownRoute(t *testing.T) {
	mux := newTestMux(t)

	req := httptest.NewRequest(http.MethodGet, "/nonexistent", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown route, got %d", w.Code)
	}
}

// ---------- mux logging integration test ----------

func TestNewMux_LogsRequests(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	mux := newTestMux(t, "DEV001")

	req := httptest.NewRequest(http.MethodGet, "/api/devices", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	logOutput := buf.String()
	if !strings.Contains(logOutput, "http request") {
		t.Errorf("expected log from logMiddleware; got: %s", logOutput)
	}
	if !strings.Contains(logOutput, "path=/api/devices") {
		t.Errorf("expected path=/api/devices in log; got: %s", logOutput)
	}
}

// ---------- run() lifecycle tests ----------

func TestRun_StartsAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())

	errCh := make(chan error, 1)
	go func() {
		errCh <- run(ctx, ":0", nil)
	}()

	// Give the server a moment to start listening.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return within 5s after context cancellation")
	}
}

func TestRun_InvalidAddr(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	err := run(ctx, ":-1", nil)
	if err == nil {
		t.Fatal("expected error for invalid address, got nil")
	}
}

// ---------- multiple commands queued in sequence ----------

func TestMultipleCommandsQueued(t *testing.T) {
	server := newTestServer(t, "DEV001")
	mux := newMux(server)

	// Queue several different commands.
	endpoints := []string{
		"/api/devices/DEV001/reboot",
		"/api/devices/DEV001/info",
		"/api/devices/DEV001/clear-data",
		"/api/devices/DEV001/open-door",
	}

	for _, ep := range endpoints {
		req := httptest.NewRequest(http.MethodPost, ep, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("POST %s: expected 200, got %d: %s", ep, w.Code, w.Body.String())
		}
	}

	// All commands should be pending.
	cmds := server.DrainCommands("DEV001")
	if len(cmds) != 4 {
		t.Errorf("expected 4 queued commands, got %d: %v", len(cmds), cmds)
	}
}

// ---------- device not found tests for all command handlers ----------

func TestSyncTime_DeviceNotFound(t *testing.T) {
	mux := newTestMux(t)

	req := httptest.NewRequest(http.MethodPost, "/api/devices/NOSUCH/sync-time", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
	var resp apiError
	decodeResponse(t, w, &resp)
	if !strings.Contains(resp.Error, "NOSUCH") {
		t.Errorf("expected error mentioning NOSUCH; got: %s", resp.Error)
	}
}

func TestClearData_DeviceNotFound(t *testing.T) {
	mux := newTestMux(t)

	req := httptest.NewRequest(http.MethodPost, "/api/devices/NOSUCH/clear-data", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestClearLog_DeviceNotFound(t *testing.T) {
	mux := newTestMux(t)

	req := httptest.NewRequest(http.MethodPost, "/api/devices/NOSUCH/clear-log", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestDeleteUser_DeviceNotFound(t *testing.T) {
	mux := newTestMux(t)

	body := deleteUserRequest{PIN: "1001"}
	req := postJSON(t, "/api/devices/NOSUCH/users/delete", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestDeleteUser_InvalidJSON(t *testing.T) {
	mux := newTestMux(t, "DEV001")

	req := httptest.NewRequest(http.MethodPost, "/api/devices/DEV001/users/delete",
		strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid JSON, got %d", w.Code)
	}
}

func TestOpenDoor_DeviceNotFound(t *testing.T) {
	mux := newTestMux(t)

	req := httptest.NewRequest(http.MethodPost, "/api/devices/NOSUCH/open-door", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestRawCommand_DeviceNotFound(t *testing.T) {
	mux := newTestMux(t)

	body := rawCommandRequest{Command: "REBOOT"}
	req := postJSON(t, "/api/devices/NOSUCH/command", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestInfo_DeviceNotFound(t *testing.T) {
	mux := newTestMux(t)

	req := httptest.NewRequest(http.MethodPost, "/api/devices/NOSUCH/info", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// ---------- queue full tests for remaining handlers ----------

func TestSyncTime_QueueFull(t *testing.T) {
	server := zkadms.NewADMSServer(zkadms.WithMaxCommandsPerDevice(1))
	t.Cleanup(func() { server.Close() })
	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatal(err)
	}

	mux := newMux(server)

	// Fill the queue.
	req := httptest.NewRequest(http.MethodPost, "/api/devices/DEV001/sync-time", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first command should succeed, got %d", w.Code)
	}

	// Second should fail.
	req = httptest.NewRequest(http.MethodPost, "/api/devices/DEV001/sync-time", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when queue full, got %d: %s", w.Code, w.Body.String())
	}
}

func TestClearData_QueueFull(t *testing.T) {
	server := zkadms.NewADMSServer(zkadms.WithMaxCommandsPerDevice(1))
	t.Cleanup(func() { server.Close() })
	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatal(err)
	}

	mux := newMux(server)

	req := httptest.NewRequest(http.MethodPost, "/api/devices/DEV001/clear-data", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first should succeed, got %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/devices/DEV001/clear-data", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestClearLog_QueueFull(t *testing.T) {
	server := zkadms.NewADMSServer(zkadms.WithMaxCommandsPerDevice(1))
	t.Cleanup(func() { server.Close() })
	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatal(err)
	}

	mux := newMux(server)

	req := httptest.NewRequest(http.MethodPost, "/api/devices/DEV001/clear-log", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first should succeed, got %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/devices/DEV001/clear-log", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestDeleteUser_QueueFull(t *testing.T) {
	server := zkadms.NewADMSServer(zkadms.WithMaxCommandsPerDevice(1))
	t.Cleanup(func() { server.Close() })
	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatal(err)
	}

	mux := newMux(server)

	body := deleteUserRequest{PIN: "1001"}
	req := postJSON(t, "/api/devices/DEV001/users/delete", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first should succeed, got %d", w.Code)
	}

	body = deleteUserRequest{PIN: "1002"}
	req = postJSON(t, "/api/devices/DEV001/users/delete", body)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestOpenDoor_QueueFull(t *testing.T) {
	server := zkadms.NewADMSServer(zkadms.WithMaxCommandsPerDevice(1))
	t.Cleanup(func() { server.Close() })
	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatal(err)
	}

	mux := newMux(server)

	req := httptest.NewRequest(http.MethodPost, "/api/devices/DEV001/open-door", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first should succeed, got %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/devices/DEV001/open-door", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestRawCommand_QueueFull(t *testing.T) {
	server := zkadms.NewADMSServer(zkadms.WithMaxCommandsPerDevice(1))
	t.Cleanup(func() { server.Close() })
	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatal(err)
	}

	mux := newMux(server)

	body := rawCommandRequest{Command: "CMD1"}
	req := postJSON(t, "/api/devices/DEV001/command", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first should succeed, got %d", w.Code)
	}

	body = rawCommandRequest{Command: "CMD2"}
	req = postJSON(t, "/api/devices/DEV001/command", body)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

// ---------- run() lifecycle tests with devices ----------

func TestRun_WithDevices(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())

	errCh := make(chan error, 1)
	go func() {
		errCh <- run(ctx, ":0", []string{"DEV001", "DEV002"})
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return within 5s after context cancellation")
	}
}

func TestRun_WithEmptyDeviceStrings(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())

	errCh := make(chan error, 1)
	go func() {
		errCh <- run(ctx, ":0", []string{"DEV001", "", "  ", "DEV002"})
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return within 5s")
	}
}

// TestRun_ExercisesCallbacks starts the server via run() and sends simulated
// ADMS device traffic to exercise the device-info and attendance callbacks.
func TestRun_ExercisesCallbacks(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- run(ctx, addr, []string{"DEV001"})
	}()

	time.Sleep(100 * time.Millisecond)

	base := "http://" + addr

	// Send attendance data — triggers WithOnAttendance callback.
	attendanceData := "USER001\t2026-03-20 09:00:00\t0\t1\t0\tWC1"
	resp, err := http.Post(base+"/iclock/cdata?SN=DEV001&table=ATTLOG",
		"text/plain", strings.NewReader(attendanceData))
	if err != nil {
		t.Fatalf("POST attendance failed: %v", err)
	}
	resp.Body.Close()

	// Send device info — triggers WithOnDeviceInfo callback.
	infoData := "FWVersion=Ver 6.60\nDeviceName=ZK-F22"
	resp, err = http.Post(base+"/iclock/cdata?SN=DEV001",
		"text/plain", strings.NewReader(infoData))
	if err != nil {
		t.Fatalf("POST device info failed: %v", err)
	}
	resp.Body.Close()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return within 5s")
	}
}

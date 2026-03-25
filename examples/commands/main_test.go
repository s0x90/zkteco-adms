package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	for _, want := range []string{"http exchange", "method=POST", "path=/test-path", "status=201", "duration="} {
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

// ---------- truncateBody tests ----------

func TestTruncateBody_Short(t *testing.T) {
	s := "short body"
	got := truncateBody(s, len(s))
	if got != s {
		t.Errorf("expected %q, got %q", s, got)
	}
}

func TestTruncateBody_ExactLimit(t *testing.T) {
	s := strings.Repeat("x", maxLogBody)
	got := truncateBody(s, len(s))
	if got != s {
		t.Errorf("expected exact body to pass through, got len %d", len(got))
	}
}

func TestTruncateBody_Truncated(t *testing.T) {
	s := strings.Repeat("x", maxLogBody+100)
	got := truncateBody(s, len(s))
	if !strings.HasPrefix(got, strings.Repeat("x", maxLogBody)) {
		t.Error("expected truncated body to start with maxLogBody x's")
	}
	if !strings.Contains(got, "truncated") {
		t.Error("expected truncation notice in output")
	}
	if !strings.Contains(got, fmt.Sprintf("%d bytes total", len(s))) {
		t.Errorf("expected total byte count in truncation notice")
	}
}

// ---------- statusRecorder body capture tests ----------

func TestStatusRecorder_CapturesBody(t *testing.T) {
	w := httptest.NewRecorder()
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

	rec.Write([]byte("hello"))
	rec.Write([]byte(" world"))

	if rec.body.String() != "hello world" {
		t.Errorf("expected captured body %q, got %q", "hello world", rec.body.String())
	}
}

// ---------- logMiddleware panic recovery test ----------

func TestLogMiddleware_PanicRecovery(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		panic("test panic")
	})

	handler := logMiddleware(inner)
	req := httptest.NewRequest(http.MethodGet, "/panic-path", nil)
	w := httptest.NewRecorder()

	defer func() {
		v := recover()
		if v == nil {
			t.Fatal("expected panic to be re-raised")
		}
		if v != "test panic" {
			t.Errorf("expected panic value %q, got %v", "test panic", v)
		}
		logOutput := buf.String()
		if !strings.Contains(logOutput, "http exchange (panic)") {
			t.Errorf("expected panic log entry; got: %s", logOutput)
		}
	}()

	handler.ServeHTTP(w, req)
}

// ---------- logMiddleware with request body and query params ----------

func TestLogMiddleware_WithRequestBody(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	var dumpBuf bytes.Buffer
	oldDumpWriter := dumpWriter
	dumpWriter = &dumpBuf
	t.Cleanup(func() { dumpWriter = oldDumpWriter })

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Write(body)
	})

	handler := logMiddleware(inner)
	reqBody := "test request body content"
	req := httptest.NewRequest(http.MethodPost, "/test?foo=bar", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Body.String() != reqBody {
		t.Errorf("expected echoed body %q, got %q", reqBody, w.Body.String())
	}

	logOutput := buf.String()
	if !strings.Contains(logOutput, "foo=bar") {
		t.Errorf("expected query params in log; got: %s", logOutput)
	}

	// Verify HTTP dump was captured (dumpWriter redirect prevents test output pollution).
	if dumpBuf.Len() == 0 {
		t.Error("expected HTTP dump output to be captured in dumpBuf")
	}
}

func TestLogMiddleware_WithLargeRequestBody(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	var dumpBuf bytes.Buffer
	oldDumpWriter := dumpWriter
	dumpWriter = &dumpBuf
	t.Cleanup(func() { dumpWriter = oldDumpWriter })

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})

	handler := logMiddleware(inner)
	largeBody := strings.Repeat("A", maxLogBody+500)
	req := httptest.NewRequest(http.MethodPost, "/large-body", strings.NewReader(largeBody))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	dumpOutput := dumpBuf.String()
	if !strings.Contains(dumpOutput, "truncated") {
		t.Errorf("expected truncation in dump for large body; got: %s", dumpOutput)
	}
}

func TestLogMiddleware_WithLargeResponseBody(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	var dumpBuf bytes.Buffer
	oldDumpWriter := dumpWriter
	dumpWriter = &dumpBuf
	t.Cleanup(func() { dumpWriter = oldDumpWriter })

	largeResp := strings.Repeat("B", maxLogBody+500)
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(largeResp))
	})

	handler := logMiddleware(inner)
	req := httptest.NewRequest(http.MethodGet, "/large-response", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	dumpOutput := dumpBuf.String()
	if !strings.Contains(dumpOutput, "truncated") {
		t.Errorf("expected truncation in dump for large response body; got: %s", dumpOutput)
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

// ---------- check tests ----------

func TestCheck_Success(t *testing.T) {
	mux := newTestMux(t, "DEV001")

	req := httptest.NewRequest(http.MethodPost, "/api/devices/DEV001/check", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp commandResponse
	decodeResponse(t, w, &resp)
	if resp.Command != "CHECK" {
		t.Errorf("expected command CHECK, got %q", resp.Command)
	}
}

// ---------- get-option tests ----------

func TestGetOption_Success(t *testing.T) {
	mux := newTestMux(t, "DEV001")

	body := getOptionRequest{Key: "DeviceName"}
	req := postJSON(t, "/api/devices/DEV001/get-option", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp commandResponse
	decodeResponse(t, w, &resp)
	if resp.Command != "GET OPTION FROM DeviceName" {
		t.Errorf("expected command %q, got %q", "GET OPTION FROM DeviceName", resp.Command)
	}
}

func TestGetOption_MissingKey(t *testing.T) {
	mux := newTestMux(t, "DEV001")

	body := getOptionRequest{}
	req := postJSON(t, "/api/devices/DEV001/get-option", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty key, got %d", w.Code)
	}
}

func TestGetOption_InvalidJSON(t *testing.T) {
	mux := newTestMux(t, "DEV001")

	req := httptest.NewRequest(http.MethodPost, "/api/devices/DEV001/get-option",
		strings.NewReader("{bad"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// ---------- shell tests ----------

func TestShell_Success(t *testing.T) {
	mux := newTestMux(t, "DEV001")

	body := shellRequest{Command: "date"}
	req := postJSON(t, "/api/devices/DEV001/shell", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp commandResponse
	decodeResponse(t, w, &resp)
	if resp.Command != "Shell date" {
		t.Errorf("expected command %q, got %q", "Shell date", resp.Command)
	}
}

func TestShell_MissingCommand(t *testing.T) {
	mux := newTestMux(t, "DEV001")

	body := shellRequest{}
	req := postJSON(t, "/api/devices/DEV001/shell", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty command, got %d", w.Code)
	}
}

func TestShell_InvalidJSON(t *testing.T) {
	mux := newTestMux(t, "DEV001")

	req := httptest.NewRequest(http.MethodPost, "/api/devices/DEV001/shell",
		strings.NewReader("{bad"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// ---------- query users tests ----------

func TestQueryUsers_Success(t *testing.T) {
	mux := newTestMux(t, "DEV001")

	req := httptest.NewRequest(http.MethodPost, "/api/devices/DEV001/users/query", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp commandResponse
	decodeResponse(t, w, &resp)
	if resp.Command != "DATA QUERY USERINFO" {
		t.Errorf("expected command %q, got %q", "DATA QUERY USERINFO", resp.Command)
	}
}

// ---------- log tests ----------

func TestLog_Success(t *testing.T) {
	mux := newTestMux(t, "DEV001")

	req := httptest.NewRequest(http.MethodPost, "/api/devices/DEV001/log", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp commandResponse
	decodeResponse(t, w, &resp)
	if resp.Command != "LOG" {
		t.Errorf("expected command LOG, got %q", resp.Command)
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
	if resp.Command != "DATA DELETE USERINFO" {
		t.Errorf("expected DATA DELETE USERINFO command; got: %q", resp.Command)
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
		"/api/devices/DEV001/check",
		"/api/devices/DEV001/sync-time",
		"/api/devices/DEV001/clear-data",
		"/api/devices/DEV001/clear-log",
		"/api/devices/DEV001/users",
		"/api/devices/DEV001/users/delete",
		"/api/devices/DEV001/users/query",
		"/api/devices/DEV001/open-door",
		"/api/devices/DEV001/get-option",
		"/api/devices/DEV001/shell",
		"/api/devices/DEV001/log",
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
	if !strings.Contains(logOutput, "http exchange") {
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

func TestCheck_DeviceNotFound(t *testing.T) {
	mux := newTestMux(t)

	req := httptest.NewRequest(http.MethodPost, "/api/devices/NOSUCH/check", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetOption_DeviceNotFound(t *testing.T) {
	mux := newTestMux(t)

	body := getOptionRequest{Key: "DeviceName"}
	req := postJSON(t, "/api/devices/NOSUCH/get-option", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestShell_DeviceNotFound(t *testing.T) {
	mux := newTestMux(t)

	body := shellRequest{Command: "date"}
	req := postJSON(t, "/api/devices/NOSUCH/shell", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestQueryUsers_DeviceNotFound(t *testing.T) {
	mux := newTestMux(t)

	req := httptest.NewRequest(http.MethodPost, "/api/devices/NOSUCH/users/query", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestLog_DeviceNotFound(t *testing.T) {
	mux := newTestMux(t)

	req := httptest.NewRequest(http.MethodPost, "/api/devices/NOSUCH/log", nil)
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

func TestCheck_QueueFull(t *testing.T) {
	server := zkadms.NewADMSServer(zkadms.WithMaxCommandsPerDevice(1))
	t.Cleanup(func() { server.Close() })
	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatal(err)
	}

	mux := newMux(server)

	req := httptest.NewRequest(http.MethodPost, "/api/devices/DEV001/check", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first should succeed, got %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/devices/DEV001/check", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetOption_QueueFull(t *testing.T) {
	server := zkadms.NewADMSServer(zkadms.WithMaxCommandsPerDevice(1))
	t.Cleanup(func() { server.Close() })
	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatal(err)
	}

	mux := newMux(server)

	body := getOptionRequest{Key: "DeviceName"}
	req := postJSON(t, "/api/devices/DEV001/get-option", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first should succeed, got %d", w.Code)
	}

	body = getOptionRequest{Key: "FWVersion"}
	req = postJSON(t, "/api/devices/DEV001/get-option", body)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestShell_QueueFull(t *testing.T) {
	server := zkadms.NewADMSServer(zkadms.WithMaxCommandsPerDevice(1))
	t.Cleanup(func() { server.Close() })
	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatal(err)
	}

	mux := newMux(server)

	body := shellRequest{Command: "date"}
	req := postJSON(t, "/api/devices/DEV001/shell", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first should succeed, got %d", w.Code)
	}

	body = shellRequest{Command: "uptime"}
	req = postJSON(t, "/api/devices/DEV001/shell", body)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestQueryUsers_QueueFull(t *testing.T) {
	server := zkadms.NewADMSServer(zkadms.WithMaxCommandsPerDevice(1))
	t.Cleanup(func() { server.Close() })
	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatal(err)
	}

	mux := newMux(server)

	req := httptest.NewRequest(http.MethodPost, "/api/devices/DEV001/users/query", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first should succeed, got %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/devices/DEV001/users/query", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestLog_QueueFull(t *testing.T) {
	server := zkadms.NewADMSServer(zkadms.WithMaxCommandsPerDevice(1))
	t.Cleanup(func() { server.Close() })
	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatal(err)
	}

	mux := newMux(server)

	req := httptest.NewRequest(http.MethodPost, "/api/devices/DEV001/log", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first should succeed, got %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/devices/DEV001/log", nil)
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

	// Wait for server to start using a readiness loop instead of a fixed sleep.
	base := "http://" + addr
	{
		client := http.Client{Timeout: 500 * time.Millisecond}
		deadline := time.Now().Add(5 * time.Second)
		for {
			if time.Now().After(deadline) {
				t.Fatalf("server did not become ready within timeout")
			}
			resp, err := client.Get(base + "/api/devices")
			if err == nil {
				resp.Body.Close()
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

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

	// Send command confirmation (Return=0) — triggers WithOnCommandResult callback (OK path).
	cmdResult := "ID=1&Return=0&CMD=INFO"
	resp, err = http.Post(base+"/iclock/devicecmd?SN=DEV001",
		"text/plain", strings.NewReader(cmdResult))
	if err != nil {
		t.Fatalf("POST devicecmd (OK) failed: %v", err)
	}
	resp.Body.Close()

	// Send command confirmation (Return=-1002) — triggers WithOnCommandResult FAIL path.
	cmdResultFail := "ID=2&Return=-1002&CMD=BAD"
	resp, err = http.Post(base+"/iclock/devicecmd?SN=DEV001",
		"text/plain", strings.NewReader(cmdResultFail))
	if err != nil {
		t.Fatalf("POST devicecmd (FAIL) failed: %v", err)
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

// TestRun_InvalidDevice exercises the RegisterDevice error path in run()
// when an invalid serial number is passed.
func TestRun_InvalidDevice(t *testing.T) {
	err := run(t.Context(), ":0", []string{"INVALID DEVICE!"})
	if err == nil {
		t.Fatal("expected error for invalid device serial number")
	}
	if !strings.Contains(err.Error(), "register device") {
		t.Errorf("expected 'register device' in error, got: %v", err)
	}
}

// ---------- writeJSON encode error ----------

// errWriter is an http.ResponseWriter whose Write always returns an error,
// exercising the json.Encode error path in writeJSON.
type errWriter struct {
	header http.Header
	status int
}

func (w *errWriter) Header() http.Header  { return w.header }
func (w *errWriter) WriteHeader(code int) { w.status = code }
func (w *errWriter) Write([]byte) (int, error) {
	return 0, fmt.Errorf("simulated write error")
}

func TestWriteJSON_EncodeError(t *testing.T) {
	// Suppress the slog.Warn that writeJSON emits on encode failure.
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	w := &errWriter{header: make(http.Header)}
	writeJSON(w, http.StatusOK, map[string]string{"hello": "world"})

	// writeJSON should not panic; it logs the error and returns silently.
	if w.status != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.status)
	}
}

// ---------- requireDevice empty serial number ----------

func TestRequireDevice_EmptySN(t *testing.T) {
	server := newTestServer(t, "DEV001")

	// Build a request without routing through the mux so that
	// r.PathValue("sn") returns "".
	req := httptest.NewRequest(http.MethodPost, "/api/devices//reboot", nil)
	w := httptest.NewRecorder()

	sn, ok := requireDevice(w, req, server)
	if ok {
		t.Fatal("expected requireDevice to return false for empty SN")
	}
	if sn != "" {
		t.Errorf("expected empty SN, got %q", sn)
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

package zkadms

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// errReader is an io.Reader that always returns an error.
type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// errResponseWriter is an http.ResponseWriter whose Write always returns an error.
// It records the status code passed to WriteHeader for assertions.
type errResponseWriter struct {
	header http.Header
	status int
}

func newErrResponseWriter() *errResponseWriter {
	return &errResponseWriter{header: make(http.Header)}
}

func (w *errResponseWriter) Header() http.Header       { return w.header }
func (w *errResponseWriter) WriteHeader(code int)      { w.status = code }
func (w *errResponseWriter) Write([]byte) (int, error) { return 0, errors.New("simulated write error") }

// errOnNthWriteResponseWriter succeeds for the first (n-1) Write calls and
// returns an error on the nth call. This lets tests exercise the trailing-
// newline write-error path in HandleInspect.
type errOnNthWriteResponseWriter struct {
	header http.Header
	status int
	calls  int
	failOn int // 1-based: fail on the Nth Write call
}

func newErrOnNthWriteResponseWriter(n int) *errOnNthWriteResponseWriter {
	return &errOnNthWriteResponseWriter{header: make(http.Header), failOn: n}
}

func (w *errOnNthWriteResponseWriter) Header() http.Header  { return w.header }
func (w *errOnNthWriteResponseWriter) WriteHeader(code int) { w.status = code }
func (w *errOnNthWriteResponseWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls >= w.failOn {
		return 0, errors.New("simulated write error on call " + fmt.Sprintf("%d", w.calls))
	}
	return len(p), nil
}

func TestHandleCData_MissingSN(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "/iclock/cdata", nil)
	w := httptest.NewRecorder()

	server.HandleCData(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Missing SN parameter") {
		t.Errorf("Expected error message about missing SN, got: %s", w.Body.String())
	}
}

func TestHandleCData_AttendanceLog(t *testing.T) {
	received := make(chan AttendanceRecord, 1)
	server := NewADMSServer(
		WithOnAttendance(func(_ context.Context, record AttendanceRecord) {
			received <- record
		}),
	)
	defer server.Close()

	// Simulate attendance data
	attendanceData := "123\t2024-01-01 08:00:00\t0\t1\t0"
	req := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN=TEST001&table=ATTLOG", bytes.NewBufferString(attendanceData))
	w := httptest.NewRecorder()

	server.HandleCData(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	select {
	case record := <-received:
		if record.UserID != "123" {
			t.Errorf("Expected UserID 123, got %s", record.UserID)
		}
		if record.Status != 0 {
			t.Errorf("Expected Status 0, got %d", record.Status)
		}
		if record.VerifyMode != 1 {
			t.Errorf("Expected VerifyMode 1, got %d", record.VerifyMode)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Timed out waiting for attendance callback")
	}
}

func TestHandleCData_MultipleAttendanceRecords(t *testing.T) {
	received := make(chan AttendanceRecord, 2)
	server := NewADMSServer(
		WithOnAttendance(func(_ context.Context, record AttendanceRecord) {
			received <- record
		}),
	)
	defer server.Close()

	// Multiple records
	attendanceData := "123\t2024-01-01 08:00:00\t0\t1\t0\n456\t2024-01-01 17:00:00\t1\t1\t0"
	req := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN=TEST001&table=ATTLOG", bytes.NewBufferString(attendanceData))
	w := httptest.NewRecorder()

	server.HandleCData(w, req)

	for i := range 2 {
		select {
		case <-received:
		case <-time.After(2 * time.Second):
			t.Fatalf("Timed out waiting for record %d", i+1)
		}
	}
}

func TestHandleCData_OperationLog(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	req := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN=TEST001&table=OPERLOG", bytes.NewBufferString("operation data"))
	w := httptest.NewRecorder()

	server.HandleCData(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "OK") {
		t.Errorf("Expected OK response, got: %s", w.Body.String())
	}
}

func TestHandleCData_WithPendingCommands(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()
	serialNumber := "TEST001"

	if err := server.RegisterDevice(serialNumber); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	if _, err := server.QueueCommand(serialNumber, "INFO"); err != nil {
		t.Fatalf("QueueCommand failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN="+serialNumber, nil)
	w := httptest.NewRecorder()

	server.HandleCData(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}
	if want := "C:1:INFO\n"; w.Body.String() != want {
		t.Errorf("Expected response %q, got: %q", want, w.Body.String())
	}
}

func TestHandleGetRequest(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()
	serialNumber := "TEST001"

	if err := server.RegisterDevice(serialNumber); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	if _, err := server.QueueCommand(serialNumber, "DATA DELETE USERINFO PIN=1001"); err != nil {
		t.Fatalf("QueueCommand failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/iclock/getrequest?SN="+serialNumber, nil)
	w := httptest.NewRecorder()

	server.HandleGetRequest(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}
	if want := "C:1:DATA DELETE USERINFO PIN=1001\n"; w.Body.String() != want {
		t.Errorf("Expected response %q, got: %q", want, w.Body.String())
	}
}

func TestHandleGetRequest_NoCommands(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "/iclock/getrequest?SN=TEST001", nil)
	w := httptest.NewRecorder()

	server.HandleGetRequest(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}
	if w.Body.String() != "OK" {
		t.Errorf("Expected OK response when no commands, got: %s", w.Body.String())
	}
}

func TestHandleDeviceCmd(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	req := httptest.NewRequest(http.MethodPost, "/iclock/devicecmd?SN=TEST001", bytes.NewBufferString("command result"))
	w := httptest.NewRecorder()

	server.HandleDeviceCmd(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "OK") {
		t.Errorf("Expected OK in response, got: %s", w.Body.String())
	}
}

func TestHandleDeviceCmd_RegistersDevice(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	// Device was never registered — HandleDeviceCmd should register it.
	req := httptest.NewRequest(http.MethodPost, "/iclock/devicecmd?SN=NEW001", bytes.NewBufferString("result"))
	w := httptest.NewRecorder()

	server.HandleDeviceCmd(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}
	device := server.GetDevice("NEW001")
	if device == nil {
		t.Fatal("Expected HandleDeviceCmd to register the device, but GetDevice returned nil")
	}
	if device.SerialNumber != "NEW001" {
		t.Errorf("Expected serial NEW001, got %s", device.SerialNumber)
	}
}

func TestServeHTTP(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	testCases := []struct {
		name     string
		method   string
		path     string
		wantCode int
	}{
		{"cdata GET", "GET", "/iclock/cdata?SN=TEST001", http.StatusOK},
		{"cdata POST", "POST", "/iclock/cdata?SN=TEST001", http.StatusOK},
		{"getrequest GET", "GET", "/iclock/getrequest?SN=TEST001", http.StatusOK},
		{"devicecmd POST", "POST", "/iclock/devicecmd?SN=TEST001", http.StatusOK},
		{"registry GET", "GET", "/iclock/registry?SN=TEST001", http.StatusOK},
		{"registry POST", "POST", "/iclock/registry?SN=TEST001", http.StatusOK},
		{"inspect GET disabled by default", "GET", "/iclock/inspect", http.StatusNotFound},
		{"unknown endpoint", "GET", "/iclock/unknown", http.StatusNotFound},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			w := httptest.NewRecorder()

			server.ServeHTTP(w, req)

			if w.Code != tc.wantCode {
				t.Errorf("Expected status %d, got %d", tc.wantCode, w.Code)
			}
		})
	}
}

func TestMethodNotAllowed(t *testing.T) {
	server := NewADMSServer(WithEnableInspect())
	defer server.Close()

	testCases := []struct {
		name   string
		method string
		path   string
	}{
		{"DELETE on cdata", "DELETE", "/iclock/cdata?SN=TEST001"},
		{"PUT on cdata", "PUT", "/iclock/cdata?SN=TEST001"},
		{"POST on getrequest", "POST", "/iclock/getrequest?SN=TEST001"},
		{"GET on devicecmd", "GET", "/iclock/devicecmd?SN=TEST001"},
		{"DELETE on registry", "DELETE", "/iclock/registry?SN=TEST001"},
		{"POST on inspect", "POST", "/iclock/inspect"},
		{"PUT on inspect", "PUT", "/iclock/inspect"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			w := httptest.NewRecorder()

			server.ServeHTTP(w, req)

			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("Expected 405, got %d", w.Code)
			}
		})
	}
}

func TestHandleRegistry_PostParsesBody(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()
	body := "DeviceType=acc,~DeviceName=SpeedFace,IPAddress=192.168.1.201"
	req := httptest.NewRequest(http.MethodPost, "/iclock/registry?SN=REG001", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	server.HandleRegistry(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	dev := server.GetDevice("REG001")
	if dev == nil {
		t.Fatal("device should be registered")
	}
	if dev.Options["DeviceType"] != "acc" {
		t.Errorf("expected DeviceType=acc, got %s", dev.Options["DeviceType"])
	}
	if dev.Options["DeviceName"] != "SpeedFace" {
		t.Errorf("expected DeviceName=SpeedFace, got %s", dev.Options["DeviceName"])
	}
	if dev.Options["IPAddress"] != "192.168.1.201" {
		t.Errorf("expected IPAddress parsed")
	}
}

func TestHandleRegistry_Callback(t *testing.T) {
	received := make(chan map[string]string, 1)
	server := NewADMSServer(
		WithOnRegistry(func(_ context.Context, sn string, info map[string]string) {
			received <- info
		}),
	)
	defer server.Close()

	body := "DeviceType=acc"
	req := httptest.NewRequest(http.MethodPost, "/iclock/registry?SN=REG002", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	server.HandleRegistry(w, req)

	select {
	case info := <-received:
		if info["DeviceType"] != "acc" {
			t.Errorf("expected DeviceType=acc, got %s", info["DeviceType"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Timed out waiting for registry callback")
	}
}

func TestHandleInspect_JSON(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()
	if err := server.RegisterDevice("A1"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}
	// Mark old activity to test offline
	server.devicesMutex.Lock()
	server.devices["A1"].LastActivity = time.Now().Add(-3 * time.Minute)
	server.devicesMutex.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/iclock/inspect", nil)
	w := httptest.NewRecorder()
	server.HandleInspect(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("expected JSON content-type, got %s", ct)
	}
	var payload struct {
		Devices []DeviceSnapshot `json:"devices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if len(payload.Devices) == 0 {
		t.Fatalf("expected at least one device")
	}
	if payload.Devices[0].Online {
		t.Errorf("expected device to be offline due to stale activity")
	}
}

func TestReadBody_ExceedsLimit(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	server.maxBodySize = 10 // 10 bytes limit
	if err := server.RegisterDevice("TEST001"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	// Send a body larger than the limit via the ATTLOG handler.
	largeBody := strings.Repeat("x", 20)
	req := httptest.NewRequest(http.MethodPost,
		"/iclock/cdata?SN=TEST001&table=ATTLOG", strings.NewReader(largeBody))
	w := httptest.NewRecorder()
	server.HandleCData(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected status %d for oversized body, got %d", http.StatusRequestEntityTooLarge, w.Code)
	}
	if !strings.Contains(w.Body.String(), "Request body too large") {
		t.Errorf("expected 'Request body too large' in response, got %q", w.Body.String())
	}
}

func TestReadBody_WithinLimit(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	server.maxBodySize = 1024 // 1 KB limit
	if err := server.RegisterDevice("TEST001"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	body := "123\t2024-01-01 08:00:00\t0\t1\t0"
	req := httptest.NewRequest(http.MethodPost,
		"/iclock/cdata?SN=TEST001&table=ATTLOG", strings.NewReader(body))
	w := httptest.NewRecorder()
	server.HandleCData(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d for body within limit, got %d", http.StatusOK, w.Code)
	}
	if !strings.Contains(w.Body.String(), "OK") {
		t.Errorf("expected 'OK' in response, got %q", w.Body.String())
	}
}

func TestReadBody_ExceedsLimit_DeviceCmd(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	server.maxBodySize = 5
	if err := server.RegisterDevice("TEST001"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost,
		"/iclock/devicecmd?SN=TEST001", strings.NewReader("this body exceeds the limit"))
	w := httptest.NewRecorder()
	server.HandleDeviceCmd(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected status %d, got %d", http.StatusRequestEntityTooLarge, w.Code)
	}
}

func TestReadBody_ExceedsLimit_Registry(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	server.maxBodySize = 5
	if err := server.RegisterDevice("TEST001"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost,
		"/iclock/registry?SN=TEST001", strings.NewReader("DeviceType=acc,DeviceName=SpeedFace"))
	w := httptest.NewRecorder()
	server.HandleRegistry(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected status %d, got %d", http.StatusRequestEntityTooLarge, w.Code)
	}
}

func TestHandler_RejectsInvalidSN(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	// All handlers should reject requests with invalid serial numbers.
	handlers := []struct {
		name   string
		method string
		path   string
	}{
		{"cdata GET", "GET", "/iclock/cdata?SN=BAD%20SN"},
		{"cdata POST", "POST", "/iclock/cdata?SN=BAD%20SN"},
		{"getrequest", "GET", "/iclock/getrequest?SN=BAD%20SN"},
		{"devicecmd", "POST", "/iclock/devicecmd?SN=BAD%20SN"},
		{"registry GET", "GET", "/iclock/registry?SN=BAD%20SN"},
		{"registry POST", "POST", "/iclock/registry?SN=BAD%20SN"},
	}

	for _, tc := range handlers {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			w := httptest.NewRecorder()
			server.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400 for invalid SN, got %d", w.Code)
			}
		})
	}
}

func TestHandler_RejectsMissingSN(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	// Handlers should reject requests without SN parameter.
	handlers := []struct {
		name   string
		method string
		path   string
	}{
		{"cdata GET", "GET", "/iclock/cdata"},
		{"cdata POST", "POST", "/iclock/cdata"},
		{"getrequest", "GET", "/iclock/getrequest"},
		{"devicecmd", "POST", "/iclock/devicecmd"},
		{"registry GET", "GET", "/iclock/registry"},
	}

	for _, tc := range handlers {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			w := httptest.NewRecorder()
			server.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400 for missing SN, got %d", w.Code)
			}
		})
	}
}

func TestHandler_RejectsWhenDeviceLimitReached(t *testing.T) {
	server := NewADMSServer(WithMaxDevices(1))
	defer server.Close()

	// Register one device via direct call.
	if err := server.RegisterDevice("DEV1"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	// HTTP request with a new SN should be rejected with 503.
	req := httptest.NewRequest(http.MethodGet, "/iclock/cdata?SN=DEV2", nil)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when device limit reached, got %d", w.Code)
	}

	// But existing device should still work.
	req = httptest.NewRequest(http.MethodGet, "/iclock/cdata?SN=DEV1", nil)
	w = httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for existing device, got %d", w.Code)
	}
}

func TestHandleDeviceCmd_Confirmation(t *testing.T) {
	received := make(chan CommandResult, 1)
	server := NewADMSServer(
		WithOnCommandResult(func(_ context.Context, result CommandResult) {
			received <- result
		}),
	)
	defer server.Close()

	body := "ID=42&Return=0&CMD=DATA"
	req := httptest.NewRequest(http.MethodPost, "/iclock/devicecmd?SN=CONF001", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	server.HandleDeviceCmd(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "OK" {
		t.Errorf("expected OK response, got %q", w.Body.String())
	}

	select {
	case result := <-received:
		if result.SerialNumber != "CONF001" {
			t.Errorf("expected SerialNumber CONF001, got %s", result.SerialNumber)
		}
		if result.ID != 42 {
			t.Errorf("expected ID 42, got %d", result.ID)
		}
		if result.ReturnCode != 0 {
			t.Errorf("expected ReturnCode 0, got %d", result.ReturnCode)
		}
		if result.Command != "DATA" {
			t.Errorf("expected Command DATA, got %s", result.Command)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Timed out waiting for command result callback")
	}
}

func TestHandleDeviceCmd_ConfirmationWithError(t *testing.T) {
	received := make(chan CommandResult, 1)
	server := NewADMSServer(
		WithOnCommandResult(func(_ context.Context, result CommandResult) {
			received <- result
		}),
	)
	defer server.Close()

	body := "ID=5&Return=1&CMD=REBOOT"
	req := httptest.NewRequest(http.MethodPost, "/iclock/devicecmd?SN=ERR001", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	server.HandleDeviceCmd(w, req)

	select {
	case result := <-received:
		if result.ReturnCode != 1 {
			t.Errorf("expected ReturnCode 1 (error), got %d", result.ReturnCode)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Timed out waiting for command result callback")
	}
}

func TestHandleDeviceCmd_BatchedConfirmations(t *testing.T) {
	received := make(chan CommandResult, 5)
	server := NewADMSServer(
		WithOnCommandResult(func(_ context.Context, result CommandResult) {
			received <- result
		}),
	)
	defer server.Close()

	// Device batches 3 confirmations in a single POST (real device behavior).
	body := "ID=1&Return=0&CMD=INFO\nID=2&Return=0&CMD=CHECK\nID=3&Return=-1002&CMD=DATA\n"
	req := httptest.NewRequest(http.MethodPost, "/iclock/devicecmd?SN=BATCH001", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	server.HandleDeviceCmd(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Should receive all 3 results.
	var results []CommandResult
	for range 3 {
		select {
		case r := <-received:
			results = append(results, r)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out; only received %d/3 results", len(results))
		}
	}

	if results[0].ID != 1 || results[0].ReturnCode != 0 || results[0].Command != "INFO" {
		t.Errorf("result[0] = %+v", results[0])
	}
	if results[1].ID != 2 || results[1].ReturnCode != 0 || results[1].Command != "CHECK" {
		t.Errorf("result[1] = %+v", results[1])
	}
	if results[2].ID != 3 || results[2].ReturnCode != -1002 || results[2].Command != "DATA" {
		t.Errorf("result[2] = %+v", results[2])
	}
	for _, r := range results {
		if r.SerialNumber != "BATCH001" {
			t.Errorf("expected SerialNumber BATCH001, got %s", r.SerialNumber)
		}
	}
}

func TestHandleDeviceCmd_NoCallbackConfigured(t *testing.T) {
	// No WithOnCommandResult — should still respond OK without panicking.
	server := NewADMSServer()
	defer server.Close()

	body := "ID=1&Return=0&CMD=INFO"
	req := httptest.NewRequest(http.MethodPost, "/iclock/devicecmd?SN=NOCB001", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	server.HandleDeviceCmd(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "OK" {
		t.Errorf("expected OK response, got %q", w.Body.String())
	}
}

func TestReadBody_MaxBytesError(t *testing.T) {
	server := NewADMSServer(WithMaxBodySize(10))
	defer server.Close()

	body := strings.Repeat("x", 100) // well over 10 bytes
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	w := httptest.NewRecorder()

	_, err := server.readBody(w, req)
	if err == nil {
		t.Fatal("expected error for oversized body")
	}
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413, got %d", w.Code)
	}
}

func TestReadBody_GenericReadError(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	req := httptest.NewRequest(http.MethodPost, "/", io.NopCloser(errReader{err: errors.New("disk failure")}))
	w := httptest.NewRecorder()

	_, err := server.readBody(w, req)
	if err == nil {
		t.Fatal("expected error for read failure")
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestRegisterOrReject_InvalidSerialNumber(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	w := httptest.NewRecorder()
	ok := server.registerOrReject(w, "!!!invalid!!!")
	if ok {
		t.Error("expected registerOrReject to return false for invalid SN")
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestRegisterOrReject_DeviceLimitReached(t *testing.T) {
	server := NewADMSServer(WithMaxDevices(1))
	defer server.Close()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	w := httptest.NewRecorder()
	ok := server.registerOrReject(w, "DEV002")
	if ok {
		t.Error("expected registerOrReject to return false when limit reached")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestHandleCData_ATTLOG_BodyTooLarge(t *testing.T) {
	server := NewADMSServer(WithMaxBodySize(10))
	defer server.Close()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}

	body := strings.Repeat("x", 100)
	req := httptest.NewRequest(http.MethodPost,
		"/iclock/cdata?SN=DEV001&table=ATTLOG", strings.NewReader(body))
	w := httptest.NewRecorder()
	server.HandleCData(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413, got %d", w.Code)
	}
}

func TestHandleCData_ATTLOG_CallbackQueueFull(t *testing.T) {
	blocker := make(chan struct{})
	server := NewADMSServer(
		WithCallbackBufferSize(1),
		WithDispatchTimeout(10*time.Millisecond),
		WithOnAttendance(func(_ context.Context, _ AttendanceRecord) {
			<-blocker
		}),
	)
	defer func() {
		close(blocker)
		server.Close()
	}()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}

	attlog := "1\t2025-01-01 08:00:00\t1\t0\t0\t0"

	// Fill the callback channel.
	req1 := httptest.NewRequest(http.MethodPost,
		"/iclock/cdata?SN=DEV001&table=ATTLOG", strings.NewReader(attlog))
	w1 := httptest.NewRecorder()
	server.HandleCData(w1, req1)

	time.Sleep(50 * time.Millisecond) // let worker pick up and block

	// Fill remaining channel capacity.
	req2 := httptest.NewRequest(http.MethodPost,
		"/iclock/cdata?SN=DEV001&table=ATTLOG", strings.NewReader(attlog))
	w2 := httptest.NewRecorder()
	server.HandleCData(w2, req2)

	// Now queue should be full — next should fail.
	req3 := httptest.NewRequest(http.MethodPost,
		"/iclock/cdata?SN=DEV001&table=ATTLOG", strings.NewReader(attlog))
	w3 := httptest.NewRecorder()
	server.HandleCData(w3, req3)

	if w3.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when callback queue is full, got %d", w3.Code)
	}
}

func TestHandleCData_DefaultTable_DeviceInfoQueueFull(t *testing.T) {
	blocker := make(chan struct{})
	server := NewADMSServer(
		WithCallbackBufferSize(1),
		WithDispatchTimeout(10*time.Millisecond),
		WithOnDeviceInfo(func(_ context.Context, _ string, _ map[string]string) {
			<-blocker
		}),
	)
	defer func() {
		close(blocker)
		server.Close()
	}()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}

	infoBody := "~DeviceName=TestDev\nMAC=00:11:22:33:44:55"

	// First dispatch — the worker picks it up and blocks.
	req1 := httptest.NewRequest(http.MethodPost,
		"/iclock/cdata?SN=DEV001", strings.NewReader(infoBody))
	w1 := httptest.NewRecorder()
	server.HandleCData(w1, req1)

	time.Sleep(50 * time.Millisecond)

	// Fill remaining buffer capacity.
	req2 := httptest.NewRequest(http.MethodPost,
		"/iclock/cdata?SN=DEV001", strings.NewReader(infoBody))
	w2 := httptest.NewRecorder()
	server.HandleCData(w2, req2)

	// This dispatch should fail (queue full) and HandleCData returns 503
	// to signal backpressure to the device.
	req3 := httptest.NewRequest(http.MethodPost,
		"/iclock/cdata?SN=DEV001", strings.NewReader(infoBody))
	w3 := httptest.NewRecorder()
	server.HandleCData(w3, req3)

	if w3.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 (device info queue-full), got %d", w3.Code)
	}
}

func TestHandleCData_DefaultTable_BodyTooLarge(t *testing.T) {
	server := NewADMSServer(WithMaxBodySize(10))
	defer server.Close()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}

	body := strings.Repeat("x", 100)
	// POST to default table (no table param) triggers the info/command path.
	req := httptest.NewRequest(http.MethodPost,
		"/iclock/cdata?SN=DEV001", strings.NewReader(body))
	w := httptest.NewRecorder()
	server.HandleCData(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413, got %d", w.Code)
	}
}

func TestHandleDeviceCmd_BodyTooLarge(t *testing.T) {
	server := NewADMSServer(WithMaxBodySize(10))
	defer server.Close()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}

	body := strings.Repeat("x", 100)
	req := httptest.NewRequest(http.MethodPost,
		"/iclock/devicecmd?SN=DEV001", strings.NewReader(body))
	w := httptest.NewRecorder()
	server.HandleDeviceCmd(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413, got %d", w.Code)
	}
}

func TestHandleDeviceCmd_CallbackQueueFull(t *testing.T) {
	blocker := make(chan struct{})
	server := NewADMSServer(
		WithCallbackBufferSize(1),
		WithDispatchTimeout(10*time.Millisecond),
		WithOnCommandResult(func(_ context.Context, _ CommandResult) {
			<-blocker
		}),
	)
	defer func() {
		close(blocker)
		server.Close()
	}()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}

	body := "ID=1&Return=0&CMD=INFO"

	// First call — worker picks up and blocks.
	req1 := httptest.NewRequest(http.MethodPost,
		"/iclock/devicecmd?SN=DEV001", strings.NewReader(body))
	w1 := httptest.NewRecorder()
	server.HandleDeviceCmd(w1, req1)

	time.Sleep(50 * time.Millisecond)

	// Fill remaining capacity.
	req2 := httptest.NewRequest(http.MethodPost,
		"/iclock/devicecmd?SN=DEV001", strings.NewReader(body))
	w2 := httptest.NewRecorder()
	server.HandleDeviceCmd(w2, req2)

	// Next dispatch should fail (queue full) — handler returns 503.
	req3 := httptest.NewRequest(http.MethodPost,
		"/iclock/devicecmd?SN=DEV001", strings.NewReader(body))
	w3 := httptest.NewRecorder()
	server.HandleDeviceCmd(w3, req3)

	// HandleDeviceCmd returns 503 when the callback queue is full.
	if w3.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w3.Code)
	}
}

func TestHandleDeviceCmd_MethodNotAllowed(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "/iclock/devicecmd?SN=DEV001", nil)
	w := httptest.NewRecorder()
	server.HandleDeviceCmd(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestHandleDeviceCmd_MissingSN(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	req := httptest.NewRequest(http.MethodPost, "/iclock/devicecmd", strings.NewReader("ID=1&Return=0&CMD=INFO"))
	w := httptest.NewRecorder()
	server.HandleDeviceCmd(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing SN, got %d", w.Code)
	}
}

func TestHandleDeviceCmd_EmptyBody(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost,
		"/iclock/devicecmd?SN=DEV001", strings.NewReader(""))
	w := httptest.NewRecorder()
	server.HandleDeviceCmd(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "OK" {
		t.Errorf("expected OK, got %q", w.Body.String())
	}
}

func TestHandleRegistry_BodyTooLarge(t *testing.T) {
	server := NewADMSServer(WithMaxBodySize(10))
	defer server.Close()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}

	body := strings.Repeat("x", 100)
	req := httptest.NewRequest(http.MethodPost,
		"/iclock/registry?SN=DEV001", strings.NewReader(body))
	w := httptest.NewRecorder()
	server.HandleRegistry(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413, got %d", w.Code)
	}
}

func TestHandleRegistry_CallbackQueueFull(t *testing.T) {
	blocker := make(chan struct{})
	server := NewADMSServer(
		WithCallbackBufferSize(1),
		WithDispatchTimeout(10*time.Millisecond),
		WithOnRegistry(func(_ context.Context, _ string, _ map[string]string) {
			<-blocker
		}),
	)
	defer func() {
		close(blocker)
		server.Close()
	}()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}

	regBody := "~DeviceName=TestDev,MAC=00:11:22:33:44:55"

	// Fill the callback channel.
	req1 := httptest.NewRequest(http.MethodPost,
		"/iclock/registry?SN=DEV001", strings.NewReader(regBody))
	w1 := httptest.NewRecorder()
	server.HandleRegistry(w1, req1)

	time.Sleep(50 * time.Millisecond)

	req2 := httptest.NewRequest(http.MethodPost,
		"/iclock/registry?SN=DEV001", strings.NewReader(regBody))
	w2 := httptest.NewRecorder()
	server.HandleRegistry(w2, req2)

	// Should return 503 (registry queue-full signals backpressure).
	req3 := httptest.NewRequest(http.MethodPost,
		"/iclock/registry?SN=DEV001", strings.NewReader(regBody))
	w3 := httptest.NewRecorder()
	server.HandleRegistry(w3, req3)

	if w3.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w3.Code)
	}
}

func TestHandleRegistry_MethodNotAllowed(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	req := httptest.NewRequest(http.MethodDelete, "/iclock/registry?SN=DEV001", nil)
	w := httptest.NewRecorder()
	server.HandleRegistry(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestHandleRegistry_MissingSN(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "/iclock/registry", nil)
	w := httptest.NewRecorder()
	server.HandleRegistry(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing SN, got %d", w.Code)
	}
}

func TestHandleRegistry_EmptyBody(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet,
		"/iclock/registry?SN=DEV001", nil)
	w := httptest.NewRecorder()
	server.HandleRegistry(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "OK" {
		t.Errorf("expected OK, got %q", w.Body.String())
	}
}

func TestHandleInspect_MethodNotAllowed(t *testing.T) {
	server := NewADMSServer(WithEnableInspect())
	defer server.Close()

	req := httptest.NewRequest(http.MethodPost, "/iclock/inspect", nil)
	w := httptest.NewRecorder()
	server.HandleInspect(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestHandleInspect_WithDevices(t *testing.T) {
	server := NewADMSServer(WithEnableInspect())
	defer server.Close()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}
	// Touch activity so the device appears online.
	server.updateDeviceActivity("DEV001")

	req := httptest.NewRequest(http.MethodGet, "/iclock/inspect", nil)
	w := httptest.NewRecorder()
	server.HandleInspect(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var snapshot struct {
		Devices []DeviceSnapshot `json:"devices"`
		Count   int              `json:"count"`
		Time    string           `json:"time"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("failed to decode inspect response: %v", err)
	}
	if snapshot.Count != 1 {
		t.Errorf("expected 1 device, got %d", snapshot.Count)
	}
	if len(snapshot.Devices) != 1 {
		t.Fatalf("expected 1 device in array, got %d", len(snapshot.Devices))
	}
	if snapshot.Devices[0].Serial != "DEV001" {
		t.Errorf("expected serial DEV001, got %q", snapshot.Devices[0].Serial)
	}
	if !snapshot.Devices[0].Online {
		t.Error("expected device to be online after updateDeviceActivity")
	}
}

func TestHandleInspect_EmptyDeviceList(t *testing.T) {
	server := NewADMSServer(WithEnableInspect())
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "/iclock/inspect", nil)
	w := httptest.NewRecorder()
	server.HandleInspect(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var snapshot struct {
		Devices []DeviceSnapshot `json:"devices"`
		Count   int              `json:"count"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if snapshot.Count != 0 {
		t.Errorf("expected 0 devices, got %d", snapshot.Count)
	}
	if snapshot.Devices == nil {
		t.Error("expected non-nil empty devices array")
	}
}

func TestHandleInspect_WriteError(t *testing.T) {
	server := NewADMSServer(WithEnableInspect())
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "/iclock/inspect", nil)
	w := newErrResponseWriter()
	server.HandleInspect(w, req)

	// The handler should not panic; the write error is logged and silently
	// absorbed. We just verify it didn't crash.
}

func TestHandleInspect_WriteError_WithDevices(t *testing.T) {
	server := NewADMSServer(WithEnableInspect())
	defer server.Close()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}
	server.updateDeviceActivity("DEV001")

	req := httptest.NewRequest(http.MethodGet, "/iclock/inspect", nil)
	w := newErrResponseWriter()
	server.HandleInspect(w, req)
}

func TestHandleInspect_TrailingNewlineWriteError(t *testing.T) {
	server := NewADMSServer(WithEnableInspect())
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "/iclock/inspect", nil)
	// First Write (JSON body) succeeds; second Write (trailing newline) fails.
	w := newErrOnNthWriteResponseWriter(2)
	server.HandleInspect(w, req)
}

func TestHandleCData_MethodNotAllowed(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	req := httptest.NewRequest(http.MethodDelete, "/iclock/cdata?SN=DEV001", nil)
	w := httptest.NewRecorder()
	server.HandleCData(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestHandleCData_DefaultTable_GET_WithPendingCommands(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}
	if _, err := server.QueueCommand("DEV001", "CHECK"); err != nil {
		t.Fatalf("QueueCommand: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/iclock/cdata?SN=DEV001", nil)
	w := httptest.NewRecorder()
	server.HandleCData(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "C:1:CHECK") {
		t.Errorf("expected response to contain C:1:CHECK, got %q", w.Body.String())
	}
}

func TestHandleCData_DefaultTable_GET_NoCommands(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/iclock/cdata?SN=DEV001", nil)
	w := httptest.NewRecorder()
	server.HandleCData(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "OK" {
		t.Errorf("expected OK, got %q", w.Body.String())
	}
}

func TestHandleGetRequest_MethodNotAllowed(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	req := httptest.NewRequest(http.MethodPost, "/iclock/getrequest?SN=DEV001", nil)
	w := httptest.NewRecorder()
	server.HandleGetRequest(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestHandleGetRequest_MissingSN(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "/iclock/getrequest", nil)
	w := httptest.NewRecorder()
	server.HandleGetRequest(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing SN, got %d", w.Code)
	}
}

func TestServeHTTP_UnknownPath(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "/iclock/unknown", nil)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown path, got %d", w.Code)
	}
}

func TestServeHTTP_InspectDisabled(t *testing.T) {
	// Inspect is disabled by default.
	server := NewADMSServer()
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "/iclock/inspect", nil)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 when inspect disabled, got %d", w.Code)
	}
}

func TestServeHTTP_InspectEnabled(t *testing.T) {
	server := NewADMSServer(WithEnableInspect())
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "/iclock/inspect", nil)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 when inspect enabled, got %d", w.Code)
	}
}

func TestRequireDevice_InvalidSN(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "/iclock/cdata?SN=!!!", nil)
	w := httptest.NewRecorder()
	sn, ok := server.requireDevice(w, req)
	if ok {
		t.Error("expected requireDevice to return false for invalid SN")
	}
	if sn != "" {
		t.Errorf("expected empty SN, got %q", sn)
	}
}

func TestWriteCommandsOrOK_MultipleCommands(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}

	for _, cmd := range []string{"INFO", "CHECK", "REBOOT"} {
		if _, err := server.QueueCommand("DEV001", cmd); err != nil {
			t.Fatalf("QueueCommand(%q): %v", cmd, err)
		}
	}

	w := httptest.NewRecorder()
	server.writeCommandsOrOK(w, "DEV001")

	body := w.Body.String()
	if !strings.Contains(body, "C:1:INFO\n") {
		t.Errorf("expected C:1:INFO in response, got %q", body)
	}
	if !strings.Contains(body, "C:2:CHECK\n") {
		t.Errorf("expected C:2:CHECK in response, got %q", body)
	}
	if !strings.Contains(body, "C:3:REBOOT\n") {
		t.Errorf("expected C:3:REBOOT in response, got %q", body)
	}
}

func TestHandleCData_BatchDispatch(t *testing.T) {
	var mu sync.Mutex
	var received []AttendanceRecord
	done := make(chan struct{})

	server := NewADMSServer(
		WithOnAttendance(func(_ context.Context, record AttendanceRecord) {
			mu.Lock()
			received = append(received, record)
			if len(received) == 3 {
				close(done)
			}
			mu.Unlock()
		}),
	)
	defer server.Close()

	// Send 3 records in a single POST — they should arrive via a single batch dispatch.
	data := "100\t2024-01-01 08:00:00\t0\t1\t0\n200\t2024-01-01 09:00:00\t1\t1\t0\n300\t2024-01-01 10:00:00\t0\t2\t0"
	req := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN=BATCH001&table=ATTLOG", bytes.NewBufferString(data))
	w := httptest.NewRecorder()

	server.HandleCData(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "OK: 3") {
		t.Errorf("Expected 'OK: 3', got %s", w.Body.String())
	}

	select {
	case <-done:
		mu.Lock()
		defer mu.Unlock()
		if len(received) != 3 {
			t.Fatalf("Expected 3 records, got %d", len(received))
		}
		if received[0].UserID != "100" || received[1].UserID != "200" || received[2].UserID != "300" {
			t.Errorf("Records arrived in wrong order: %v", received)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Timed out waiting for batched attendance records")
	}
}

func TestHandleCData_QueueFull_Returns503(t *testing.T) {
	server := NewADMSServer(
		WithOnAttendance(func(_ context.Context, record AttendanceRecord) {}),
	)
	defer server.Close()

	// Block the worker with a long-running callback so it can't drain the queue.
	workerBlocked := make(chan struct{})
	workerRelease := make(chan struct{})
	server.callbackCh <- func() {
		close(workerBlocked)
		<-workerRelease
	}

	// Wait until the worker is actually blocked.
	<-workerBlocked

	// Now fill the remaining channel capacity. Worker is stuck, so nothing drains.
	for range cap(server.callbackCh) {
		server.callbackCh <- func() {}
	}

	// Queue is full and worker is blocked — dispatch should fail after ~1s.
	data := "999\t2024-01-01 08:00:00\t0\t1\t0"
	req := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN=FULL001&table=ATTLOG", bytes.NewBufferString(data))
	w := httptest.NewRecorder()

	server.HandleCData(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("Expected 503, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "FAIL") {
		t.Errorf("Expected FAIL in response body, got: %s", w.Body.String())
	}

	// Unblock the worker so Close() can drain.
	close(workerRelease)
}

func TestHandleCData_QueueFull_DeviceRetries(t *testing.T) {
	received := make(chan AttendanceRecord, 1)
	server := NewADMSServer(
		WithOnAttendance(func(_ context.Context, record AttendanceRecord) {
			received <- record
		}),
	)

	// Block the worker so it can't drain the queue.
	workerBlocked := make(chan struct{})
	workerRelease := make(chan struct{})
	server.callbackCh <- func() {
		close(workerBlocked)
		<-workerRelease
	}
	<-workerBlocked

	// Fill the remaining channel capacity with no-ops. Worker is stuck.
	for range cap(server.callbackCh) {
		server.callbackCh <- func() {}
	}

	// First attempt: queue is full, should get 503.
	data := "888\t2024-01-01 08:00:00\t0\t1\t0"
	req1 := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN=RETRY001&table=ATTLOG", bytes.NewBufferString(data))
	w1 := httptest.NewRecorder()
	server.HandleCData(w1, req1)

	if w1.Code != http.StatusServiceUnavailable {
		t.Fatalf("Expected 503 on first attempt, got %d", w1.Code)
	}

	// Release the worker and drain the channel to simulate queue recovery.
	close(workerRelease)

	// Drain remaining no-ops non-blocking to avoid racing with the worker.
	draining := true
	for draining {
		select {
		case <-server.callbackCh:
		default:
			draining = false
		}
	}
	// Wait for the worker to finish processing and the queue to be fully empty.
	deadline := time.Now().Add(3 * time.Second)
	for len(server.callbackCh) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("Timed out waiting for callback queue to drain")
		}
		runtime.Gosched()
	}

	// Second attempt: queue has space, should succeed.
	req2 := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN=RETRY001&table=ATTLOG", bytes.NewBufferString(data))
	w2 := httptest.NewRecorder()
	server.HandleCData(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("Expected 200 on retry, got %d", w2.Code)
	}
	if !strings.Contains(w2.Body.String(), "OK: 1") {
		t.Errorf("Expected 'OK: 1', got %s", w2.Body.String())
	}

	select {
	case rec := <-received:
		if rec.UserID != "888" {
			t.Errorf("Expected UserID 888, got %s", rec.UserID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Timed out waiting for retried attendance callback")
	}

	server.Close()
}

func TestHandleCData_USERINFO(t *testing.T) {
	received := make(chan []UserRecord, 1)
	server := NewADMSServer(
		WithOnQueryUsers(func(_ context.Context, sn string, users []UserRecord) {
			if sn != "UINFO001" {
				// Don't block the channel on unexpected serial numbers.
				return
			}
			received <- users
		}),
	)
	defer server.Close()

	userData := "PIN=100\tName=Charlie\tPrivilege=0\tCard=AABB\tPassword=pw1\nPIN=200\tName=Diana\tPrivilege=14\n"
	req := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN=UINFO001&table=USERINFO", bytes.NewBufferString(userData))
	w := httptest.NewRecorder()

	server.HandleCData(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "OK" {
		t.Errorf("expected OK response body, got %q", w.Body.String())
	}

	select {
	case users := <-received:
		if len(users) != 2 {
			t.Fatalf("expected 2 users, got %d", len(users))
		}
		if users[0].PIN != "100" || users[0].Name != "Charlie" || users[0].Card != "AABB" || users[0].Password != "pw1" {
			t.Errorf("unexpected first user: %+v", users[0])
		}
		if users[1].PIN != "200" || users[1].Name != "Diana" || users[1].Privilege != 14 {
			t.Errorf("unexpected second user: %+v", users[1])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for query users callback")
	}
}

func TestHandleCData_USERINFO_NoCallback(t *testing.T) {
	// No WithOnQueryUsers option — should still return 200 OK without error.
	server := NewADMSServer()
	defer server.Close()

	userData := "PIN=1\tName=Solo\n"
	req := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN=NOCB001&table=USERINFO", bytes.NewBufferString(userData))
	w := httptest.NewRecorder()

	server.HandleCData(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "OK" {
		t.Errorf("expected OK response body, got %q", w.Body.String())
	}
}

func TestHandleCData_USERINFO_BodyTooLarge(t *testing.T) {
	server := NewADMSServer(WithMaxBodySize(10))
	defer server.Close()

	body := strings.Repeat("x", 100)
	req := httptest.NewRequest(http.MethodPost,
		"/iclock/cdata?SN=DEV001&table=USERINFO", strings.NewReader(body))
	w := httptest.NewRecorder()
	server.HandleCData(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413, got %d", w.Code)
	}
}

func TestHandleCData_USERINFO_CallbackQueueFull(t *testing.T) {
	blocker := make(chan struct{})
	server := NewADMSServer(
		WithCallbackBufferSize(1),
		WithDispatchTimeout(10*time.Millisecond),
		WithOnQueryUsers(func(_ context.Context, _ string, _ []UserRecord) {
			<-blocker
		}),
	)
	defer func() {
		close(blocker)
		server.Close()
	}()

	userData := "PIN=1\tName=Alice\n"

	// Fill the callback channel.
	req1 := httptest.NewRequest(http.MethodPost,
		"/iclock/cdata?SN=DEV001&table=USERINFO", strings.NewReader(userData))
	w1 := httptest.NewRecorder()
	server.HandleCData(w1, req1)

	time.Sleep(50 * time.Millisecond) // let worker pick up and block

	// Fill remaining channel capacity.
	req2 := httptest.NewRequest(http.MethodPost,
		"/iclock/cdata?SN=DEV001&table=USERINFO", strings.NewReader(userData))
	w2 := httptest.NewRecorder()
	server.HandleCData(w2, req2)

	// Now queue should be full — dispatch will fail and handler returns 503.
	req3 := httptest.NewRequest(http.MethodPost,
		"/iclock/cdata?SN=DEV001&table=USERINFO", strings.NewReader(userData))
	w3 := httptest.NewRecorder()
	server.HandleCData(w3, req3)

	// USERINFO handler returns 503 when callback queue is full.
	if w3.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w3.Code)
	}
}

func TestHandleCData_AttendanceUsesDeviceTimezone(t *testing.T) {
	istanbul, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		t.Fatal(err)
	}

	received := make(chan AttendanceRecord, 1)
	server := NewADMSServer(
		WithOnAttendance(func(_ context.Context, rec AttendanceRecord) {
			received <- rec
		}),
	)
	defer server.Close()

	if err := server.RegisterDevice("DEV001", WithDeviceTimezone(istanbul)); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	data := "123\t2024-06-15 09:00:00\t0\t15\t0"
	req := httptest.NewRequest(http.MethodPost,
		"/iclock/cdata?SN=DEV001&table=ATTLOG", strings.NewReader(data))
	w := httptest.NewRecorder()
	server.HandleCData(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	select {
	case rec := <-received:
		// 09:00 Istanbul (UTC+3) = 06:00 UTC
		expectedUTC := time.Date(2024, 6, 15, 6, 0, 0, 0, time.UTC)
		if !rec.Timestamp.Equal(expectedUTC) {
			t.Errorf("expected %v, got %v", expectedUTC, rec.Timestamp.UTC())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for attendance callback")
	}
}

func TestHandleCData_AttendanceUsesServerDefaultTimezone(t *testing.T) {
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}

	received := make(chan AttendanceRecord, 1)
	server := NewADMSServer(
		WithDefaultTimezone(tokyo),
		WithOnAttendance(func(_ context.Context, rec AttendanceRecord) {
			received <- rec
		}),
	)
	defer server.Close()

	// Device registered without explicit timezone — should use server default.
	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	data := "123\t2024-06-15 09:00:00\t0\t15\t0"
	req := httptest.NewRequest(http.MethodPost,
		"/iclock/cdata?SN=DEV001&table=ATTLOG", strings.NewReader(data))
	w := httptest.NewRecorder()
	server.HandleCData(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	select {
	case rec := <-received:
		// 09:00 Tokyo (UTC+9) = 00:00 UTC
		expectedUTC := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)
		if !rec.Timestamp.Equal(expectedUTC) {
			t.Errorf("expected %v, got %v", expectedUTC, rec.Timestamp.UTC())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for attendance callback")
	}
}

func TestHandleCData_MultiDeviceDifferentTimezones(t *testing.T) {
	istanbul, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		t.Fatal(err)
	}
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}

	received := make(chan AttendanceRecord, 2)
	server := NewADMSServer(
		WithOnAttendance(func(_ context.Context, rec AttendanceRecord) {
			received <- rec
		}),
	)
	defer server.Close()

	if err := server.RegisterDevice("IST001", WithDeviceTimezone(istanbul)); err != nil {
		t.Fatalf("RegisterDevice IST001 failed: %v", err)
	}
	if err := server.RegisterDevice("TKY001", WithDeviceTimezone(tokyo)); err != nil {
		t.Fatalf("RegisterDevice TKY001 failed: %v", err)
	}

	// Both devices report "09:00:00" but in different timezones.
	data := "123\t2024-06-15 09:00:00\t0\t15\t0"

	req1 := httptest.NewRequest(http.MethodPost,
		"/iclock/cdata?SN=IST001&table=ATTLOG", strings.NewReader(data))
	w1 := httptest.NewRecorder()
	server.HandleCData(w1, req1)

	req2 := httptest.NewRequest(http.MethodPost,
		"/iclock/cdata?SN=TKY001&table=ATTLOG", strings.NewReader(data))
	w2 := httptest.NewRecorder()
	server.HandleCData(w2, req2)

	istanbulExpected := time.Date(2024, 6, 15, 6, 0, 0, 0, time.UTC) // UTC+3
	tokyoExpected := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)    // UTC+9

	results := make(map[string]time.Time)
	for range 2 {
		select {
		case rec := <-received:
			results[rec.SerialNumber] = rec.Timestamp.UTC()
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for attendance callbacks")
		}
	}

	if !results["IST001"].Equal(istanbulExpected) {
		t.Errorf("IST001: expected %v, got %v", istanbulExpected, results["IST001"])
	}
	if !results["TKY001"].Equal(tokyoExpected) {
		t.Errorf("TKY001: expected %v, got %v", tokyoExpected, results["TKY001"])
	}
}

func TestDeviceSnapshot_IncludesTimezone(t *testing.T) {
	istanbul, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		t.Fatal(err)
	}

	server := NewADMSServer(WithEnableInspect())
	defer server.Close()

	if err := server.RegisterDevice("DEV001", WithDeviceTimezone(istanbul)); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/iclock/inspect", nil)
	w := httptest.NewRecorder()
	server.HandleInspect(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp struct {
		Devices []DeviceSnapshot `json:"devices"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(resp.Devices) != 1 {
		t.Fatalf("expected 1 device, got %d", len(resp.Devices))
	}
	if resp.Devices[0].Timezone != "Europe/Istanbul" {
		t.Errorf("expected timezone Europe/Istanbul in snapshot, got %q", resp.Devices[0].Timezone)
	}
}

func TestDeviceSnapshot_DefaultTimezoneInSnapshot(t *testing.T) {
	server := NewADMSServer(WithEnableInspect())
	defer server.Close()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/iclock/inspect", nil)
	w := httptest.NewRecorder()
	server.HandleInspect(w, req)

	var resp struct {
		Devices []DeviceSnapshot `json:"devices"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(resp.Devices) != 1 {
		t.Fatalf("expected 1 device, got %d", len(resp.Devices))
	}
	if resp.Devices[0].Timezone != "UTC" {
		t.Errorf("expected UTC timezone in snapshot, got %q", resp.Devices[0].Timezone)
	}
}

func BenchmarkHandleCData(b *testing.B) {
	server := NewADMSServer()
	defer server.Close()
	attendanceData := "123\t2024-01-01 08:00:00\t0\t1\t0"

	for b.Loop() {
		req := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN=TEST001&table=ATTLOG", bytes.NewBufferString(attendanceData))
		w := httptest.NewRecorder()
		server.HandleCData(w, req)
	}
}

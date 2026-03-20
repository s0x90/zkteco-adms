package main

import (
	"bytes"
	"context"
	"fmt"
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

	// Write body without calling WriteHeader; status should stay 200.
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

func TestStatusRecorder_MultipleWriteHeader(t *testing.T) {
	w := httptest.NewRecorder()
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

	rec.WriteHeader(http.StatusBadRequest)
	rec.WriteHeader(http.StatusInternalServerError)

	// Our recorder captures every call; the last one wins.
	if rec.status != http.StatusInternalServerError {
		t.Errorf("expected status %d, got %d",
			http.StatusInternalServerError, rec.status)
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

func TestLogMiddleware_DefaultStatusOK(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok")) // no explicit WriteHeader
	})

	handler := logMiddleware(inner)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if !strings.Contains(buf.String(), "status=200") {
		t.Errorf("expected status=200 in log; got: %s", buf.String())
	}
}

func TestLogMiddleware_PreservesResponseBody(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "hello world")
	})

	handler := logMiddleware(inner)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Body.String() != "hello world" {
		t.Errorf("expected body %q, got %q", "hello world", w.Body.String())
	}
	if w.Header().Get("Content-Type") != "text/plain" {
		t.Errorf("expected Content-Type text/plain, got %q", w.Header().Get("Content-Type"))
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

// ---------- statusHandler tests ----------
// These call the real statusHandler() from main.go.

func TestStatusHandler_NoDevices(t *testing.T) {
	server := newTestServer(t)
	handler := statusHandler(server)

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Connected Devices: 0") {
		t.Errorf("expected 0 connected devices; body: %s", w.Body.String())
	}
}

func TestStatusHandler_WithDevices(t *testing.T) {
	server := newTestServer(t, "DEV001", "DEV002")
	handler := statusHandler(server)

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "Connected Devices: 2") {
		t.Errorf("expected 2 connected devices; body: %s", body)
	}
	for _, sn := range []string{"DEV001", "DEV002"} {
		if !strings.Contains(body, sn) {
			t.Errorf("expected %s in body; body: %s", sn, body)
		}
	}
}

func TestStatusHandler_ContentType(t *testing.T) {
	server := newTestServer(t)
	handler := statusHandler(server)

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	ct := w.Header().Get("Content-Type")
	if ct != "text/plain" {
		t.Errorf("expected Content-Type text/plain, got %q", ct)
	}
}

func TestStatusHandler_ShowsOnlineStatus(t *testing.T) {
	server := newTestServer(t, "DEV001")
	handler := statusHandler(server)

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	body := w.Body.String()
	// Freshly registered devices show as online (LastActivity is set
	// at registration time) depending on the OnlineThreshold.
	if !strings.Contains(body, "Online:") {
		t.Errorf("expected Online field in output; body: %s", body)
	}
}

func TestStatusHandler_ShowsLastActivity(t *testing.T) {
	server := newTestServer(t, "DEV001")
	handler := statusHandler(server)

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "Last Activity:") {
		t.Errorf("expected Last Activity field; body: %s", body)
	}
}

// ---------- commandHandler tests ----------
// These call the real commandHandler() from main.go.

func TestCommandHandler_MethodNotAllowed(t *testing.T) {
	server := newTestServer(t)
	handler := commandHandler(server)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/command?sn=DEV001&cmd=REBOOT", nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("expected 405 for %s, got %d", method, w.Code)
			}
		})
	}
}

func TestCommandHandler_MissingParams(t *testing.T) {
	server := newTestServer(t, "DEV001")
	handler := commandHandler(server)

	tests := []struct {
		name string
		url  string
	}{
		{"missing both", "/command"},
		{"missing cmd", "/command?sn=DEV001"},
		{"missing sn", "/command?cmd=REBOOT"},
		{"empty values", "/command?sn=&cmd="},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.url, nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400 for %q, got %d", tc.url, w.Code)
			}
		})
	}
}

func TestCommandHandler_Success(t *testing.T) {
	server := newTestServer(t, "DEV001")
	handler := commandHandler(server)

	req := httptest.NewRequest(http.MethodPost, "/command?sn=DEV001&cmd=REBOOT", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Command queued") {
		t.Errorf("expected success message; body: %s", body)
	}
	if !strings.Contains(body, "DEV001") {
		t.Errorf("expected device serial in response; body: %s", body)
	}
	if !strings.Contains(body, "REBOOT") {
		t.Errorf("expected command in response; body: %s", body)
	}
}

func TestCommandHandler_UnknownDevice(t *testing.T) {
	server := newTestServer(t) // no devices registered
	handler := commandHandler(server)

	// QueueCommand succeeds even for unknown serials (it just queues
	// the command for later pickup), so we expect 200.
	req := httptest.NewRequest(http.MethodPost, "/command?sn=NOSUCH&cmd=REBOOT", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for unknown device (queue accepts it), got %d", w.Code)
	}
}

func TestCommandHandler_QueueFull(t *testing.T) {
	// Create a server with a very small command queue.
	server := zkadms.NewADMSServer(zkadms.WithMaxCommandsPerDevice(1))
	t.Cleanup(func() { server.Close() })
	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatal(err)
	}

	handler := commandHandler(server)

	// Fill the queue.
	req := httptest.NewRequest(http.MethodPost, "/command?sn=DEV001&cmd=CMD1", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first command should succeed, got %d: %s", w.Code, w.Body.String())
	}

	// Second command should fail with 503.
	req = httptest.NewRequest(http.MethodPost, "/command?sn=DEV001&cmd=CMD2", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when queue is full, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------- newMux routing tests ----------
// Exercises the real newMux() which wires everything together.

func TestNewMux_StatusRoute(t *testing.T) {
	server := newTestServer(t, "DEV001")
	mux := newMux(server)

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for /status, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Connected Devices:") {
		t.Errorf("expected device list in /status response; body: %s", w.Body.String())
	}
}

func TestNewMux_CommandRoute(t *testing.T) {
	server := newTestServer(t, "DEV001")
	mux := newMux(server)

	req := httptest.NewRequest(http.MethodPost, "/command?sn=DEV001&cmd=INFO", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for POST /command, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Command queued") {
		t.Errorf("expected queued message; body: %s", w.Body.String())
	}
}

func TestNewMux_IClockRoute(t *testing.T) {
	server := newTestServer(t)
	mux := newMux(server)

	// The ADMS server handles /iclock/* paths. A bare GET with no
	// serial number should still reach the handler (which returns an
	// error status, not a 404 from the mux).
	req := httptest.NewRequest(http.MethodGet, "/iclock/cdata", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	// We don't assert on the exact status; the important thing is
	// that the route was matched (i.e. not a 404 from mux).
	if w.Code == http.StatusNotFound {
		t.Error("/iclock/cdata should be handled by the ADMS server, not 404")
	}
}

func TestNewMux_UnknownRoute(t *testing.T) {
	server := newTestServer(t)
	mux := newMux(server)

	req := httptest.NewRequest(http.MethodGet, "/nonexistent", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown route, got %d", w.Code)
	}
}

// ---------- integration: mux with logMiddleware ----------

func TestNewMux_LogsRequests(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	server := newTestServer(t)
	mux := newMux(server)

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	logOutput := buf.String()
	if !strings.Contains(logOutput, "http request") {
		t.Errorf("expected log output from logMiddleware; got: %s", logOutput)
	}
	if !strings.Contains(logOutput, "path=/status") {
		t.Errorf("expected path=/status in log; got: %s", logOutput)
	}
}

// ---------- edge cases ----------

func TestStatusHandler_ManyDevices(t *testing.T) {
	// Register more than a handful of devices to ensure all are listed.
	sns := make([]string, 10)
	for i := range sns {
		sns[i] = fmt.Sprintf("DEV%03d", i)
	}
	server := newTestServer(t, sns...)
	handler := statusHandler(server)

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "Connected Devices: 10") {
		t.Errorf("expected 10 devices; body: %s", body)
	}
	for _, sn := range sns {
		if !strings.Contains(body, sn) {
			t.Errorf("expected %s in body", sn)
		}
	}
}

func TestCommandHandler_SpecialCharsInCommand(t *testing.T) {
	server := newTestServer(t, "DEV001")
	handler := commandHandler(server)

	// Commands may contain special characters; ensure they pass through.
	// URL-encode the command to avoid malformed request.
	req := httptest.NewRequest(http.MethodPost,
		"/command?sn=DEV001&cmd=DATA+UPDATE+tablename", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "DATA UPDATE tablename") {
		t.Errorf("expected command echoed in response; body: %s", w.Body.String())
	}
}

// ---------- run() tests ----------

func TestRun_StartsAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())

	errCh := make(chan error, 1)
	go func() {
		errCh <- run(ctx, ":0")
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

	// An obviously invalid address should make ListenAndServe fail immediately.
	err := run(ctx, ":-1")
	if err == nil {
		t.Fatal("expected error for invalid address, got nil")
	}
}

// TestRun_ExercisesCallbacks starts the server via run() and sends simulated
// ADMS device traffic to exercise the attendance, device-info, and registry
// callbacks wired inside run().
func TestRun_ExercisesCallbacks(t *testing.T) {
	// Use a listener so we know the actual port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close() // free the port for run()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- run(ctx, addr)
	}()

	// Wait for server to start.
	time.Sleep(100 * time.Millisecond)

	base := "http://" + addr

	// 1. Send attendance data — triggers WithOnAttendance callback.
	attendanceData := "USER001\t2026-03-20 09:00:00\t0\t1\t0\tWC1"
	resp, err := http.Post(base+"/iclock/cdata?SN=DEVICE001&table=ATTLOG",
		"text/plain", strings.NewReader(attendanceData))
	if err != nil {
		t.Fatalf("POST attendance failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 for attendance POST, got %d", resp.StatusCode)
	}

	// 2. Send device info — triggers WithOnDeviceInfo callback.
	infoData := "FWVersion=Ver 6.60\nDeviceName=ZK-F22"
	resp, err = http.Post(base+"/iclock/cdata?SN=DEVICE001",
		"text/plain", strings.NewReader(infoData))
	if err != nil {
		t.Fatalf("POST device info failed: %v", err)
	}
	resp.Body.Close()

	// 3. Send registry data — triggers WithOnRegistry callback.
	//    Registry body uses comma-separated key=value pairs.
	registryData := "UserCount=50,FPCount=100,AttCount=5000,FWVersion=Ver 6.60,IPAddress=192.168.1.201"
	resp, err = http.Post(base+"/iclock/registry?SN=DEVICE001",
		"text/plain", strings.NewReader(registryData))
	if err != nil {
		t.Fatalf("POST registry failed: %v", err)
	}
	resp.Body.Close()

	// 4. Verify /status shows the devices.
	resp, err = http.Get(base + "/status")
	if err != nil {
		t.Fatalf("GET /status failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 for /status, got %d", resp.StatusCode)
	}

	// Give callbacks time to be processed.
	time.Sleep(100 * time.Millisecond)

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

// TestRun_RegistryCallbackManyEntries exercises the registry callback's
// truncation logic (shown >= 8 break).
func TestRun_RegistryCallbackManyEntries(t *testing.T) {
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
		errCh <- run(ctx, addr)
	}()

	time.Sleep(100 * time.Millisecond)

	base := "http://" + addr

	// Send registry data with more than 8 key-value pairs to trigger the break.
	// Registry body uses comma-separated key=value pairs.
	var parts []string
	for i := range 15 {
		parts = append(parts, fmt.Sprintf("Key%d=Value%d", i, i))
	}
	registryData := strings.Join(parts, ",")
	resp, err := http.Post(base+"/iclock/registry?SN=DEVICE001",
		"text/plain", strings.NewReader(registryData))
	if err != nil {
		t.Fatalf("POST registry failed: %v", err)
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

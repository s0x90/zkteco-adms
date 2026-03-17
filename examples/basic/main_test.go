package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	zkdevicesync "github.com/s0x90/zkteco-sync"
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
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(slog.Default()) })

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
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(slog.Default()) })

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

// ---------- /status endpoint tests ----------

func newTestServer(t *testing.T, devices ...string) *zkdevicesync.ADMSServer {
	t.Helper()
	server := zkdevicesync.NewADMSServer()
	t.Cleanup(func() { server.Close() })
	for _, sn := range devices {
		if err := server.RegisterDevice(sn); err != nil {
			t.Fatalf("RegisterDevice(%q) failed: %v", sn, err)
		}
	}
	return server
}

func statusHandler(server *zkdevicesync.ADMSServer) http.Handler {
	return logMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
	}))
}

func TestStatusEndpoint_NoDevices(t *testing.T) {
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

func TestStatusEndpoint_WithDevices(t *testing.T) {
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
	if !strings.Contains(body, "DEV001") {
		t.Errorf("expected DEV001 in body; body: %s", body)
	}
	if !strings.Contains(body, "DEV002") {
		t.Errorf("expected DEV002 in body; body: %s", body)
	}
}

func TestStatusEndpoint_ContentType(t *testing.T) {
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

// ---------- /command endpoint tests ----------

func commandHandler(server *zkdevicesync.ADMSServer) http.Handler {
	return logMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}))
}

func TestCommandEndpoint_MethodNotAllowed(t *testing.T) {
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

func TestCommandEndpoint_MissingParams(t *testing.T) {
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

func TestCommandEndpoint_Success(t *testing.T) {
	server := newTestServer(t, "DEV001")
	handler := commandHandler(server)

	req := httptest.NewRequest(http.MethodPost, "/command?sn=DEV001&cmd=REBOOT", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Command queued") {
		t.Errorf("expected success message; body: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "DEV001") {
		t.Errorf("expected device serial in response; body: %s", w.Body.String())
	}
}

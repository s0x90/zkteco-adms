package main

import (
	"bytes"
	"context"
	"encoding/json"
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
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	logOutput := buf.String()
	for _, want := range []string{"http request", "method=GET", "path=/test", "status=201", "duration="} {
		if !strings.Contains(logOutput, want) {
			t.Errorf("log output missing %q; got: %s", want, logOutput)
		}
	}
}

func TestLogMiddleware_PreservesResponse(t *testing.T) {
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
		t.Errorf("expected Content-Type application/json, got %q", w.Header().Get("Content-Type"))
	}
}

// ---------- AttendanceStore tests ----------

func TestNewAttendanceStore(t *testing.T) {
	store := NewAttendanceStore()
	if store == nil {
		t.Fatal("NewAttendanceStore returned nil")
	}
	records := store.GetRecords()
	if len(records) != 0 {
		t.Errorf("expected 0 records, got %d", len(records))
	}
}

func TestAttendanceStore_SaveAndGet(t *testing.T) {
	store := NewAttendanceStore()
	ts := time.Date(2026, 3, 17, 9, 0, 0, 0, time.UTC)

	rec := zkadms.AttendanceRecord{
		UserID:       "USER001",
		Timestamp:    ts,
		Status:       0,
		VerifyMode:   1,
		WorkCode:     "",
		SerialNumber: "DEV001",
	}

	if err := store.SaveAttendance(rec); err != nil {
		t.Fatalf("SaveAttendance failed: %v", err)
	}

	records := store.GetRecords()
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	if records[0].UserID != "USER001" {
		t.Errorf("expected UserID USER001, got %s", records[0].UserID)
	}
	if records[0].SerialNumber != "DEV001" {
		t.Errorf("expected SerialNumber DEV001, got %s", records[0].SerialNumber)
	}
}

func TestAttendanceStore_GetRecordsReturnsCopy(t *testing.T) {
	store := NewAttendanceStore()
	ts := time.Date(2026, 3, 17, 9, 0, 0, 0, time.UTC)

	if err := store.SaveAttendance(zkadms.AttendanceRecord{
		UserID: "USER001", Timestamp: ts, SerialNumber: "DEV001",
	}); err != nil {
		t.Fatal(err)
	}

	records := store.GetRecords()
	records[0].UserID = "MODIFIED"

	original := store.GetRecords()
	if original[0].UserID != "USER001" {
		t.Error("GetRecords should return a copy; original was modified")
	}
}

func TestAttendanceStore_GetRecordsByUser(t *testing.T) {
	store := NewAttendanceStore()
	ts := time.Date(2026, 3, 17, 9, 0, 0, 0, time.UTC)

	for _, uid := range []string{"USER001", "USER002", "USER001", "USER003"} {
		if err := store.SaveAttendance(zkadms.AttendanceRecord{
			UserID: uid, Timestamp: ts, SerialNumber: "DEV001",
		}); err != nil {
			t.Fatal(err)
		}
	}

	records := store.GetRecordsByUser("USER001")
	if len(records) != 2 {
		t.Errorf("expected 2 records for USER001, got %d", len(records))
	}

	records = store.GetRecordsByUser("UNKNOWN")
	if len(records) != 0 {
		t.Errorf("expected 0 records for UNKNOWN, got %d", len(records))
	}
}

func TestAttendanceStore_GetRecordsByDateRange(t *testing.T) {
	store := NewAttendanceStore()

	times := []time.Time{
		time.Date(2026, 3, 16, 8, 0, 0, 0, time.UTC),  // yesterday
		time.Date(2026, 3, 17, 9, 0, 0, 0, time.UTC),  // today morning
		time.Date(2026, 3, 17, 17, 0, 0, 0, time.UTC), // today evening
		time.Date(2026, 3, 18, 8, 0, 0, 0, time.UTC),  // tomorrow
	}
	for i, ts := range times {
		if err := store.SaveAttendance(zkadms.AttendanceRecord{
			UserID: "USER001", Timestamp: ts, SerialNumber: "DEV001", Status: i,
		}); err != nil {
			t.Fatal(err)
		}
	}

	start := time.Date(2026, 3, 17, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 3, 18, 0, 0, 0, 0, time.UTC)

	records := store.GetRecordsByDateRange(start, end)
	if len(records) != 2 {
		t.Errorf("expected 2 records in range, got %d", len(records))
	}
}

func TestAttendanceStore_GetRecordsByDateRange_Empty(t *testing.T) {
	store := NewAttendanceStore()

	start := time.Date(2026, 3, 17, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 3, 18, 0, 0, 0, 0, time.UTC)

	records := store.GetRecordsByDateRange(start, end)
	if len(records) != 0 {
		t.Errorf("expected 0 records, got %d", len(records))
	}
}

// ---------- helpers ----------

func seedStore(t *testing.T, store *AttendanceStore, records ...zkadms.AttendanceRecord) {
	t.Helper()
	for _, rec := range records {
		if err := store.SaveAttendance(rec); err != nil {
			t.Fatalf("SaveAttendance failed: %v", err)
		}
	}
}

// ---------- attendanceHandler tests ----------
// These call the real attendanceHandler() from main.go.

func TestAttendanceHandler_EmptyStore(t *testing.T) {
	store := NewAttendanceStore()
	handler := attendanceHandler(store)

	req := httptest.NewRequest(http.MethodGet, "/api/attendance", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q",
			w.Header().Get("Content-Type"))
	}

	var resp attendanceResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Total != 0 {
		t.Errorf("expected total 0, got %d", resp.Total)
	}
	if len(resp.Records) != 0 {
		t.Errorf("expected 0 records, got %d", len(resp.Records))
	}
}

func TestAttendanceHandler_AllRecords(t *testing.T) {
	store := NewAttendanceStore()
	ts := time.Date(2026, 3, 17, 9, 0, 0, 0, time.UTC)
	seedStore(t, store,
		zkadms.AttendanceRecord{UserID: "U1", Timestamp: ts, Status: 0, SerialNumber: "D1"},
		zkadms.AttendanceRecord{UserID: "U2", Timestamp: ts, Status: 1, SerialNumber: "D2"},
	)

	handler := attendanceHandler(store)
	req := httptest.NewRequest(http.MethodGet, "/api/attendance", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	var resp attendanceResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Total != 2 {
		t.Errorf("expected total 2, got %d", resp.Total)
	}
	if len(resp.Records) != 2 {
		t.Errorf("expected 2 records, got %d", len(resp.Records))
	}
	// All records should have UserID populated.
	for i, rec := range resp.Records {
		if rec.UserID == "" {
			t.Errorf("record %d: expected non-empty UserID", i)
		}
	}
}

func TestAttendanceHandler_FilterByUser(t *testing.T) {
	store := NewAttendanceStore()
	ts := time.Date(2026, 3, 17, 9, 0, 0, 0, time.UTC)
	seedStore(t, store,
		zkadms.AttendanceRecord{UserID: "U1", Timestamp: ts, Status: 0, SerialNumber: "D1"},
		zkadms.AttendanceRecord{UserID: "U2", Timestamp: ts, Status: 1, SerialNumber: "D1"},
		zkadms.AttendanceRecord{UserID: "U1", Timestamp: ts.Add(time.Hour), Status: 1, SerialNumber: "D1"},
	)

	handler := attendanceHandler(store)
	req := httptest.NewRequest(http.MethodGet, "/api/attendance?user_id=U1", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	var resp attendanceResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.UserID != "U1" {
		t.Errorf("expected user_id U1 in response, got %q", resp.UserID)
	}
	if resp.Count != 2 {
		t.Errorf("expected count 2, got %d", resp.Count)
	}
	if len(resp.Records) != 2 {
		t.Errorf("expected 2 records, got %d", len(resp.Records))
	}
	// Filtered records should NOT have UserID set (matches handler behavior).
	for i, rec := range resp.Records {
		if rec.UserID != "" {
			t.Errorf("record %d: filtered records should omit UserID, got %q", i, rec.UserID)
		}
	}
}

func TestAttendanceHandler_FilterByUnknownUser(t *testing.T) {
	store := NewAttendanceStore()
	ts := time.Date(2026, 3, 17, 9, 0, 0, 0, time.UTC)
	seedStore(t, store,
		zkadms.AttendanceRecord{UserID: "U1", Timestamp: ts, SerialNumber: "D1"},
	)

	handler := attendanceHandler(store)
	req := httptest.NewRequest(http.MethodGet, "/api/attendance?user_id=UNKNOWN", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	var resp attendanceResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Count != 0 {
		t.Errorf("expected count 0, got %d", resp.Count)
	}
	if len(resp.Records) != 0 {
		t.Errorf("expected 0 records, got %d", len(resp.Records))
	}
}

func TestAttendanceHandler_RecordFields(t *testing.T) {
	store := NewAttendanceStore()
	ts := time.Date(2026, 3, 17, 14, 30, 0, 0, time.UTC)
	seedStore(t, store,
		zkadms.AttendanceRecord{
			UserID: "U1", Timestamp: ts, Status: 1,
			SerialNumber: "DEV-MAIN", VerifyMode: 3, WorkCode: "WC1",
		},
	)

	handler := attendanceHandler(store)
	req := httptest.NewRequest(http.MethodGet, "/api/attendance", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	var resp attendanceResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(resp.Records))
	}
	rec := resp.Records[0]
	if rec.UserID != "U1" {
		t.Errorf("expected UserID U1, got %q", rec.UserID)
	}
	if rec.Status != 1 {
		t.Errorf("expected Status 1, got %d", rec.Status)
	}
	if rec.Device != "DEV-MAIN" {
		t.Errorf("expected Device DEV-MAIN, got %q", rec.Device)
	}
	if rec.Timestamp != "2026-03-17T14:30:00Z" {
		t.Errorf("expected Timestamp 2026-03-17T14:30:00Z, got %q", rec.Timestamp)
	}
}

// ---------- summaryHandler tests ----------
// These call the real summaryHandler() from main.go.

func TestSummaryHandler_EmptyStore(t *testing.T) {
	store := NewAttendanceStore()
	handler := summaryHandler(store)

	req := httptest.NewRequest(http.MethodGet, "/api/summary/today", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp summaryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	today := time.Now().Format("2006-01-02")
	if resp.Date != today {
		t.Errorf("expected date %s, got %s", today, resp.Date)
	}
	if resp.Count != 0 {
		t.Errorf("expected count 0, got %d", resp.Count)
	}
}

func TestSummaryHandler_OnlyTodayRecords(t *testing.T) {
	store := NewAttendanceStore()
	now := time.Now()

	seedStore(t, store,
		// Yesterday — should be excluded.
		zkadms.AttendanceRecord{
			UserID: "U1", Timestamp: now.Add(-24 * time.Hour),
			SerialNumber: "D1", Status: 0,
		},
		// Today morning.
		zkadms.AttendanceRecord{
			UserID: "U1", Timestamp: now,
			SerialNumber: "D1", Status: 0,
		},
		// Today afternoon.
		zkadms.AttendanceRecord{
			UserID: "U2", Timestamp: now,
			SerialNumber: "D1", Status: 1,
		},
		// Tomorrow — should be excluded.
		zkadms.AttendanceRecord{
			UserID: "U3", Timestamp: now.Add(24 * time.Hour),
			SerialNumber: "D1", Status: 0,
		},
	)

	handler := summaryHandler(store)
	req := httptest.NewRequest(http.MethodGet, "/api/summary/today", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	var resp summaryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Count != 2 {
		t.Errorf("expected 2 today records, got %d", resp.Count)
	}
	if len(resp.Records) != 2 {
		t.Errorf("expected 2 records in response, got %d", len(resp.Records))
	}
}

func TestSummaryHandler_ContentType(t *testing.T) {
	store := NewAttendanceStore()
	handler := summaryHandler(store)

	req := httptest.NewRequest(http.MethodGet, "/api/summary/today", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}
}

func TestSummaryHandler_ValidJSON(t *testing.T) {
	store := NewAttendanceStore()
	now := time.Now()
	seedStore(t, store,
		zkadms.AttendanceRecord{
			UserID: "U1", Timestamp: now, SerialNumber: "D1", Status: 0,
		},
	)

	handler := summaryHandler(store)
	req := httptest.NewRequest(http.MethodGet, "/api/summary/today", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	var resp summaryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}

	if resp.Count != len(resp.Records) {
		t.Errorf("count (%d) does not match records length (%d)",
			resp.Count, len(resp.Records))
	}

	// Verify record fields are populated.
	for i, rec := range resp.Records {
		if rec.UserID == "" {
			t.Errorf("record %d: expected non-empty UserID", i)
		}
		if rec.Timestamp == "" {
			t.Errorf("record %d: expected non-empty Timestamp", i)
		}
		if rec.Device == "" {
			t.Errorf("record %d: expected non-empty Device", i)
		}
	}
}

func TestSummaryHandler_RecordDate(t *testing.T) {
	store := NewAttendanceStore()
	handler := summaryHandler(store)

	req := httptest.NewRequest(http.MethodGet, "/api/summary/today", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	var resp summaryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	// Date should be parseable and match today.
	parsed, err := time.Parse("2006-01-02", resp.Date)
	if err != nil {
		t.Fatalf("date %q not in expected format: %v", resp.Date, err)
	}
	now := time.Now()
	if parsed.Year() != now.Year() || parsed.Month() != now.Month() || parsed.Day() != now.Day() {
		t.Errorf("expected today's date, got %s", resp.Date)
	}
}

// ---------- newMux routing tests ----------
// Exercises the real newMux() which wires everything together.

func newTestMux(t *testing.T) (*http.ServeMux, *AttendanceStore) {
	t.Helper()
	store := NewAttendanceStore()
	server := zkadms.NewADMSServer()
	t.Cleanup(func() { server.Close() })
	return newMux(server, store), store
}

func TestNewMux_AttendanceRoute(t *testing.T) {
	mux, store := newTestMux(t)
	ts := time.Date(2026, 3, 17, 9, 0, 0, 0, time.UTC)
	seedStore(t, store,
		zkadms.AttendanceRecord{UserID: "U1", Timestamp: ts, SerialNumber: "D1"},
	)

	req := httptest.NewRequest(http.MethodGet, "/api/attendance", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for /api/attendance, got %d", w.Code)
	}

	var resp attendanceResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp.Total != 1 {
		t.Errorf("expected total 1, got %d", resp.Total)
	}
}

func TestNewMux_SummaryRoute(t *testing.T) {
	mux, _ := newTestMux(t)

	req := httptest.NewRequest(http.MethodGet, "/api/summary/today", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for /api/summary/today, got %d", w.Code)
	}
	if w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("expected application/json, got %q", w.Header().Get("Content-Type"))
	}
}

func TestNewMux_IClockRoute(t *testing.T) {
	mux, _ := newTestMux(t)

	req := httptest.NewRequest(http.MethodGet, "/iclock/cdata", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	// The important thing is that the route is matched (not 404).
	if w.Code == http.StatusNotFound {
		t.Error("/iclock/cdata should be handled by ADMS server, not 404")
	}
}

func TestNewMux_UnknownRoute(t *testing.T) {
	mux, _ := newTestMux(t)

	req := httptest.NewRequest(http.MethodGet, "/nonexistent", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown route, got %d", w.Code)
	}
}

func TestNewMux_LogsRequests(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	mux, _ := newTestMux(t)

	req := httptest.NewRequest(http.MethodGet, "/api/attendance", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	logOutput := buf.String()
	if !strings.Contains(logOutput, "http request") {
		t.Errorf("expected log output from logMiddleware; got: %s", logOutput)
	}
	if !strings.Contains(logOutput, "path=/api/attendance") {
		t.Errorf("expected path=/api/attendance in log; got: %s", logOutput)
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
// ADMS device traffic to exercise the attendance and device-info callbacks
// wired inside run().
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
		errCh <- run(ctx, addr)
	}()

	time.Sleep(100 * time.Millisecond)

	base := "http://" + addr

	// 1. Send attendance data — triggers WithOnAttendance callback which
	//    calls store.SaveAttendance.
	attendanceData := "USER001\t2026-03-20 09:00:00\t0\t1\t0\tWC1"
	resp, err := http.Post(base+"/iclock/cdata?SN=TEST001&table=ATTLOG",
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
	resp, err = http.Post(base+"/iclock/cdata?SN=TEST001",
		"text/plain", strings.NewReader(infoData))
	if err != nil {
		t.Fatalf("POST device info failed: %v", err)
	}
	resp.Body.Close()

	// Give callbacks time to be processed.
	time.Sleep(200 * time.Millisecond)

	// 3. Verify the attendance record was stored via the API.
	resp, err = http.Get(base + "/api/attendance")
	if err != nil {
		t.Fatalf("GET /api/attendance failed: %v", err)
	}
	defer resp.Body.Close()

	var attResp attendanceResponse
	if err := json.NewDecoder(resp.Body).Decode(&attResp); err != nil {
		t.Fatalf("failed to decode attendance response: %v", err)
	}
	if attResp.Total != 1 {
		t.Errorf("expected 1 attendance record stored via callback, got %d", attResp.Total)
	}

	// 4. Verify today's summary shows the record.
	resp, err = http.Get(base + "/api/summary/today")
	if err != nil {
		t.Fatalf("GET /api/summary/today failed: %v", err)
	}
	resp.Body.Close()

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

// ---------- concurrent access test ----------

func TestAttendanceStore_ConcurrentAccess(t *testing.T) {
	store := NewAttendanceStore()
	ts := time.Date(2026, 3, 17, 9, 0, 0, 0, time.UTC)

	// Write from multiple goroutines.
	done := make(chan struct{})
	for i := range 10 {
		go func(id int) {
			defer func() { done <- struct{}{} }()
			for j := range 10 {
				_ = store.SaveAttendance(zkadms.AttendanceRecord{
					UserID:       fmt.Sprintf("USER%d", id),
					Timestamp:    ts.Add(time.Duration(j) * time.Minute),
					SerialNumber: "DEV001",
				})
			}
		}(i)
	}
	for range 10 {
		<-done
	}

	records := store.GetRecords()
	if len(records) != 100 {
		t.Errorf("expected 100 records, got %d", len(records))
	}
}

// ---------- attendanceHandler additional tests ----------

func TestAttendanceHandler_MultipleUsers(t *testing.T) {
	store := NewAttendanceStore()
	ts := time.Date(2026, 3, 17, 9, 0, 0, 0, time.UTC)
	seedStore(t, store,
		zkadms.AttendanceRecord{UserID: "U1", Timestamp: ts, Status: 0, SerialNumber: "D1"},
		zkadms.AttendanceRecord{UserID: "U2", Timestamp: ts, Status: 0, SerialNumber: "D1"},
		zkadms.AttendanceRecord{UserID: "U3", Timestamp: ts, Status: 0, SerialNumber: "D2"},
		zkadms.AttendanceRecord{UserID: "U1", Timestamp: ts.Add(8 * time.Hour), Status: 1, SerialNumber: "D1"},
	)

	handler := attendanceHandler(store)

	// Filter by U1 — should get 2 records.
	req := httptest.NewRequest(http.MethodGet, "/api/attendance?user_id=U1", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	var resp attendanceResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Count != 2 {
		t.Errorf("expected 2 records for U1, got %d", resp.Count)
	}

	// All records — should get 4.
	req = httptest.NewRequest(http.MethodGet, "/api/attendance", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Total != 4 {
		t.Errorf("expected 4 total records, got %d", resp.Total)
	}
}

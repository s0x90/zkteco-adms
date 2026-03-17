package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
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
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(slog.Default()) })

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

// ---------- /api/attendance endpoint tests ----------

func seedStore(t *testing.T, store *AttendanceStore, records ...zkadms.AttendanceRecord) {
	t.Helper()
	for _, rec := range records {
		if err := store.SaveAttendance(rec); err != nil {
			t.Fatalf("SaveAttendance failed: %v", err)
		}
	}
}

func attendanceHandler(store *AttendanceStore) http.Handler {
	return logMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID := r.URL.Query().Get("user_id")
		w.Header().Set("Content-Type", "application/json")

		var resp attendanceResponse
		if userID != "" {
			records := store.GetRecordsByUser(userID)
			resp.UserID = userID
			resp.Count = len(records)
			resp.Records = make([]attendanceEntry, len(records))
			for i, rec := range records {
				resp.Records[i] = attendanceEntry{
					Timestamp: rec.Timestamp.Format(time.RFC3339),
					Status:    rec.Status,
					Device:    rec.SerialNumber,
				}
			}
		} else {
			records := store.GetRecords()
			resp.Total = len(records)
			resp.Records = make([]attendanceEntry, len(records))
			for i, rec := range records {
				resp.Records[i] = attendanceEntry{
					UserID:    rec.UserID,
					Timestamp: rec.Timestamp.Format(time.RFC3339),
					Status:    rec.Status,
					Device:    rec.SerialNumber,
				}
			}
		}

		json.NewEncoder(w).Encode(resp)
	}))
}

func TestAttendanceEndpoint_EmptyStore(t *testing.T) {
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

func TestAttendanceEndpoint_AllRecords(t *testing.T) {
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

func TestAttendanceEndpoint_FilterByUser(t *testing.T) {
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
	// Filtered records should NOT have UserID set (matches original handler behavior).
	for i, rec := range resp.Records {
		if rec.UserID != "" {
			t.Errorf("record %d: filtered records should omit UserID, got %q", i, rec.UserID)
		}
	}
}

func TestAttendanceEndpoint_FilterByUnknownUser(t *testing.T) {
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

// ---------- /api/summary/today endpoint tests ----------

func summaryHandler(store *AttendanceStore) http.Handler {
	return logMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		now := time.Now()
		startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		endOfDay := startOfDay.Add(24 * time.Hour)

		records := store.GetRecordsByDateRange(startOfDay, endOfDay)

		resp := summaryResponse{
			Date:    startOfDay.Format("2006-01-02"),
			Count:   len(records),
			Records: make([]attendanceEntry, len(records)),
		}
		for i, rec := range records {
			resp.Records[i] = attendanceEntry{
				UserID:    rec.UserID,
				Timestamp: rec.Timestamp.Format(time.RFC3339),
				Status:    rec.Status,
				Device:    rec.SerialNumber,
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
}

func TestSummaryEndpoint_EmptyStore(t *testing.T) {
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

func TestSummaryEndpoint_OnlyTodayRecords(t *testing.T) {
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

func TestSummaryEndpoint_ContentType(t *testing.T) {
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

func TestSummaryEndpoint_ValidJSON(t *testing.T) {
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

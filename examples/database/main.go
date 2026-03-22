// Command database demonstrates a ZKTeco ADMS server with an in-memory
// attendance store and JSON query endpoints, modeled after a typical
// database-backed integration.
//
// SECURITY WARNING: This example does NOT include any authentication or
// authorization. The /api/attendance and /api/summary/today endpoints are
// publicly accessible and expose employee attendance data. In a production
// deployment you MUST add authentication middleware (e.g. API keys, OAuth2,
// mTLS) before exposing these routes to a network.
//
// Run with:
//
//	go run ./examples/database
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	zkadms "github.com/s0x90/zkteco-adms"
	// Uncomment when using actual database
	// _ "github.com/lib/pq"
)

// maxLogBody is the maximum number of bytes logged for request/response bodies.
const maxLogBody = 1024

// statusRecorder wraps http.ResponseWriter to capture the status code and
// response body for debug logging.
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
					"dump", buf.String(),
				)
				panic(panicVal) // re-panic for upstream recovery
			}

			slog.Info("http exchange",
				"method", r.Method,
				"path", fullPath,
				"status", rec.status,
				"duration", elapsed,
				"dump", buf.String(),
			)
		}()

		next.ServeHTTP(rec, r)
	})
}

// AttendanceStore demonstrates how to integrate with a database.
type AttendanceStore struct {
	mu      sync.Mutex
	records []zkadms.AttendanceRecord
	// In a real application, you would use an actual database connection
	// db *sql.DB
}

// NewAttendanceStore returns a new empty store.
func NewAttendanceStore() *AttendanceStore {
	return &AttendanceStore{
		records: make([]zkadms.AttendanceRecord, 0),
	}
}

// SaveAttendance saves an attendance record.
func (s *AttendanceStore) SaveAttendance(record zkadms.AttendanceRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// In a real application, you would insert into a database:
	/*
		query := `INSERT INTO attendance_records
				  (user_id, timestamp, status, verify_mode, work_code, device_sn)
				  VALUES (?, ?, ?, ?, ?, ?)`
		_, err := s.db.Exec(query,
			record.UserID,
			record.Timestamp,
			record.Status,
			record.VerifyMode,
			record.WorkCode,
			record.SerialNumber)
		return err
	*/

	// For this example, we'll just store in memory
	s.records = append(s.records, record)
	fmt.Printf("Saved attendance record: User %s at %s from device %s\n",
		record.UserID, record.Timestamp.Format(time.RFC3339), record.SerialNumber)
	return nil
}

// GetRecords returns all stored records.
func (s *AttendanceStore) GetRecords() []zkadms.AttendanceRecord {
	s.mu.Lock()
	defer s.mu.Unlock()

	result := make([]zkadms.AttendanceRecord, len(s.records))
	copy(result, s.records)
	return result
}

// GetRecordsByUser returns records for a specific user.
func (s *AttendanceStore) GetRecordsByUser(userID string) []zkadms.AttendanceRecord {
	s.mu.Lock()
	defer s.mu.Unlock()

	var result []zkadms.AttendanceRecord
	for _, record := range s.records {
		if record.UserID == userID {
			result = append(result, record)
		}
	}
	return result
}

// GetRecordsByDateRange returns records within a date range.
func (s *AttendanceStore) GetRecordsByDateRange(start, end time.Time) []zkadms.AttendanceRecord {
	s.mu.Lock()
	defer s.mu.Unlock()

	var result []zkadms.AttendanceRecord
	for _, record := range s.records {
		if record.Timestamp.After(start) && record.Timestamp.Before(end) {
			result = append(result, record)
		}
	}
	return result
}

// attendanceResponse is the JSON structure for attendance API responses.
type attendanceResponse struct {
	UserID  string            `json:"user_id,omitempty"`
	Total   int               `json:"total,omitempty"`
	Count   int               `json:"count,omitempty"`
	Records []attendanceEntry `json:"records"`
}

type attendanceEntry struct {
	UserID    string `json:"user_id,omitempty"`
	Timestamp string `json:"timestamp"`
	Status    int    `json:"status"`
	Device    string `json:"device,omitempty"`
}

// summaryResponse is the JSON structure for the daily summary endpoint.
type summaryResponse struct {
	Date    string            `json:"date"`
	Count   int               `json:"count"`
	Records []attendanceEntry `json:"records"`
}

// attendanceHandler returns an http.Handler that responds with JSON
// attendance records. If the "user_id" query parameter is set, only
// records for that user are returned; otherwise all records are returned.
func attendanceHandler(store *AttendanceStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

		if err := json.NewEncoder(w).Encode(resp); err != nil {
			log.Printf("Error encoding attendance response: %v", err)
		}
	})
}

// summaryHandler returns an http.Handler that responds with a JSON daily
// summary of today's attendance records.
func summaryHandler(store *AttendanceStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			log.Printf("Error encoding summary response: %v", err)
		}
	})
}

// newMux builds the HTTP mux with all routes wired up to the given ADMS
// server and attendance store.
func newMux(server *zkadms.ADMSServer, store *AttendanceStore) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/iclock/", logMiddleware(server))
	// WARNING: These routes have no authentication and expose attendance
	// data. Add auth middleware before deploying to production.
	mux.Handle("/api/attendance", logMiddleware(attendanceHandler(store)))
	mux.Handle("/api/summary/today", logMiddleware(summaryHandler(store)))
	return mux
}

// run creates the attendance store and ADMS server, wires HTTP routes, and
// blocks until ctx is canceled. On cancellation the HTTP server is gracefully
// shut down and the ADMS callback queue is drained before returning.
func run(ctx context.Context, addr string) error {
	// Initialize the attendance store
	store := NewAttendanceStore()

	// Create the ADMS server with functional options
	server := zkadms.NewADMSServer(
		// Save attendance records to the store
		zkadms.WithOnAttendance(func(ctx context.Context, record zkadms.AttendanceRecord) {
			if err := store.SaveAttendance(record); err != nil {
				log.Printf("Error saving attendance: %v", err)
			}
		}),

		// Log device info updates
		zkadms.WithOnDeviceInfo(func(ctx context.Context, sn string, info map[string]string) {
			log.Printf("Device %s info updated: %v", sn, info)
		}),
	)
	defer server.Close()

	mux := newMux(server, store)

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

	log.Printf("Server with database integration starting on %s\n", addr)
	log.Println("Endpoints:")
	log.Println("  /iclock/*            - ZKTeco device endpoints")
	log.Println("  /api/attendance      - Query attendance records")
	log.Println("  /api/summary/today   - Get today's attendance summary")

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

// Example of how to use with an actual database (PostgreSQL):
/*
func initDB() (*sql.DB, error) {
	db, err := sql.Open("postgres", "postgres://user:password@localhost/attendance?sslmode=disable")
	if err != nil {
		return nil, err
	}

	// Create table if not exists
	schema := `
	CREATE TABLE IF NOT EXISTS attendance_records (
		id SERIAL PRIMARY KEY,
		user_id VARCHAR(50) NOT NULL,
		timestamp TIMESTAMP NOT NULL,
		status INTEGER NOT NULL,
		verify_mode INTEGER NOT NULL,
		work_code VARCHAR(50),
		device_sn VARCHAR(50) NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		INDEX idx_user_timestamp (user_id, timestamp),
		INDEX idx_device_timestamp (device_sn, timestamp)
	);
	`

	_, err = db.Exec(schema)
	return db, err
}
*/

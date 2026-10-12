// Command database demonstrates a ZKTeco ADMS server that persists attendance
// records to a SQLite database and exposes JSON query endpoints over them.
//
// Records survive restarts: they are stored in the file given by -db (default
// attendance.db). The SQLite driver is modernc.org/sqlite, a pure-Go
// implementation, so no cgo or system SQLite is needed. This example is its
// own Go module so the library itself stays dependency-free.
//
// SECURITY WARNING: This example does NOT include any authentication or
// authorization. The /api/attendance and /api/summary/today endpoints are
// publicly accessible and expose employee attendance data. In a production
// deployment you MUST add authentication middleware (e.g. API keys, OAuth2,
// mTLS) before exposing these routes to a network.
//
// Run with:
//
//	cd examples/database
//	go run . -addr :8080 -db attendance.db
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	zkadms "github.com/s0x90/zkteco-adms"
	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// maxLogBody is the maximum number of bytes logged for request/response bodies.
const maxLogBody = 1024

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

// schema creates the attendance table. The UNIQUE constraint makes saving
// idempotent: a device retries an ATTLOG upload after a 503, so the same
// punch can arrive more than once and must not be stored twice. Timestamps are
// stored as Unix seconds so range queries do not depend on a timezone.
const schema = `
CREATE TABLE IF NOT EXISTS attendance_records (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id     TEXT    NOT NULL,
	timestamp   INTEGER NOT NULL,
	status      INTEGER NOT NULL,
	verify_mode INTEGER NOT NULL,
	work_code   TEXT    NOT NULL DEFAULT '',
	device_sn   TEXT    NOT NULL,
	created_at  INTEGER NOT NULL DEFAULT (unixepoch()),
	UNIQUE (device_sn, user_id, timestamp)
);
CREATE INDEX IF NOT EXISTS idx_attendance_user_ts ON attendance_records (user_id, timestamp);
CREATE INDEX IF NOT EXISTS idx_attendance_ts ON attendance_records (timestamp);
`

// selectColumns is the column list shared by every query so scanRecords can
// rely on a fixed order.
const selectColumns = `SELECT user_id, timestamp, status, verify_mode, work_code, device_sn FROM attendance_records`

// AttendanceStore persists attendance records in SQLite.
type AttendanceStore struct {
	db *sql.DB
}

// NewAttendanceStore opens (creating if needed) the SQLite database at path
// and ensures the schema exists. Use ":memory:" for a throwaway database.
func NewAttendanceStore(path string) (*AttendanceStore, error) {
	// busy_timeout makes a locked database wait instead of failing at once, and
	// WAL lets readers proceed while a write is in flight.
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	if path == ":memory:" {
		dsn = "file::memory:?_pragma=busy_timeout(5000)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// A single connection keeps this example simple and correct: every
	// connection to ":memory:" would otherwise get its own empty database, and
	// SQLite allows only one writer at a time anyway.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	return &AttendanceStore{db: db}, nil
}

// Close releases the database. Call it only after the ADMS server has been
// closed, since callbacks still queued on the server write through this store.
func (s *AttendanceStore) Close() error {
	return s.db.Close()
}

// SaveAttendance inserts an attendance record. A record for the same device,
// user and timestamp that is already stored is ignored.
func (s *AttendanceStore) SaveAttendance(ctx context.Context, record zkadms.AttendanceRecord) error {
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO attendance_records
		 (user_id, timestamp, status, verify_mode, work_code, device_sn)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		record.UserID,
		record.Timestamp.Unix(),
		record.Status,
		record.VerifyMode,
		record.WorkCode,
		record.SerialNumber)
	if err != nil {
		return fmt.Errorf("insert attendance: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		log.Printf("Duplicate attendance ignored: User %s at %s from device %s",
			record.UserID, record.Timestamp.Format(time.RFC3339), record.SerialNumber)
		return nil
	}
	log.Printf("Saved attendance record: User %s at %s from device %s",
		record.UserID, record.Timestamp.Format(time.RFC3339), record.SerialNumber)
	return nil
}

// GetRecords returns all stored records, oldest first.
func (s *AttendanceStore) GetRecords(ctx context.Context) ([]zkadms.AttendanceRecord, error) {
	return s.query(ctx, selectColumns+` ORDER BY timestamp, id`)
}

// GetRecordsByUser returns records for a specific user, oldest first.
func (s *AttendanceStore) GetRecordsByUser(ctx context.Context, userID string) ([]zkadms.AttendanceRecord, error) {
	return s.query(ctx, selectColumns+` WHERE user_id = ? ORDER BY timestamp, id`, userID)
}

// GetRecordsByDateRange returns records with start <= timestamp < end, oldest
// first.
func (s *AttendanceStore) GetRecordsByDateRange(ctx context.Context, start, end time.Time) ([]zkadms.AttendanceRecord, error) {
	return s.query(ctx, selectColumns+` WHERE timestamp >= ? AND timestamp < ? ORDER BY timestamp, id`,
		start.Unix(), end.Unix())
}

// query runs a SELECT built from selectColumns and scans the rows.
func (s *AttendanceStore) query(ctx context.Context, q string, args ...any) ([]zkadms.AttendanceRecord, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query attendance: %w", err)
	}
	defer rows.Close()

	records := make([]zkadms.AttendanceRecord, 0)
	for rows.Next() {
		var (
			rec zkadms.AttendanceRecord
			ts  int64
		)
		if err := rows.Scan(&rec.UserID, &ts, &rec.Status, &rec.VerifyMode, &rec.WorkCode, &rec.SerialNumber); err != nil {
			return nil, fmt.Errorf("scan attendance: %w", err)
		}
		rec.Timestamp = time.Unix(ts, 0).UTC()
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate attendance: %w", err)
	}
	return records, nil
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

// serverError logs err and replies with a generic 500 so database details are
// not leaked to API clients.
func serverError(w http.ResponseWriter, msg string, err error) {
	log.Printf("%s: %v", msg, err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

// attendanceHandler returns an http.Handler that responds with JSON
// attendance records. If the "user_id" query parameter is set, only
// records for that user are returned; otherwise all records are returned.
func attendanceHandler(store *AttendanceStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID := r.URL.Query().Get("user_id")

		var resp attendanceResponse
		if userID != "" {
			records, err := store.GetRecordsByUser(r.Context(), userID)
			if err != nil {
				serverError(w, "Error querying attendance", err)
				return
			}
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
			records, err := store.GetRecords(r.Context())
			if err != nil {
				serverError(w, "Error querying attendance", err)
				return
			}
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

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			log.Printf("Error encoding attendance response: %v", err)
		}
	})
}

// summaryHandler returns an http.Handler that responds with a JSON daily
// summary of today's attendance records.
func summaryHandler(store *AttendanceStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		endOfDay := startOfDay.Add(24 * time.Hour)

		records, err := store.GetRecordsByDateRange(r.Context(), startOfDay, endOfDay)
		if err != nil {
			serverError(w, "Error querying summary", err)
			return
		}

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

// run opens the SQLite attendance store, creates the ADMS server, wires HTTP
// routes, and blocks until ctx is canceled. On cancellation the HTTP server is
// gracefully shut down, the ADMS callback queue is drained, and only then is
// the database closed.
func run(ctx context.Context, addr, dbPath string) error {
	store, err := NewAttendanceStore(dbPath)
	if err != nil {
		return err
	}
	// Deferred first so it runs last: queued callbacks write to the store
	// while server.Close drains them.
	defer store.Close()

	// Create the ADMS server with functional options
	server := zkadms.NewADMSServer(
		// Save attendance records to the store
		zkadms.WithOnAttendance(func(ctx context.Context, record zkadms.AttendanceRecord) {
			if err := store.SaveAttendance(ctx, record); err != nil {
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

	log.Printf("Server with SQLite storage (%s) starting on %s\n", dbPath, addr)
	log.Println("Endpoints:")
	log.Println("  /iclock/*            - ZKTeco device endpoints")
	log.Println("  /api/attendance      - Query attendance records")
	log.Println("  /api/summary/today   - Get today's attendance summary")

	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	log.Println("server stopped")
	return nil
}

func main() {
	addr := flag.String("addr", ":8080", "address to listen on")
	dbPath := flag.String("db", "attendance.db", "path to the SQLite database file")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *addr, *dbPath); err != nil {
		log.Fatal(err)
	}
}

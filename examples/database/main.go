// Command database demonstrates a ZKTeco ADMS server with an in-memory
// attendance store and JSON query endpoints, modeled after a typical
// database-backed integration.
//
// Run with:
//
//	go run ./examples/database
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"sync"
	"time"

	zkadms "github.com/s0x90/zkteco-adms"
	// Uncomment when using actual database
	// _ "github.com/lib/pq"
)

// statusRecorder wraps http.ResponseWriter to capture the status code.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// logMiddleware logs each HTTP request with method, path, remote address,
// response status code, and duration.
func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		defer func() {
			slog.Info("http request",
				"method", r.Method,
				"path", r.URL.Path,
				"remote", r.RemoteAddr,
				"status", rec.status,
				"duration", time.Since(start),
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

func main() {
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

	// Set up HTTP routes
	http.Handle("/iclock/", logMiddleware(server))

	// API endpoint to query attendance records
	http.Handle("/api/attendance", logMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	})))

	// API endpoint to get daily summary
	http.Handle("/api/summary/today", logMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	})))

	addr := ":8080"
	log.Printf("Server with database integration starting on %s\n", addr)
	log.Println("Endpoints:")
	log.Println("  /iclock/*            - ZKTeco device endpoints")
	log.Println("  /api/attendance      - Query attendance records")
	log.Println("  /api/summary/today   - Get today's attendance summary")

	if err := http.ListenAndServe(addr, nil); err != nil {
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

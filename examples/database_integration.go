package main

import (
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	zkdevicesync "github.com/s0x90/zk-device-sync"
	
	// Uncomment when using actual database
	// _ "github.com/lib/pq"
)

// AttendanceStore demonstrates how to integrate with a database
type AttendanceStore struct {
	mu      sync.Mutex
	records []zkdevicesync.AttendanceRecord
	// In a real application, you would use an actual database connection
	// db *sql.DB
}

func NewAttendanceStore() *AttendanceStore {
	return &AttendanceStore{
		records: make([]zkdevicesync.AttendanceRecord, 0),
	}
}

// SaveAttendance saves an attendance record
func (s *AttendanceStore) SaveAttendance(record zkdevicesync.AttendanceRecord) error {
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

// GetRecords returns all stored records
func (s *AttendanceStore) GetRecords() []zkdevicesync.AttendanceRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	
	result := make([]zkdevicesync.AttendanceRecord, len(s.records))
	copy(result, s.records)
	return result
}

// GetRecordsByUser returns records for a specific user
func (s *AttendanceStore) GetRecordsByUser(userID string) []zkdevicesync.AttendanceRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	
	var result []zkdevicesync.AttendanceRecord
	for _, record := range s.records {
		if record.UserID == userID {
			result = append(result, record)
		}
	}
	return result
}

// GetRecordsByDateRange returns records within a date range
func (s *AttendanceStore) GetRecordsByDateRange(start, end time.Time) []zkdevicesync.AttendanceRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	
	var result []zkdevicesync.AttendanceRecord
	for _, record := range s.records {
		if record.Timestamp.After(start) && record.Timestamp.Before(end) {
			result = append(result, record)
		}
	}
	return result
}

func main() {
	// Initialize the attendance store
	store := NewAttendanceStore()

	// Create the iclock server
	server := zkdevicesync.NewIClockServer()

	// Set up attendance callback to save to database
	server.OnAttendance = func(record zkdevicesync.AttendanceRecord) {
		if err := store.SaveAttendance(record); err != nil {
			log.Printf("Error saving attendance: %v", err)
		}
	}

	// Set up device info callback
	server.OnDeviceInfo = func(sn string, info map[string]string) {
		log.Printf("Device %s info updated: %v", sn, info)
	}

	// Set up HTTP routes
	http.Handle("/iclock/", server)

	// API endpoint to query attendance records
	http.HandleFunc("/api/attendance", func(w http.ResponseWriter, r *http.Request) {
		userID := r.URL.Query().Get("user_id")
		
		w.Header().Set("Content-Type", "application/json")
		
		if userID != "" {
			records := store.GetRecordsByUser(userID)
			fmt.Fprintf(w, `{"user_id": "%s", "count": %d, "records": [`, userID, len(records))
			for i, record := range records {
				if i > 0 {
					fmt.Fprint(w, ",")
				}
				fmt.Fprintf(w, `{"timestamp": "%s", "status": %d, "device": "%s"}`,
					record.Timestamp.Format(time.RFC3339),
					record.Status,
					record.SerialNumber)
			}
			fmt.Fprint(w, "]}")
		} else {
			records := store.GetRecords()
			fmt.Fprintf(w, `{"total": %d, "records": [`, len(records))
			for i, record := range records {
				if i > 0 {
					fmt.Fprint(w, ",")
				}
				fmt.Fprintf(w, `{"user_id": "%s", "timestamp": "%s", "status": %d, "device": "%s"}`,
					record.UserID,
					record.Timestamp.Format(time.RFC3339),
					record.Status,
					record.SerialNumber)
			}
			fmt.Fprint(w, "]}")
		}
	})

	// API endpoint to get daily summary
	http.HandleFunc("/api/summary/today", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		endOfDay := startOfDay.Add(24 * time.Hour)
		
		records := store.GetRecordsByDateRange(startOfDay, endOfDay)
		
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"date": "%s", "count": %d, "records": [`,
			startOfDay.Format("2006-01-02"), len(records))
		
		for i, record := range records {
			if i > 0 {
				fmt.Fprint(w, ",")
			}
			fmt.Fprintf(w, `{"user_id": "%s", "timestamp": "%s", "status": %d}`,
				record.UserID,
				record.Timestamp.Format(time.RFC3339),
				record.Status)
		}
		fmt.Fprint(w, "]}")
	})

	addr := ":8080"
	log.Printf("Server with database integration starting on %s\n", addr)
	log.Println("Endpoints:")
	log.Println("  /iclock/* - ZKTeco device endpoints")
	log.Println("  /api/attendance - Query attendance records")
	log.Println("  /api/summary/today - Get today's attendance summary")
	
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

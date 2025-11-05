package main

import (
	"fmt"
	"log"
	"net/http"
	"time"

	zkdevicesync "github.com/s0x90/zk-device-sync"
)

func main() {
	// Create a new iclock server
	server := zkdevicesync.NewIClockServer()

	// Set up callback for attendance records
	server.OnAttendance = func(record zkdevicesync.AttendanceRecord) {
		fmt.Printf("Attendance Record:\n")
		fmt.Printf("  Device: %s\n", record.SerialNumber)
		fmt.Printf("  User ID: %s\n", record.UserID)
		fmt.Printf("  Timestamp: %s\n", record.Timestamp.Format(time.RFC3339))
		fmt.Printf("  Status: %d (0=Check In, 1=Check Out)\n", record.Status)
		fmt.Printf("  Verify Mode: %d (0=Password, 1=Fingerprint, 2=Card)\n", record.VerifyMode)
		fmt.Printf("  Work Code: %s\n", record.WorkCode)
		fmt.Println("---")
	}

	// Set up callback for device information
	server.OnDeviceInfo = func(sn string, info map[string]string) {
		fmt.Printf("Device Info for %s:\n", sn)
		for key, value := range info {
			fmt.Printf("  %s: %s\n", key, value)
		}
		fmt.Println("---")
	}

	// Register some known devices (optional)
	server.RegisterDevice("DEVICE001")
	server.RegisterDevice("DEVICE002")

	// Set up HTTP routes
	http.Handle("/iclock/", server)

	// Add a status endpoint to view connected devices
	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		devices := server.ListDevices()
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "Connected Devices: %d\n\n", len(devices))
		for _, device := range devices {
			fmt.Fprintf(w, "Serial Number: %s\n", device.SerialNumber)
			fmt.Fprintf(w, "Last Activity: %s\n", device.LastActivity.Format(time.RFC3339))
			fmt.Fprintf(w, "Options: %v\n", device.Options)
			fmt.Fprintln(w, "---")
		}
	})

	// Add a command endpoint to send commands to devices
	http.HandleFunc("/command", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		sn := r.URL.Query().Get("sn")
		cmd := r.URL.Query().Get("cmd")

		if sn == "" || cmd == "" {
			http.Error(w, "Missing sn or cmd parameter", http.StatusBadRequest)
			return
		}

		server.SendCommand(sn, cmd)
		fmt.Fprintf(w, "Command queued for device %s: %s\n", sn, cmd)
	})

	// Start the server
	addr := ":8080"
	fmt.Printf("ZKTeco iClock Server starting on %s\n", addr)
	fmt.Println("Endpoints:")
	fmt.Println("  /iclock/cdata - Attendance data endpoint")
	fmt.Println("  /iclock/getrequest - Device polling endpoint")
	fmt.Println("  /iclock/devicecmd - Command confirmation endpoint")
	fmt.Println("  /status - View connected devices")
	fmt.Println("  /command - Send commands to devices (POST)")
	fmt.Println()

	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal(err)
	}
}

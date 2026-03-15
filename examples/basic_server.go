//go:build basic_server
// +build basic_server

package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	zkdevicesync "github.com/s0x90/zkteco-sync"
)

func main() {
	// Create a new ADMS server with functional options
	server := zkdevicesync.NewADMSServer(
		// Enable the /iclock/inspect debug endpoint.
		zkdevicesync.WithEnableInspect(),

		// Callback for attendance records
		zkdevicesync.WithOnAttendance(func(ctx context.Context, record zkdevicesync.AttendanceRecord) {
			fmt.Printf("Attendance Record:\n")
			fmt.Printf("  Device: %s\n", record.SerialNumber)
			fmt.Printf("  User ID: %s\n", record.UserID)
			fmt.Printf("  Timestamp: %s\n", record.Timestamp.Format(time.RFC3339))
			fmt.Printf("  Status: %d (0=Check In, 1=Check Out)\n", record.Status)
			fmt.Printf("  Verify Mode: %d (0=Password, 1=Fingerprint, 2=Card)\n", record.VerifyMode)
			fmt.Printf("  Work Code: %s\n", record.WorkCode)
			fmt.Println("---")
		}),

		// Callback for device information
		zkdevicesync.WithOnDeviceInfo(func(ctx context.Context, sn string, info map[string]string) {
			fmt.Printf("Device Info for %s:\n", sn)
			for key, value := range info {
				fmt.Printf("  %s: %s\n", key, value)
			}
			fmt.Println("---")
		}),

		// Callback for device registry/capabilities
		zkdevicesync.WithOnRegistry(func(ctx context.Context, sn string, info map[string]string) {
			fmt.Printf("Registry Info for %s (partial):\n", sn)
			shown := 0
			for k, v := range info {
				fmt.Printf("  %s: %s\n", k, v)
				shown++
				if shown >= 8 { // avoid spamming console
					break
				}
			}
			fmt.Println("---")
		}),
	)
	defer server.Close()

	// Register some known devices (optional).
	// RegisterDevice now returns an error for invalid serial numbers or
	// when the device limit (WithMaxDevices) has been reached.
	for _, sn := range []string{"DEVICE001", "DEVICE002"} {
		if err := server.RegisterDevice(sn); err != nil {
			log.Fatalf("failed to register device %s: %v", sn, err)
		}
	}

	// Set up HTTP routes
	http.Handle("/iclock/", server)

	// Add a status endpoint to view connected devices
	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
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

		if err := server.QueueCommand(sn, cmd); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(w, "Command queued for device %s: %s\n", sn, cmd)
	})

	// Start the server
	addr := ":8080"
	fmt.Printf("ZKTeco iClock Server starting on %s\n", addr)
	fmt.Println("Endpoints:")
	fmt.Println("  /iclock/cdata - Attendance data endpoint")
	fmt.Println("  /iclock/registry - Device registry/capabilities endpoint")
	fmt.Println("  /iclock/getrequest - Device polling endpoint")
	fmt.Println("  /iclock/devicecmd - Command confirmation endpoint")
	fmt.Println("  /iclock/inspect - JSON device snapshot (opt-in via WithEnableInspect)")
	fmt.Println("  /status - View connected devices")
	fmt.Println("  /command - Send commands to devices (POST)")
	fmt.Println()

	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal(err)
	}
}

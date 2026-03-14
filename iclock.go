// Package zkdevicesync provides an implementation of the iClock protocol
// for ZKTeco biometric attendance devices.
//
// The iClock protocol is an HTTP-based protocol used by ZKTeco devices to
// communicate with servers for sending attendance data and receiving commands.
//
// Basic usage:
//
//	server := zkdevicesync.NewIClockServer()
//	server.OnAttendance = func(record zkdevicesync.AttendanceRecord) {
//	    fmt.Printf("User %s at %s\n", record.UserID, record.Timestamp)
//	}
//	http.Handle("/iclock/", server)
//	http.ListenAndServe(":8080", nil)
//
// The server implements three main endpoints:
//   - /iclock/cdata - receives attendance logs and device data
//   - /iclock/getrequest - handles device polling for commands
//   - /iclock/devicecmd - receives command execution confirmations
//
// Call Close when the server is no longer needed to drain the callback queue.
package zkdevicesync

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Device represents a ZKTeco device
type Device struct {
	SerialNumber string
	LastActivity time.Time
	Options      map[string]string
}

// copy returns a deep copy of the Device, including its Options map.
func (d *Device) copy() *Device {
	return &Device{
		SerialNumber: d.SerialNumber,
		LastActivity: d.LastActivity,
		Options:      maps.Clone(d.Options),
	}
}

// AttendanceRecord represents an attendance transaction from the device
type AttendanceRecord struct {
	UserID       string
	Timestamp    time.Time
	Status       int // 0=Check In, 1=Check Out, 2=Break Out, 3=Break In, etc.
	VerifyMode   int // 0=Password, 1=Fingerprint, 2=Card, etc.
	WorkCode     string
	SerialNumber string
}

// DeviceSnapshot is the JSON representation of a device in the /iclock/inspect response.
type DeviceSnapshot struct {
	Serial       string            `json:"serial"`
	LastActivity string            `json:"lastActivity"`
	Online       bool              `json:"online"`
	Options      map[string]string `json:"options"`
}

// IClockServer manages communication with ZKTeco devices using the iclock protocol.
//
// Callbacks (OnAttendance, OnDeviceInfo, OnRegistry) are dispatched asynchronously
// via an internal worker goroutine so they never block device HTTP responses.
// Call Close to drain the callback queue when the server is shutting down.
type IClockServer struct {
	devices      map[string]*Device
	devicesMutex sync.RWMutex
	commandQueue map[string][]string // Serial number -> commands queue
	queueMutex   sync.RWMutex
	OnAttendance func(record AttendanceRecord)
	OnDeviceInfo func(sn string, info map[string]string)
	OnRegistry   func(sn string, info map[string]string)
	logger       *log.Logger

	callbackCh   chan func()
	callbackDone chan struct{}
	closeCh      chan struct{}
	closeOnce    sync.Once
}

// NewIClockServer creates a new iclock server instance.
// Call Close when the server is no longer needed.
func NewIClockServer() *IClockServer {
	s := &IClockServer{
		devices:      make(map[string]*Device),
		commandQueue: make(map[string][]string),
		logger:       log.Default(),
		callbackCh:   make(chan func(), 256),
		callbackDone: make(chan struct{}),
		closeCh:      make(chan struct{}),
	}
	go s.callbackWorker()
	return s
}

// Close drains the callback queue and stops the worker goroutine.
// It blocks until all pending callbacks have been executed.
// Close is safe to call multiple times.
func (s *IClockServer) Close() {
	s.closeOnce.Do(func() {
		close(s.closeCh)
		<-s.callbackDone
	})
}

// callbackWorker processes queued callbacks sequentially.
// Panics in user callbacks are recovered so the worker stays alive.
func (s *IClockServer) callbackWorker() {
	defer close(s.callbackDone)
	for {
		select {
		case fn := <-s.callbackCh:
			s.safeCall(fn)
		case <-s.closeCh:
			// Drain remaining callbacks before exiting.
			for {
				select {
				case fn := <-s.callbackCh:
					s.safeCall(fn)
				default:
					return
				}
			}
		}
	}
}

// safeCall executes fn, recovering from any panic.
func (s *IClockServer) safeCall(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Printf("[Callback] panic recovered: %v", r)
		}
	}()
	fn()
}

// dispatchCallback enqueues a callback for asynchronous execution.
// If the callback channel is full or the server is closed, the event is dropped.
func (s *IClockServer) dispatchCallback(fn func()) {
	// Fast-path: if the server is already closing/closed, drop the callback.
	select {
	case <-s.closeCh:
		return
	default:
	}

	// Server is not closed at this point; try to enqueue without blocking.
	select {
	case s.callbackCh <- fn:
	default:
		s.logger.Printf("[Callback] WARNING: callback queue full, dropping event")
	}
}

// RegisterDevice registers a new device
func (s *IClockServer) RegisterDevice(serialNumber string) {
	s.devicesMutex.Lock()
	defer s.devicesMutex.Unlock()

	if _, exists := s.devices[serialNumber]; !exists {
		s.devices[serialNumber] = &Device{
			SerialNumber: serialNumber,
			LastActivity: time.Now(),
			Options:      make(map[string]string),
		}
	}
}

// GetDevice retrieves a copy of device information.
// Returns nil if the device is not registered.
func (s *IClockServer) GetDevice(serialNumber string) *Device {
	s.devicesMutex.RLock()
	defer s.devicesMutex.RUnlock()
	d := s.devices[serialNumber]
	if d == nil {
		return nil
	}
	return d.copy()
}

// IsDeviceOnline reports whether the device has been active within the last 2 minutes.
func (s *IClockServer) IsDeviceOnline(serialNumber string) bool {
	s.devicesMutex.RLock()
	defer s.devicesMutex.RUnlock()
	d := s.devices[serialNumber]
	return s.isDeviceOnline(d)
}

// QueueCommand adds a command to be sent to the device
func (s *IClockServer) QueueCommand(serialNumber, command string) {
	s.queueMutex.Lock()
	defer s.queueMutex.Unlock()

	s.commandQueue[serialNumber] = append(s.commandQueue[serialNumber], command)
}

// GetCommands retrieves pending commands for a device
func (s *IClockServer) GetCommands(serialNumber string) []string {
	s.queueMutex.Lock()
	defer s.queueMutex.Unlock()

	commands := s.commandQueue[serialNumber]
	delete(s.commandQueue, serialNumber)
	return commands
}

// HandleCData handles the /iclock/cdata endpoint for attendance data
func (s *IClockServer) HandleCData(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	query := r.URL.Query()
	serialNumber := query.Get("SN")
	if serialNumber == "" {
		http.Error(w, "Missing SN parameter", http.StatusBadRequest)
		return
	}

	// Register/update device
	s.RegisterDevice(serialNumber)
	s.updateDeviceActivity(serialNumber)

	// Logging basic request info
	s.logger.Printf("[iClock Protocol] %s %s - Device: %s", r.Method, r.URL.Path, serialNumber)
	for _, k := range []string{"options", "pushver", "PushOptionsFlag", "table"} {
		if v := query.Get(k); v != "" {
			s.logger.Printf("[iClock Protocol]   %s: %s", k, v)
		}
	}

	// Handle different table types
	table := query.Get("table")

	switch table {
	case "ATTLOG":
		// Parse attendance log data
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Failed to read body", http.StatusBadRequest)
			return
		}

		// Log truncated body size/content
		if len(body) > 0 {
			preview := string(body)
			if len(preview) > 200 {
				preview = preview[:200] + "..."
			}
			s.logger.Printf("[iClock Protocol]   ATTLOG body (truncated): %s", preview)
		}

		records := s.parseAttendanceRecords(string(body), serialNumber)
		if cb := s.OnAttendance; cb != nil {
			for _, record := range records {
				s.dispatchCallback(func() { cb(record) })
			}
		}

		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "OK: %d", len(records))

	case "OPERLOG":
		// Operation log - acknowledge
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "OK")

	default:
		// Handle INFO or other requests
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "Failed to read body", http.StatusBadRequest)
				return
			}
			if cb := s.OnDeviceInfo; len(body) > 0 && cb != nil {
				info := s.parseDeviceInfo(string(body))
				sn := serialNumber
				s.dispatchCallback(func() { cb(sn, info) })
			}
			if len(body) > 0 {
				preview := string(body)
				if len(preview) > 200 {
					preview = preview[:200] + "..."
				}
				s.logger.Printf("[iClock Protocol]   INFO body (truncated): %s", preview)
			}
		}

		// Check for pending commands
		commands := s.GetCommands(serialNumber)
		if len(commands) > 0 {
			w.WriteHeader(http.StatusOK)
			for _, cmd := range commands {
				fmt.Fprintf(w, "C:%s\n", cmd)
			}
		} else {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "OK")
		}
	}
}

// HandleGetRequest handles the /iclock/getrequest endpoint
func (s *IClockServer) HandleGetRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	serialNumber := r.URL.Query().Get("SN")
	if serialNumber == "" {
		http.Error(w, "Missing SN parameter", http.StatusBadRequest)
		return
	}

	s.RegisterDevice(serialNumber)
	s.updateDeviceActivity(serialNumber)

	s.logger.Printf("[iClock Protocol] %s %s - Device: %s", r.Method, r.URL.Path, serialNumber)

	// Check for pending commands
	commands := s.GetCommands(serialNumber)
	if len(commands) > 0 {
		w.WriteHeader(http.StatusOK)
		for _, cmd := range commands {
			fmt.Fprintf(w, "C:%s\n", cmd)
		}
	} else {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "OK")
	}
}

// HandleDeviceCmd handles the /iclock/devicecmd endpoint
func (s *IClockServer) HandleDeviceCmd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	serialNumber := r.URL.Query().Get("SN")
	if serialNumber == "" {
		http.Error(w, "Missing SN parameter", http.StatusBadRequest)
		return
	}

	s.RegisterDevice(serialNumber)
	s.updateDeviceActivity(serialNumber)

	s.logger.Printf("[iClock Protocol] %s %s - Device: %s", r.Method, r.URL.Path, serialNumber)

	// Device is reporting command execution result
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}
	if len(body) > 0 {
		preview := string(body)
		if len(preview) > 200 {
			preview = preview[:200] + "..."
		}
		s.logger.Printf("[iClock Protocol]   devicecmd body (truncated): %s", preview)
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "OK: Command received: %s", string(body))
}

// updateDeviceActivity updates the last activity timestamp for a device
func (s *IClockServer) updateDeviceActivity(serialNumber string) {
	s.devicesMutex.Lock()
	defer s.devicesMutex.Unlock()

	if device, exists := s.devices[serialNumber]; exists {
		wasOnline := s.isDeviceOnline(device)
		device.LastActivity = time.Now()
		if !wasOnline {
			s.logger.Printf("[Heartbeat] Device %s marked as online", serialNumber)
		} else {
			s.logger.Printf("[Heartbeat] Device %s activity", serialNumber)
		}
	}
}

func (s *IClockServer) isDeviceOnline(device *Device) bool {
	if device == nil {
		return false
	}
	return time.Since(device.LastActivity) <= 2*time.Minute
}

// parseAttendanceRecords parses attendance records from the device data
func (s *IClockServer) parseAttendanceRecords(data string, serialNumber string) []AttendanceRecord {
	var records []AttendanceRecord
	for line := range strings.SplitSeq(strings.TrimSpace(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) >= 2 {
			record := AttendanceRecord{SerialNumber: serialNumber}
			record.UserID = parts[0]
			if timestamp, err := time.Parse("2006-01-02 15:04:05", parts[1]); err == nil {
				record.Timestamp = timestamp
			} else if ts, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
				record.Timestamp = time.Unix(ts, 0)
			}
			if len(parts) >= 3 {
				record.Status, _ = strconv.Atoi(parts[2])
			}
			if len(parts) >= 4 {
				record.VerifyMode, _ = strconv.Atoi(parts[3])
			}
			if len(parts) >= 5 {
				record.WorkCode = parts[4]
			}
			records = append(records, record)
		}
	}
	return records
}

// parseDeviceInfo parses device information from POST data
func (s *IClockServer) parseDeviceInfo(data string) map[string]string {
	info := make(map[string]string)

	for line := range strings.SplitSeq(strings.TrimSpace(data), "\n") {
		line = strings.TrimSpace(line)
		if key, value, ok := strings.Cut(line, "="); ok {
			info[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}

	return info
}

// ServeHTTP implements http.Handler interface for convenient routing
func (s *IClockServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/iclock/cdata"):
		s.HandleCData(w, r)
	case strings.HasSuffix(path, "/iclock/getrequest"):
		s.HandleGetRequest(w, r)
	case strings.HasSuffix(path, "/iclock/devicecmd"):
		s.HandleDeviceCmd(w, r)
	case strings.HasSuffix(path, "/iclock/registry"):
		s.HandleRegistry(w, r)
	case strings.HasSuffix(path, "/iclock/inspect"):
		s.HandleInspect(w, r)
	default:
		http.NotFound(w, r)
	}
}

// SendCommand sends a command to a device (to be retrieved on next poll)
func (s *IClockServer) SendCommand(serialNumber, command string) {
	s.QueueCommand(serialNumber, command)
}

// SendDataCommand formats and sends a DATA command
func (s *IClockServer) SendDataCommand(serialNumber, table, data string) {
	cmd := fmt.Sprintf("DATA QUERY %s\n%s", table, data)
	s.SendCommand(serialNumber, cmd)
}

// SendInfoCommand requests device information
func (s *IClockServer) SendInfoCommand(serialNumber string) {
	s.SendCommand(serialNumber, "INFO")
}

// ParseQueryParams parses URL query parameters commonly used in iclock protocol
func ParseQueryParams(urlStr string) (map[string]string, error) {
	u, err := url.Parse(urlStr)
	if err != nil {
		return nil, err
	}

	params := make(map[string]string)
	for key, values := range u.Query() {
		if len(values) > 0 {
			params[key] = values[0]
		}
	}

	return params, nil
}

// ListDevices returns copies of all registered devices.
// The returned slice and Device values are safe to use without additional locking.
func (s *IClockServer) ListDevices() []*Device {
	s.devicesMutex.RLock()
	defer s.devicesMutex.RUnlock()

	devices := make([]*Device, 0, len(s.devices))
	for _, device := range s.devices {
		devices = append(devices, device.copy())
	}

	return devices
}

// --- Additional Handlers & Parsers ---

// HandleRegistry processes /iclock/registry requests for device registration & capabilities
func (s *IClockServer) HandleRegistry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	query := r.URL.Query()
	serialNumber := query.Get("SN")
	if serialNumber == "" {
		http.Error(w, "Missing SN parameter", http.StatusBadRequest)
		return
	}
	s.RegisterDevice(serialNumber)
	s.updateDeviceActivity(serialNumber)
	s.logger.Printf("[iClock Protocol] %s %s - Device: %s", r.Method, r.URL.Path, serialNumber)
	for _, k := range []string{"options", "pushver", "PushOptionsFlag"} {
		if v := query.Get(k); v != "" {
			s.logger.Printf("[iClock Protocol]   %s: %s", k, v)
		}
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}
	if len(body) > 0 {
		preview := string(body)
		if len(preview) > 300 {
			preview = preview[:300] + "..."
		}
		s.logger.Printf("[iClock Protocol]   Body (truncated): %s", preview)
		info := s.parseRegistryBody(string(body))
		s.devicesMutex.Lock()
		if dev := s.devices[serialNumber]; dev != nil {
			maps.Copy(dev.Options, info)
		}
		s.devicesMutex.Unlock()
		if cb := s.OnRegistry; cb != nil {
			sn := serialNumber
			s.dispatchCallback(func() { cb(sn, info) })
		}
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "OK")
}

// HandleInspect serves /iclock/inspect returning JSON device snapshot
func (s *IClockServer) HandleInspect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	snapshot := struct {
		Devices []DeviceSnapshot `json:"devices"`
		Count   int              `json:"count"`
		Time    time.Time        `json:"time"`
	}{Devices: []DeviceSnapshot{}, Time: time.Now()}

	s.devicesMutex.RLock()
	for _, d := range s.devices {
		dc := d.copy()
		snap := DeviceSnapshot{
			Serial:       dc.SerialNumber,
			LastActivity: dc.LastActivity.Format(time.RFC3339),
			Online:       s.isDeviceOnline(d),
			Options:      dc.Options,
		}
		snapshot.Devices = append(snapshot.Devices, snap)
	}
	snapshot.Count = len(snapshot.Devices)
	s.devicesMutex.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(snapshot); err != nil {
		http.Error(w, "failed to encode response", http.StatusInternalServerError)
		return
	}
}

// parseRegistryBody parses comma-separated key=value pairs from registry POST body
func (s *IClockServer) parseRegistryBody(data string) map[string]string {
	info := make(map[string]string)
	for part := range strings.SplitSeq(data, ",") {
		part = strings.TrimSpace(part)
		if key, value, ok := strings.Cut(part, "="); ok {
			info[strings.TrimPrefix(strings.TrimSpace(key), "~")] = strings.TrimSpace(value)
		}
	}
	return info
}

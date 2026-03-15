// Package zkdevicesync provides an implementation of the ADMS protocol
// for ZKTeco biometric attendance devices.
//
// The ADMS protocol is an HTTP-based protocol used by ZKTeco devices to
// communicate with servers for sending attendance data and receiving commands.
//
// Basic usage:
//
//	server := zkdevicesync.NewADMSServer()
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

// ADMSServer manages communication with ZKTeco devices using the ADMS protocol.
//
// Callbacks (OnAttendance, OnDeviceInfo, OnRegistry) are dispatched asynchronously
// via an internal worker goroutine so they never block device HTTP responses.
// Call Close to drain the callback queue when the server is shutting down.
type ADMSServer struct {
	devices      map[string]*Device
	devicesMutex sync.RWMutex
	commandQueue map[string][]string // Serial number -> commands queue
	queueMutex   sync.RWMutex

	// OnAttendance is called for each attendance record received.
	// Records from a single HTTP request are dispatched as a batch internally,
	// but this callback is invoked once per record.
	// Set this field before passing the server to http.ListenAndServe or
	// otherwise handling requests; it is read without synchronization from
	// HTTP handler goroutines.
	OnAttendance func(record AttendanceRecord)

	// OnDeviceInfo is called when a device posts its info parameters.
	// Set this field before serving; it is read without synchronization.
	OnDeviceInfo func(sn string, info map[string]string)

	// OnRegistry is called when a device registers or re-registers.
	// Set this field before serving; it is read without synchronization.
	OnRegistry func(sn string, info map[string]string)

	logger *log.Logger

	callbackCh   chan func()
	callbackMu   sync.RWMutex // guards callbackCh close in coordination with dispatchers
	callbackDone chan struct{}
	closeCh      chan struct{}
	closeOnce    sync.Once
}

// NewADMSServer creates a new  server instance.
// Set callback fields (OnAttendance, OnDeviceInfo, OnRegistry) before
// passing the server to http.ListenAndServe. Call Close when the server
// is no longer needed to drain pending callbacks and stop the worker.
func NewADMSServer() *ADMSServer {
	s := &ADMSServer{
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
func (s *ADMSServer) Close() {
	s.closeOnce.Do(func() {
		// Signal all dispatchers to stop accepting new callbacks.
		close(s.closeCh)
		// Wait for any in-flight dispatchCallback calls to finish sending.
		// RLock holders (dispatchers) will see closeCh and exit their select.
		s.callbackMu.Lock()
		// Now safe: no dispatcher can be mid-send on callbackCh.
		close(s.callbackCh)
		s.callbackMu.Unlock()
		// Wait for the worker to drain all remaining callbacks and exit.
		<-s.callbackDone
	})
}

// callbackWorker processes queued callbacks sequentially.
// It exits when callbackCh is closed and fully drained.
// Panics in user callbacks are recovered so the worker stays alive.
func (s *ADMSServer) callbackWorker() {
	defer close(s.callbackDone)
	for fn := range s.callbackCh {
		s.safeCall(fn)
	}
}

// safeCall executes fn, recovering from any panic.
func (s *ADMSServer) safeCall(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Printf("[Callback] panic recovered: %v", r)
		}
	}()
	fn()
}

// dispatchCallback enqueues a callback for asynchronous execution.
// It blocks for up to 1 second if the channel is full before giving up.
// Returns true if the callback was enqueued, false if the server is closed
// or the timeout expired.
func (s *ADMSServer) dispatchCallback(fn func()) bool {
	s.callbackMu.RLock()
	defer s.callbackMu.RUnlock()

	// Fast-path: if the server is already closing/closed, reject the callback.
	select {
	case <-s.closeCh:
		return false
	default:
	}

	// Fast-path: non-blocking send when channel has capacity.
	select {
	case s.callbackCh <- fn:
		return true
	default:
	}

	// Slow-path: channel is full, block up to 1s before giving up.
	timer := time.NewTimer(1 * time.Second)
	defer timer.Stop()

	select {
	case s.callbackCh <- fn:
		return true
	case <-timer.C:
		s.logger.Printf("[Callback] WARNING: callback queue full after 1s timeout, dropping event")
		return false
	case <-s.closeCh:
		return false
	}
}

// RegisterDevice registers a new device
func (s *ADMSServer) RegisterDevice(serialNumber string) {
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
func (s *ADMSServer) GetDevice(serialNumber string) *Device {
	s.devicesMutex.RLock()
	defer s.devicesMutex.RUnlock()
	d := s.devices[serialNumber]
	if d == nil {
		return nil
	}
	return d.copy()
}

// IsDeviceOnline reports whether the device has been active within the last 2 minutes.
func (s *ADMSServer) IsDeviceOnline(serialNumber string) bool {
	s.devicesMutex.RLock()
	defer s.devicesMutex.RUnlock()
	d := s.devices[serialNumber]
	return s.isDeviceOnline(d)
}

// QueueCommand adds a command to be sent to the device
func (s *ADMSServer) QueueCommand(serialNumber, command string) {
	s.queueMutex.Lock()
	defer s.queueMutex.Unlock()

	s.commandQueue[serialNumber] = append(s.commandQueue[serialNumber], command)
}

// GetCommands retrieves pending commands for a device
func (s *ADMSServer) GetCommands(serialNumber string) []string {
	s.queueMutex.Lock()
	defer s.queueMutex.Unlock()

	commands := s.commandQueue[serialNumber]
	delete(s.commandQueue, serialNumber)
	return commands
}

// HandleCData handles the /iclock/cdata endpoint for attendance data
func (s *ADMSServer) HandleCData(w http.ResponseWriter, r *http.Request) {
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
	s.logger.Printf("[ADMS Protocol] %s %s - Device: %s", r.Method, r.URL.Path, serialNumber)
	for _, k := range []string{"options", "pushver", "PushOptionsFlag", "table"} {
		if v := query.Get(k); v != "" {
			s.logger.Printf("[ADMS Protocol]   %s: %s", k, v)
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
			s.logger.Printf("[ADMS Protocol]   ATTLOG body (truncated): %s", preview)
		}

		records := s.parseAttendanceRecords(string(body), serialNumber)
		if cb := s.OnAttendance; cb != nil && len(records) > 0 {
			batch := records // capture for closure
			ok := s.dispatchCallback(func() {
				for _, record := range batch {
					s.safeCall(func() { cb(record) })
				}
			})
			if !ok {
				s.logger.Printf("[ADMS Protocol] FAIL: callback queue full, %d records from device %s not processed",
					len(records), serialNumber)
				http.Error(w,
					fmt.Sprintf("FAIL: callback queue full, %d records not processed", len(records)),
					http.StatusServiceUnavailable)
				return
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
				if !s.dispatchCallback(func() { cb(sn, info) }) {
					s.logger.Printf("[ADMS Protocol] WARNING: callback queue full, device info from %s dropped", serialNumber)
				}
			}
			if len(body) > 0 {
				preview := string(body)
				if len(preview) > 200 {
					preview = preview[:200] + "..."
				}
				s.logger.Printf("[ADMS Protocol]   INFO body (truncated): %s", preview)
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
func (s *ADMSServer) HandleGetRequest(w http.ResponseWriter, r *http.Request) {
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

	s.logger.Printf("[ADMS Protocol] %s %s - Device: %s", r.Method, r.URL.Path, serialNumber)

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
func (s *ADMSServer) HandleDeviceCmd(w http.ResponseWriter, r *http.Request) {
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

	s.logger.Printf("[ADMS Protocol] %s %s - Device: %s", r.Method, r.URL.Path, serialNumber)

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
		s.logger.Printf("[ADMS Protocol]   devicecmd body (truncated): %s", preview)
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "OK: Command received: %s", string(body))
}

// updateDeviceActivity updates the last activity timestamp for a device
func (s *ADMSServer) updateDeviceActivity(serialNumber string) {
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

func (s *ADMSServer) isDeviceOnline(device *Device) bool {
	if device == nil {
		return false
	}
	return time.Since(device.LastActivity) <= 2*time.Minute
}

// parseAttendanceRecords parses attendance records from the device ATTLOG data.
// Each line must have at least a UserID and a parseable timestamp (either
// "2006-01-02 15:04:05" format or a Unix epoch integer). Malformed lines are
// skipped and logged so downstream systems never receive zero-value timestamps.
func (s *ADMSServer) parseAttendanceRecords(data string, serialNumber string) []AttendanceRecord {
	var records []AttendanceRecord
	var skipped int
	for line := range strings.SplitSeq(strings.TrimSpace(data), "\n") {
		line = strings.TrimRight(line, "\r") // handle \r\n line endings
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			skipped++
			s.logger.Printf("[Parse] device %s: skipping malformed ATTLOG line (need >=2 fields, got %d): %q",
				serialNumber, len(parts), line)
			continue
		}

		userID := strings.TrimSpace(parts[0])
		if userID == "" {
			skipped++
			s.logger.Printf("[Parse] device %s: skipping ATTLOG line with empty UserID: %q",
				serialNumber, line)
			continue
		}

		var ts time.Time
		if parsed, err := time.Parse("2006-01-02 15:04:05", parts[1]); err == nil {
			ts = parsed
		} else if epoch, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
			ts = time.Unix(epoch, 0)
		} else {
			skipped++
			s.logger.Printf("[Parse] device %s: skipping ATTLOG line with unparseable timestamp %q: %q",
				serialNumber, parts[1], line)
			continue
		}

		record := AttendanceRecord{
			SerialNumber: serialNumber,
			UserID:       userID,
			Timestamp:    ts,
		}
		if len(parts) >= 3 {
			if v, err := strconv.Atoi(parts[2]); err == nil {
				record.Status = v
			} else {
				s.logger.Printf("[Parse] device %s: non-integer Status field %q, defaulting to 0",
					serialNumber, parts[2])
			}
		}
		if len(parts) >= 4 {
			if v, err := strconv.Atoi(parts[3]); err == nil {
				record.VerifyMode = v
			} else {
				s.logger.Printf("[Parse] device %s: non-integer VerifyMode field %q, defaulting to 0",
					serialNumber, parts[3])
			}
		}
		if len(parts) >= 5 {
			record.WorkCode = parts[4]
		}
		records = append(records, record)
	}
	if skipped > 0 {
		s.logger.Printf("[Parse] device %s: skipped %d malformed ATTLOG line(s) out of %d total",
			serialNumber, skipped, len(records)+skipped)
	}
	return records
}

// parseDeviceInfo parses device information from POST data
func (s *ADMSServer) parseDeviceInfo(data string) map[string]string {
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
func (s *ADMSServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
func (s *ADMSServer) SendCommand(serialNumber, command string) {
	s.QueueCommand(serialNumber, command)
}

// SendDataCommand formats and sends a DATA command
func (s *ADMSServer) SendDataCommand(serialNumber, table, data string) {
	cmd := fmt.Sprintf("DATA QUERY %s\n%s", table, data)
	s.SendCommand(serialNumber, cmd)
}

// SendInfoCommand requests device information
func (s *ADMSServer) SendInfoCommand(serialNumber string) {
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
func (s *ADMSServer) ListDevices() []*Device {
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
func (s *ADMSServer) HandleRegistry(w http.ResponseWriter, r *http.Request) {
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
	s.logger.Printf("[ADMS Protocol] %s %s - Device: %s", r.Method, r.URL.Path, serialNumber)
	for _, k := range []string{"options", "pushver", "PushOptionsFlag"} {
		if v := query.Get(k); v != "" {
			s.logger.Printf("[ADMS Protocol]   %s: %s", k, v)
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
		s.logger.Printf("[ADMS Protocol]   Body (truncated): %s", preview)
		info := s.parseRegistryBody(string(body))
		s.devicesMutex.Lock()
		if dev := s.devices[serialNumber]; dev != nil {
			maps.Copy(dev.Options, info)
		}
		s.devicesMutex.Unlock()
		if cb := s.OnRegistry; cb != nil {
			sn := serialNumber
			if !s.dispatchCallback(func() { cb(sn, info) }) {
				s.logger.Printf("[ADMS Protocol] WARNING: callback queue full, registry info from %s dropped", serialNumber)
			}
		}
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "OK")
}

// HandleInspect serves /iclock/inspect returning JSON device snapshot
func (s *ADMSServer) HandleInspect(w http.ResponseWriter, r *http.Request) {
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
func (s *ADMSServer) parseRegistryBody(data string) map[string]string {
	info := make(map[string]string)
	for part := range strings.SplitSeq(data, ",") {
		part = strings.TrimSpace(part)
		if key, value, ok := strings.Cut(part, "="); ok {
			info[strings.TrimPrefix(strings.TrimSpace(key), "~")] = strings.TrimSpace(value)
		}
	}
	return info
}

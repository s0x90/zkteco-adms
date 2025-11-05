package zkdevicesync

import (
	"fmt"
	"io"
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

// AttendanceRecord represents an attendance transaction from the device
type AttendanceRecord struct {
	UserID       string
	Timestamp    time.Time
	Status       int    // 0=Check In, 1=Check Out, 2=Break Out, 3=Break In, etc.
	VerifyMode   int    // 0=Password, 1=Fingerprint, 2=Card, etc.
	WorkCode     string
	SerialNumber string
}

// IClockServer manages communication with ZKTeco devices using the iclock protocol
type IClockServer struct {
	devices       map[string]*Device
	devicesMutex  sync.RWMutex
	commandQueue  map[string][]string // Serial number -> commands queue
	queueMutex    sync.RWMutex
	OnAttendance  func(record AttendanceRecord)
	OnDeviceInfo  func(sn string, info map[string]string)
}

// NewIClockServer creates a new iclock server instance
func NewIClockServer() *IClockServer {
	return &IClockServer{
		devices:      make(map[string]*Device),
		commandQueue: make(map[string][]string),
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

// GetDevice retrieves device information
func (s *IClockServer) GetDevice(serialNumber string) *Device {
	s.devicesMutex.RLock()
	defer s.devicesMutex.RUnlock()
	return s.devices[serialNumber]
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
	s.commandQueue[serialNumber] = nil // Clear the queue
	return commands
}

// HandleCData handles the /iclock/cdata endpoint for attendance data
func (s *IClockServer) HandleCData(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	serialNumber := r.Form.Get("SN")
	if serialNumber == "" {
		http.Error(w, "Missing SN parameter", http.StatusBadRequest)
		return
	}

	// Register/update device
	s.RegisterDevice(serialNumber)
	s.updateDeviceActivity(serialNumber)

	// Handle different table types
	table := r.Form.Get("table")
	
	switch table {
	case "ATTLOG":
		// Parse attendance log data
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Failed to read body", http.StatusBadRequest)
			return
		}
		
		records := s.parseAttendanceRecords(string(body), serialNumber)
		if s.OnAttendance != nil {
			for _, record := range records {
				s.OnAttendance(record)
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
		if r.Method == "POST" {
			body, _ := io.ReadAll(r.Body)
			if len(body) > 0 && s.OnDeviceInfo != nil {
				info := s.parseDeviceInfo(string(body))
				s.OnDeviceInfo(serialNumber, info)
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
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	serialNumber := r.Form.Get("SN")
	if serialNumber == "" {
		http.Error(w, "Missing SN parameter", http.StatusBadRequest)
		return
	}

	s.RegisterDevice(serialNumber)
	s.updateDeviceActivity(serialNumber)

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
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	serialNumber := r.Form.Get("SN")
	if serialNumber == "" {
		http.Error(w, "Missing SN parameter", http.StatusBadRequest)
		return
	}

	s.updateDeviceActivity(serialNumber)

	// Device is reporting command execution result
	body, _ := io.ReadAll(r.Body)
	
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "OK: Command received: %s", string(body))
}

// updateDeviceActivity updates the last activity timestamp for a device
func (s *IClockServer) updateDeviceActivity(serialNumber string) {
	s.devicesMutex.Lock()
	defer s.devicesMutex.Unlock()
	
	if device, exists := s.devices[serialNumber]; exists {
		device.LastActivity = time.Now()
	}
}

// parseAttendanceRecords parses attendance records from the device data
func (s *IClockServer) parseAttendanceRecords(data string, serialNumber string) []AttendanceRecord {
	var records []AttendanceRecord
	
	lines := strings.Split(strings.TrimSpace(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		
		// Format: UserID\tTimestamp\tStatus\tVerifyMode\tWorkCode
		// Example: 1\t2024-01-01 08:00:00\t0\t1\t0
		parts := strings.Split(line, "\t")
		if len(parts) >= 2 {
			record := AttendanceRecord{
				SerialNumber: serialNumber,
			}
			
			record.UserID = parts[0]
			
			// Parse timestamp - common formats: "2024-01-01 08:00:00" or Unix timestamp
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
	
	lines := strings.Split(strings.TrimSpace(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, "=") {
			continue
		}
		
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			value := strings.TrimSpace(parts[1])
			info[key] = value
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

// ListDevices returns all registered devices
func (s *IClockServer) ListDevices() []*Device {
	s.devicesMutex.RLock()
	defer s.devicesMutex.RUnlock()
	
	devices := make([]*Device, 0, len(s.devices))
	for _, device := range s.devices {
		devices = append(devices, device)
	}
	
	return devices
}

// Package zkdevicesync provides an implementation of the ADMS protocol
// for ZKTeco biometric attendance devices.
//
// The ADMS protocol is an HTTP-based protocol used by ZKTeco devices to
// communicate with servers for sending attendance data and receiving commands.
//
// Basic usage:
//
//	server := zkdevicesync.NewADMSServer(
//	    zkdevicesync.WithOnAttendance(func(ctx context.Context, record zkdevicesync.AttendanceRecord) {
//	        fmt.Printf("User %s at %s\n", record.UserID, record.Timestamp)
//	    }),
//	)
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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// defaultMaxBodySize is the default maximum request body size (10 MB).
	defaultMaxBodySize int64 = 10 << 20

	// defaultCallbackBufferSize is the default capacity of the callback channel.
	defaultCallbackBufferSize = 256

	// defaultOnlineThreshold is the duration after which a device is
	// considered offline if no activity has been received.
	defaultOnlineThreshold = 2 * time.Minute

	// defaultDispatchTimeout is the maximum duration a dispatch will block
	// when the callback channel is full before dropping the event.
	defaultDispatchTimeout = 1 * time.Second
)

// Sentinel errors returned by the server.
var (
	// ErrServerClosed is returned when an operation is attempted on a closed server.
	ErrServerClosed = errors.New("zkdevicesync: server closed")

	// ErrCallbackQueueFull is returned when the callback queue is full and the
	// dispatch timeout has expired.
	ErrCallbackQueueFull = errors.New("zkdevicesync: callback queue full")
)

// Option configures an [ADMSServer]. Use the With* functions to obtain options.
type Option func(*ADMSServer)

// WithLogger sets the structured logger for the server.
// If not provided, [slog.Default] is used.
func WithLogger(l *slog.Logger) Option {
	return func(s *ADMSServer) {
		if l != nil {
			s.logger = l
		}
	}
}

// WithMaxBodySize sets the maximum allowed request body size in bytes.
// If not provided, the default is 10 MB.
func WithMaxBodySize(n int64) Option {
	return func(s *ADMSServer) {
		if n > 0 {
			s.maxBodySize = n
		}
	}
}

// WithCallbackBufferSize sets the capacity of the internal callback channel.
// A larger buffer tolerates burstier workloads before applying back-pressure.
// If not provided, the default is 256.
func WithCallbackBufferSize(n int) Option {
	return func(s *ADMSServer) {
		if n > 0 {
			s.callbackBufferSize = n
		}
	}
}

// WithOnlineThreshold sets the duration after which a device is considered
// offline if no activity has been received. If not provided, the default is
// 2 minutes.
func WithOnlineThreshold(d time.Duration) Option {
	return func(s *ADMSServer) {
		if d > 0 {
			s.onlineThreshold = d
		}
	}
}

// WithDispatchTimeout sets the maximum duration a callback dispatch will
// block when the channel is full before dropping the event.
// If not provided, the default is 1 second.
func WithDispatchTimeout(d time.Duration) Option {
	return func(s *ADMSServer) {
		if d > 0 {
			s.dispatchTimeout = d
		}
	}
}

// WithOnAttendance registers a callback invoked for each attendance record
// received from a device. The context is derived from the server's base
// context and is cancelled when the server is closed.
func WithOnAttendance(fn func(ctx context.Context, record AttendanceRecord)) Option {
	return func(s *ADMSServer) {
		s.onAttendance = fn
	}
}

// WithOnDeviceInfo registers a callback invoked when a device posts its
// info parameters.
func WithOnDeviceInfo(fn func(ctx context.Context, sn string, info map[string]string)) Option {
	return func(s *ADMSServer) {
		s.onDeviceInfo = fn
	}
}

// WithOnRegistry registers a callback invoked when a device registers or
// re-registers with the server.
func WithOnRegistry(fn func(ctx context.Context, sn string, info map[string]string)) Option {
	return func(s *ADMSServer) {
		s.onRegistry = fn
	}
}

// WithBaseContext sets the parent context for the server. Callback contexts
// are derived from this context. If not provided, [context.Background] is used.
func WithBaseContext(ctx context.Context) Option {
	return func(s *ADMSServer) {
		if ctx != nil {
			s.baseCtx = ctx
		}
	}
}

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
//
// Deprecated: The exported callback fields OnAttendance, OnDeviceInfo, and
// OnRegistry are retained for backward compatibility but will be removed in a
// future major version. Use [WithOnAttendance], [WithOnDeviceInfo], and
// [WithOnRegistry] instead.
type ADMSServer struct {
	devices      map[string]*Device
	devicesMutex sync.RWMutex
	commandQueue map[string][]string // Serial number -> commands queue
	queueMutex   sync.RWMutex

	// OnAttendance is called for each attendance record received.
	//
	// Deprecated: Use [WithOnAttendance] instead.
	OnAttendance func(record AttendanceRecord)

	// OnDeviceInfo is called when a device posts its info parameters.
	//
	// Deprecated: Use [WithOnDeviceInfo] instead.
	OnDeviceInfo func(sn string, info map[string]string)

	// OnRegistry is called when a device registers or re-registers.
	//
	// Deprecated: Use [WithOnRegistry] instead.
	OnRegistry func(sn string, info map[string]string)

	// Functional-options callbacks (take context).
	onAttendance func(ctx context.Context, record AttendanceRecord)
	onDeviceInfo func(ctx context.Context, sn string, info map[string]string)
	onRegistry   func(ctx context.Context, sn string, info map[string]string)

	logger *slog.Logger

	maxBodySize        int64         // maximum allowed request body size in bytes
	callbackBufferSize int           // capacity of callbackCh (used at construction)
	onlineThreshold    time.Duration // duration before device is considered offline
	dispatchTimeout    time.Duration // max block time for callback dispatch

	baseCtx       context.Context
	baseCtxCancel context.CancelFunc

	callbackCh   chan func()
	callbackMu   sync.RWMutex // guards callbackCh close in coordination with dispatchers
	callbackDone chan struct{}
	closeCh      chan struct{}
	closeOnce    sync.Once
}

// NewADMSServer creates a new ADMS server instance.
//
// Use [Option] values to configure the server:
//
//	server := NewADMSServer(
//	    WithLogger(slog.Default()),
//	    WithOnAttendance(func(ctx context.Context, r AttendanceRecord) { ... }),
//	)
//
// Call [ADMSServer.Close] when the server is no longer needed to drain pending
// callbacks and stop the worker.
func NewADMSServer(opts ...Option) *ADMSServer {
	s := &ADMSServer{
		devices:            make(map[string]*Device),
		commandQueue:       make(map[string][]string),
		logger:             slog.Default(),
		maxBodySize:        defaultMaxBodySize,
		callbackBufferSize: defaultCallbackBufferSize,
		onlineThreshold:    defaultOnlineThreshold,
		dispatchTimeout:    defaultDispatchTimeout,
		callbackDone:       make(chan struct{}),
		closeCh:            make(chan struct{}),
	}
	for _, opt := range opts {
		opt(s)
	}
	// Create base context after options have been applied so WithBaseContext
	// can provide a parent.
	if s.baseCtx == nil {
		s.baseCtx = context.Background()
	}
	s.baseCtx, s.baseCtxCancel = context.WithCancel(s.baseCtx)

	s.callbackCh = make(chan func(), s.callbackBufferSize)
	go s.callbackWorker()
	return s
}

// Close drains the callback queue and stops the worker goroutine.
// It blocks until all pending callbacks have been executed.
// Close is safe to call multiple times.
func (s *ADMSServer) Close() {
	s.closeOnce.Do(func() {
		// Cancel the base context so callbacks see cancellation.
		s.baseCtxCancel()
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
			s.logger.Error("callback panic recovered", "panic", r)
		}
	}()
	fn()
}

// readBody reads the request body with a size limit enforced by http.MaxBytesReader.
// It returns the body bytes or writes an HTTP error and returns a non-nil error.
// Oversized requests receive a 413 status; other read failures receive a 400.
func (s *ADMSServer) readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodySize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			s.logger.Warn("request body too large", "limit", s.maxBodySize)
			http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
			return nil, fmt.Errorf("readBody: %w", err)
		}
		s.logger.Warn("failed to read body", "error", err)
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return nil, fmt.Errorf("readBody: %w", err)
	}
	return body, nil
}

// dispatchCallback enqueues a callback for asynchronous execution.
// It blocks for up to the configured dispatch timeout if the channel is full
// before giving up. Returns true if the callback was enqueued, false if the
// server is closed or the timeout expired.
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

	// Slow-path: channel is full, block up to dispatchTimeout before giving up.
	timer := time.NewTimer(s.dispatchTimeout)
	defer timer.Stop()

	select {
	case s.callbackCh <- fn:
		return true
	case <-timer.C:
		s.logger.Warn("callback queue full, dropping event",
			"timeout", s.dispatchTimeout)
		return false
	case <-s.closeCh:
		return false
	}
}

// dispatchAttendance dispatches attendance records to the configured callback.
// It prefers the functional-options callback (onAttendance) over the deprecated
// struct field (OnAttendance). Returns true if the dispatch was successful or
// no callback is set, false if the queue is full.
func (s *ADMSServer) dispatchAttendance(records []AttendanceRecord) bool {
	if len(records) == 0 {
		return true
	}

	cbNew := s.onAttendance
	cbOld := s.OnAttendance
	if cbNew == nil && cbOld == nil {
		return true
	}

	ctx := s.baseCtx
	batch := records // capture for closure
	return s.dispatchCallback(func() {
		for _, record := range batch {
			s.safeCall(func() {
				if cbNew != nil {
					cbNew(ctx, record)
				} else {
					cbOld(record)
				}
			})
		}
	})
}

// dispatchDeviceInfo dispatches device info to the configured callback.
func (s *ADMSServer) dispatchDeviceInfo(sn string, info map[string]string) bool {
	cbNew := s.onDeviceInfo
	cbOld := s.OnDeviceInfo
	if cbNew == nil && cbOld == nil {
		return true
	}

	ctx := s.baseCtx
	return s.dispatchCallback(func() {
		if cbNew != nil {
			cbNew(ctx, sn, info)
		} else {
			cbOld(sn, info)
		}
	})
}

// dispatchRegistry dispatches registry info to the configured callback.
func (s *ADMSServer) dispatchRegistry(sn string, info map[string]string) bool {
	cbNew := s.onRegistry
	cbOld := s.OnRegistry
	if cbNew == nil && cbOld == nil {
		return true
	}

	ctx := s.baseCtx
	return s.dispatchCallback(func() {
		if cbNew != nil {
			cbNew(ctx, sn, info)
		} else {
			cbOld(sn, info)
		}
	})
}

// bodyPreview returns a truncated preview of body for logging (max 200 bytes).
func bodyPreview(body []byte) string {
	preview := string(body)
	if len(preview) > 200 {
		return preview[:200] + "..."
	}
	return preview
}

// RegisterDevice registers a new device.
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

// IsDeviceOnline reports whether the device has been active within the
// configured online threshold (default 2 minutes).
func (s *ADMSServer) IsDeviceOnline(serialNumber string) bool {
	s.devicesMutex.RLock()
	defer s.devicesMutex.RUnlock()
	d := s.devices[serialNumber]
	return s.isDeviceOnline(d)
}

// QueueCommand adds a command to be sent to the device on its next poll.
func (s *ADMSServer) QueueCommand(serialNumber, command string) {
	s.queueMutex.Lock()
	defer s.queueMutex.Unlock()

	s.commandQueue[serialNumber] = append(s.commandQueue[serialNumber], command)
}

// DrainCommands retrieves and removes all pending commands for a device.
// After this call, the device's command queue is empty.
func (s *ADMSServer) DrainCommands(serialNumber string) []string {
	s.queueMutex.Lock()
	defer s.queueMutex.Unlock()

	commands := s.commandQueue[serialNumber]
	delete(s.commandQueue, serialNumber)
	return commands
}

// GetCommands retrieves and removes pending commands for a device.
//
// Deprecated: Use [ADMSServer.DrainCommands] instead. GetCommands is
// misleadingly named because it mutates (deletes) the command queue as a
// side effect.
func (s *ADMSServer) GetCommands(serialNumber string) []string {
	return s.DrainCommands(serialNumber)
}

// HandleCData handles the /iclock/cdata endpoint for attendance data.
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
	s.logger.Debug("cdata request",
		"method", r.Method, "path", r.URL.Path, "device", serialNumber)
	for _, k := range []string{"options", "pushver", "PushOptionsFlag", "table"} {
		if v := query.Get(k); v != "" {
			s.logger.Debug("cdata param", "key", k, "value", v)
		}
	}

	// Handle different table types
	table := query.Get("table")

	switch table {
	case "ATTLOG":
		// Parse attendance log data
		body, err := s.readBody(w, r)
		if err != nil {
			return
		}

		// Log truncated body size/content
		if len(body) > 0 {
			s.logger.Debug("ATTLOG body", "preview", bodyPreview(body))
		}

		records := s.parseAttendanceRecords(string(body), serialNumber)
		if !s.dispatchAttendance(records) {
			s.logger.Error("callback queue full, records not processed",
				"count", len(records), "device", serialNumber)
			http.Error(w,
				fmt.Sprintf("FAIL: callback queue full, %d records not processed", len(records)),
				http.StatusServiceUnavailable)
			return
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
			body, err := s.readBody(w, r)
			if err != nil {
				return
			}
			if len(body) > 0 {
				info := s.parseDeviceInfo(string(body))
				if !s.dispatchDeviceInfo(serialNumber, info) {
					s.logger.Warn("callback queue full, device info dropped",
						"device", serialNumber)
				}
				s.logger.Debug("INFO body", "preview", bodyPreview(body))
			}
		}

		// Check for pending commands
		commands := s.DrainCommands(serialNumber)
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

// HandleGetRequest handles the /iclock/getrequest endpoint.
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

	s.logger.Debug("getrequest",
		"method", r.Method, "path", r.URL.Path, "device", serialNumber)

	// Check for pending commands
	commands := s.DrainCommands(serialNumber)
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

// HandleDeviceCmd handles the /iclock/devicecmd endpoint.
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

	s.logger.Debug("devicecmd",
		"method", r.Method, "path", r.URL.Path, "device", serialNumber)

	// Device is reporting command execution result
	body, err := s.readBody(w, r)
	if err != nil {
		return
	}
	if len(body) > 0 {
		s.logger.Debug("devicecmd body", "preview", bodyPreview(body))
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "OK: Command received: %s", string(body))
}

// updateDeviceActivity updates the last activity timestamp for a device.
func (s *ADMSServer) updateDeviceActivity(serialNumber string) {
	s.devicesMutex.Lock()
	defer s.devicesMutex.Unlock()

	if device, exists := s.devices[serialNumber]; exists {
		wasOnline := s.isDeviceOnline(device)
		device.LastActivity = time.Now()
		if !wasOnline {
			s.logger.Info("device online", "device", serialNumber)
		} else {
			s.logger.Debug("device activity", "device", serialNumber)
		}
	}
}

func (s *ADMSServer) isDeviceOnline(device *Device) bool {
	if device == nil {
		return false
	}
	return time.Since(device.LastActivity) <= s.onlineThreshold
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
			s.logger.Warn("skipping malformed ATTLOG line",
				"device", serialNumber, "fields", len(parts), "line", line)
			continue
		}

		userID := strings.TrimSpace(parts[0])
		if userID == "" {
			skipped++
			s.logger.Warn("skipping ATTLOG line with empty UserID",
				"device", serialNumber, "line", line)
			continue
		}

		var ts time.Time
		if parsed, err := time.Parse("2006-01-02 15:04:05", parts[1]); err == nil {
			ts = parsed
		} else if epoch, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
			ts = time.Unix(epoch, 0)
		} else {
			skipped++
			s.logger.Warn("skipping ATTLOG line with unparseable timestamp",
				"device", serialNumber, "timestamp", parts[1], "line", line)
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
				s.logger.Warn("non-integer Status field, defaulting to 0",
					"device", serialNumber, "value", parts[2])
			}
		}
		if len(parts) >= 4 {
			if v, err := strconv.Atoi(parts[3]); err == nil {
				record.VerifyMode = v
			} else {
				s.logger.Warn("non-integer VerifyMode field, defaulting to 0",
					"device", serialNumber, "value", parts[3])
			}
		}
		if len(parts) >= 5 {
			record.WorkCode = parts[4]
		}
		records = append(records, record)
	}
	if skipped > 0 {
		s.logger.Warn("skipped malformed ATTLOG lines",
			"device", serialNumber, "skipped", skipped, "total", len(records)+skipped)
	}
	return records
}

// parseDeviceInfo parses device information from POST data.
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

// ServeHTTP implements http.Handler interface for convenient routing.
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

// SendCommand sends a command to a device (to be retrieved on next poll).
//
// Deprecated: SendCommand is a trivial alias for [ADMSServer.QueueCommand].
// Use QueueCommand directly.
func (s *ADMSServer) SendCommand(serialNumber, command string) {
	s.QueueCommand(serialNumber, command)
}

// SendDataCommand formats and sends a DATA command.
func (s *ADMSServer) SendDataCommand(serialNumber, table, data string) {
	cmd := fmt.Sprintf("DATA QUERY %s\n%s", table, data)
	s.QueueCommand(serialNumber, cmd)
}

// SendInfoCommand requests device information.
func (s *ADMSServer) SendInfoCommand(serialNumber string) {
	s.QueueCommand(serialNumber, "INFO")
}

// ParseQueryParams parses URL query parameters commonly used in iclock protocol.
func ParseQueryParams(urlStr string) (map[string]string, error) {
	u, err := url.Parse(urlStr)
	if err != nil {
		return nil, fmt.Errorf("parse URL: %w", err)
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

// HandleRegistry processes /iclock/registry requests for device registration & capabilities.
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
	s.logger.Debug("registry request",
		"method", r.Method, "path", r.URL.Path, "device", serialNumber)
	for _, k := range []string{"options", "pushver", "PushOptionsFlag"} {
		if v := query.Get(k); v != "" {
			s.logger.Debug("registry param", "key", k, "value", v)
		}
	}
	body, err := s.readBody(w, r)
	if err != nil {
		return
	}
	if len(body) > 0 {
		s.logger.Debug("registry body", "preview", bodyPreview(body))
		info := s.parseRegistryBody(string(body))
		s.devicesMutex.Lock()
		if dev := s.devices[serialNumber]; dev != nil {
			maps.Copy(dev.Options, info)
		}
		s.devicesMutex.Unlock()
		if !s.dispatchRegistry(serialNumber, info) {
			s.logger.Warn("callback queue full, registry info dropped",
				"device", serialNumber)
		}
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "OK")
}

// HandleInspect serves /iclock/inspect returning JSON device snapshot.
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

	data, err := json.Marshal(snapshot)
	if err != nil {
		http.Error(w, "failed to encode response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
	w.Write([]byte("\n"))
}

// parseRegistryBody parses comma-separated key=value pairs from registry POST body.
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

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
	"regexp"
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

	// maxSerialNumberLength is the maximum allowed length for a device serial number.
	maxSerialNumberLength = 64

	// maxBodyPreviewLen is the maximum number of bytes shown in body log previews.
	maxBodyPreviewLen = 200

	// Route path suffixes for the ADMS protocol endpoints.
	routeCData      = "/iclock/cdata"
	routeGetRequest = "/iclock/getrequest"
	routeDeviceCmd  = "/iclock/devicecmd"
	routeRegistry   = "/iclock/registry"
	routeInspect    = "/iclock/inspect"

	// Table names sent by devices in the "table" query parameter.
	tableATTLOG  = "ATTLOG"
	tableOPERLOG = "OPERLOG"

	// timestampFormat is the date/time layout used by ZKTeco devices.
	timestampFormat = "2006-01-02 15:04:05"

	// ATTLOG tab-separated field indices.
	attFieldUserID     = 0
	attFieldTimestamp  = 1
	attFieldStatus     = 2
	attFieldVerifyMode = 3
	attFieldWorkCode   = 4
	// attMinFields is the minimum number of tab-separated fields for a valid ATTLOG line.
	attMinFields = 2

	// Query parameter names used by the iclock protocol.
	paramSN              = "SN"
	paramTable           = "table"
	paramOptions         = "options"
	paramPushVer         = "pushver"
	paramPushOptionsFlag = "PushOptionsFlag"

	// Common HTTP response strings.
	respOK               = "OK"
	respMethodNotAllowed = "Method not allowed"
	respMissingSN        = "Missing SN parameter"
	respInvalidSN        = "Invalid SN parameter"
	respDeviceLimitMsg   = "Device limit reached"

	// cmdFormat is the format string for writing pending commands.
	cmdFormat = "C:%s\n"
)

// Sentinel errors returned by the server.
var (
	// ErrServerClosed is returned when an operation is attempted on a closed server.
	ErrServerClosed = errors.New("zkdevicesync: server closed")

	// ErrCallbackQueueFull is returned when the callback queue is full and the
	// dispatch timeout has expired.
	ErrCallbackQueueFull = errors.New("zkdevicesync: callback queue full")

	// ErrMaxDevicesReached is returned by [ADMSServer.RegisterDevice] when the
	// device limit configured via [WithMaxDevices] has been reached.
	ErrMaxDevicesReached = errors.New("zkdevicesync: maximum number of devices reached")

	// ErrCommandQueueFull is returned by [ADMSServer.QueueCommand] when the
	// per-device command queue limit configured via [WithMaxCommandsPerDevice]
	// has been reached.
	ErrCommandQueueFull = errors.New("zkdevicesync: command queue full for device")

	// ErrInvalidSerialNumber is returned when a serial number fails validation.
	ErrInvalidSerialNumber = errors.New("zkdevicesync: invalid serial number")
)

// serialNumberRe matches valid device serial numbers: 1–64 alphanumeric
// characters, hyphens, or underscores.
var serialNumberRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

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

// WithMaxDevices sets the maximum number of devices that can be registered
// simultaneously. When the limit is reached, new device registrations from
// HTTP requests are rejected with 503 Service Unavailable.
// A value of 0 (the default) means unlimited.
func WithMaxDevices(n int) Option {
	return func(s *ADMSServer) {
		if n >= 0 {
			s.maxDevices = n
		}
	}
}

// WithMaxCommandsPerDevice sets the maximum number of queued commands per
// device. When the limit is reached, [ADMSServer.QueueCommand] returns
// [ErrCommandQueueFull].
// A value of 0 (the default) means unlimited.
func WithMaxCommandsPerDevice(n int) Option {
	return func(s *ADMSServer) {
		if n >= 0 {
			s.maxCommandsPerDevice = n
		}
	}
}

// WithEnableInspect enables the /iclock/inspect endpoint in the default
// [ADMSServer.ServeHTTP] router. By default, the inspect endpoint is
// disabled because it exposes device metadata (serial numbers, last
// activity, options) without authentication. You can always register
// [ADMSServer.HandleInspect] manually on a separate, authenticated route.
func WithEnableInspect() Option {
	return func(s *ADMSServer) {
		s.enableInspect = true
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
	// Must be set before serving requests; concurrent mutation is not safe.
	//
	// Deprecated: Use [WithOnAttendance] instead.
	OnAttendance func(record AttendanceRecord)

	// OnDeviceInfo is called when a device posts its info parameters.
	// Must be set before serving requests; concurrent mutation is not safe.
	//
	// Deprecated: Use [WithOnDeviceInfo] instead.
	OnDeviceInfo func(sn string, info map[string]string)

	// OnRegistry is called when a device registers or re-registers.
	// Must be set before serving requests; concurrent mutation is not safe.
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

	maxDevices           int  // 0 = unlimited
	maxCommandsPerDevice int  // 0 = unlimited
	enableInspect        bool // false = /iclock/inspect not routed in ServeHTTP

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

// requireMethod checks that the request method is one of the allowed methods.
// On failure it writes a 405 response and returns false.
func requireMethod(w http.ResponseWriter, r *http.Request, methods ...string) bool {
	for _, m := range methods {
		if r.Method == m {
			return true
		}
	}
	http.Error(w, respMethodNotAllowed, http.StatusMethodNotAllowed)
	return false
}

// requireDevice validates the SN parameter, registers the device, and updates
// its activity timestamp. On failure it writes an HTTP error and returns ("", false).
func (s *ADMSServer) requireDevice(w http.ResponseWriter, r *http.Request) (string, bool) {
	sn, ok := s.requireSerialNumber(w, r)
	if !ok {
		return "", false
	}
	if !s.registerOrReject(w, sn) {
		return "", false
	}
	s.updateDeviceActivity(sn)
	return sn, true
}

// writeCommandsOrOK drains pending commands for a device and writes them as
// "C:<cmd>\n" lines. If no commands are pending, it writes "OK".
func (s *ADMSServer) writeCommandsOrOK(w http.ResponseWriter, serialNumber string) {
	commands := s.DrainCommands(serialNumber)
	w.WriteHeader(http.StatusOK)
	if len(commands) > 0 {
		for _, cmd := range commands {
			fmt.Fprintf(w, cmdFormat, cmd)
		}
	} else {
		fmt.Fprint(w, respOK)
	}
}

// logRequest emits a debug log line with standard request metadata.
func (s *ADMSServer) logRequest(label string, r *http.Request, serialNumber string) {
	s.logger.Debug(label,
		"method", r.Method, "path", r.URL.Path, "device", serialNumber)
}

// logQueryParams logs the values of common iclock query parameters that are present.
func (s *ADMSServer) logQueryParams(label string, query url.Values, keys ...string) {
	for _, k := range keys {
		if v := query.Get(k); v != "" {
			s.logger.Debug(label, "key", k, "value", v)
		}
	}
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

// dispatchMapCallback dispatches a map-based callback (device info or registry)
// to the appropriate new-style or deprecated callback function.
func (s *ADMSServer) dispatchMapCallback(
	sn string,
	info map[string]string,
	cbNew func(ctx context.Context, sn string, info map[string]string),
	cbOld func(sn string, info map[string]string),
) bool {
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

// dispatchDeviceInfo dispatches device info to the configured callback.
func (s *ADMSServer) dispatchDeviceInfo(sn string, info map[string]string) bool {
	return s.dispatchMapCallback(sn, info, s.onDeviceInfo, s.OnDeviceInfo)
}

// dispatchRegistry dispatches registry info to the configured callback.
func (s *ADMSServer) dispatchRegistry(sn string, info map[string]string) bool {
	return s.dispatchMapCallback(sn, info, s.onRegistry, s.OnRegistry)
}

// bodyPreview returns a truncated preview of body for logging (max maxBodyPreviewLen bytes).
func bodyPreview(body []byte) string {
	if len(body) > maxBodyPreviewLen {
		return string(body[:maxBodyPreviewLen]) + "..."
	}
	return string(body)
}

// validateSerialNumber checks that a serial number is non-empty and matches
// the expected format (1–64 alphanumeric characters, hyphens, or underscores).
func validateSerialNumber(sn string) error {
	if sn == "" {
		return fmt.Errorf("%w: empty serial number", ErrInvalidSerialNumber)
	}
	if len(sn) > maxSerialNumberLength {
		return fmt.Errorf("%w: exceeds %d characters", ErrInvalidSerialNumber, maxSerialNumberLength)
	}
	if !serialNumberRe.MatchString(sn) {
		return fmt.Errorf("%w: contains invalid characters", ErrInvalidSerialNumber)
	}
	return nil
}

// requireSerialNumber extracts and validates the SN query parameter.
// On failure it writes an HTTP error response and returns ("", false).
func (s *ADMSServer) requireSerialNumber(w http.ResponseWriter, r *http.Request) (string, bool) {
	sn := r.URL.Query().Get(paramSN)
	if sn == "" {
		http.Error(w, respMissingSN, http.StatusBadRequest)
		return "", false
	}
	if err := validateSerialNumber(sn); err != nil {
		s.logger.Warn("invalid serial number", "sn", sn, "error", err)
		http.Error(w, respInvalidSN, http.StatusBadRequest)
		return "", false
	}
	return sn, true
}

// registerOrReject calls RegisterDevice and writes an appropriate HTTP error
// if registration fails (e.g. device limit reached). Returns true on success.
func (s *ADMSServer) registerOrReject(w http.ResponseWriter, serialNumber string) bool {
	if err := s.RegisterDevice(serialNumber); err != nil {
		if errors.Is(err, ErrMaxDevicesReached) {
			s.logger.Warn("device limit reached, rejecting registration",
				"device", serialNumber, "limit", s.maxDevices)
			http.Error(w, respDeviceLimitMsg, http.StatusServiceUnavailable)
		} else {
			s.logger.Warn("device registration failed",
				"device", serialNumber, "error", err)
			http.Error(w, respInvalidSN, http.StatusBadRequest)
		}
		return false
	}
	return true
}

// RegisterDevice registers a new device.
// It returns [ErrMaxDevicesReached] if the server's device limit (configured
// via [WithMaxDevices]) has been reached and the device is not already known.
// It returns [ErrInvalidSerialNumber] if the serial number is malformed.
func (s *ADMSServer) RegisterDevice(serialNumber string) error {
	if err := validateSerialNumber(serialNumber); err != nil {
		return err
	}

	s.devicesMutex.Lock()
	defer s.devicesMutex.Unlock()

	if _, exists := s.devices[serialNumber]; !exists {
		if s.maxDevices > 0 && len(s.devices) >= s.maxDevices {
			return ErrMaxDevicesReached
		}
		s.devices[serialNumber] = &Device{
			SerialNumber: serialNumber,
			LastActivity: time.Now(),
			Options:      make(map[string]string),
		}
	}
	return nil
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
// It returns [ErrCommandQueueFull] if the per-device limit configured via
// [WithMaxCommandsPerDevice] has been reached.
func (s *ADMSServer) QueueCommand(serialNumber, command string) error {
	s.queueMutex.Lock()
	defer s.queueMutex.Unlock()

	if s.maxCommandsPerDevice > 0 && len(s.commandQueue[serialNumber]) >= s.maxCommandsPerDevice {
		return ErrCommandQueueFull
	}
	s.commandQueue[serialNumber] = append(s.commandQueue[serialNumber], command)
	return nil
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
	if !requireMethod(w, r, http.MethodGet, http.MethodPost) {
		return
	}

	serialNumber, ok := s.requireDevice(w, r)
	if !ok {
		return
	}

	query := r.URL.Query()
	s.logRequest("cdata request", r, serialNumber)
	s.logQueryParams("cdata param", query, paramOptions, paramPushVer, paramPushOptionsFlag, paramTable)

	// Handle different table types
	table := query.Get(paramTable)

	switch table {
	case tableATTLOG:
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

	case tableOPERLOG:
		// Operation log - acknowledge
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, respOK)

	default:
		// Handle INFO or other requests
		if r.Method == http.MethodPost {
			body, err := s.readBody(w, r)
			if err != nil {
				return
			}
			if len(body) > 0 {
				info := s.parseKVPairs(string(body), "\n", nil)
				if !s.dispatchDeviceInfo(serialNumber, info) {
					s.logger.Warn("callback queue full, device info dropped",
						"device", serialNumber)
				}
				s.logger.Debug("INFO body", "preview", bodyPreview(body))
			}
		}

		// Check for pending commands
		s.writeCommandsOrOK(w, serialNumber)
	}
}

// HandleGetRequest handles the /iclock/getrequest endpoint.
func (s *ADMSServer) HandleGetRequest(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}

	serialNumber, ok := s.requireDevice(w, r)
	if !ok {
		return
	}

	s.logRequest("getrequest", r, serialNumber)

	s.writeCommandsOrOK(w, serialNumber)
}

// HandleDeviceCmd handles the /iclock/devicecmd endpoint.
func (s *ADMSServer) HandleDeviceCmd(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}

	serialNumber, ok := s.requireDevice(w, r)
	if !ok {
		return
	}

	s.logRequest("devicecmd", r, serialNumber)

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
		if len(parts) < attMinFields {
			skipped++
			s.logger.Warn("skipping malformed ATTLOG line",
				"device", serialNumber, "fields", len(parts), "line", line)
			continue
		}

		userID := strings.TrimSpace(parts[attFieldUserID])
		if userID == "" {
			skipped++
			s.logger.Warn("skipping ATTLOG line with empty UserID",
				"device", serialNumber, "line", line)
			continue
		}

		var ts time.Time
		if parsed, err := time.Parse(timestampFormat, parts[attFieldTimestamp]); err == nil {
			ts = parsed
		} else if epoch, err := strconv.ParseInt(parts[attFieldTimestamp], 10, 64); err == nil {
			ts = time.Unix(epoch, 0)
		} else {
			skipped++
			s.logger.Warn("skipping ATTLOG line with unparseable timestamp",
				"device", serialNumber, "timestamp", parts[attFieldTimestamp], "line", line)
			continue
		}

		record := AttendanceRecord{
			SerialNumber: serialNumber,
			UserID:       userID,
			Timestamp:    ts,
		}
		if len(parts) > attFieldStatus {
			if v, err := strconv.Atoi(parts[attFieldStatus]); err == nil {
				record.Status = v
			} else {
				s.logger.Warn("non-integer Status field, defaulting to 0",
					"device", serialNumber, "value", parts[attFieldStatus])
			}
		}
		if len(parts) > attFieldVerifyMode {
			if v, err := strconv.Atoi(parts[attFieldVerifyMode]); err == nil {
				record.VerifyMode = v
			} else {
				s.logger.Warn("non-integer VerifyMode field, defaulting to 0",
					"device", serialNumber, "value", parts[attFieldVerifyMode])
			}
		}
		if len(parts) > attFieldWorkCode {
			record.WorkCode = parts[attFieldWorkCode]
		}
		records = append(records, record)
	}
	if skipped > 0 {
		s.logger.Warn("skipped malformed ATTLOG lines",
			"device", serialNumber, "skipped", skipped, "total", len(records)+skipped)
	}
	return records
}

// trimTildePrefix removes a leading "~" from s.
func trimTildePrefix(s string) string {
	return strings.TrimPrefix(s, "~")
}

// parseKVPairs parses key=value pairs separated by sep. Each pair is split on
// "=". If transformKey is non-nil it is applied to each key before insertion.
// This generalises the two device parsers:
//   - Device info: sep="\n", transformKey=nil
//   - Registry:    sep=",",  transformKey=trimTildePrefix
func (s *ADMSServer) parseKVPairs(data, sep string, transformKey func(string) string) map[string]string {
	info := make(map[string]string)
	for part := range strings.SplitSeq(strings.TrimSpace(data), sep) {
		part = strings.TrimSpace(part)
		if key, value, ok := strings.Cut(part, "="); ok {
			k := strings.TrimSpace(key)
			if transformKey != nil {
				k = transformKey(k)
			}
			info[k] = strings.TrimSpace(value)
		}
	}
	return info
}

// parseDeviceInfo parses device information from POST data.
//
// Deprecated: Kept for backward compatibility; delegates to parseKVPairs.
func (s *ADMSServer) parseDeviceInfo(data string) map[string]string {
	return s.parseKVPairs(data, "\n", nil)
}

// parseRegistryBody parses comma-separated key=value pairs from registry POST body.
//
// Deprecated: Kept for backward compatibility; delegates to parseKVPairs.
func (s *ADMSServer) parseRegistryBody(data string) map[string]string {
	return s.parseKVPairs(data, ",", trimTildePrefix)
}

// ServeHTTP implements http.Handler interface for convenient routing.
func (s *ADMSServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, routeCData):
		s.HandleCData(w, r)
	case strings.HasSuffix(path, routeGetRequest):
		s.HandleGetRequest(w, r)
	case strings.HasSuffix(path, routeDeviceCmd):
		s.HandleDeviceCmd(w, r)
	case strings.HasSuffix(path, routeRegistry):
		s.HandleRegistry(w, r)
	case strings.HasSuffix(path, routeInspect):
		if !s.enableInspect {
			http.NotFound(w, r)
			return
		}
		s.HandleInspect(w, r)
	default:
		http.NotFound(w, r)
	}
}

// SendCommand sends a command to a device (to be retrieved on next poll).
//
// Deprecated: SendCommand is a trivial wrapper around [ADMSServer.QueueCommand].
// Use QueueCommand directly to observe the returned error.
func (s *ADMSServer) SendCommand(serialNumber, command string) {
	_ = s.QueueCommand(serialNumber, command)
}

// SendDataCommand formats and queues a DATA QUERY command.
// It returns an error if the command queue is full (see [WithMaxCommandsPerDevice]).
func (s *ADMSServer) SendDataCommand(serialNumber, table, data string) error {
	cmd := fmt.Sprintf("DATA QUERY %s\n%s", table, data)
	return s.QueueCommand(serialNumber, cmd)
}

// SendInfoCommand queues an INFO command to request device information.
// It returns an error if the command queue is full (see [WithMaxCommandsPerDevice]).
func (s *ADMSServer) SendInfoCommand(serialNumber string) error {
	return s.QueueCommand(serialNumber, "INFO")
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
	if !requireMethod(w, r, http.MethodGet, http.MethodPost) {
		return
	}

	serialNumber, ok := s.requireDevice(w, r)
	if !ok {
		return
	}

	query := r.URL.Query()
	s.logRequest("registry request", r, serialNumber)
	s.logQueryParams("registry param", query, paramOptions, paramPushVer, paramPushOptionsFlag)

	body, err := s.readBody(w, r)
	if err != nil {
		return
	}
	if len(body) > 0 {
		s.logger.Debug("registry body", "preview", bodyPreview(body))
		info := s.parseKVPairs(string(body), ",", trimTildePrefix)
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
	fmt.Fprint(w, respOK)
}

// HandleInspect serves /iclock/inspect returning JSON device snapshot.
func (s *ADMSServer) HandleInspect(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
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
	if _, err = w.Write(data); err != nil {
		s.logger.Warn("failed to write inspect response", "error", err)
		return
	}
	if _, err = w.Write([]byte("\n")); err != nil {
		s.logger.Warn("failed to write inspect trailing newline", "error", err)
	}
}

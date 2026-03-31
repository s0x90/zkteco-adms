// Package zkadms provides an implementation of the ADMS protocol
// for ZKTeco biometric attendance devices.
//
// The ADMS protocol is an HTTP-based protocol used by ZKTeco devices to
// communicate with servers for sending attendance data and receiving commands.
//
// Basic usage:
//
//	server := zkadms.NewADMSServer(
//	    zkadms.WithOnAttendance(func(ctx context.Context, record zkadms.AttendanceRecord) {
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
package zkadms

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
	"sync/atomic"
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

	// defaultMaxDevices is the default maximum number of registered devices.
	// Use [WithMaxDevices] to override or [WithUnlimitedDevices] to remove the cap.
	defaultMaxDevices = 1000

	// defaultDeviceEvictionInterval is how often the eviction worker checks for
	// stale devices.
	defaultDeviceEvictionInterval = 5 * time.Minute

	// defaultDeviceEvictionTimeout is the inactivity duration after which a
	// device is automatically evicted.
	defaultDeviceEvictionTimeout = 24 * time.Hour

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
	tableATTLOG   = "ATTLOG"
	tableOPERLOG  = "OPERLOG"
	tableUSERINFO = "USERINFO"

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
	// The device expects "C:<ID>:<CMD>\n" where ID is a monotonically
	// increasing integer used to correlate command confirmations.
	cmdFormat = "C:%d:%s\n"
)

// Verify mode constants for the ADMS protocol.
//
// These values represent the verification method used by ZKTeco devices
// when recording attendance via the ADMS (Push) HTTP protocol. Note that
// these differ from the binary TCP/IP protocol values documented in the
// ZK protocol specification.
//
// Devices may report different numeric codes depending on firmware version
// and configured verification rules. Use [VerifyModeName] to obtain a
// human-readable label for any verify mode value.
const (
	VerifyModePassword    = 0  // Password verification
	VerifyModeFingerprint = 1  // Fingerprint verification
	VerifyModeCard        = 4  // RF card verification
	VerifyModeFace        = 15 // Facial recognition
	VerifyModePalm        = 25 // Palm verification
)

// verifyModeNames maps known ADMS verify mode values to human-readable names.
var verifyModeNames = map[int]string{
	0:  "Password",
	1:  "Fingerprint",
	2:  "Card",     // legacy/alternative card code used by some firmware
	3:  "Password", // alternative password code used by some devices
	4:  "Card",     // primary ADMS card code
	5:  "Fingerprint+Card",
	6:  "Fingerprint+Password",
	7:  "Card+Password",
	8:  "Card+Fingerprint+Password",
	9:  "Other",
	15: "Face",
	25: "Palm",
}

// VerifyModeName returns a human-readable name for the given ADMS verify mode
// value. If the value is not recognized, it returns "Unknown (<value>)".
func VerifyModeName(mode int) string {
	if name, ok := verifyModeNames[mode]; ok {
		return name
	}
	return fmt.Sprintf("Unknown (%d)", mode)
}

// Sentinel errors returned by the server.
var (
	// ErrServerClosed is returned when an operation is attempted on a closed server.
	ErrServerClosed = errors.New("zkadms: server closed")

	// ErrCallbackQueueFull is returned when the callback queue is full and the
	// dispatch timeout has expired.
	ErrCallbackQueueFull = errors.New("zkadms: callback queue full")

	// ErrMaxDevicesReached is returned by [ADMSServer.RegisterDevice] when the
	// device limit configured via [WithMaxDevices] has been reached.
	ErrMaxDevicesReached = errors.New("zkadms: maximum number of devices reached")

	// ErrCommandQueueFull is returned by [ADMSServer.QueueCommand] when the
	// per-device command queue limit configured via [WithMaxCommandsPerDevice]
	// has been reached.
	ErrCommandQueueFull = errors.New("zkadms: command queue full for device")

	// ErrInvalidSerialNumber is returned when a serial number fails validation.
	ErrInvalidSerialNumber = errors.New("zkadms: invalid serial number")

	// ErrDeviceNotFound is returned when an operation targets a device that
	// is not registered with the server.
	ErrDeviceNotFound = errors.New("zkadms: device not found")
)

// serialNumberRe matches valid device serial numbers: 1–64 alphanumeric
// characters, hyphens, or underscores.
var serialNumberRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Option configures an [ADMSServer]. Use the With* functions to obtain options.
type Option func(*ADMSServer)

// DeviceOption configures a [Device] during registration.
// Use the WithDevice* functions to obtain device options.
type DeviceOption func(*Device)

// WithDeviceTimezone sets the timezone for a device. Attendance timestamps
// from this device are interpreted in the given location using
// [time.ParseInLocation]. If not set, the server's default timezone is used
// (see [WithDefaultTimezone]).
func WithDeviceTimezone(loc *time.Location) DeviceOption {
	return func(d *Device) {
		if loc != nil {
			d.Timezone = loc
		}
	}
}

// WithDefaultTimezone sets the fallback timezone used to interpret attendance
// timestamps from devices that have no explicit timezone configured via
// [WithDeviceTimezone]. The default is [time.UTC].
func WithDefaultTimezone(loc *time.Location) Option {
	return func(s *ADMSServer) {
		if loc != nil {
			s.defaultTimezone = loc
		}
	}
}

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
// context and is canceled when the server is closed.
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

// WithOnCommandResult registers a callback invoked when a device reports the
// result of a previously sent command via the /iclock/devicecmd endpoint.
// The [CommandResult.ReturnCode] is 0 on success.
func WithOnCommandResult(fn func(ctx context.Context, result CommandResult)) Option {
	return func(s *ADMSServer) {
		s.onCommandResult = fn
	}
}

// WithOnQueryUsers registers a callback invoked when a device pushes user
// records in response to a DATA QUERY USERINFO command. The device sends user
// data via POST /iclock/cdata with table=USERINFO, which is parsed into
// [UserRecord] values.
func WithOnQueryUsers(fn func(ctx context.Context, sn string, users []UserRecord)) Option {
	return func(s *ADMSServer) {
		s.onQueryUsers = fn
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
// The default is 1000. Use [WithUnlimitedDevices] to remove the limit.
func WithMaxDevices(n int) Option {
	return func(s *ADMSServer) {
		if n >= 0 {
			s.maxDevices = n
		}
	}
}

// WithUnlimitedDevices removes the default device registration limit.
// By default the server allows up to 1000 devices; this option sets the
// limit to 0 (unlimited). Use with caution: without a limit, a flood of
// requests with unique serial numbers can grow the device map without bound.
func WithUnlimitedDevices() Option {
	return func(s *ADMSServer) {
		s.maxDevices = 0
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

// WithDeviceEvictionInterval sets how often the background eviction worker
// checks for stale devices. The default is 5 minutes. The eviction worker
// removes devices that have been inactive longer than the eviction timeout
// (see [WithDeviceEvictionTimeout]) and cleans up their command queues.
func WithDeviceEvictionInterval(d time.Duration) Option {
	return func(s *ADMSServer) {
		if d > 0 {
			s.deviceEvictionInterval = d
		}
	}
}

// WithDeviceEvictionTimeout sets the inactivity duration after which a
// device is automatically removed by the background eviction worker.
// The default is 24 hours. When a device is evicted, its pending command
// queue is also cleaned up.
func WithDeviceEvictionTimeout(d time.Duration) Option {
	return func(s *ADMSServer) {
		if d > 0 {
			s.deviceEvictionTimeout = d
		}
	}
}

// Device represents a ZKTeco device
type Device struct {
	SerialNumber string
	LastActivity time.Time
	Options      map[string]string
	Timezone     *time.Location // nil = use server default (see WithDefaultTimezone)
}

// copy returns a deep copy of the Device, including its Options map.
// Timezone is a shallow copy because *time.Location is immutable after creation.
func (d *Device) copy() *Device {
	return &Device{
		SerialNumber: d.SerialNumber,
		LastActivity: d.LastActivity,
		Options:      maps.Clone(d.Options),
		Timezone:     d.Timezone,
	}
}

// AttendanceRecord represents an attendance transaction from the device
type AttendanceRecord struct {
	UserID       string
	Timestamp    time.Time
	Status       int // 0=Check In, 1=Check Out, 2=Break Out, 3=Break In, 4=Overtime In, 5=Overtime Out
	VerifyMode   int // Verification method; see VerifyMode* constants and [VerifyModeName].
	WorkCode     string
	SerialNumber string
}

// DeviceSnapshot is the JSON representation of a device in the /iclock/inspect response.
type DeviceSnapshot struct {
	Serial       string            `json:"serial"`
	LastActivity string            `json:"lastActivity"`
	Online       bool              `json:"online"`
	Options      map[string]string `json:"options"`
	Timezone     string            `json:"timezone"`
}

// CommandResult represents the result of a command execution reported by a
// device via the /iclock/devicecmd endpoint.
//
// After the server sends "C:<ID>:<CMD>\n" via /iclock/getrequest, the device
// executes the command and POSTs back a confirmation containing the command ID
// and a return code. A ReturnCode of 0 indicates success.
type CommandResult struct {
	// SerialNumber is the device that executed the command.
	SerialNumber string
	// ID is the command identifier assigned by the server.
	ID int64
	// ReturnCode is the device's result code (0 = success).
	ReturnCode int
	// Command is the command type echoed back by the device (e.g. "DATA"),
	// if present in the confirmation body.
	Command string
	// QueuedCommand is the original command string that was queued via
	// [ADMSServer.QueueCommand] (e.g. "DATA UPDATE USERINFO PIN=1\tName=John").
	// This field enables callers to correlate a device's "CMD=DATA"
	// confirmation back to the specific operation that triggered it.
	// It is empty if the command ID is not found in the server's pending map
	// (e.g. the device echoed an ID that was never assigned by this server).
	QueuedCommand string
}

// CommandEntry pairs a pre-assigned command ID with the command string.
// IDs are assigned at queue time so callers can correlate confirmations.
type CommandEntry struct {
	// ID is the monotonically increasing identifier assigned at queue time.
	ID int64
	// Command is the raw command string (e.g. "DATA UPDATE USERINFO PIN=1\tName=John").
	Command string
}

// pendingEntry tracks a queued command that has not yet been confirmed by the
// device. It stores the originating serial number so that eviction can remove
// all entries for a stale device — including commands already drained and sent.
type pendingEntry struct {
	sn  string
	cmd string
}

// UserRecord represents a user record returned by the device in response to
// a DATA QUERY USERINFO command. The device pushes user data via
// POST /iclock/cdata with tab-separated key=value fields.
type UserRecord struct {
	// PIN is the user's personal identification number (unique on the device).
	PIN string
	// Name is the user's display name.
	Name string
	// Privilege is the user's privilege level (0 = normal, 14 = admin).
	Privilege int
	// Card is the user's RFID card number, if any.
	Card string
	// Password is the user's password, if any.
	Password string
}

// ADMSServer manages communication with ZKTeco devices using the ADMS protocol.
//
// Callbacks are dispatched asynchronously via an internal worker goroutine and
// are designed not to block device HTTP responses during normal operation.
// Under backpressure, dispatch may wait up to dispatchTimeout when the
// callback queue is full. Use [WithOnAttendance], [WithOnDeviceInfo],
// [WithOnRegistry], [WithOnCommandResult], and [WithOnQueryUsers] to register
// callbacks.
// Call Close to drain the callback queue when the server is shutting down.
type ADMSServer struct {
	devices      map[string]*Device
	devicesMutex sync.RWMutex
	commandQueue map[string][]CommandEntry // Serial number -> queued commands
	queueMutex   sync.RWMutex

	// pendingCommands maps command IDs (assigned at queue time) to their
	// original command strings and originating device serial numbers.
	// Entries are added by QueueCommand and consumed by HandleDeviceCmd
	// when the device confirms execution.
	pendingCommands map[int64]pendingEntry

	onAttendance    func(ctx context.Context, record AttendanceRecord)
	onDeviceInfo    func(ctx context.Context, sn string, info map[string]string)
	onRegistry      func(ctx context.Context, sn string, info map[string]string)
	onCommandResult func(ctx context.Context, result CommandResult)
	onQueryUsers    func(ctx context.Context, sn string, users []UserRecord)

	logger *slog.Logger

	cmdID atomic.Int64 // monotonically increasing command ID counter

	maxBodySize        int64         // maximum allowed request body size in bytes
	callbackBufferSize int           // capacity of callbackCh (used at construction)
	onlineThreshold    time.Duration // duration before device is considered offline
	dispatchTimeout    time.Duration // max block time for callback dispatch

	maxDevices           int  // 0 = unlimited
	maxCommandsPerDevice int  // 0 = unlimited
	enableInspect        bool // false = /iclock/inspect not routed in ServeHTTP

	deviceEvictionInterval time.Duration // how often the eviction worker runs
	deviceEvictionTimeout  time.Duration // inactivity threshold for eviction

	defaultTimezone *time.Location // fallback TZ for devices without explicit timezone

	baseCtx       context.Context
	baseCtxCancel context.CancelFunc

	callbackCh   chan func()
	callbackMu   sync.RWMutex // guards callbackCh close in coordination with dispatchers
	callbackDone chan struct{}
	evictionDone chan struct{}
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
		devices:                make(map[string]*Device),
		commandQueue:           make(map[string][]CommandEntry),
		pendingCommands:        make(map[int64]pendingEntry),
		logger:                 slog.Default(),
		maxBodySize:            defaultMaxBodySize,
		maxDevices:             defaultMaxDevices,
		callbackBufferSize:     defaultCallbackBufferSize,
		onlineThreshold:        defaultOnlineThreshold,
		dispatchTimeout:        defaultDispatchTimeout,
		deviceEvictionInterval: defaultDeviceEvictionInterval,
		deviceEvictionTimeout:  defaultDeviceEvictionTimeout,
		defaultTimezone:        time.UTC,
		callbackDone:           make(chan struct{}),
		evictionDone:           make(chan struct{}),
		closeCh:                make(chan struct{}),
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
	go s.evictionWorker()
	return s
}

// Close drains the callback queue and stops the worker goroutine.
// It blocks until all pending callbacks have been executed and the
// eviction worker has exited.
// Close is safe to call multiple times.
func (s *ADMSServer) Close() {
	s.closeOnce.Do(func() {
		// Cancel the base context so callbacks see cancellation.
		s.baseCtxCancel()
		// Signal all dispatchers and the eviction worker to stop.
		close(s.closeCh)
		// Wait for the eviction worker to exit before tearing down the
		// callback pipeline, so no late eviction log races with closure.
		<-s.evictionDone
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

// evictionWorker periodically removes devices that have been inactive longer
// than the configured eviction timeout. It also cleans up pending commands for
// evicted devices. The worker exits when closeCh is closed.
func (s *ADMSServer) evictionWorker() {
	defer close(s.evictionDone)
	ticker := time.NewTicker(s.deviceEvictionInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.closeCh:
			return
		case <-ticker.C:
			s.evictStaleDevices()
		}
	}
}

// evictStaleDevices removes devices inactive longer than the eviction timeout
// and cleans up their command queues. Lock ordering: devicesMutex first, then
// queueMutex — never held simultaneously.
func (s *ADMSServer) evictStaleDevices() {
	// Phase 1: collect stale serial numbers under devicesMutex.
	var stale []string
	now := time.Now()
	s.devicesMutex.Lock()
	for sn, d := range s.devices {
		if now.Sub(d.LastActivity) > s.deviceEvictionTimeout {
			stale = append(stale, sn)
			delete(s.devices, sn)
		}
	}
	s.devicesMutex.Unlock()

	if len(stale) == 0 {
		return
	}

	// Phase 2: clean command queues and pending command mappings under queueMutex.
	s.queueMutex.Lock()
	staleSet := make(map[string]struct{}, len(stale))
	for _, sn := range stale {
		staleSet[sn] = struct{}{}
		delete(s.commandQueue, sn)
	}
	// Remove all pending commands for evicted devices, including commands
	// that were already drained and sent but never confirmed.
	for id, entry := range s.pendingCommands {
		if _, ok := staleSet[entry.sn]; ok {
			delete(s.pendingCommands, id)
		}
	}
	s.queueMutex.Unlock()

	s.logger.Info("evicted stale devices", "count", len(stale))
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
// "C:<id>:<cmd>\n" lines using the IDs assigned at queue time. The device uses
// the ID to confirm execution via /iclock/devicecmd.
// If no commands are pending, it writes "OK".
func (s *ADMSServer) writeCommandsOrOK(w http.ResponseWriter, serialNumber string) {
	commands := s.DrainCommands(serialNumber)
	w.WriteHeader(http.StatusOK)
	if len(commands) > 0 {
		for _, entry := range commands {
			fmt.Fprintf(w, cmdFormat, entry.ID, entry.Command)
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
// Returns true if the dispatch was successful or no callback is set, false if
// the queue is full.
func (s *ADMSServer) dispatchAttendance(records []AttendanceRecord) bool {
	if len(records) == 0 {
		return true
	}

	cb := s.onAttendance
	if cb == nil {
		return true
	}

	ctx := s.baseCtx
	batch := records // capture for closure
	return s.dispatchCallback(func() {
		for _, record := range batch {
			s.safeCall(func() {
				cb(ctx, record)
			})
		}
	})
}

// dispatchMapCallback dispatches a map-based callback (device info or registry).
func (s *ADMSServer) dispatchMapCallback(
	sn string,
	info map[string]string,
	cb func(ctx context.Context, sn string, info map[string]string),
) bool {
	if cb == nil {
		return true
	}
	ctx := s.baseCtx
	return s.dispatchCallback(func() {
		cb(ctx, sn, info)
	})
}

// dispatchDeviceInfo dispatches device info to the configured callback.
func (s *ADMSServer) dispatchDeviceInfo(sn string, info map[string]string) bool {
	return s.dispatchMapCallback(sn, info, s.onDeviceInfo)
}

// dispatchRegistry dispatches registry info to the configured callback.
func (s *ADMSServer) dispatchRegistry(sn string, info map[string]string) bool {
	return s.dispatchMapCallback(sn, info, s.onRegistry)
}

// dispatchCommandResult dispatches a command result to the configured callback.
func (s *ADMSServer) dispatchCommandResult(result CommandResult) bool {
	cb := s.onCommandResult
	if cb == nil {
		return true
	}
	ctx := s.baseCtx
	return s.dispatchCallback(func() {
		cb(ctx, result)
	})
}

// dispatchQueryUsers dispatches parsed user records to the configured callback.
func (s *ADMSServer) dispatchQueryUsers(sn string, users []UserRecord) bool {
	if len(users) == 0 {
		return true
	}
	cb := s.onQueryUsers
	if cb == nil {
		return true
	}
	ctx := s.baseCtx
	batch := users // capture for closure
	return s.dispatchCallback(func() {
		s.safeCall(func() {
			cb(ctx, sn, batch)
		})
	})
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

// RegisterDevice registers a new device or updates an existing one.
// It returns [ErrMaxDevicesReached] if the server's device limit (configured
// via [WithMaxDevices]) has been reached and the device is not already known.
// It returns [ErrInvalidSerialNumber] if the serial number is malformed.
//
// Use [DeviceOption] values to configure the device:
//
//	loc, _ := time.LoadLocation("Europe/Istanbul")
//	server.RegisterDevice("SN001", WithDeviceTimezone(loc))
func (s *ADMSServer) RegisterDevice(serialNumber string, opts ...DeviceOption) error {
	if err := validateSerialNumber(serialNumber); err != nil {
		return err
	}

	s.devicesMutex.Lock()
	defer s.devicesMutex.Unlock()

	dev, exists := s.devices[serialNumber]
	if !exists {
		if s.maxDevices > 0 && len(s.devices) >= s.maxDevices {
			return ErrMaxDevicesReached
		}
		dev = &Device{
			SerialNumber: serialNumber,
			LastActivity: time.Now(),
			Options:      make(map[string]string),
		}
		s.devices[serialNumber] = dev
	}
	for _, opt := range opts {
		opt(dev)
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

// SetDeviceTimezone sets the timezone for a registered device. Attendance
// timestamps from this device will be interpreted in the given location.
// Pass nil to clear the device-specific timezone and fall back to the
// server default (see [WithDefaultTimezone]).
// Returns [ErrDeviceNotFound] if the device is not registered.
func (s *ADMSServer) SetDeviceTimezone(serialNumber string, loc *time.Location) error {
	s.devicesMutex.Lock()
	defer s.devicesMutex.Unlock()
	d := s.devices[serialNumber]
	if d == nil {
		return ErrDeviceNotFound
	}
	d.Timezone = loc
	return nil
}

// GetDeviceTimezone returns the effective timezone for a device.
// Resolution order: device-specific timezone, server default
// ([WithDefaultTimezone]), then [time.UTC].
// Returns nil if the device is not registered.
func (s *ADMSServer) GetDeviceTimezone(serialNumber string) *time.Location {
	s.devicesMutex.RLock()
	defer s.devicesMutex.RUnlock()
	if s.devices[serialNumber] == nil {
		return nil
	}
	return s.deviceLocationLocked(serialNumber)
}

// QueueCommand adds a command to be sent to the device on its next poll.
// It assigns a monotonically increasing command ID at queue time and returns
// the ID so callers can correlate the command with its [CommandResult].
// It returns [ErrCommandQueueFull] (with ID 0) if the per-device limit
// configured via [WithMaxCommandsPerDevice] has been reached.
func (s *ADMSServer) QueueCommand(serialNumber, command string) (int64, error) {
	id := s.cmdID.Add(1)

	s.queueMutex.Lock()
	defer s.queueMutex.Unlock()

	if s.maxCommandsPerDevice > 0 && len(s.commandQueue[serialNumber]) >= s.maxCommandsPerDevice {
		return 0, ErrCommandQueueFull
	}
	s.commandQueue[serialNumber] = append(s.commandQueue[serialNumber], CommandEntry{ID: id, Command: command})
	s.pendingCommands[id] = pendingEntry{sn: serialNumber, cmd: command}
	return id, nil
}

// DrainCommands retrieves and removes all pending commands for a device.
// After this call, the device's command queue is empty.
func (s *ADMSServer) DrainCommands(serialNumber string) []CommandEntry {
	s.queueMutex.Lock()
	defer s.queueMutex.Unlock()

	commands := s.commandQueue[serialNumber]
	delete(s.commandQueue, serialNumber)
	return commands
}

// PendingCommandsCount returns the number of queued commands for a device
// without modifying the queue.
func (s *ADMSServer) PendingCommandsCount(serialNumber string) int {
	s.queueMutex.RLock()
	defer s.queueMutex.RUnlock()
	return len(s.commandQueue[serialNumber])
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

		// Resolve the device's timezone for timestamp interpretation.
		s.devicesMutex.RLock()
		loc := s.deviceLocationLocked(serialNumber)
		s.devicesMutex.RUnlock()

		records := s.parseAttendanceRecords(string(body), serialNumber, loc)
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

	case tableUSERINFO:
		// User records pushed in response to DATA QUERY USERINFO.
		body, err := s.readBody(w, r)
		if err != nil {
			return
		}

		if len(body) > 0 {
			users := s.parseUserRecords(string(body), serialNumber)
			s.logger.Debug("USERINFO records processed",
				"count", len(users), "device", serialNumber)
			if !s.dispatchQueryUsers(serialNumber, users) {
				s.logger.Warn("callback queue full, user records dropped",
					"count", len(users), "device", serialNumber)
			}
		}

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
//
// After the server sends "C:<ID>:<CMD>\n" via /iclock/getrequest, the device
// executes the command and POSTs the result here. The body typically contains
// key=value pairs separated by "&", for example:
//
//	ID=1&Return=0&CMD=USER ADD
//
// A Return value of 0 indicates success. The parsed result is dispatched to
// the callback registered via [WithOnCommandResult].
func (s *ADMSServer) HandleDeviceCmd(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}

	serialNumber, ok := s.requireDevice(w, r)
	if !ok {
		return
	}

	s.logRequest("devicecmd", r, serialNumber)

	body, err := s.readBody(w, r)
	if err != nil {
		return
	}

	if len(body) > 0 {
		s.logger.Debug("devicecmd body", "device", serialNumber, "preview", bodyPreview(body))
	}

	results := s.parseCommandResults(string(body), serialNumber)

	for _, result := range results {
		// Populate QueuedCommand from the pending commands map.
		s.queueMutex.Lock()
		if entry, ok := s.pendingCommands[result.ID]; ok {
			result.QueuedCommand = entry.cmd
			delete(s.pendingCommands, result.ID)
		}
		s.queueMutex.Unlock()

		s.logger.Info("command result",
			"device", serialNumber,
			"id", result.ID,
			"return", result.ReturnCode,
			"cmd", result.Command)
		s.logger.Debug("command result detail",
			"device", serialNumber,
			"id", result.ID,
			"queued_cmd", result.QueuedCommand)

		if !s.dispatchCommandResult(result) {
			s.logger.Warn("callback queue full, command result dropped",
				"device", serialNumber, "id", result.ID)
		}
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, respOK)
}

// parseCommandResults parses a devicecmd confirmation body into one or more
// [CommandResult] values. The device uses two different formats:
//
// Batched format (ampersand-separated KV pairs, one result per line):
//
//	ID=1&Return=0&CMD=INFO\nID=2&Return=0&CMD=CHECK\n
//
// Shell/multiline format (newline-separated KV pairs, single result):
//
//	ID=32\nReturn=0\nCMD=Shell\nContent=output\n
//
// The parser accumulates key=value pairs into the current result. When a new
// ID= is encountered, it flushes the previous result and starts a new one.
// This handles both formats transparently.
func (s *ADMSServer) parseCommandResults(body, serialNumber string) []CommandResult {
	var results []CommandResult
	current := CommandResult{SerialNumber: serialNumber}
	hasID := false

	flush := func() {
		if hasID {
			results = append(results, current)
		}
		current = CommandResult{SerialNumber: serialNumber}
		hasID = false
	}

	// Normalize: treat both \n and & as delimiters between KV pairs.
	body = strings.ReplaceAll(body, "\n", "&")
	for part := range strings.SplitSeq(body, "&") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		switch strings.ToUpper(strings.TrimSpace(key)) {
		case "ID":
			id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil {
				s.logger.Warn("devicecmd: unparseable ID", "device", serialNumber, "value", value)
				continue
			}
			// New ID means new result — flush the previous one.
			if hasID {
				flush()
			}
			current.ID = id
			hasID = true
		case "RETURN":
			if code, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
				current.ReturnCode = code
			} else {
				s.logger.Warn("devicecmd: unparseable Return", "device", serialNumber, "value", value)
			}
		case "CMD":
			current.Command = strings.TrimSpace(value)
		}
	}
	flush() // flush the last result
	return results
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

// deviceLocationLocked returns the timezone to use when parsing attendance
// timestamps for the given device. Resolution order: device-specific timezone,
// server default (defaultTimezone), then time.UTC.
//
// The caller must hold devicesMutex (at least RLock).
func (s *ADMSServer) deviceLocationLocked(serialNumber string) *time.Location {
	if d := s.devices[serialNumber]; d != nil && d.Timezone != nil {
		return d.Timezone
	}
	if s.defaultTimezone != nil {
		return s.defaultTimezone
	}
	return time.UTC
}

// parseAttendanceRecords parses attendance records from the device ATTLOG data.
// Each line must have at least a UserID and a parseable timestamp (either
// "2006-01-02 15:04:05" format or a Unix epoch integer). Malformed lines are
// skipped and logged so downstream systems never receive zero-value timestamps.
//
// The loc parameter specifies the timezone in which device-local timestamps
// (the "2006-01-02 15:04:05" format) are interpreted. Unix epoch timestamps
// are inherently UTC and are not affected by loc.
func (s *ADMSServer) parseAttendanceRecords(data string, serialNumber string, loc *time.Location) []AttendanceRecord {
	var records []AttendanceRecord
	var skipped int

	if loc == nil {
		if s.defaultTimezone != nil {
			loc = s.defaultTimezone
		} else {
			loc = time.UTC
		}
	}

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
		if parsed, err := time.ParseInLocation(timestampFormat, parts[attFieldTimestamp], loc); err == nil {
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

// parseUserRecords parses tab-separated USERINFO lines pushed by a device in
// response to a DATA QUERY USERINFO command. Each line has the form:
//
//	PIN=1\tName=John\tPrivilege=0\tCard=\tPassword=
//
// Lines that lack a PIN field are skipped with a warning.
func (s *ADMSServer) parseUserRecords(data, serialNumber string) []UserRecord {
	var records []UserRecord
	var skipped int
	for line := range strings.SplitSeq(strings.TrimSpace(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		fields := make(map[string]string)
		for part := range strings.SplitSeq(line, "\t") {
			if key, value, ok := strings.Cut(part, "="); ok {
				fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
			}
		}
		pin := fields["PIN"]
		if pin == "" {
			skipped++
			s.logger.Warn("skipping USERINFO line without PIN",
				"device", serialNumber, "line_len", len(line))
			continue
		}
		privilege, _ := strconv.Atoi(fields["Privilege"])
		records = append(records, UserRecord{
			PIN:       pin,
			Name:      fields["Name"],
			Privilege: privilege,
			Card:      fields["Card"],
			Password:  fields["Password"],
		})
	}
	if skipped > 0 {
		s.logger.Warn("skipped malformed USERINFO lines",
			"device", serialNumber, "skipped", skipped, "total", len(records)+skipped)
	}
	return records
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

// SendInfoCommand queues an INFO command to request device information.
// It returns the assigned command ID and an error if the command queue is full
// (see [WithMaxCommandsPerDevice]).
func (s *ADMSServer) SendInfoCommand(serialNumber string) (int64, error) {
	return s.QueueCommand(serialNumber, "INFO")
}

// SendUserAddCommand queues a DATA UPDATE USERINFO command to add or update
// a user on the device. The wire format uses tab-separated key=value pairs:
//
//	DATA UPDATE USERINFO PIN=<pin>\tName=<name>\tPrivilege=<privilege>\tCard=<card>
//
// Privilege values: 0 = normal user, 14 = admin.
//
// Note: the ADMS datasheet documents this as "USER ADD", but real devices
// (e.g. ZAM180-NF firmware) require the DATA UPDATE USERINFO prefix instead.
// The device confirms execution by POSTing to /iclock/devicecmd with CMD=DATA.
//
// It returns the assigned command ID and an error if the command queue is full
// (see [WithMaxCommandsPerDevice]).
func (s *ADMSServer) SendUserAddCommand(serialNumber, pin, name string, privilege int, card string) (int64, error) {
	cmd := fmt.Sprintf("DATA UPDATE USERINFO PIN=%s\tName=%s\tPrivilege=%d\tCard=%s", pin, name, privilege, card)
	return s.QueueCommand(serialNumber, cmd)
}

// SendUserDeleteCommand queues a DATA DELETE USERINFO command to remove a user
// from the device. The wire format is:
//
//	DATA DELETE USERINFO PIN=<pin>
//
// Note: the ADMS datasheet documents this as "USER DEL", but real devices
// (e.g. ZAM180-NF firmware) require the DATA DELETE USERINFO prefix instead.
// The device confirms execution by POSTing to /iclock/devicecmd with CMD=DATA.
//
// It returns the assigned command ID and an error if the command queue is full
// (see [WithMaxCommandsPerDevice]).
func (s *ADMSServer) SendUserDeleteCommand(serialNumber, pin string) (int64, error) {
	cmd := fmt.Sprintf("DATA DELETE USERINFO PIN=%s", pin)
	return s.QueueCommand(serialNumber, cmd)
}

// SendCheckCommand queues a CHECK (heartbeat) command to verify device
// responsiveness. The device confirms by POSTing to /iclock/devicecmd
// with CMD=CHECK and Return=0.
//
// It returns the assigned command ID and an error if the command queue is full
// (see [WithMaxCommandsPerDevice]).
func (s *ADMSServer) SendCheckCommand(serialNumber string) (int64, error) {
	return s.QueueCommand(serialNumber, "CHECK")
}

// SendGetOptionCommand queues a GET OPTION command to retrieve a device
// configuration value. The device confirms by POSTing to /iclock/devicecmd
// with CMD=GET OPTION.
//
// Confirmed keys on ZAM180-NF firmware: DeviceName, FWVersion, IPAddress,
// MACAddress, Platform, WorkCode, LockCount, UserCount, FPCount,
// AttLogCount, FaceCount, TransactionCount, MaxUserCount, MaxAttLogCount,
// MaxFingerCount, MaxFaceCount.
//
// The option value is typically delivered via the device info push
// (POST /iclock/cdata) rather than in the command confirmation body.
//
// It returns the assigned command ID and an error if the command queue is full
// (see [WithMaxCommandsPerDevice]).
func (s *ADMSServer) SendGetOptionCommand(serialNumber, key string) (int64, error) {
	cmd := fmt.Sprintf("GET OPTION FROM %s", key)
	return s.QueueCommand(serialNumber, cmd)
}

// SendShellCommand queues a Shell command for execution on the device.
// The device executes the command and confirms by POSTing to /iclock/devicecmd
// with CMD=Shell, Return=0, and the output in the Content field.
//
// WARNING: This executes arbitrary commands on the device's Linux OS.
// Use with extreme caution — incorrect commands can brick the device.
//
// Example:
//
//	server.SendShellCommand("SERIAL001", "date")
//	// Device responds: ID=1&Return=0&CMD=Shell\nContent=Tue Mar 24 16:12:26 GMT 2026
//
// It returns the assigned command ID and an error if the command queue is full
// (see [WithMaxCommandsPerDevice]).
func (s *ADMSServer) SendShellCommand(serialNumber, command string) (int64, error) {
	cmd := fmt.Sprintf("Shell %s", command)
	return s.QueueCommand(serialNumber, cmd)
}

// SendQueryUsersCommand queues a DATA QUERY USERINFO command to request
// all user records from the device. The device responds by pushing user data
// via POST /iclock/cdata with table=USERINFO, which is parsed into
// [UserRecord] values and dispatched via the [WithOnQueryUsers] callback.
// The device also sends a command confirmation by POSTing to /iclock/devicecmd
// with CMD=DATA and Return=0.
//
// To query a specific user by PIN, use [ADMSServer.QueueCommand] directly:
//
//	server.QueueCommand(serialNumber, "DATA QUERY USERINFO PIN=1")
//
// It returns the assigned command ID and an error if the command queue is full
// (see [WithMaxCommandsPerDevice]).
func (s *ADMSServer) SendQueryUsersCommand(serialNumber string) (int64, error) {
	return s.QueueCommand(serialNumber, "DATA QUERY USERINFO")
}

// SendLogCommand queues a LOG command to request log data from the device.
// The device confirms by POSTing to /iclock/devicecmd with CMD=LOG
// and Return=0.
//
// It returns the assigned command ID and an error if the command queue is full
// (see [WithMaxCommandsPerDevice]).
func (s *ADMSServer) SendLogCommand(serialNumber string) (int64, error) {
	return s.QueueCommand(serialNumber, "LOG")
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
			Timezone:     s.deviceLocationLocked(dc.SerialNumber).String(),
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

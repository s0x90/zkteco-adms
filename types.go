package zkadms

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
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

	// ErrInvalidCommandField is returned when a command field contains
	// control characters that could cause injection on the ADMS wire protocol.
	ErrInvalidCommandField = errors.New("zkadms: command field contains forbidden control characters")
)

// serialNumberRe matches valid device serial numbers: 1–64 alphanumeric
// characters, hyphens, or underscores.
var serialNumberRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

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

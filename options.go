package zkadms

import (
	"context"
	"log/slog"
	"time"
)

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

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
// The server implements five endpoints:
//   - /iclock/cdata - receives attendance logs, device info, and user query results
//   - /iclock/registry - handles device registration and capability payloads
//   - /iclock/getrequest - handles device polling for commands
//   - /iclock/devicecmd - receives command execution confirmations
//   - /iclock/inspect - returns JSON device summary (opt-in via [WithEnableInspect])
//
// Call Close when the server is no longer needed to drain the callback queue.
package zkadms

import (
	"context"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

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
// eviction worker has exited. The base context passed to callbacks
// remains valid until all queued callbacks have finished executing,
// so database calls and HTTP requests inside callbacks complete
// normally during shutdown.
// Close is safe to call multiple times.
func (s *ADMSServer) Close() {
	s.closeOnce.Do(func() {
		// Signal all dispatchers and the eviction worker to stop
		// accepting new work. Note: baseCtxCancel is intentionally
		// deferred until after the callback worker drains, so that
		// in-flight callbacks still have a live context for I/O.
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
		// Cancel the base context after all callbacks have completed.
		s.baseCtxCancel()
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
			s.logger.Error("callback panic recovered",
				"panic", r,
				"stack", string(debug.Stack()),
			)
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

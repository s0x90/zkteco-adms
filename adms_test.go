package zkadms

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewADMSServer(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()
	if server == nil {
		t.Fatal("NewADMSServer returned nil")
	}
	if server.devices == nil {
		t.Error("devices map not initialized")
	}
	if server.commandQueue == nil {
		t.Error("commandQueue map not initialized")
	}
}

func TestNewADMSServer_Defaults(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if server.maxBodySize != defaultMaxBodySize {
		t.Errorf("expected default maxBodySize %d, got %d", defaultMaxBodySize, server.maxBodySize)
	}
	if server.onlineThreshold != defaultOnlineThreshold {
		t.Errorf("expected default onlineThreshold %v, got %v", defaultOnlineThreshold, server.onlineThreshold)
	}
	if server.dispatchTimeout != defaultDispatchTimeout {
		t.Errorf("expected default dispatchTimeout %v, got %v", defaultDispatchTimeout, server.dispatchTimeout)
	}
	if cap(server.callbackCh) != defaultCallbackBufferSize {
		t.Errorf("expected default callbackCh capacity %d, got %d", defaultCallbackBufferSize, cap(server.callbackCh))
	}
	if server.maxDevices != defaultMaxDevices {
		t.Errorf("expected default maxDevices %d, got %d", defaultMaxDevices, server.maxDevices)
	}
	if server.deviceEvictionInterval != defaultDeviceEvictionInterval {
		t.Errorf("expected default deviceEvictionInterval %v, got %v", defaultDeviceEvictionInterval, server.deviceEvictionInterval)
	}
	if server.deviceEvictionTimeout != defaultDeviceEvictionTimeout {
		t.Errorf("expected default deviceEvictionTimeout %v, got %v", defaultDeviceEvictionTimeout, server.deviceEvictionTimeout)
	}
}

func TestCloseIdempotent(t *testing.T) {
	server := NewADMSServer()
	server.Close()
	server.Close() // must not panic
}

func TestClose_DrainsAllAcceptedCallbacks(t *testing.T) {
	server := NewADMSServer()

	// Block the worker so callbacks pile up in the channel.
	workerBlocked := make(chan struct{})
	workerRelease := make(chan struct{})
	server.callbackCh <- func() {
		close(workerBlocked)
		<-workerRelease
	}
	<-workerBlocked

	// Dispatch several callbacks while the worker is blocked.
	const n = 50
	var count atomic.Int64
	for range n {
		ok := server.dispatchCallback(func() {
			count.Add(1)
		})
		if !ok {
			t.Fatal("dispatchCallback should succeed while server is open")
		}
	}

	// Release the worker and close the server. Close must block until
	// all 50 accepted callbacks have been executed.
	close(workerRelease)
	server.Close()

	if got := count.Load(); got != n {
		t.Errorf("Expected all %d callbacks to run before Close returned, got %d", n, got)
	}
}

func TestClose_ConcurrentDispatchAndClose(t *testing.T) {
	server := NewADMSServer()

	var accepted atomic.Int64
	var executed atomic.Int64
	var wg sync.WaitGroup

	// Spawn goroutines that continuously dispatch callbacks.
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				ok := server.dispatchCallback(func() {
					executed.Add(1)
				})
				if !ok {
					return // server closed or queue full
				}
				accepted.Add(1)
			}
		}()
	}

	// Let dispatchers run briefly, then close.
	time.Sleep(10 * time.Millisecond)
	server.Close()
	wg.Wait()

	// Every accepted callback must have been executed.
	if a, e := accepted.Load(), executed.Load(); a != e {
		t.Errorf("accepted=%d but executed=%d; Close did not drain all accepted callbacks", a, e)
	}
}

func TestClose_DeliversLiveContextToCallbacks(t *testing.T) {
	ctxErrs := make(chan error, 10)
	server := NewADMSServer(
		WithOnDeviceInfo(func(ctx context.Context, _ string, _ map[string]string) {
			ctxErrs <- ctx.Err()
		}),
	)

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	// Dispatch a callback.
	infoBody := "~DeviceName=TestDev\nMAC=00:11:22:33:44:55"
	req := httptest.NewRequest(http.MethodPost,
		"/iclock/cdata?SN=DEV001", strings.NewReader(infoBody))
	w := httptest.NewRecorder()
	server.HandleCData(w, req)

	// Close drains the callback queue; callbacks should receive a live context.
	server.Close()

	select {
	case err := <-ctxErrs:
		if err != nil {
			t.Errorf("expected live context (nil Err) during callback drain, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for callback")
	}
}

func TestDispatchAfterClose(t *testing.T) {
	server := NewADMSServer()
	server.Close()

	// Must not panic: dispatchCallback should detect the closed state and return false.
	ok := server.dispatchCallback(func() {
		t.Error("callback should not be executed after Close")
	})
	if ok {
		t.Error("Expected dispatchCallback to return false after Close")
	}
}

func TestDispatchCallback_ReturnsFalseAfterClose(t *testing.T) {
	server := NewADMSServer()
	server.Close()

	executed := false
	ok := server.dispatchCallback(func() {
		executed = true
	})

	if ok {
		t.Error("Expected dispatchCallback to return false after Close")
	}
	if executed {
		t.Error("Callback should not have been executed after Close")
	}
}

func TestDispatchCallback_AfterClose(t *testing.T) {
	server := NewADMSServer()
	server.Close()

	ok := server.dispatchCallback(func() {})
	if ok {
		t.Error("dispatchCallback should return false after server close")
	}
}

func TestDispatchCallback_SlowPathSuccess(t *testing.T) {
	// Buffer of 1 + a consumer that drains slowly.
	server := NewADMSServer(
		WithCallbackBufferSize(1),
		WithDispatchTimeout(2*time.Second),
		WithOnAttendance(func(_ context.Context, _ AttendanceRecord) {
			time.Sleep(100 * time.Millisecond)
		}),
	)
	defer server.Close()

	record := AttendanceRecord{UserID: "1", Timestamp: time.Now(), SerialNumber: "DEV001"}

	// First dispatch: enters the channel immediately.
	ok1 := server.dispatchAttendance([]AttendanceRecord{record})
	if !ok1 {
		t.Fatal("first dispatch should succeed")
	}

	// Second dispatch: channel is full, but the worker will drain it
	// within the 2s timeout, so this should succeed via the slow-path.
	ok2 := server.dispatchAttendance([]AttendanceRecord{record})
	if !ok2 {
		t.Fatal("second dispatch should succeed via slow-path")
	}
}

func TestDispatchCallback_CloseWhileWaiting(t *testing.T) {
	blocker := make(chan struct{})
	server := NewADMSServer(
		WithCallbackBufferSize(1),
		WithDispatchTimeout(5*time.Second),
		WithOnAttendance(func(_ context.Context, _ AttendanceRecord) {
			<-blocker
		}),
	)

	record := AttendanceRecord{UserID: "1", Timestamp: time.Now(), SerialNumber: "DEV001"}

	// Fill channel.
	server.dispatchAttendance([]AttendanceRecord{record})
	time.Sleep(50 * time.Millisecond) // let worker pick up and block

	// Fill remaining channel capacity.
	server.dispatchAttendance([]AttendanceRecord{record})

	// Dispatch in a goroutine — it will enter the slow-path and block.
	done := make(chan bool, 1)
	go func() {
		ok := server.dispatchAttendance([]AttendanceRecord{record})
		done <- ok
	}()

	// Give it a moment to enter the slow-path select, then close the server.
	time.Sleep(50 * time.Millisecond)
	close(blocker)
	server.Close()

	select {
	case ok := <-done:
		// Either false (server closed) or true (space freed) — both are valid.
		_ = ok
	case <-time.After(3 * time.Second):
		t.Fatal("dispatch goroutine did not return after server close")
	}
}

func TestDispatchAttendance_EmptyRecords(t *testing.T) {
	called := false
	server := NewADMSServer(WithOnAttendance(func(_ context.Context, _ AttendanceRecord) {
		called = true
	}))
	defer server.Close()

	ok := server.dispatchAttendance(nil)
	if !ok {
		t.Error("dispatchAttendance should return true for empty records")
	}
	if called {
		t.Error("callback should not be called for empty records")
	}
}

func TestDispatchAttendance_CallbackQueueFull(t *testing.T) {
	// Create server with tiny buffer and a blocking callback.
	blocker := make(chan struct{})
	server := NewADMSServer(
		WithCallbackBufferSize(1),
		WithDispatchTimeout(10*time.Millisecond),
		WithOnAttendance(func(_ context.Context, _ AttendanceRecord) {
			<-blocker // block forever until test is done
		}),
	)
	defer func() {
		close(blocker)
		server.Close()
	}()

	record := AttendanceRecord{
		SerialNumber: "DEV001",
		UserID:       "1",
		Timestamp:    time.Now(),
	}

	// Fill the callback channel.
	server.dispatchAttendance([]AttendanceRecord{record})
	// Give the worker a moment to pick up the first callback and block.
	time.Sleep(50 * time.Millisecond)

	// This should fill the channel.
	server.dispatchAttendance([]AttendanceRecord{record})

	// Now the channel is full and the worker is blocked — next dispatch should fail.
	ok := server.dispatchAttendance([]AttendanceRecord{record})
	if ok {
		t.Error("expected dispatch to fail when callback queue is full")
	}
}

func TestDispatchCommandResult_NilCallback(t *testing.T) {
	server := NewADMSServer() // no WithOnCommandResult
	defer server.Close()

	ok := server.dispatchCommandResult(CommandResult{ID: 1, Command: "INFO"})
	if !ok {
		t.Error("dispatchCommandResult should return true when no callback configured")
	}
}

func TestDispatchQueryUsers_EmptySlice(t *testing.T) {
	called := make(chan struct{}, 1)
	server := NewADMSServer(
		WithOnQueryUsers(func(_ context.Context, _ string, _ []UserRecord) {
			called <- struct{}{}
		}),
	)
	defer server.Close()

	// dispatchQueryUsers with empty slice should return true without calling the callback.
	if !server.dispatchQueryUsers("DEV001", nil) {
		t.Error("expected dispatchQueryUsers to return true for nil users")
	}
	if !server.dispatchQueryUsers("DEV001", []UserRecord{}) {
		t.Error("expected dispatchQueryUsers to return true for empty users")
	}

	select {
	case <-called:
		t.Error("callback should NOT be called for empty users")
	case <-time.After(100 * time.Millisecond):
		// expected
	}
}

func TestCallbackPanicRecovery(t *testing.T) {
	received := make(chan string, 1)
	var callCount atomic.Int64

	server := NewADMSServer(
		WithOnAttendance(func(_ context.Context, record AttendanceRecord) {
			n := callCount.Add(1)
			if n == 1 {
				panic("boom")
			}
			received <- record.UserID
		}),
	)
	defer server.Close()

	// First callback panics.
	req1 := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN=TEST001&table=ATTLOG",
		bytes.NewBufferString("100\t2024-01-01 08:00:00\t0\t1\t0"))
	w1 := httptest.NewRecorder()
	server.HandleCData(w1, req1)

	// Second callback should still work after panic recovery.
	req2 := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN=TEST001&table=ATTLOG",
		bytes.NewBufferString("200\t2024-01-01 09:00:00\t0\t1\t0"))
	w2 := httptest.NewRecorder()
	server.HandleCData(w2, req2)

	select {
	case uid := <-received:
		if uid != "200" {
			t.Errorf("Expected UserID 200, got %s", uid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Worker did not recover from panic; second callback never arrived")
	}
}

func TestCallbackStableAfterEnqueue(t *testing.T) {
	received := make(chan AttendanceRecord, 1)
	server := NewADMSServer(
		WithOnAttendance(func(_ context.Context, record AttendanceRecord) {
			received <- record
		}),
	)
	defer server.Close()

	attendanceData := "123\t2024-01-01 08:00:00\t0\t1\t0"
	req := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN=TEST001&table=ATTLOG", bytes.NewBufferString(attendanceData))
	w := httptest.NewRecorder()
	server.HandleCData(w, req)

	// The callback is captured when dispatchAttendance enqueues the closure; the enqueued
	// closure must still execute even if the server's callback is later mutated.
	select {
	case rec := <-received:
		if rec.UserID != "123" {
			t.Errorf("Expected UserID 123, got %s", rec.UserID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Timed out waiting for attendance callback")
	}
}

func TestAttendanceRecordSerialNumber(t *testing.T) {
	serialNumber := "TEST001"

	received := make(chan AttendanceRecord, 1)
	server := NewADMSServer(
		WithOnAttendance(func(_ context.Context, record AttendanceRecord) {
			received <- record
		}),
	)
	defer server.Close()

	attendanceData := "123\t2024-01-01 08:00:00\t0\t1\t0"
	req := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN="+serialNumber+"&table=ATTLOG", bytes.NewBufferString(attendanceData))
	w := httptest.NewRecorder()

	server.HandleCData(w, req)

	select {
	case record := <-received:
		if record.SerialNumber != serialNumber {
			t.Errorf("Expected SerialNumber %s, got %s", serialNumber, record.SerialNumber)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Timed out waiting for attendance callback")
	}
}

func TestConcurrentDispatchAndClose(t *testing.T) {
	server := NewADMSServer(
		WithCallbackBufferSize(10),
		WithOnAttendance(func(_ context.Context, _ AttendanceRecord) {}),
		WithOnCommandResult(func(_ context.Context, _ CommandResult) {}),
	)

	record := AttendanceRecord{UserID: "1", Timestamp: time.Now(), SerialNumber: "DEV001"}
	result := CommandResult{ID: 1, Command: "INFO"}

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			server.dispatchAttendance([]AttendanceRecord{record})
		})
		wg.Go(func() {
			server.dispatchCommandResult(result)
		})
	}

	// Close while dispatches are in flight.
	server.Close()
	wg.Wait()
}

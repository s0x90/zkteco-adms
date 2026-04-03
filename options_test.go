package zkadms

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWithOnAttendance_ReceivesContext(t *testing.T) {
	ctx := t.Context()

	received := make(chan context.Context, 1)
	server := NewADMSServer(
		WithBaseContext(ctx),
		WithOnAttendance(func(cbCtx context.Context, record AttendanceRecord) {
			received <- cbCtx
		}),
	)
	defer server.Close()

	data := "123\t2024-01-01 08:00:00\t0\t1\t0"
	req := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN=TEST001&table=ATTLOG", bytes.NewBufferString(data))
	w := httptest.NewRecorder()
	server.HandleCData(w, req)

	select {
	case cbCtx := <-received:
		// The callback context should be derived from our base context.
		// It should not be canceled yet (server still open).
		if err := cbCtx.Err(); err != nil {
			t.Errorf("expected non-canceled context, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Timed out waiting for WithOnAttendance callback")
	}
}

func TestWithOnAttendance_ContextCancelledAfterClose(t *testing.T) {
	received := make(chan context.Context, 1)
	server := NewADMSServer(
		WithOnAttendance(func(ctx context.Context, record AttendanceRecord) {
			received <- ctx
		}),
	)

	data := "123\t2024-01-01 08:00:00\t0\t1\t0"
	req := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN=TEST001&table=ATTLOG", bytes.NewBufferString(data))
	w := httptest.NewRecorder()
	server.HandleCData(w, req)

	// Get the context from the callback.
	var cbCtx context.Context
	select {
	case cbCtx = <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("Timed out waiting for callback")
	}

	// Close the server — this should cancel the base context.
	server.Close()

	if err := cbCtx.Err(); err == nil {
		t.Error("expected context to be canceled after Close, but it was not")
	}
}

func TestWithOnDeviceInfo_ReceivesContext(t *testing.T) {
	received := make(chan context.Context, 1)
	server := NewADMSServer(
		WithOnDeviceInfo(func(ctx context.Context, sn string, info map[string]string) {
			received <- ctx
		}),
	)
	defer server.Close()

	body := "DeviceName=ZKDevice\nSerialNumber=TEST001"
	req := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN=TEST001", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	server.HandleCData(w, req)

	select {
	case cbCtx := <-received:
		if err := cbCtx.Err(); err != nil {
			t.Errorf("expected non-canceled context, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Timed out waiting for WithOnDeviceInfo callback")
	}
}

func TestWithOnRegistry_ReceivesContext(t *testing.T) {
	received := make(chan context.Context, 1)
	server := NewADMSServer(
		WithOnRegistry(func(ctx context.Context, sn string, info map[string]string) {
			received <- ctx
		}),
	)
	defer server.Close()

	body := "DeviceType=acc"
	req := httptest.NewRequest(http.MethodPost, "/iclock/registry?SN=REG_CTX", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	server.HandleRegistry(w, req)

	select {
	case cbCtx := <-received:
		if err := cbCtx.Err(); err != nil {
			t.Errorf("expected non-canceled context, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Timed out waiting for WithOnRegistry callback")
	}
}

func TestWithLogger(t *testing.T) {
	// Just verify it doesn't panic and that the logger is used.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	server := NewADMSServer(WithLogger(logger))
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "/iclock/getrequest?SN=LOG001", nil)
	w := httptest.NewRecorder()
	server.HandleGetRequest(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// Our slog logger should have logged something at debug level.
	if buf.Len() == 0 {
		t.Error("expected slog logger to have output, but buffer is empty")
	}
}

func TestWithMaxBodySize_Option(t *testing.T) {
	server := NewADMSServer(WithMaxBodySize(10))
	defer server.Close()

	largeBody := strings.Repeat("x", 20)
	req := httptest.NewRequest(http.MethodPost,
		"/iclock/cdata?SN=TEST001&table=ATTLOG", strings.NewReader(largeBody))
	w := httptest.NewRecorder()
	server.HandleCData(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected status %d, got %d", http.StatusRequestEntityTooLarge, w.Code)
	}
}

func TestWithCallbackBufferSize(t *testing.T) {
	server := NewADMSServer(WithCallbackBufferSize(2))
	defer server.Close()

	// The channel capacity should be 2.
	if cap(server.callbackCh) != 2 {
		t.Errorf("expected callback channel capacity 2, got %d", cap(server.callbackCh))
	}
}

func TestWithOnlineThreshold(t *testing.T) {
	server := NewADMSServer(WithOnlineThreshold(50 * time.Millisecond))
	defer server.Close()

	if err := server.RegisterDevice("THRESH001"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}
	if !server.IsDeviceOnline("THRESH001") {
		t.Fatal("newly registered device should be online")
	}

	// Wait for the threshold to expire.
	time.Sleep(100 * time.Millisecond)

	if server.IsDeviceOnline("THRESH001") {
		t.Error("device should be offline after threshold expired")
	}
}

func TestWithDispatchTimeout(t *testing.T) {
	// Use a very short dispatch timeout so the test doesn't take long.
	server := NewADMSServer(WithDispatchTimeout(10 * time.Millisecond))
	defer server.Close()

	if server.dispatchTimeout != 10*time.Millisecond {
		t.Errorf("expected dispatchTimeout 10ms, got %v", server.dispatchTimeout)
	}
}

func TestWithBaseContext_Canceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	received := make(chan context.Context, 1)

	server := NewADMSServer(
		WithBaseContext(ctx),
		WithOnAttendance(func(cbCtx context.Context, record AttendanceRecord) {
			received <- cbCtx
		}),
	)
	defer server.Close()

	// Cancel the parent context.
	cancel()

	data := "123\t2024-01-01 08:00:00\t0\t1\t0"
	req := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN=TEST001&table=ATTLOG", bytes.NewBufferString(data))
	w := httptest.NewRecorder()
	server.HandleCData(w, req)

	select {
	case cbCtx := <-received:
		if err := cbCtx.Err(); err == nil {
			t.Error("expected context to be canceled (parent canceled), but it was not")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Timed out waiting for callback")
	}
}

func TestWithLogger_NilIgnored(t *testing.T) {
	// WithLogger(nil) should leave the default logger in place.
	server := NewADMSServer(WithLogger(nil))
	defer server.Close()

	if server.logger != slog.Default() {
		t.Error("expected default slog logger when WithLogger(nil) is passed")
	}
}

func TestWithMaxBodySize_ZeroIgnored(t *testing.T) {
	server := NewADMSServer(WithMaxBodySize(0))
	defer server.Close()

	if server.maxBodySize != defaultMaxBodySize {
		t.Errorf("expected default maxBodySize, got %d", server.maxBodySize)
	}
}

func TestWithCallbackBufferSize_ZeroIgnored(t *testing.T) {
	server := NewADMSServer(WithCallbackBufferSize(0))
	defer server.Close()

	if cap(server.callbackCh) != defaultCallbackBufferSize {
		t.Errorf("expected default buffer size, got %d", cap(server.callbackCh))
	}
}

func TestWithMaxDevices(t *testing.T) {
	server := NewADMSServer(WithMaxDevices(2))
	defer server.Close()

	if err := server.RegisterDevice("DEV1"); err != nil {
		t.Fatalf("first RegisterDevice failed: %v", err)
	}
	if err := server.RegisterDevice("DEV2"); err != nil {
		t.Fatalf("second RegisterDevice failed: %v", err)
	}

	// Third should fail.
	err := server.RegisterDevice("DEV3")
	if !errors.Is(err, ErrMaxDevicesReached) {
		t.Errorf("expected ErrMaxDevicesReached, got %v", err)
	}

	// Re-registering an existing device should succeed (not counted as new).
	if err := server.RegisterDevice("DEV1"); err != nil {
		t.Errorf("re-registering existing device should succeed, got %v", err)
	}
}

func TestWithMaxDevices_ZeroMeansUnlimited(t *testing.T) {
	server := NewADMSServer(WithMaxDevices(0))
	defer server.Close()

	// Should be able to register many devices with 0 meaning unlimited.
	for i := range 50 {
		sn := "DEV" + strings.Repeat("A", i+1)
		if len(sn) > 64 {
			sn = sn[:64]
		}
		_ = server.RegisterDevice(sn)
	}
}

func TestWithMaxCommandsPerDevice(t *testing.T) {
	server := NewADMSServer(WithMaxCommandsPerDevice(3))
	defer server.Close()

	sn := "CMD001"
	if err := server.RegisterDevice(sn); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	ids := make([]int64, 3)
	for i := range 3 {
		id, err := server.QueueCommand(sn, "CMD"+string(rune('1'+i)))
		if err != nil {
			t.Fatalf("QueueCommand %d failed: %v", i+1, err)
		}
		ids[i] = id
	}

	// Fourth should fail — 3 queued + 0 pending = 3, which equals the limit.
	_, err := server.QueueCommand(sn, "CMD4")
	if !errors.Is(err, ErrCommandQueueFull) {
		t.Errorf("expected ErrCommandQueueFull, got %v", err)
	}

	// Draining moves commands out of the queue, but they remain in
	// pendingCommands until the device confirms execution. Simulate
	// device confirmation via HandleDeviceCmd to free the slots.
	_ = server.DrainCommands(sn)
	for _, id := range ids {
		body := fmt.Sprintf("ID=%d&Return=0&CMD=DATA", id)
		req := httptest.NewRequest(http.MethodPost,
			"/iclock/devicecmd?SN="+sn, strings.NewReader(body))
		w := httptest.NewRecorder()
		server.HandleDeviceCmd(w, req)
	}

	// Now all 3 pending entries are confirmed and removed; queueing should succeed.
	if _, err := server.QueueCommand(sn, "CMD5"); err != nil {
		t.Errorf("after drain+confirm, QueueCommand should succeed: %v", err)
	}
}

func TestWithMaxCommandsPerDevice_ZeroMeansUnlimited(t *testing.T) {
	server := NewADMSServer(WithMaxCommandsPerDevice(0))
	defer server.Close()

	if err := server.RegisterDevice("DEV1"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	for i := range 100 {
		if _, err := server.QueueCommand("DEV1", fmt.Sprintf("CMD_%d", i)); err != nil {
			t.Fatalf("QueueCommand %d failed: %v", i, err)
		}
	}
}

func TestWithMaxDevices_NegativeIgnored(t *testing.T) {
	server := NewADMSServer(WithMaxDevices(-5))
	defer server.Close()

	if server.maxDevices != defaultMaxDevices {
		t.Errorf("expected negative to be ignored (default %d), got %d", defaultMaxDevices, server.maxDevices)
	}
}

func TestWithMaxCommandsPerDevice_NegativeIgnored(t *testing.T) {
	server := NewADMSServer(WithMaxCommandsPerDevice(-5))
	defer server.Close()

	if server.maxCommandsPerDevice != 0 {
		t.Errorf("expected negative to be ignored (0), got %d", server.maxCommandsPerDevice)
	}
}

func TestInspectDisabledByDefault(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "/iclock/inspect", nil)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 when inspect disabled, got %d", w.Code)
	}
}

func TestInspectEnabledWithOption(t *testing.T) {
	server := NewADMSServer(WithEnableInspect())
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "/iclock/inspect", nil)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 when inspect enabled, got %d", w.Code)
	}
}

func TestWithDeviceEvictionInterval(t *testing.T) {
	server := NewADMSServer(WithDeviceEvictionInterval(42 * time.Second))
	defer server.Close()

	if server.deviceEvictionInterval != 42*time.Second {
		t.Errorf("expected 42s, got %v", server.deviceEvictionInterval)
	}
}

func TestWithDeviceEvictionInterval_ZeroIgnored(t *testing.T) {
	server := NewADMSServer(WithDeviceEvictionInterval(0))
	defer server.Close()

	if server.deviceEvictionInterval != defaultDeviceEvictionInterval {
		t.Errorf("expected default %v, got %v", defaultDeviceEvictionInterval, server.deviceEvictionInterval)
	}
}

func TestWithDeviceEvictionTimeout(t *testing.T) {
	server := NewADMSServer(WithDeviceEvictionTimeout(12 * time.Hour))
	defer server.Close()

	if server.deviceEvictionTimeout != 12*time.Hour {
		t.Errorf("expected 12h, got %v", server.deviceEvictionTimeout)
	}
}

func TestWithDeviceEvictionTimeout_ZeroIgnored(t *testing.T) {
	server := NewADMSServer(WithDeviceEvictionTimeout(0))
	defer server.Close()

	if server.deviceEvictionTimeout != defaultDeviceEvictionTimeout {
		t.Errorf("expected default %v, got %v", defaultDeviceEvictionTimeout, server.deviceEvictionTimeout)
	}
}

func TestWithUnlimitedDevices(t *testing.T) {
	server := NewADMSServer(WithUnlimitedDevices())
	defer server.Close()

	if server.maxDevices != 0 {
		t.Errorf("expected unlimited (0), got %d", server.maxDevices)
	}

	// Should accept many devices without error.
	for i := range 50 {
		sn := fmt.Sprintf("UNLIM%03d", i)
		if err := server.RegisterDevice(sn); err != nil {
			t.Fatalf("RegisterDevice(%s) failed: %v", sn, err)
		}
	}
}

func TestDefaultMaxDevices(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if server.maxDevices != 1000 {
		t.Errorf("expected default maxDevices=1000, got %d", server.maxDevices)
	}
}

func TestWithOnCommandResult_ReceivesContext(t *testing.T) {
	received := make(chan context.Context, 1)
	server := NewADMSServer(
		WithBaseContext(t.Context()),
		WithOnCommandResult(func(ctx context.Context, result CommandResult) {
			received <- ctx
		}),
	)
	defer server.Close()

	body := "ID=1&Return=0&CMD=INFO"
	req := httptest.NewRequest(http.MethodPost, "/iclock/devicecmd?SN=CTX001", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	server.HandleDeviceCmd(w, req)

	select {
	case cbCtx := <-received:
		if err := cbCtx.Err(); err != nil {
			t.Errorf("expected non-canceled context, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Timed out waiting for WithOnCommandResult callback")
	}
}

func TestWithOnCommandResult_ContextCancelledAfterClose(t *testing.T) {
	received := make(chan context.Context, 1)
	server := NewADMSServer(
		WithOnCommandResult(func(ctx context.Context, result CommandResult) {
			received <- ctx
		}),
	)

	body := "ID=1&Return=0&CMD=INFO"
	req := httptest.NewRequest(http.MethodPost, "/iclock/devicecmd?SN=CTXCLOSE", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	server.HandleDeviceCmd(w, req)

	var cbCtx context.Context
	select {
	case cbCtx = <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("Timed out waiting for callback")
	}

	server.Close()

	if err := cbCtx.Err(); err == nil {
		t.Error("expected context to be canceled after Close, but it was not")
	}
}

func TestWithOnQueryUsers(t *testing.T) {
	called := false
	fn := func(_ context.Context, _ string, _ []UserRecord) {
		called = true
	}
	server := NewADMSServer(WithOnQueryUsers(fn))
	defer server.Close()

	if server.onQueryUsers == nil {
		t.Fatal("expected onQueryUsers to be set")
	}

	// Invoke the stored callback to verify it's the one we passed.
	server.onQueryUsers(context.Background(), "TEST", nil)
	if !called {
		t.Error("expected callback to have been invoked")
	}
}

func TestWithDefaultTimezone(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}

	server := NewADMSServer(WithDefaultTimezone(berlin))
	defer server.Close()

	if server.defaultTimezone.String() != "Europe/Berlin" {
		t.Errorf("expected server default Europe/Berlin, got %s", server.defaultTimezone.String())
	}
}

func TestWithDefaultTimezone_NilIgnored(t *testing.T) {
	server := NewADMSServer(WithDefaultTimezone(nil))
	defer server.Close()

	if server.defaultTimezone.String() != "UTC" {
		t.Errorf("expected UTC when nil passed, got %s", server.defaultTimezone.String())
	}
}

func TestWithDeviceTimezone_NilSetsNil(t *testing.T) {
	d := &Device{}
	opt := WithDeviceTimezone(nil)
	opt(d)
	if d.Timezone != nil {
		t.Errorf("expected nil timezone, got %v", d.Timezone)
	}
}

package zkadms

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRegisterDevice(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()
	serialNumber := "TEST001"

	if err := server.RegisterDevice(serialNumber); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	device := server.GetDevice(serialNumber)
	if device == nil {
		t.Fatal("Device not registered")
	}
	if device.SerialNumber != serialNumber {
		t.Errorf("Expected serial number %s, got %s", serialNumber, device.SerialNumber)
	}
	if device.Options == nil {
		t.Error("Device options not initialized")
	}
}

func TestIsDeviceOnline(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	// Non-existent device
	if server.IsDeviceOnline("GHOST") {
		t.Error("Expected false for non-existent device")
	}

	if err := server.RegisterDevice("ONLINE001"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}
	if !server.IsDeviceOnline("ONLINE001") {
		t.Error("Expected newly registered device to be online")
	}

	// Simulate stale activity
	server.devicesMutex.Lock()
	server.devices["ONLINE001"].LastActivity = time.Now().Add(-5 * time.Minute)
	server.devicesMutex.Unlock()

	if server.IsDeviceOnline("ONLINE001") {
		t.Error("Expected device with stale activity to be offline")
	}
}

func TestListDevices(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	for _, sn := range []string{"TEST001", "TEST002", "TEST003"} {
		if err := server.RegisterDevice(sn); err != nil {
			t.Fatalf("RegisterDevice(%s) failed: %v", sn, err)
		}
	}

	devices := server.ListDevices()
	if len(devices) != 3 {
		t.Errorf("Expected 3 devices, got %d", len(devices))
	}

	// Check that all devices are present
	serialNumbers := make(map[string]bool)
	for _, device := range devices {
		serialNumbers[device.SerialNumber] = true
	}

	for _, sn := range []string{"TEST001", "TEST002", "TEST003"} {
		if !serialNumbers[sn] {
			t.Errorf("Expected device %s in list", sn)
		}
	}
}

func TestUpdateDeviceActivity(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()
	serialNumber := "TEST001"

	if err := server.RegisterDevice(serialNumber); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	time.Sleep(10 * time.Millisecond)

	initialTime := server.GetDevice(serialNumber).LastActivity

	time.Sleep(10 * time.Millisecond)

	server.updateDeviceActivity(serialNumber)

	updatedTime := server.GetDevice(serialNumber).LastActivity

	if !updatedTime.After(initialTime) {
		t.Error("Expected LastActivity to be updated")
	}
}

func TestConcurrentAccess(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()
	serialNumber := "TEST001"

	// Register device first so QueueCommand calls succeed.
	if err := server.RegisterDevice(serialNumber); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	var wg sync.WaitGroup

	// Concurrent device registration (idempotent)
	for range 10 {
		wg.Go(func() {
			_ = server.RegisterDevice(serialNumber)
		})
	}

	// Concurrent command queuing
	for range 10 {
		wg.Go(func() {
			_, _ = server.QueueCommand(serialNumber, "INFO")
		})
	}

	// Wait for all goroutines
	wg.Wait()

	// Verify device exists
	device := server.GetDevice(serialNumber)
	if device == nil {
		t.Error("Device should exist after concurrent registration")
	}

	// Verify commands were queued
	commands := server.DrainCommands(serialNumber)
	if len(commands) != 10 {
		t.Errorf("Expected 10 commands, got %d", len(commands))
	}
}

func TestRegisterDevice_InvalidSerialNumber(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	err := server.RegisterDevice("")
	if !errors.Is(err, ErrInvalidSerialNumber) {
		t.Errorf("expected ErrInvalidSerialNumber for empty SN, got %v", err)
	}

	err = server.RegisterDevice("BAD SN")
	if !errors.Is(err, ErrInvalidSerialNumber) {
		t.Errorf("expected ErrInvalidSerialNumber for SN with space, got %v", err)
	}

	err = server.RegisterDevice(strings.Repeat("X", 65))
	if !errors.Is(err, ErrInvalidSerialNumber) {
		t.Errorf("expected ErrInvalidSerialNumber for too-long SN, got %v", err)
	}
}

func TestRegisterDevice_Idempotent(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if err := server.RegisterDevice("DEV1"); err != nil {
		t.Fatalf("first register failed: %v", err)
	}
	// Registering again should succeed and not create a duplicate.
	if err := server.RegisterDevice("DEV1"); err != nil {
		t.Fatalf("second register should be idempotent, got: %v", err)
	}

	devices := server.ListDevices()
	if len(devices) != 1 {
		t.Errorf("expected 1 device after duplicate register, got %d", len(devices))
	}
}

// --- Eviction worker tests ---

func TestEvictionWorker_RemovesStaleDevices(t *testing.T) {
	server := NewADMSServer(
		WithDeviceEvictionInterval(50*time.Millisecond),
		WithDeviceEvictionTimeout(100*time.Millisecond),
	)
	defer server.Close()

	if err := server.RegisterDevice("STALE01"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	// Backdate the device's LastActivity so it appears stale.
	server.devicesMutex.Lock()
	server.devices["STALE01"].LastActivity = time.Now().Add(-time.Hour)
	server.devicesMutex.Unlock()

	// Wait long enough for at least one eviction cycle.
	time.Sleep(200 * time.Millisecond)

	if d := server.GetDevice("STALE01"); d != nil {
		t.Error("expected stale device to be evicted, but it still exists")
	}
}

func TestEvictionWorker_KeepsActiveDevices(t *testing.T) {
	server := NewADMSServer(
		WithDeviceEvictionInterval(50*time.Millisecond),
		WithDeviceEvictionTimeout(10*time.Second),
	)
	defer server.Close()

	if err := server.RegisterDevice("ACTIVE01"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	// Wait for a few eviction cycles.
	time.Sleep(200 * time.Millisecond)

	if d := server.GetDevice("ACTIVE01"); d == nil {
		t.Error("expected active device to survive eviction, but it was removed")
	}
}

func TestEvictionWorker_CleansCommandQueue(t *testing.T) {
	server := NewADMSServer(
		WithDeviceEvictionInterval(50*time.Millisecond),
		WithDeviceEvictionTimeout(100*time.Millisecond),
	)
	defer server.Close()

	if err := server.RegisterDevice("CMDDEV01"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}
	if _, err := server.QueueCommand("CMDDEV01", "REBOOT"); err != nil {
		t.Fatalf("QueueCommand failed: %v", err)
	}

	// Backdate so device is stale.
	server.devicesMutex.Lock()
	server.devices["CMDDEV01"].LastActivity = time.Now().Add(-time.Hour)
	server.devicesMutex.Unlock()

	time.Sleep(200 * time.Millisecond)

	// Device should be gone.
	if d := server.GetDevice("CMDDEV01"); d != nil {
		t.Error("expected stale device to be evicted")
	}
	// Command queue should be cleaned up.
	cmds := server.DrainCommands("CMDDEV01")
	if len(cmds) != 0 {
		t.Errorf("expected empty command queue after eviction, got %d commands", len(cmds))
	}
}

func TestEvictionWorker_StopsOnClose(t *testing.T) {
	server := NewADMSServer(
		WithDeviceEvictionInterval(10*time.Millisecond),
		WithDeviceEvictionTimeout(time.Hour),
	)

	// Close should return promptly, meaning the eviction worker exited.
	done := make(chan struct{})
	go func() {
		server.Close()
		close(done)
	}()

	select {
	case <-done:
		// Success: Close returned.
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return within 2 seconds; eviction worker may be stuck")
	}
}

func TestUpdateDeviceActivity_UnknownDevice(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	// Should not panic; it's a no-op for unknown devices.
	server.updateDeviceActivity("UNKNOWN123")
}

func TestUpdateDeviceActivity_OfflineToOnline(t *testing.T) {
	server := NewADMSServer(WithOnlineThreshold(100 * time.Millisecond))
	defer server.Close()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}

	// RegisterDevice sets LastActivity to time.Now(), so the device starts online.
	if !server.IsDeviceOnline("DEV001") {
		t.Fatal("device should be online immediately after registration")
	}

	// Wait for the threshold to expire so the device goes offline.
	time.Sleep(150 * time.Millisecond)

	if server.IsDeviceOnline("DEV001") {
		t.Fatal("device should be offline after threshold expiry")
	}

	// Touch activity — triggers the offline→online transition (covers the log path).
	server.updateDeviceActivity("DEV001")

	if !server.IsDeviceOnline("DEV001") {
		t.Error("device should be online after updateDeviceActivity")
	}

	// Let it expire again, then touch once more to exercise the path a second time.
	time.Sleep(150 * time.Millisecond)

	if server.IsDeviceOnline("DEV001") {
		t.Fatal("device should be offline after second threshold expiry")
	}

	server.updateDeviceActivity("DEV001")

	if !server.IsDeviceOnline("DEV001") {
		t.Error("device should be online again after second updateDeviceActivity")
	}
}

func TestPendingCommands_CleanupOnEviction(t *testing.T) {
	server := NewADMSServer(
		WithDeviceEvictionInterval(50*time.Millisecond),
		WithDeviceEvictionTimeout(100*time.Millisecond),
	)
	defer server.Close()

	if err := server.RegisterDevice("EVICT01"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	// Queue commands, then drain to populate pendingCommands.
	id1, err := server.QueueCommand("EVICT01", "CMD_A")
	if err != nil {
		t.Fatalf("QueueCommand A failed: %v", err)
	}
	id2, err := server.QueueCommand("EVICT01", "CMD_B")
	if err != nil {
		t.Fatalf("QueueCommand B failed: %v", err)
	}
	_ = server.DrainCommands("EVICT01")

	// Verify entries exist in pendingCommands before eviction.
	server.queueMutex.Lock()
	_, ok1 := server.pendingCommands[id1]
	_, ok2 := server.pendingCommands[id2]
	server.queueMutex.Unlock()
	if !ok1 || !ok2 {
		t.Fatal("expected both pending commands to exist before eviction")
	}

	// Backdate last activity so eviction picks it up.
	server.devicesMutex.Lock()
	server.devices["EVICT01"].LastActivity = time.Now().Add(-time.Hour)
	server.devicesMutex.Unlock()

	// Wait for eviction cycle.
	time.Sleep(250 * time.Millisecond)

	if d := server.GetDevice("EVICT01"); d != nil {
		t.Error("expected device to be evicted")
	}

	// Verify pendingCommands entries were cleaned up.
	server.queueMutex.Lock()
	_, still1 := server.pendingCommands[id1]
	_, still2 := server.pendingCommands[id2]
	server.queueMutex.Unlock()
	if still1 || still2 {
		t.Error("expected pending commands to be cleaned up after eviction")
	}
}

func TestPendingCommands_CleanupOnEviction_Drained(t *testing.T) {
	server := NewADMSServer(
		WithDeviceEvictionInterval(50*time.Millisecond),
		WithDeviceEvictionTimeout(100*time.Millisecond),
	)
	defer server.Close()

	if err := server.RegisterDevice("EVICT02"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	// Queue commands to populate pendingCommands.
	id1, err := server.QueueCommand("EVICT02", "CMD_X")
	if err != nil {
		t.Fatalf("QueueCommand X failed: %v", err)
	}
	id2, err := server.QueueCommand("EVICT02", "CMD_Y")
	if err != nil {
		t.Fatalf("QueueCommand Y failed: %v", err)
	}

	// Drain commands — simulates the device fetching them via /iclock/devicecmd.
	// This removes them from commandQueue but leaves them in pendingCommands.
	drained := server.DrainCommands("EVICT02")
	if len(drained) != 2 {
		t.Fatalf("expected 2 drained commands, got %d", len(drained))
	}

	// Verify entries still exist in pendingCommands after drain.
	server.queueMutex.Lock()
	_, ok1 := server.pendingCommands[id1]
	_, ok2 := server.pendingCommands[id2]
	server.queueMutex.Unlock()
	if !ok1 || !ok2 {
		t.Fatal("expected pending commands to survive drain")
	}

	// Backdate last activity so eviction picks it up.
	server.devicesMutex.Lock()
	server.devices["EVICT02"].LastActivity = time.Now().Add(-time.Hour)
	server.devicesMutex.Unlock()

	// Wait for eviction cycle.
	time.Sleep(250 * time.Millisecond)

	if d := server.GetDevice("EVICT02"); d != nil {
		t.Error("expected device to be evicted")
	}

	// Verify pendingCommands entries were cleaned up even though commands
	// had already been drained from the queue before eviction.
	server.queueMutex.Lock()
	_, still1 := server.pendingCommands[id1]
	_, still2 := server.pendingCommands[id2]
	server.queueMutex.Unlock()
	if still1 || still2 {
		t.Error("expected drained pending commands to be cleaned up after eviction")
	}
}

func TestRegisterDevice_WithTimezone(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	istanbul, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		t.Fatal(err)
	}

	if err := server.RegisterDevice("DEV001", WithDeviceTimezone(istanbul)); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	dev := server.GetDevice("DEV001")
	if dev == nil {
		t.Fatal("device should be registered")
	}
	if dev.Timezone == nil {
		t.Fatal("expected device timezone to be set")
	}
	if dev.Timezone.String() != "Europe/Istanbul" {
		t.Errorf("expected Europe/Istanbul, got %s", dev.Timezone.String())
	}
}

func TestRegisterDevice_WithoutTimezone(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	dev := server.GetDevice("DEV001")
	if dev.Timezone != nil {
		t.Errorf("expected nil timezone, got %s", dev.Timezone.String())
	}
}

func TestRegisterDevice_ReRegistration_AppliesOpts(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}
	dev := server.GetDevice("DEV001")
	if dev.Timezone != nil {
		t.Errorf("expected nil timezone initially, got %v", dev.Timezone)
	}

	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	if err := server.RegisterDevice("DEV001", WithDeviceTimezone(berlin)); err != nil {
		t.Fatalf("re-RegisterDevice failed: %v", err)
	}
	dev = server.GetDevice("DEV001")
	if dev.Timezone == nil || dev.Timezone.String() != "Europe/Berlin" {
		t.Errorf("expected Europe/Berlin after re-registration, got %v", dev.Timezone)
	}
}

func TestSetDeviceTimezone(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	if err := server.SetDeviceTimezone("DEV001", tokyo); err != nil {
		t.Fatalf("SetDeviceTimezone failed: %v", err)
	}

	dev := server.GetDevice("DEV001")
	if dev.Timezone == nil || dev.Timezone.String() != "Asia/Tokyo" {
		t.Errorf("expected Asia/Tokyo, got %v", dev.Timezone)
	}
}

func TestSetDeviceTimezone_ClearWithNil(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	if err := server.RegisterDevice("DEV001", WithDeviceTimezone(tokyo)); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	if err := server.SetDeviceTimezone("DEV001", nil); err != nil {
		t.Fatalf("SetDeviceTimezone(nil) failed: %v", err)
	}
	dev := server.GetDevice("DEV001")
	if dev.Timezone != nil {
		t.Errorf("expected nil timezone after clearing, got %v", dev.Timezone)
	}
}

func TestSetDeviceTimezone_UnknownDevice(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	err := server.SetDeviceTimezone("NOSUCH", time.UTC)
	if !errors.Is(err, ErrDeviceNotFound) {
		t.Errorf("expected ErrDeviceNotFound, got %v", err)
	}
}

func TestGetDeviceTimezone_FallbackChain(t *testing.T) {
	// Case 1: device timezone set → returns device timezone
	istanbul, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		t.Fatal(err)
	}
	server := NewADMSServer()
	defer server.Close()

	if err := server.RegisterDevice("DEV001", WithDeviceTimezone(istanbul)); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}
	loc := server.GetDeviceTimezone("DEV001")
	if loc == nil || loc.String() != "Europe/Istanbul" {
		t.Errorf("expected Europe/Istanbul, got %v", loc)
	}

	// Case 2: no device timezone → returns server default
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	server2 := NewADMSServer(WithDefaultTimezone(berlin))
	defer server2.Close()

	if err := server2.RegisterDevice("DEV002"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}
	loc = server2.GetDeviceTimezone("DEV002")
	if loc == nil || loc.String() != "Europe/Berlin" {
		t.Errorf("expected Europe/Berlin (server default), got %v", loc)
	}

	// Case 3: no device timezone, no custom server default → returns UTC
	server3 := NewADMSServer()
	defer server3.Close()

	if err := server3.RegisterDevice("DEV003"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}
	loc = server3.GetDeviceTimezone("DEV003")
	if loc == nil || loc.String() != "UTC" {
		t.Errorf("expected UTC (fallback), got %v", loc)
	}

	// Case 4: unknown device → returns nil
	loc = server3.GetDeviceTimezone("NOSUCH")
	if loc != nil {
		t.Errorf("expected nil for unknown device, got %v", loc)
	}
}

func TestGetDeviceTimezone_DeviceOverridesDefault(t *testing.T) {
	istanbul, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		t.Fatal(err)
	}
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}

	server := NewADMSServer(WithDefaultTimezone(berlin))
	defer server.Close()

	if err := server.RegisterDevice("DEV001", WithDeviceTimezone(istanbul)); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	loc := server.GetDeviceTimezone("DEV001")
	if loc == nil || loc.String() != "Europe/Istanbul" {
		t.Errorf("expected device-specific Europe/Istanbul to override server default, got %v", loc)
	}
}

func TestDeviceLocationLocked_NilDefaultTimezone(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()
	// Force defaultTimezone to nil to exercise the UTC fallback.
	server.defaultTimezone = nil
	server.devicesMutex.RLock()
	loc := server.deviceLocationLocked("NONEXISTENT")
	server.devicesMutex.RUnlock()
	if loc != time.UTC {
		t.Errorf("expected UTC fallback, got %v", loc)
	}
}

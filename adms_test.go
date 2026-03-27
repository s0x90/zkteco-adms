package zkadms

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// errReader is an io.Reader that always returns an error.
type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

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

func TestGetDeviceReturnsCopy(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if err := server.RegisterDevice("COPY001"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	d1 := server.GetDevice("COPY001")
	d1.SerialNumber = "MUTATED"
	d1.Options["injected"] = "bad"

	d2 := server.GetDevice("COPY001")
	if d2.SerialNumber != "COPY001" {
		t.Errorf("Internal state was mutated: got SerialNumber %s", d2.SerialNumber)
	}
	if d2.Options["injected"] != "" {
		t.Errorf("Internal Options was mutated: got injected=%s", d2.Options["injected"])
	}
}

func TestListDevicesReturnsCopies(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if err := server.RegisterDevice("LD001"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	devices := server.ListDevices()
	if len(devices) != 1 {
		t.Fatalf("Expected 1 device, got %d", len(devices))
	}
	devices[0].SerialNumber = "MUTATED"
	devices[0].Options["injected"] = "bad"

	fresh := server.GetDevice("LD001")
	if fresh.SerialNumber != "LD001" {
		t.Errorf("Internal state was mutated via ListDevices")
	}
	if fresh.Options["injected"] != "" {
		t.Errorf("Internal Options was mutated via ListDevices")
	}
}

func TestQueueAndGetCommands(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()
	serialNumber := "TEST001"

	if _, err := server.QueueCommand(serialNumber, "INFO"); err != nil {
		t.Fatalf("QueueCommand failed: %v", err)
	}
	if _, err := server.QueueCommand(serialNumber, "DATA UPDATE USERINFO PIN=1001\tName=Test\tPrivilege=0\tCard="); err != nil {
		t.Fatalf("QueueCommand failed: %v", err)
	}

	commands := server.DrainCommands(serialNumber)
	if len(commands) != 2 {
		t.Errorf("Expected 2 commands, got %d", len(commands))
	}
	if commands[0].cmd != "INFO" {
		t.Errorf("Expected first command to be INFO, got %s", commands[0].cmd)
	}
	if commands[1].cmd != "DATA UPDATE USERINFO PIN=1001\tName=Test\tPrivilege=0\tCard=" {
		t.Errorf("Expected second command to be DATA UPDATE USERINFO, got %s", commands[1].cmd)
	}

	// Queue should be cleared after retrieval
	commands = server.DrainCommands(serialNumber)
	if len(commands) != 0 {
		t.Errorf("Expected empty queue after retrieval, got %d commands", len(commands))
	}
}

func TestDrainCommandsDeletesKey(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if _, err := server.QueueCommand("DEL001", "INFO"); err != nil {
		t.Fatalf("QueueCommand failed: %v", err)
	}
	_ = server.DrainCommands("DEL001")

	server.queueMutex.RLock()
	_, exists := server.commandQueue["DEL001"]
	server.queueMutex.RUnlock()

	if exists {
		t.Error("Expected map key to be deleted after DrainCommands, but it still exists")
	}
}

func TestHandleCData_MissingSN(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "/iclock/cdata", nil)
	w := httptest.NewRecorder()

	server.HandleCData(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Missing SN parameter") {
		t.Errorf("Expected error message about missing SN, got: %s", w.Body.String())
	}
}

func TestHandleCData_AttendanceLog(t *testing.T) {
	received := make(chan AttendanceRecord, 1)
	server := NewADMSServer(
		WithOnAttendance(func(_ context.Context, record AttendanceRecord) {
			received <- record
		}),
	)
	defer server.Close()

	// Simulate attendance data
	attendanceData := "123\t2024-01-01 08:00:00\t0\t1\t0"
	req := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN=TEST001&table=ATTLOG", bytes.NewBufferString(attendanceData))
	w := httptest.NewRecorder()

	server.HandleCData(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	select {
	case record := <-received:
		if record.UserID != "123" {
			t.Errorf("Expected UserID 123, got %s", record.UserID)
		}
		if record.Status != 0 {
			t.Errorf("Expected Status 0, got %d", record.Status)
		}
		if record.VerifyMode != 1 {
			t.Errorf("Expected VerifyMode 1, got %d", record.VerifyMode)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Timed out waiting for attendance callback")
	}
}

func TestHandleCData_MultipleAttendanceRecords(t *testing.T) {
	received := make(chan AttendanceRecord, 2)
	server := NewADMSServer(
		WithOnAttendance(func(_ context.Context, record AttendanceRecord) {
			received <- record
		}),
	)
	defer server.Close()

	// Multiple records
	attendanceData := "123\t2024-01-01 08:00:00\t0\t1\t0\n456\t2024-01-01 17:00:00\t1\t1\t0"
	req := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN=TEST001&table=ATTLOG", bytes.NewBufferString(attendanceData))
	w := httptest.NewRecorder()

	server.HandleCData(w, req)

	for i := range 2 {
		select {
		case <-received:
		case <-time.After(2 * time.Second):
			t.Fatalf("Timed out waiting for record %d", i+1)
		}
	}
}

func TestHandleCData_OperationLog(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	req := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN=TEST001&table=OPERLOG", bytes.NewBufferString("operation data"))
	w := httptest.NewRecorder()

	server.HandleCData(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "OK") {
		t.Errorf("Expected OK response, got: %s", w.Body.String())
	}
}

func TestHandleCData_WithPendingCommands(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()
	serialNumber := "TEST001"

	if _, err := server.QueueCommand(serialNumber, "INFO"); err != nil {
		t.Fatalf("QueueCommand failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN="+serialNumber, nil)
	w := httptest.NewRecorder()

	server.HandleCData(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}
	if want := "C:1:INFO\n"; w.Body.String() != want {
		t.Errorf("Expected response %q, got: %q", want, w.Body.String())
	}
}

func TestHandleGetRequest(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()
	serialNumber := "TEST001"

	if _, err := server.QueueCommand(serialNumber, "DATA DELETE USERINFO PIN=1001"); err != nil {
		t.Fatalf("QueueCommand failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/iclock/getrequest?SN="+serialNumber, nil)
	w := httptest.NewRecorder()

	server.HandleGetRequest(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}
	if want := "C:1:DATA DELETE USERINFO PIN=1001\n"; w.Body.String() != want {
		t.Errorf("Expected response %q, got: %q", want, w.Body.String())
	}
}

func TestHandleGetRequest_NoCommands(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "/iclock/getrequest?SN=TEST001", nil)
	w := httptest.NewRecorder()

	server.HandleGetRequest(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}
	if w.Body.String() != "OK" {
		t.Errorf("Expected OK response when no commands, got: %s", w.Body.String())
	}
}

func TestHandleDeviceCmd(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	req := httptest.NewRequest(http.MethodPost, "/iclock/devicecmd?SN=TEST001", bytes.NewBufferString("command result"))
	w := httptest.NewRecorder()

	server.HandleDeviceCmd(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "OK") {
		t.Errorf("Expected OK in response, got: %s", w.Body.String())
	}
}

func TestHandleDeviceCmd_RegistersDevice(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	// Device was never registered — HandleDeviceCmd should register it.
	req := httptest.NewRequest(http.MethodPost, "/iclock/devicecmd?SN=NEW001", bytes.NewBufferString("result"))
	w := httptest.NewRecorder()

	server.HandleDeviceCmd(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}
	device := server.GetDevice("NEW001")
	if device == nil {
		t.Fatal("Expected HandleDeviceCmd to register the device, but GetDevice returned nil")
	}
	if device.SerialNumber != "NEW001" {
		t.Errorf("Expected serial NEW001, got %s", device.SerialNumber)
	}
}

func TestServeHTTP(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	testCases := []struct {
		name     string
		method   string
		path     string
		wantCode int
	}{
		{"cdata GET", "GET", "/iclock/cdata?SN=TEST001", http.StatusOK},
		{"cdata POST", "POST", "/iclock/cdata?SN=TEST001", http.StatusOK},
		{"getrequest GET", "GET", "/iclock/getrequest?SN=TEST001", http.StatusOK},
		{"devicecmd POST", "POST", "/iclock/devicecmd?SN=TEST001", http.StatusOK},
		{"registry GET", "GET", "/iclock/registry?SN=TEST001", http.StatusOK},
		{"registry POST", "POST", "/iclock/registry?SN=TEST001", http.StatusOK},
		{"inspect GET disabled by default", "GET", "/iclock/inspect", http.StatusNotFound},
		{"unknown endpoint", "GET", "/iclock/unknown", http.StatusNotFound},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			w := httptest.NewRecorder()

			server.ServeHTTP(w, req)

			if w.Code != tc.wantCode {
				t.Errorf("Expected status %d, got %d", tc.wantCode, w.Code)
			}
		})
	}
}

func TestMethodNotAllowed(t *testing.T) {
	server := NewADMSServer(WithEnableInspect())
	defer server.Close()

	testCases := []struct {
		name   string
		method string
		path   string
	}{
		{"DELETE on cdata", "DELETE", "/iclock/cdata?SN=TEST001"},
		{"PUT on cdata", "PUT", "/iclock/cdata?SN=TEST001"},
		{"POST on getrequest", "POST", "/iclock/getrequest?SN=TEST001"},
		{"GET on devicecmd", "GET", "/iclock/devicecmd?SN=TEST001"},
		{"DELETE on registry", "DELETE", "/iclock/registry?SN=TEST001"},
		{"POST on inspect", "POST", "/iclock/inspect"},
		{"PUT on inspect", "PUT", "/iclock/inspect"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			w := httptest.NewRecorder()

			server.ServeHTTP(w, req)

			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("Expected 405, got %d", w.Code)
			}
		})
	}
}

func TestHandleRegistry_PostParsesBody(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()
	body := "DeviceType=acc,~DeviceName=SpeedFace,IPAddress=192.168.1.201"
	req := httptest.NewRequest(http.MethodPost, "/iclock/registry?SN=REG001", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	server.HandleRegistry(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	dev := server.GetDevice("REG001")
	if dev == nil {
		t.Fatal("device should be registered")
	}
	if dev.Options["DeviceType"] != "acc" {
		t.Errorf("expected DeviceType=acc, got %s", dev.Options["DeviceType"])
	}
	if dev.Options["DeviceName"] != "SpeedFace" {
		t.Errorf("expected DeviceName=SpeedFace, got %s", dev.Options["DeviceName"])
	}
	if dev.Options["IPAddress"] != "192.168.1.201" {
		t.Errorf("expected IPAddress parsed")
	}
}

func TestHandleRegistry_Callback(t *testing.T) {
	received := make(chan map[string]string, 1)
	server := NewADMSServer(
		WithOnRegistry(func(_ context.Context, sn string, info map[string]string) {
			received <- info
		}),
	)
	defer server.Close()

	body := "DeviceType=acc"
	req := httptest.NewRequest(http.MethodPost, "/iclock/registry?SN=REG002", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	server.HandleRegistry(w, req)

	select {
	case info := <-received:
		if info["DeviceType"] != "acc" {
			t.Errorf("expected DeviceType=acc, got %s", info["DeviceType"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Timed out waiting for registry callback")
	}
}

func TestHandleInspect_JSON(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()
	if err := server.RegisterDevice("A1"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}
	// Mark old activity to test offline
	server.devicesMutex.Lock()
	server.devices["A1"].LastActivity = time.Now().Add(-3 * time.Minute)
	server.devicesMutex.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/iclock/inspect", nil)
	w := httptest.NewRecorder()
	server.HandleInspect(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("expected JSON content-type, got %s", ct)
	}
	var payload struct {
		Devices []DeviceSnapshot `json:"devices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if len(payload.Devices) == 0 {
		t.Fatalf("expected at least one device")
	}
	if payload.Devices[0].Online {
		t.Errorf("expected device to be offline due to stale activity")
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

func TestParseKVPairs_Registry(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()
	body := "Key1=Val1, ~Key2=Val2,~Key3=Val3"
	info := server.parseKVPairs(body, ",", trimTildePrefix)
	if info["Key1"] != "Val1" || info["Key2"] != "Val2" || info["Key3"] != "Val3" {
		t.Errorf("unexpected parse result: %#v", info)
	}
}

func TestVerifyModeName(t *testing.T) {
	tests := []struct {
		mode int
		want string
	}{
		{VerifyModePassword, "Password"},
		{VerifyModeFingerprint, "Fingerprint"},
		{VerifyModeCard, "Card"},
		{VerifyModeFace, "Face"},
		{VerifyModePalm, "Palm"},
		// Alternative/legacy codes
		{2, "Card"},
		{3, "Password"},
		// Multi-factor combinations
		{5, "Fingerprint+Card"},
		{6, "Fingerprint+Password"},
		{7, "Card+Password"},
		{8, "Card+Fingerprint+Password"},
		{9, "Other"},
		// Unknown value
		{99, "Unknown (99)"},
		{-1, "Unknown (-1)"},
	}
	for _, tt := range tests {
		got := VerifyModeName(tt.mode)
		if got != tt.want {
			t.Errorf("VerifyModeName(%d) = %q, want %q", tt.mode, got, tt.want)
		}
	}
}

func TestVerifyModeConstants(t *testing.T) {
	// Verify that the exported constants have the expected values.
	if VerifyModePassword != 0 {
		t.Errorf("VerifyModePassword = %d, want 0", VerifyModePassword)
	}
	if VerifyModeFingerprint != 1 {
		t.Errorf("VerifyModeFingerprint = %d, want 1", VerifyModeFingerprint)
	}
	if VerifyModeCard != 4 {
		t.Errorf("VerifyModeCard = %d, want 4", VerifyModeCard)
	}
	if VerifyModeFace != 15 {
		t.Errorf("VerifyModeFace = %d, want 15", VerifyModeFace)
	}
	if VerifyModePalm != 25 {
		t.Errorf("VerifyModePalm = %d, want 25", VerifyModePalm)
	}
}

func TestParseAttendanceRecords(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	testCases := []struct {
		name     string
		data     string
		expected int
		checkFn  func(*testing.T, []AttendanceRecord)
	}{
		{
			name:     "single record",
			data:     "123\t2024-01-01 08:00:00\t0\t1\t0",
			expected: 1,
			checkFn: func(t *testing.T, records []AttendanceRecord) {
				if records[0].UserID != "123" {
					t.Errorf("Expected UserID 123, got %s", records[0].UserID)
				}
			},
		},
		{
			name:     "multiple records",
			data:     "123\t2024-01-01 08:00:00\t0\t1\t0\n456\t2024-01-01 17:00:00\t1\t1\t0",
			expected: 2,
			checkFn:  nil,
		},
		{
			name:     "unix timestamp",
			data:     "789\t1704096000\t0\t1\t0",
			expected: 1,
			checkFn: func(t *testing.T, records []AttendanceRecord) {
				if records[0].UserID != "789" {
					t.Errorf("Expected UserID 789, got %s", records[0].UserID)
				}
				if records[0].Timestamp.IsZero() {
					t.Error("Expected non-zero timestamp")
				}
			},
		},
		{
			name:     "empty data",
			data:     "",
			expected: 0,
			checkFn:  nil,
		},
		{
			name:     "minimal record",
			data:     "999\t2024-01-01 08:00:00",
			expected: 1,
			checkFn: func(t *testing.T, records []AttendanceRecord) {
				if records[0].UserID != "999" {
					t.Errorf("Expected UserID 999, got %s", records[0].UserID)
				}
			},
		},
		{
			name:     "too few fields rejected",
			data:     "only-one-field",
			expected: 0,
			checkFn:  nil,
		},
		{
			name:     "invalid timestamp rejected",
			data:     "100\tnot-a-date\t0\t1\t0",
			expected: 0,
			checkFn:  nil,
		},
		{
			name:     "empty userid rejected",
			data:     "100\t2024-01-01 08:00:00\t0\t1\t0\n\t2024-01-01 09:00:00\t0\t1\t0",
			expected: 1,
			checkFn: func(t *testing.T, records []AttendanceRecord) {
				if records[0].UserID != "100" {
					t.Errorf("Expected UserID 100, got %s", records[0].UserID)
				}
			},
		},
		{
			name:     "mixed valid and invalid lines",
			data:     "100\t2024-01-01 08:00:00\t0\t1\t0\njunk\n200\tbad-ts\t0\n300\t2024-01-01 09:00:00\t0\t1\t0",
			expected: 2,
			checkFn: func(t *testing.T, records []AttendanceRecord) {
				if records[0].UserID != "100" {
					t.Errorf("First record: expected UserID 100, got %s", records[0].UserID)
				}
				if records[1].UserID != "300" {
					t.Errorf("Second record: expected UserID 300, got %s", records[1].UserID)
				}
			},
		},
		{
			name:     "non-integer status defaults to zero",
			data:     "100\t2024-01-01 08:00:00\tabc\t1\t0",
			expected: 1,
			checkFn: func(t *testing.T, records []AttendanceRecord) {
				if records[0].Status != 0 {
					t.Errorf("Expected Status 0 for non-integer input, got %d", records[0].Status)
				}
			},
		},
		{
			name:     "non-integer verifymode defaults to zero",
			data:     "100\t2024-01-01 08:00:00\t0\txyz\t0",
			expected: 1,
			checkFn: func(t *testing.T, records []AttendanceRecord) {
				if records[0].VerifyMode != 0 {
					t.Errorf("Expected VerifyMode 0 for non-integer input, got %d", records[0].VerifyMode)
				}
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			records := server.parseAttendanceRecords(tc.data, "TEST001")
			if len(records) != tc.expected {
				t.Errorf("Expected %d records, got %d", tc.expected, len(records))
			}
			if tc.checkFn != nil && len(records) > 0 {
				tc.checkFn(t, records)
			}
		})
	}
}

func TestParseKVPairs_DeviceInfo(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	data := "DeviceName=ZKDevice\nSerialNumber=TEST001\nFirmwareVersion=1.0.0"
	info := server.parseKVPairs(data, "\n", nil)

	if info["DeviceName"] != "ZKDevice" {
		t.Errorf("Expected DeviceName=ZKDevice, got %s", info["DeviceName"])
	}
	if info["SerialNumber"] != "TEST001" {
		t.Errorf("Expected SerialNumber=TEST001, got %s", info["SerialNumber"])
	}
	if info["FirmwareVersion"] != "1.0.0" {
		t.Errorf("Expected FirmwareVersion=1.0.0, got %s", info["FirmwareVersion"])
	}
}

func TestQueueCommand(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()
	serialNumber := "TEST001"

	if _, err := server.QueueCommand(serialNumber, "INFO"); err != nil {
		t.Fatalf("QueueCommand failed: %v", err)
	}

	commands := server.DrainCommands(serialNumber)
	if len(commands) != 1 {
		t.Errorf("Expected 1 command, got %d", len(commands))
	}
	if commands[0].cmd != "INFO" {
		t.Errorf("Expected INFO command, got %s", commands[0].cmd)
	}
}

func TestSendUserAddCommand(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()
	serialNumber := "TEST001"

	if _, err := server.SendUserAddCommand(serialNumber, "1001", "John Doe", 0, "12345678"); err != nil {
		t.Fatalf("SendUserAddCommand failed: %v", err)
	}

	commands := server.DrainCommands(serialNumber)
	if len(commands) != 1 {
		t.Errorf("Expected 1 command, got %d", len(commands))
	}
	want := "DATA UPDATE USERINFO PIN=1001\tName=John Doe\tPrivilege=0\tCard=12345678"
	if commands[0].cmd != want {
		t.Errorf("Expected command %q, got %q", want, commands[0].cmd)
	}
}

func TestSendUserDeleteCommand(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()
	serialNumber := "TEST001"

	if _, err := server.SendUserDeleteCommand(serialNumber, "1001"); err != nil {
		t.Fatalf("SendUserDeleteCommand failed: %v", err)
	}

	commands := server.DrainCommands(serialNumber)
	if len(commands) != 1 {
		t.Errorf("Expected 1 command, got %d", len(commands))
	}
	want := "DATA DELETE USERINFO PIN=1001"
	if commands[0].cmd != want {
		t.Errorf("Expected command %q, got %q", want, commands[0].cmd)
	}
}

func TestSendInfoCommand(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()
	serialNumber := "TEST001"

	if _, err := server.SendInfoCommand(serialNumber); err != nil {
		t.Fatalf("SendInfoCommand failed: %v", err)
	}

	commands := server.DrainCommands(serialNumber)
	if len(commands) != 1 {
		t.Errorf("Expected 1 command, got %d", len(commands))
	}
	if commands[0].cmd != "INFO" {
		t.Errorf("Expected INFO command, got %s", commands[0].cmd)
	}
}

func TestSendCheckCommand(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if _, err := server.SendCheckCommand("TEST001"); err != nil {
		t.Fatalf("SendCheckCommand failed: %v", err)
	}

	commands := server.DrainCommands("TEST001")
	if len(commands) != 1 {
		t.Fatalf("Expected 1 command, got %d", len(commands))
	}
	if commands[0].cmd != "CHECK" {
		t.Errorf("Expected CHECK command, got %s", commands[0].cmd)
	}
}

func TestSendGetOptionCommand(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if _, err := server.SendGetOptionCommand("TEST001", "DeviceName"); err != nil {
		t.Fatalf("SendGetOptionCommand failed: %v", err)
	}

	commands := server.DrainCommands("TEST001")
	if len(commands) != 1 {
		t.Fatalf("Expected 1 command, got %d", len(commands))
	}
	want := "GET OPTION FROM DeviceName"
	if commands[0].cmd != want {
		t.Errorf("Expected command %q, got %q", want, commands[0].cmd)
	}
}

func TestSendShellCommand(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if _, err := server.SendShellCommand("TEST001", "date"); err != nil {
		t.Fatalf("SendShellCommand failed: %v", err)
	}

	commands := server.DrainCommands("TEST001")
	if len(commands) != 1 {
		t.Fatalf("Expected 1 command, got %d", len(commands))
	}
	want := "Shell date"
	if commands[0].cmd != want {
		t.Errorf("Expected command %q, got %q", want, commands[0].cmd)
	}
}

func TestSendQueryUsersCommand(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if _, err := server.SendQueryUsersCommand("TEST001"); err != nil {
		t.Fatalf("SendQueryUsersCommand failed: %v", err)
	}

	commands := server.DrainCommands("TEST001")
	if len(commands) != 1 {
		t.Fatalf("Expected 1 command, got %d", len(commands))
	}
	want := "DATA QUERY USERINFO"
	if commands[0].cmd != want {
		t.Errorf("Expected command %q, got %q", want, commands[0].cmd)
	}
}

func TestSendLogCommand(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if _, err := server.SendLogCommand("TEST001"); err != nil {
		t.Fatalf("SendLogCommand failed: %v", err)
	}

	commands := server.DrainCommands("TEST001")
	if len(commands) != 1 {
		t.Fatalf("Expected 1 command, got %d", len(commands))
	}
	if commands[0].cmd != "LOG" {
		t.Errorf("Expected LOG command, got %s", commands[0].cmd)
	}
}

func TestParseQueryParams(t *testing.T) {
	testURL := "http://example.com/iclock/cdata?SN=TEST001&table=ATTLOG&Stamp=1234567890"

	params, err := ParseQueryParams(testURL)
	if err != nil {
		t.Fatalf("ParseQueryParams failed: %v", err)
	}

	if params["SN"] != "TEST001" {
		t.Errorf("Expected SN=TEST001, got %s", params["SN"])
	}
	if params["table"] != "ATTLOG" {
		t.Errorf("Expected table=ATTLOG, got %s", params["table"])
	}
	if params["Stamp"] != "1234567890" {
		t.Errorf("Expected Stamp=1234567890, got %s", params["Stamp"])
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

	var wg sync.WaitGroup

	// Concurrent device registration
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

func TestCloseIdempotent(t *testing.T) {
	server := NewADMSServer()
	server.Close()
	server.Close() // must not panic
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

func BenchmarkHandleCData(b *testing.B) {
	server := NewADMSServer()
	defer server.Close()
	attendanceData := "123\t2024-01-01 08:00:00\t0\t1\t0"

	for b.Loop() {
		req := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN=TEST001&table=ATTLOG", bytes.NewBufferString(attendanceData))
		w := httptest.NewRecorder()
		server.HandleCData(w, req)
	}
}

func BenchmarkParseAttendanceRecords(b *testing.B) {
	server := NewADMSServer()
	defer server.Close()
	data := "123\t2024-01-01 08:00:00\t0\t1\t0\n456\t2024-01-01 17:00:00\t1\t1\t0\n789\t2024-01-01 12:00:00\t2\t1\t0"

	for b.Loop() {
		server.parseAttendanceRecords(data, "TEST001")
	}
}

func TestHandleCData_BatchDispatch(t *testing.T) {
	var mu sync.Mutex
	var received []AttendanceRecord
	done := make(chan struct{})

	server := NewADMSServer(
		WithOnAttendance(func(_ context.Context, record AttendanceRecord) {
			mu.Lock()
			received = append(received, record)
			if len(received) == 3 {
				close(done)
			}
			mu.Unlock()
		}),
	)
	defer server.Close()

	// Send 3 records in a single POST — they should arrive via a single batch dispatch.
	data := "100\t2024-01-01 08:00:00\t0\t1\t0\n200\t2024-01-01 09:00:00\t1\t1\t0\n300\t2024-01-01 10:00:00\t0\t2\t0"
	req := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN=BATCH001&table=ATTLOG", bytes.NewBufferString(data))
	w := httptest.NewRecorder()

	server.HandleCData(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "OK: 3") {
		t.Errorf("Expected 'OK: 3', got %s", w.Body.String())
	}

	select {
	case <-done:
		mu.Lock()
		defer mu.Unlock()
		if len(received) != 3 {
			t.Fatalf("Expected 3 records, got %d", len(received))
		}
		if received[0].UserID != "100" || received[1].UserID != "200" || received[2].UserID != "300" {
			t.Errorf("Records arrived in wrong order: %v", received)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Timed out waiting for batched attendance records")
	}
}

func TestHandleCData_QueueFull_Returns503(t *testing.T) {
	server := NewADMSServer(
		WithOnAttendance(func(_ context.Context, record AttendanceRecord) {}),
	)
	defer server.Close()

	// Block the worker with a long-running callback so it can't drain the queue.
	workerBlocked := make(chan struct{})
	workerRelease := make(chan struct{})
	server.callbackCh <- func() {
		close(workerBlocked)
		<-workerRelease
	}

	// Wait until the worker is actually blocked.
	<-workerBlocked

	// Now fill the remaining channel capacity. Worker is stuck, so nothing drains.
	for range cap(server.callbackCh) {
		server.callbackCh <- func() {}
	}

	// Queue is full and worker is blocked — dispatch should fail after ~1s.
	data := "999\t2024-01-01 08:00:00\t0\t1\t0"
	req := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN=FULL001&table=ATTLOG", bytes.NewBufferString(data))
	w := httptest.NewRecorder()

	server.HandleCData(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("Expected 503, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "FAIL") {
		t.Errorf("Expected FAIL in response body, got: %s", w.Body.String())
	}

	// Unblock the worker so Close() can drain.
	close(workerRelease)
}

func TestHandleCData_QueueFull_DeviceRetries(t *testing.T) {
	received := make(chan AttendanceRecord, 1)
	server := NewADMSServer(
		WithOnAttendance(func(_ context.Context, record AttendanceRecord) {
			received <- record
		}),
	)

	// Block the worker so it can't drain the queue.
	workerBlocked := make(chan struct{})
	workerRelease := make(chan struct{})
	server.callbackCh <- func() {
		close(workerBlocked)
		<-workerRelease
	}
	<-workerBlocked

	// Fill the remaining channel capacity with no-ops. Worker is stuck.
	for range cap(server.callbackCh) {
		server.callbackCh <- func() {}
	}

	// First attempt: queue is full, should get 503.
	data := "888\t2024-01-01 08:00:00\t0\t1\t0"
	req1 := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN=RETRY001&table=ATTLOG", bytes.NewBufferString(data))
	w1 := httptest.NewRecorder()
	server.HandleCData(w1, req1)

	if w1.Code != http.StatusServiceUnavailable {
		t.Fatalf("Expected 503 on first attempt, got %d", w1.Code)
	}

	// Release the worker and drain the channel to simulate queue recovery.
	close(workerRelease)

	// Drain remaining no-ops non-blocking to avoid racing with the worker.
	draining := true
	for draining {
		select {
		case <-server.callbackCh:
		default:
			draining = false
		}
	}
	// Wait for the worker to finish processing and the queue to be fully empty.
	deadline := time.Now().Add(3 * time.Second)
	for len(server.callbackCh) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("Timed out waiting for callback queue to drain")
		}
		runtime.Gosched()
	}

	// Second attempt: queue has space, should succeed.
	req2 := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN=RETRY001&table=ATTLOG", bytes.NewBufferString(data))
	w2 := httptest.NewRecorder()
	server.HandleCData(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("Expected 200 on retry, got %d", w2.Code)
	}
	if !strings.Contains(w2.Body.String(), "OK: 1") {
		t.Errorf("Expected 'OK: 1', got %s", w2.Body.String())
	}

	select {
	case rec := <-received:
		if rec.UserID != "888" {
			t.Errorf("Expected UserID 888, got %s", rec.UserID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Timed out waiting for retried attendance callback")
	}

	server.Close()
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

func TestReadBody_ExceedsLimit(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	server.maxBodySize = 10 // 10 bytes limit
	if err := server.RegisterDevice("TEST001"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	// Send a body larger than the limit via the ATTLOG handler.
	largeBody := strings.Repeat("x", 20)
	req := httptest.NewRequest(http.MethodPost,
		"/iclock/cdata?SN=TEST001&table=ATTLOG", strings.NewReader(largeBody))
	w := httptest.NewRecorder()
	server.HandleCData(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected status %d for oversized body, got %d", http.StatusRequestEntityTooLarge, w.Code)
	}
	if !strings.Contains(w.Body.String(), "Request body too large") {
		t.Errorf("expected 'Request body too large' in response, got %q", w.Body.String())
	}
}

func TestReadBody_WithinLimit(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	server.maxBodySize = 1024 // 1 KB limit
	if err := server.RegisterDevice("TEST001"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	body := "123\t2024-01-01 08:00:00\t0\t1\t0"
	req := httptest.NewRequest(http.MethodPost,
		"/iclock/cdata?SN=TEST001&table=ATTLOG", strings.NewReader(body))
	w := httptest.NewRecorder()
	server.HandleCData(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d for body within limit, got %d", http.StatusOK, w.Code)
	}
	if !strings.Contains(w.Body.String(), "OK") {
		t.Errorf("expected 'OK' in response, got %q", w.Body.String())
	}
}

func TestReadBody_ExceedsLimit_DeviceCmd(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	server.maxBodySize = 5
	if err := server.RegisterDevice("TEST001"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost,
		"/iclock/devicecmd?SN=TEST001", strings.NewReader("this body exceeds the limit"))
	w := httptest.NewRecorder()
	server.HandleDeviceCmd(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected status %d, got %d", http.StatusRequestEntityTooLarge, w.Code)
	}
}

func TestReadBody_ExceedsLimit_Registry(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	server.maxBodySize = 5
	if err := server.RegisterDevice("TEST001"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost,
		"/iclock/registry?SN=TEST001", strings.NewReader("DeviceType=acc,DeviceName=SpeedFace"))
	w := httptest.NewRecorder()
	server.HandleRegistry(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected status %d, got %d", http.StatusRequestEntityTooLarge, w.Code)
	}
}

// --- Phase 2: Functional Options API Tests ---

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

func TestDrainCommands(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if _, err := server.QueueCommand("DRAIN001", "CMD1"); err != nil {
		t.Fatalf("QueueCommand failed: %v", err)
	}
	if _, err := server.QueueCommand("DRAIN001", "CMD2"); err != nil {
		t.Fatalf("QueueCommand failed: %v", err)
	}

	commands := server.DrainCommands("DRAIN001")
	if len(commands) != 2 {
		t.Fatalf("expected 2 commands, got %d", len(commands))
	}
	if commands[0].cmd != "CMD1" || commands[1].cmd != "CMD2" {
		t.Errorf("unexpected commands: %v", commands)
	}

	// Should be empty now.
	commands = server.DrainCommands("DRAIN001")
	if len(commands) != 0 {
		t.Errorf("expected empty queue after drain, got %d", len(commands))
	}
}

func TestDrainCommands_DeletesKey(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if _, err := server.QueueCommand("DRAIN_DEL", "INFO"); err != nil {
		t.Fatalf("QueueCommand failed: %v", err)
	}
	_ = server.DrainCommands("DRAIN_DEL")

	server.queueMutex.RLock()
	_, exists := server.commandQueue["DRAIN_DEL"]
	server.queueMutex.RUnlock()

	if exists {
		t.Error("expected map key deleted after DrainCommands")
	}
}

func TestPendingCommandsCount(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	// Zero for unknown device.
	if n := server.PendingCommandsCount("NODEV"); n != 0 {
		t.Errorf("expected 0 pending commands for unknown device, got %d", n)
	}

	// Queue a few commands and verify count without draining.
	for _, cmd := range []string{"INFO", "CHECK", "REBOOT"} {
		if _, err := server.QueueCommand("COUNT001", cmd); err != nil {
			t.Fatalf("QueueCommand(%q) failed: %v", cmd, err)
		}
	}
	if n := server.PendingCommandsCount("COUNT001"); n != 3 {
		t.Errorf("expected 3 pending commands, got %d", n)
	}

	// Drain one command and verify count decreases.
	_ = server.DrainCommands("COUNT001")
	if n := server.PendingCommandsCount("COUNT001"); n != 0 {
		t.Errorf("expected 0 pending commands after drain, got %d", n)
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

func TestSentinelErrors(t *testing.T) {
	// Verify sentinel errors exist and have expected messages.
	if ErrServerClosed.Error() != "zkadms: server closed" {
		t.Errorf("unexpected ErrServerClosed message: %s", ErrServerClosed.Error())
	}
	if ErrCallbackQueueFull.Error() != "zkadms: callback queue full" {
		t.Errorf("unexpected ErrCallbackQueueFull message: %s", ErrCallbackQueueFull.Error())
	}
}

// --- Phase 3: Security hardening tests ---

func TestValidateSerialNumber(t *testing.T) {
	tests := []struct {
		name    string
		sn      string
		wantErr bool
	}{
		{"valid alphanumeric", "TEST001", false},
		{"valid with hyphens", "DEVICE-001", false},
		{"valid with underscores", "DEVICE_001", false},
		{"valid mixed", "A1-B2_C3", false},
		{"valid single char", "X", false},
		{"valid 64 chars", strings.Repeat("A", 64), false},
		{"empty", "", true},
		{"too long 65 chars", strings.Repeat("A", 65), true},
		{"contains spaces", "TEST 001", true},
		{"contains dots", "TEST.001", true},
		{"contains slash", "TEST/001", true},
		{"contains colon", "TEST:001", true},
		{"contains newline", "TEST\n001", true},
		{"contains null byte", "TEST\x00001", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSerialNumber(tc.sn)
			if tc.wantErr && err == nil {
				t.Errorf("expected error for SN %q, got nil", tc.sn)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error for SN %q: %v", tc.sn, err)
			}
			if tc.wantErr && err != nil && !errors.Is(err, ErrInvalidSerialNumber) {
				t.Errorf("expected ErrInvalidSerialNumber, got %v", err)
			}
		})
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

	// Should be able to register many devices with 0 (default/unlimited).
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
	for i := range 3 {
		if _, err := server.QueueCommand(sn, "CMD"+string(rune('1'+i))); err != nil {
			t.Fatalf("QueueCommand %d failed: %v", i+1, err)
		}
	}

	// Fourth should fail.
	_, err := server.QueueCommand(sn, "CMD4")
	if !errors.Is(err, ErrCommandQueueFull) {
		t.Errorf("expected ErrCommandQueueFull, got %v", err)
	}

	// Draining should free up space.
	_ = server.DrainCommands(sn)
	if _, err := server.QueueCommand(sn, "CMD5"); err != nil {
		t.Errorf("after drain, QueueCommand should succeed: %v", err)
	}
}

func TestWithMaxCommandsPerDevice_ZeroMeansUnlimited(t *testing.T) {
	server := NewADMSServer(WithMaxCommandsPerDevice(0))
	defer server.Close()

	for i := range 100 {
		if _, err := server.QueueCommand("DEV1", "CMD"+string(rune(i))); err != nil {
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

func TestHandler_RejectsInvalidSN(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	// All handlers should reject requests with invalid serial numbers.
	handlers := []struct {
		name   string
		method string
		path   string
	}{
		{"cdata GET", "GET", "/iclock/cdata?SN=BAD%20SN"},
		{"cdata POST", "POST", "/iclock/cdata?SN=BAD%20SN"},
		{"getrequest", "GET", "/iclock/getrequest?SN=BAD%20SN"},
		{"devicecmd", "POST", "/iclock/devicecmd?SN=BAD%20SN"},
		{"registry GET", "GET", "/iclock/registry?SN=BAD%20SN"},
		{"registry POST", "POST", "/iclock/registry?SN=BAD%20SN"},
	}

	for _, tc := range handlers {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			w := httptest.NewRecorder()
			server.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400 for invalid SN, got %d", w.Code)
			}
		})
	}
}

func TestHandler_RejectsMissingSN(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	// Handlers should reject requests without SN parameter.
	handlers := []struct {
		name   string
		method string
		path   string
	}{
		{"cdata GET", "GET", "/iclock/cdata"},
		{"cdata POST", "POST", "/iclock/cdata"},
		{"getrequest", "GET", "/iclock/getrequest"},
		{"devicecmd", "POST", "/iclock/devicecmd"},
		{"registry GET", "GET", "/iclock/registry"},
	}

	for _, tc := range handlers {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			w := httptest.NewRecorder()
			server.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400 for missing SN, got %d", w.Code)
			}
		})
	}
}

func TestHandler_RejectsWhenDeviceLimitReached(t *testing.T) {
	server := NewADMSServer(WithMaxDevices(1))
	defer server.Close()

	// Register one device via direct call.
	if err := server.RegisterDevice("DEV1"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	// HTTP request with a new SN should be rejected with 503.
	req := httptest.NewRequest(http.MethodGet, "/iclock/cdata?SN=DEV2", nil)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when device limit reached, got %d", w.Code)
	}

	// But existing device should still work.
	req = httptest.NewRequest(http.MethodGet, "/iclock/cdata?SN=DEV1", nil)
	w = httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for existing device, got %d", w.Code)
	}
}

func TestSendUserAddCommand_ReturnsError(t *testing.T) {
	server := NewADMSServer(WithMaxCommandsPerDevice(1))
	defer server.Close()
	sn := "TEST001"

	// First command should succeed.
	if _, err := server.SendUserAddCommand(sn, "1001", "John", 0, ""); err != nil {
		t.Fatalf("SendUserAddCommand failed: %v", err)
	}

	// Second should fail due to queue limit.
	_, err := server.SendUserAddCommand(sn, "1002", "Jane", 0, "")
	if !errors.Is(err, ErrCommandQueueFull) {
		t.Errorf("expected ErrCommandQueueFull, got %v", err)
	}
}

func TestSendUserDeleteCommand_ReturnsError(t *testing.T) {
	server := NewADMSServer(WithMaxCommandsPerDevice(1))
	defer server.Close()
	sn := "TEST001"

	// First command should succeed.
	if _, err := server.SendUserDeleteCommand(sn, "1001"); err != nil {
		t.Fatalf("SendUserDeleteCommand failed: %v", err)
	}

	// Second should fail due to queue limit.
	_, err := server.SendUserDeleteCommand(sn, "1002")
	if !errors.Is(err, ErrCommandQueueFull) {
		t.Errorf("expected ErrCommandQueueFull, got %v", err)
	}
}

func TestSendInfoCommand_ReturnsError(t *testing.T) {
	server := NewADMSServer(WithMaxCommandsPerDevice(1))
	defer server.Close()
	sn := "TEST001"

	// First command should succeed.
	if _, err := server.SendInfoCommand(sn); err != nil {
		t.Fatalf("SendInfoCommand failed: %v", err)
	}

	// Second should fail due to queue limit.
	_, err := server.SendInfoCommand(sn)
	if !errors.Is(err, ErrCommandQueueFull) {
		t.Errorf("expected ErrCommandQueueFull, got %v", err)
	}
}

func TestSendCheckCommand_ReturnsError(t *testing.T) {
	server := NewADMSServer(WithMaxCommandsPerDevice(1))
	defer server.Close()
	sn := "TEST001"

	if _, err := server.SendCheckCommand(sn); err != nil {
		t.Fatalf("SendCheckCommand failed: %v", err)
	}

	_, err := server.SendCheckCommand(sn)
	if !errors.Is(err, ErrCommandQueueFull) {
		t.Errorf("expected ErrCommandQueueFull, got %v", err)
	}
}

func TestSendGetOptionCommand_ReturnsError(t *testing.T) {
	server := NewADMSServer(WithMaxCommandsPerDevice(1))
	defer server.Close()
	sn := "TEST001"

	if _, err := server.SendGetOptionCommand(sn, "DeviceName"); err != nil {
		t.Fatalf("SendGetOptionCommand failed: %v", err)
	}

	_, err := server.SendGetOptionCommand(sn, "FWVersion")
	if !errors.Is(err, ErrCommandQueueFull) {
		t.Errorf("expected ErrCommandQueueFull, got %v", err)
	}
}

func TestSendShellCommand_ReturnsError(t *testing.T) {
	server := NewADMSServer(WithMaxCommandsPerDevice(1))
	defer server.Close()
	sn := "TEST001"

	if _, err := server.SendShellCommand(sn, "date"); err != nil {
		t.Fatalf("SendShellCommand failed: %v", err)
	}

	_, err := server.SendShellCommand(sn, "uptime")
	if !errors.Is(err, ErrCommandQueueFull) {
		t.Errorf("expected ErrCommandQueueFull, got %v", err)
	}
}

func TestSendQueryUsersCommand_ReturnsError(t *testing.T) {
	server := NewADMSServer(WithMaxCommandsPerDevice(1))
	defer server.Close()
	sn := "TEST001"

	if _, err := server.SendQueryUsersCommand(sn); err != nil {
		t.Fatalf("SendQueryUsersCommand failed: %v", err)
	}

	_, err := server.SendQueryUsersCommand(sn)
	if !errors.Is(err, ErrCommandQueueFull) {
		t.Errorf("expected ErrCommandQueueFull, got %v", err)
	}
}

func TestSendLogCommand_ReturnsError(t *testing.T) {
	server := NewADMSServer(WithMaxCommandsPerDevice(1))
	defer server.Close()
	sn := "TEST001"

	if _, err := server.SendLogCommand(sn); err != nil {
		t.Fatalf("SendLogCommand failed: %v", err)
	}

	_, err := server.SendLogCommand(sn)
	if !errors.Is(err, ErrCommandQueueFull) {
		t.Errorf("expected ErrCommandQueueFull, got %v", err)
	}
}

func TestNewSecuritySentinelErrors(t *testing.T) {
	// Verify the new sentinel errors exist and are properly structured.
	if !strings.Contains(ErrMaxDevicesReached.Error(), "maximum number of devices") {
		t.Errorf("unexpected ErrMaxDevicesReached message: %s", ErrMaxDevicesReached.Error())
	}
	if !strings.Contains(ErrCommandQueueFull.Error(), "command queue full") {
		t.Errorf("unexpected ErrCommandQueueFull message: %s", ErrCommandQueueFull.Error())
	}
	if !strings.Contains(ErrInvalidSerialNumber.Error(), "invalid serial number") {
		t.Errorf("unexpected ErrInvalidSerialNumber message: %s", ErrInvalidSerialNumber.Error())
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

// --- Phase 3 (continued): Command ID tracking and confirmation tests ---

func TestParseCommandResults(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	// Single-result test cases (expect exactly 1 result).
	singleTests := []struct {
		name       string
		body       string
		wantID     int64
		wantReturn int
		wantCmd    string
	}{
		{
			name:       "standard ampersand format",
			body:       "ID=42&Return=0&CMD=USER ADD",
			wantID:     42,
			wantReturn: 0,
			wantCmd:    "USER ADD",
		},
		{
			name:       "extra whitespace",
			body:       " ID = 10 & Return = 0 & CMD = USER DEL ",
			wantID:     10,
			wantReturn: 0,
			wantCmd:    "USER DEL",
		},
		{
			name:       "missing Return field",
			body:       "ID=5&CMD=REBOOT",
			wantID:     5,
			wantReturn: 0,
			wantCmd:    "REBOOT",
		},
		{
			name:       "missing CMD field",
			body:       "ID=5&Return=2",
			wantID:     5,
			wantReturn: 2,
			wantCmd:    "",
		},
		{
			name:       "non-numeric Return ignored",
			body:       "ID=1&Return=xyz&CMD=INFO",
			wantID:     1,
			wantReturn: 0,
			wantCmd:    "INFO",
		},
		{
			name:       "case insensitive keys",
			body:       "id=99&return=0&cmd=CHECK",
			wantID:     99,
			wantReturn: 0,
			wantCmd:    "CHECK",
		},
		{
			name:       "unknown keys ignored",
			body:       "ID=1&Return=0&CMD=INFO&Extra=ignored",
			wantID:     1,
			wantReturn: 0,
			wantCmd:    "INFO",
		},
		{
			name:       "trailing newline",
			body:       "ID=7&Return=0&CMD=CHECK\n",
			wantID:     7,
			wantReturn: 0,
			wantCmd:    "CHECK",
		},
		{
			name:       "negative return code",
			body:       "ID=3&Return=-1002&CMD=DATA",
			wantID:     3,
			wantReturn: -1002,
			wantCmd:    "DATA",
		},
	}

	for _, tc := range singleTests {
		t.Run(tc.name, func(t *testing.T) {
			results := server.parseCommandResults(tc.body, "DEV001")
			if len(results) != 1 {
				t.Fatalf("expected 1 result, got %d", len(results))
			}
			result := results[0]
			if result.SerialNumber != "DEV001" {
				t.Errorf("expected SerialNumber DEV001, got %s", result.SerialNumber)
			}
			if result.ID != tc.wantID {
				t.Errorf("expected ID %d, got %d", tc.wantID, result.ID)
			}
			if result.ReturnCode != tc.wantReturn {
				t.Errorf("expected ReturnCode %d, got %d", tc.wantReturn, result.ReturnCode)
			}
			if result.Command != tc.wantCmd {
				t.Errorf("expected Command %q, got %q", tc.wantCmd, result.Command)
			}
		})
	}

	// Empty / no-ID cases (expect 0 results).
	emptyTests := []struct {
		name string
		body string
	}{
		{"empty body", ""},
		{"only whitespace", "   \n\n  "},
		{"missing ID field", "Return=0&CMD=INFO"},
		{"non-numeric ID", "ID=abc&Return=0&CMD=INFO"},
	}

	for _, tc := range emptyTests {
		t.Run(tc.name, func(t *testing.T) {
			results := server.parseCommandResults(tc.body, "DEV001")
			if len(results) != 0 {
				t.Errorf("expected 0 results, got %d: %+v", len(results), results)
			}
		})
	}

	// Batched confirmation test cases (real device behavior).
	t.Run("batched two results", func(t *testing.T) {
		body := "ID=19&Return=0&CMD=DATA\nID=20&Return=0&CMD=DATA\n"
		results := server.parseCommandResults(body, "DEV001")
		if len(results) != 2 {
			t.Fatalf("expected 2 results, got %d", len(results))
		}
		if results[0].ID != 19 || results[0].ReturnCode != 0 || results[0].Command != "DATA" {
			t.Errorf("result[0] = %+v", results[0])
		}
		if results[1].ID != 20 || results[1].ReturnCode != 0 || results[1].Command != "DATA" {
			t.Errorf("result[1] = %+v", results[1])
		}
	})

	t.Run("batched three results mixed codes", func(t *testing.T) {
		body := "ID=1&Return=0&CMD=INFO\nID=2&Return=-1002&CMD=USER ADD\nID=3&Return=0&CMD=CHECK\n"
		results := server.parseCommandResults(body, "DEV001")
		if len(results) != 3 {
			t.Fatalf("expected 3 results, got %d", len(results))
		}
		if results[0].ID != 1 || results[0].ReturnCode != 0 || results[0].Command != "INFO" {
			t.Errorf("result[0] = %+v", results[0])
		}
		if results[1].ID != 2 || results[1].ReturnCode != -1002 || results[1].Command != "USER ADD" {
			t.Errorf("result[1] = %+v", results[1])
		}
		if results[2].ID != 3 || results[2].ReturnCode != 0 || results[2].Command != "CHECK" {
			t.Errorf("result[2] = %+v", results[2])
		}
	})

	t.Run("batched with extra data after CMD", func(t *testing.T) {
		// Real device includes extra info after CMD on INFO confirmation.
		body := "ID=1&Return=0&CMD=INFO\n~DeviceName=SpeedFace\nMAC=00:17:61:12:f8:db\n"
		results := server.parseCommandResults(body, "DEV001")
		// Only 1 result — the extra lines have no ID field.
		if len(results) != 1 {
			t.Fatalf("expected 1 result, got %d", len(results))
		}
		if results[0].ID != 1 || results[0].Command != "INFO" {
			t.Errorf("result[0] = %+v", results[0])
		}
	})
}

func TestCommandIDIncrement(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()
	serialNumber := "IDTEST001"

	// Queue 3 commands.
	for _, cmd := range []string{"INFO", "REBOOT", "CHECK"} {
		if _, err := server.QueueCommand(serialNumber, cmd); err != nil {
			t.Fatalf("QueueCommand(%q) failed: %v", cmd, err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/iclock/getrequest?SN="+serialNumber, nil)
	w := httptest.NewRecorder()
	server.HandleGetRequest(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Response should contain 3 commands with incrementing IDs.
	body := w.Body.String()
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 command lines, got %d: %q", len(lines), body)
	}

	wantLines := []string{
		"C:1:INFO",
		"C:2:REBOOT",
		"C:3:CHECK",
	}
	for i, want := range wantLines {
		if lines[i] != want {
			t.Errorf("line %d: expected %q, got %q", i, want, lines[i])
		}
	}
}

func TestCommandIDIncrement_AcrossRequests(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	// First request: 1 command → ID=1
	if _, err := server.QueueCommand("DEV1", "INFO"); err != nil {
		t.Fatal(err)
	}
	req1 := httptest.NewRequest(http.MethodGet, "/iclock/getrequest?SN=DEV1", nil)
	w1 := httptest.NewRecorder()
	server.HandleGetRequest(w1, req1)

	if want := "C:1:INFO\n"; w1.Body.String() != want {
		t.Errorf("first request: expected %q, got %q", want, w1.Body.String())
	}

	// Second request: 1 command → ID=2 (counter continues)
	if _, err := server.QueueCommand("DEV1", "REBOOT"); err != nil {
		t.Fatal(err)
	}
	req2 := httptest.NewRequest(http.MethodGet, "/iclock/getrequest?SN=DEV1", nil)
	w2 := httptest.NewRecorder()
	server.HandleGetRequest(w2, req2)

	if want := "C:2:REBOOT\n"; w2.Body.String() != want {
		t.Errorf("second request: expected %q, got %q", want, w2.Body.String())
	}
}

func TestCommandIDIncrement_AcrossDevices(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	// Commands for different devices share the same ID counter.
	if _, err := server.QueueCommand("DEVA", "INFO"); err != nil {
		t.Fatal(err)
	}
	if _, err := server.QueueCommand("DEVB", "REBOOT"); err != nil {
		t.Fatal(err)
	}

	// Drain DEVA → ID=1
	reqA := httptest.NewRequest(http.MethodGet, "/iclock/getrequest?SN=DEVA", nil)
	wA := httptest.NewRecorder()
	server.HandleGetRequest(wA, reqA)

	if want := "C:1:INFO\n"; wA.Body.String() != want {
		t.Errorf("DEVA: expected %q, got %q", want, wA.Body.String())
	}

	// Drain DEVB → ID=2
	reqB := httptest.NewRequest(http.MethodGet, "/iclock/getrequest?SN=DEVB", nil)
	wB := httptest.NewRecorder()
	server.HandleGetRequest(wB, reqB)

	if want := "C:2:REBOOT\n"; wB.Body.String() != want {
		t.Errorf("DEVB: expected %q, got %q", want, wB.Body.String())
	}
}

func TestHandleDeviceCmd_Confirmation(t *testing.T) {
	received := make(chan CommandResult, 1)
	server := NewADMSServer(
		WithOnCommandResult(func(_ context.Context, result CommandResult) {
			received <- result
		}),
	)
	defer server.Close()

	body := "ID=42&Return=0&CMD=DATA"
	req := httptest.NewRequest(http.MethodPost, "/iclock/devicecmd?SN=CONF001", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	server.HandleDeviceCmd(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "OK" {
		t.Errorf("expected OK response, got %q", w.Body.String())
	}

	select {
	case result := <-received:
		if result.SerialNumber != "CONF001" {
			t.Errorf("expected SerialNumber CONF001, got %s", result.SerialNumber)
		}
		if result.ID != 42 {
			t.Errorf("expected ID 42, got %d", result.ID)
		}
		if result.ReturnCode != 0 {
			t.Errorf("expected ReturnCode 0, got %d", result.ReturnCode)
		}
		if result.Command != "DATA" {
			t.Errorf("expected Command DATA, got %s", result.Command)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Timed out waiting for command result callback")
	}
}

func TestHandleDeviceCmd_ConfirmationWithError(t *testing.T) {
	received := make(chan CommandResult, 1)
	server := NewADMSServer(
		WithOnCommandResult(func(_ context.Context, result CommandResult) {
			received <- result
		}),
	)
	defer server.Close()

	body := "ID=5&Return=1&CMD=REBOOT"
	req := httptest.NewRequest(http.MethodPost, "/iclock/devicecmd?SN=ERR001", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	server.HandleDeviceCmd(w, req)

	select {
	case result := <-received:
		if result.ReturnCode != 1 {
			t.Errorf("expected ReturnCode 1 (error), got %d", result.ReturnCode)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Timed out waiting for command result callback")
	}
}

func TestHandleDeviceCmd_BatchedConfirmations(t *testing.T) {
	received := make(chan CommandResult, 5)
	server := NewADMSServer(
		WithOnCommandResult(func(_ context.Context, result CommandResult) {
			received <- result
		}),
	)
	defer server.Close()

	// Device batches 3 confirmations in a single POST (real device behavior).
	body := "ID=1&Return=0&CMD=INFO\nID=2&Return=0&CMD=CHECK\nID=3&Return=-1002&CMD=DATA\n"
	req := httptest.NewRequest(http.MethodPost, "/iclock/devicecmd?SN=BATCH001", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	server.HandleDeviceCmd(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Should receive all 3 results.
	var results []CommandResult
	for range 3 {
		select {
		case r := <-received:
			results = append(results, r)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out; only received %d/3 results", len(results))
		}
	}

	if results[0].ID != 1 || results[0].ReturnCode != 0 || results[0].Command != "INFO" {
		t.Errorf("result[0] = %+v", results[0])
	}
	if results[1].ID != 2 || results[1].ReturnCode != 0 || results[1].Command != "CHECK" {
		t.Errorf("result[1] = %+v", results[1])
	}
	if results[2].ID != 3 || results[2].ReturnCode != -1002 || results[2].Command != "DATA" {
		t.Errorf("result[2] = %+v", results[2])
	}
	for _, r := range results {
		if r.SerialNumber != "BATCH001" {
			t.Errorf("expected SerialNumber BATCH001, got %s", r.SerialNumber)
		}
	}
}

func TestHandleDeviceCmd_NoCallbackConfigured(t *testing.T) {
	// No WithOnCommandResult — should still respond OK without panicking.
	server := NewADMSServer()
	defer server.Close()

	body := "ID=1&Return=0&CMD=INFO"
	req := httptest.NewRequest(http.MethodPost, "/iclock/devicecmd?SN=NOCB001", bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	server.HandleDeviceCmd(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "OK" {
		t.Errorf("expected OK response, got %q", w.Body.String())
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

func TestCommandResultType(t *testing.T) {
	// Verify CommandResult fields can be populated and read.
	r := CommandResult{
		SerialNumber: "DEV001",
		ID:           42,
		ReturnCode:   0,
		Command:      "DATA",
	}
	if r.SerialNumber != "DEV001" {
		t.Errorf("unexpected SerialNumber: %s", r.SerialNumber)
	}
	if r.ID != 42 {
		t.Errorf("unexpected ID: %d", r.ID)
	}
	if r.ReturnCode != 0 {
		t.Errorf("unexpected ReturnCode: %d", r.ReturnCode)
	}
	if r.Command != "DATA" {
		t.Errorf("unexpected Command: %s", r.Command)
	}
}

// ---------------------------------------------------------------------------
// readBody coverage
// ---------------------------------------------------------------------------

func TestReadBody_MaxBytesError(t *testing.T) {
	server := NewADMSServer(WithMaxBodySize(10))
	defer server.Close()

	body := strings.Repeat("x", 100) // well over 10 bytes
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	w := httptest.NewRecorder()

	_, err := server.readBody(w, req)
	if err == nil {
		t.Fatal("expected error for oversized body")
	}
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413, got %d", w.Code)
	}
}

func TestReadBody_GenericReadError(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	req := httptest.NewRequest(http.MethodPost, "/", io.NopCloser(errReader{err: errors.New("disk failure")}))
	w := httptest.NewRecorder()

	_, err := server.readBody(w, req)
	if err == nil {
		t.Fatal("expected error for read failure")
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// bodyPreview coverage
// ---------------------------------------------------------------------------

func TestBodyPreview_Truncation(t *testing.T) {
	long := strings.Repeat("A", maxBodyPreviewLen+50)
	got := bodyPreview([]byte(long))

	if !strings.HasSuffix(got, "...") {
		t.Errorf("expected truncated preview to end with '...', got %q", got)
	}
	if len(got) != maxBodyPreviewLen+3 { // 200 chars + "..."
		t.Errorf("expected length %d, got %d", maxBodyPreviewLen+3, len(got))
	}
}

func TestBodyPreview_Short(t *testing.T) {
	short := "hello"
	got := bodyPreview([]byte(short))
	if got != short {
		t.Errorf("expected %q, got %q", short, got)
	}
}

func TestBodyPreview_ExactBoundary(t *testing.T) {
	exact := strings.Repeat("B", maxBodyPreviewLen)
	got := bodyPreview([]byte(exact))
	if got != exact {
		t.Errorf("body at exact boundary should not be truncated")
	}
}

// ---------------------------------------------------------------------------
// dispatchAttendance coverage
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// registerOrReject coverage
// ---------------------------------------------------------------------------

func TestRegisterOrReject_InvalidSerialNumber(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	w := httptest.NewRecorder()
	ok := server.registerOrReject(w, "!!!invalid!!!")
	if ok {
		t.Error("expected registerOrReject to return false for invalid SN")
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestRegisterOrReject_DeviceLimitReached(t *testing.T) {
	server := NewADMSServer(WithMaxDevices(1))
	defer server.Close()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	w := httptest.NewRecorder()
	ok := server.registerOrReject(w, "DEV002")
	if ok {
		t.Error("expected registerOrReject to return false when limit reached")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// HandleCData coverage — body too large, callback queue full
// ---------------------------------------------------------------------------

func TestHandleCData_ATTLOG_BodyTooLarge(t *testing.T) {
	server := NewADMSServer(WithMaxBodySize(10))
	defer server.Close()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}

	body := strings.Repeat("x", 100)
	req := httptest.NewRequest(http.MethodPost,
		"/iclock/cdata?SN=DEV001&table=ATTLOG", strings.NewReader(body))
	w := httptest.NewRecorder()
	server.HandleCData(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413, got %d", w.Code)
	}
}

func TestHandleCData_ATTLOG_CallbackQueueFull(t *testing.T) {
	blocker := make(chan struct{})
	server := NewADMSServer(
		WithCallbackBufferSize(1),
		WithDispatchTimeout(10*time.Millisecond),
		WithOnAttendance(func(_ context.Context, _ AttendanceRecord) {
			<-blocker
		}),
	)
	defer func() {
		close(blocker)
		server.Close()
	}()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}

	attlog := "1\t2025-01-01 08:00:00\t1\t0\t0\t0"

	// Fill the callback channel.
	req1 := httptest.NewRequest(http.MethodPost,
		"/iclock/cdata?SN=DEV001&table=ATTLOG", strings.NewReader(attlog))
	w1 := httptest.NewRecorder()
	server.HandleCData(w1, req1)

	time.Sleep(50 * time.Millisecond) // let worker pick up and block

	// Fill remaining channel capacity.
	req2 := httptest.NewRequest(http.MethodPost,
		"/iclock/cdata?SN=DEV001&table=ATTLOG", strings.NewReader(attlog))
	w2 := httptest.NewRecorder()
	server.HandleCData(w2, req2)

	// Now queue should be full — next should fail.
	req3 := httptest.NewRequest(http.MethodPost,
		"/iclock/cdata?SN=DEV001&table=ATTLOG", strings.NewReader(attlog))
	w3 := httptest.NewRecorder()
	server.HandleCData(w3, req3)

	if w3.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when callback queue is full, got %d", w3.Code)
	}
}

func TestHandleCData_DefaultTable_DeviceInfoQueueFull(t *testing.T) {
	blocker := make(chan struct{})
	server := NewADMSServer(
		WithCallbackBufferSize(1),
		WithDispatchTimeout(10*time.Millisecond),
		WithOnDeviceInfo(func(_ context.Context, _ string, _ map[string]string) {
			<-blocker
		}),
	)
	defer func() {
		close(blocker)
		server.Close()
	}()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}

	infoBody := "~DeviceName=TestDev\nMAC=00:11:22:33:44:55"

	// First dispatch — the worker picks it up and blocks.
	req1 := httptest.NewRequest(http.MethodPost,
		"/iclock/cdata?SN=DEV001", strings.NewReader(infoBody))
	w1 := httptest.NewRecorder()
	server.HandleCData(w1, req1)

	time.Sleep(50 * time.Millisecond)

	// Fill remaining buffer capacity.
	req2 := httptest.NewRequest(http.MethodPost,
		"/iclock/cdata?SN=DEV001", strings.NewReader(infoBody))
	w2 := httptest.NewRecorder()
	server.HandleCData(w2, req2)

	// This dispatch should fail (queue full), but HandleCData still returns OK
	// because device info dispatch failure is non-fatal (just logged as a warning).
	req3 := httptest.NewRequest(http.MethodPost,
		"/iclock/cdata?SN=DEV001", strings.NewReader(infoBody))
	w3 := httptest.NewRecorder()
	server.HandleCData(w3, req3)

	if w3.Code != http.StatusOK {
		t.Errorf("expected 200 (device info queue-full is non-fatal), got %d", w3.Code)
	}
}

func TestHandleCData_DefaultTable_BodyTooLarge(t *testing.T) {
	server := NewADMSServer(WithMaxBodySize(10))
	defer server.Close()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}

	body := strings.Repeat("x", 100)
	// POST to default table (no table param) triggers the info/command path.
	req := httptest.NewRequest(http.MethodPost,
		"/iclock/cdata?SN=DEV001", strings.NewReader(body))
	w := httptest.NewRecorder()
	server.HandleCData(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// HandleDeviceCmd coverage — body too large, callback queue full
// ---------------------------------------------------------------------------

func TestHandleDeviceCmd_BodyTooLarge(t *testing.T) {
	server := NewADMSServer(WithMaxBodySize(10))
	defer server.Close()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}

	body := strings.Repeat("x", 100)
	req := httptest.NewRequest(http.MethodPost,
		"/iclock/devicecmd?SN=DEV001", strings.NewReader(body))
	w := httptest.NewRecorder()
	server.HandleDeviceCmd(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413, got %d", w.Code)
	}
}

func TestHandleDeviceCmd_CallbackQueueFull(t *testing.T) {
	blocker := make(chan struct{})
	server := NewADMSServer(
		WithCallbackBufferSize(1),
		WithDispatchTimeout(10*time.Millisecond),
		WithOnCommandResult(func(_ context.Context, _ CommandResult) {
			<-blocker
		}),
	)
	defer func() {
		close(blocker)
		server.Close()
	}()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}

	body := "ID=1&Return=0&CMD=INFO"

	// First call — worker picks up and blocks.
	req1 := httptest.NewRequest(http.MethodPost,
		"/iclock/devicecmd?SN=DEV001", strings.NewReader(body))
	w1 := httptest.NewRecorder()
	server.HandleDeviceCmd(w1, req1)

	time.Sleep(50 * time.Millisecond)

	// Fill remaining capacity.
	req2 := httptest.NewRequest(http.MethodPost,
		"/iclock/devicecmd?SN=DEV001", strings.NewReader(body))
	w2 := httptest.NewRecorder()
	server.HandleDeviceCmd(w2, req2)

	// Next dispatch should fail (queue full) — but handler still returns OK.
	req3 := httptest.NewRequest(http.MethodPost,
		"/iclock/devicecmd?SN=DEV001", strings.NewReader(body))
	w3 := httptest.NewRecorder()
	server.HandleDeviceCmd(w3, req3)

	// HandleDeviceCmd always responds OK regardless of queue status.
	if w3.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w3.Code)
	}
}

func TestHandleDeviceCmd_MethodNotAllowed(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "/iclock/devicecmd?SN=DEV001", nil)
	w := httptest.NewRecorder()
	server.HandleDeviceCmd(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestHandleDeviceCmd_MissingSN(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	req := httptest.NewRequest(http.MethodPost, "/iclock/devicecmd", strings.NewReader("ID=1&Return=0&CMD=INFO"))
	w := httptest.NewRecorder()
	server.HandleDeviceCmd(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing SN, got %d", w.Code)
	}
}

func TestHandleDeviceCmd_EmptyBody(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost,
		"/iclock/devicecmd?SN=DEV001", strings.NewReader(""))
	w := httptest.NewRecorder()
	server.HandleDeviceCmd(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "OK" {
		t.Errorf("expected OK, got %q", w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// updateDeviceActivity coverage — unknown device
// ---------------------------------------------------------------------------

func TestUpdateDeviceActivity_UnknownDevice(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	// Should not panic; it's a no-op for unknown devices.
	server.updateDeviceActivity("UNKNOWN123")
}

// ---------------------------------------------------------------------------
// ParseQueryParams coverage — invalid URL
// ---------------------------------------------------------------------------

func TestParseQueryParams_InvalidURL(t *testing.T) {
	_, err := ParseQueryParams("://bad url")
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

func TestParseQueryParams_EmptyQueryValue(t *testing.T) {
	params, err := ParseQueryParams("http://host/path?key=")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v, ok := params["key"]; !ok || v != "" {
		t.Errorf("expected key with empty value, got %q (ok=%v)", v, ok)
	}
}

// ---------------------------------------------------------------------------
// HandleRegistry coverage — body too large, callback queue full
// ---------------------------------------------------------------------------

func TestHandleRegistry_BodyTooLarge(t *testing.T) {
	server := NewADMSServer(WithMaxBodySize(10))
	defer server.Close()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}

	body := strings.Repeat("x", 100)
	req := httptest.NewRequest(http.MethodPost,
		"/iclock/registry?SN=DEV001", strings.NewReader(body))
	w := httptest.NewRecorder()
	server.HandleRegistry(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413, got %d", w.Code)
	}
}

func TestHandleRegistry_CallbackQueueFull(t *testing.T) {
	blocker := make(chan struct{})
	server := NewADMSServer(
		WithCallbackBufferSize(1),
		WithDispatchTimeout(10*time.Millisecond),
		WithOnRegistry(func(_ context.Context, _ string, _ map[string]string) {
			<-blocker
		}),
	)
	defer func() {
		close(blocker)
		server.Close()
	}()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}

	regBody := "~DeviceName=TestDev,MAC=00:11:22:33:44:55"

	// Fill the callback channel.
	req1 := httptest.NewRequest(http.MethodPost,
		"/iclock/registry?SN=DEV001", strings.NewReader(regBody))
	w1 := httptest.NewRecorder()
	server.HandleRegistry(w1, req1)

	time.Sleep(50 * time.Millisecond)

	req2 := httptest.NewRequest(http.MethodPost,
		"/iclock/registry?SN=DEV001", strings.NewReader(regBody))
	w2 := httptest.NewRecorder()
	server.HandleRegistry(w2, req2)

	// Should still return OK (registry queue-full is non-fatal).
	req3 := httptest.NewRequest(http.MethodPost,
		"/iclock/registry?SN=DEV001", strings.NewReader(regBody))
	w3 := httptest.NewRecorder()
	server.HandleRegistry(w3, req3)

	if w3.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w3.Code)
	}
}

func TestHandleRegistry_MethodNotAllowed(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	req := httptest.NewRequest(http.MethodDelete, "/iclock/registry?SN=DEV001", nil)
	w := httptest.NewRecorder()
	server.HandleRegistry(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestHandleRegistry_MissingSN(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "/iclock/registry", nil)
	w := httptest.NewRecorder()
	server.HandleRegistry(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing SN, got %d", w.Code)
	}
}

func TestHandleRegistry_EmptyBody(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet,
		"/iclock/registry?SN=DEV001", nil)
	w := httptest.NewRecorder()
	server.HandleRegistry(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "OK" {
		t.Errorf("expected OK, got %q", w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// HandleInspect coverage — POST rejection, with devices
// ---------------------------------------------------------------------------

func TestHandleInspect_MethodNotAllowed(t *testing.T) {
	server := NewADMSServer(WithEnableInspect())
	defer server.Close()

	req := httptest.NewRequest(http.MethodPost, "/iclock/inspect", nil)
	w := httptest.NewRecorder()
	server.HandleInspect(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestHandleInspect_WithDevices(t *testing.T) {
	server := NewADMSServer(WithEnableInspect())
	defer server.Close()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}
	// Touch activity so the device appears online.
	server.updateDeviceActivity("DEV001")

	req := httptest.NewRequest(http.MethodGet, "/iclock/inspect", nil)
	w := httptest.NewRecorder()
	server.HandleInspect(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var snapshot struct {
		Devices []DeviceSnapshot `json:"devices"`
		Count   int              `json:"count"`
		Time    string           `json:"time"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("failed to decode inspect response: %v", err)
	}
	if snapshot.Count != 1 {
		t.Errorf("expected 1 device, got %d", snapshot.Count)
	}
	if len(snapshot.Devices) != 1 {
		t.Fatalf("expected 1 device in array, got %d", len(snapshot.Devices))
	}
	if snapshot.Devices[0].Serial != "DEV001" {
		t.Errorf("expected serial DEV001, got %q", snapshot.Devices[0].Serial)
	}
	if !snapshot.Devices[0].Online {
		t.Error("expected device to be online after updateDeviceActivity")
	}
}

func TestHandleInspect_EmptyDeviceList(t *testing.T) {
	server := NewADMSServer(WithEnableInspect())
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "/iclock/inspect", nil)
	w := httptest.NewRecorder()
	server.HandleInspect(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var snapshot struct {
		Devices []DeviceSnapshot `json:"devices"`
		Count   int              `json:"count"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if snapshot.Count != 0 {
		t.Errorf("expected 0 devices, got %d", snapshot.Count)
	}
	if snapshot.Devices == nil {
		t.Error("expected non-nil empty devices array")
	}
}

// errResponseWriter is an http.ResponseWriter whose Write always returns an error.
// It records the status code passed to WriteHeader for assertions.
type errResponseWriter struct {
	header http.Header
	status int
}

func newErrResponseWriter() *errResponseWriter {
	return &errResponseWriter{header: make(http.Header)}
}

func (w *errResponseWriter) Header() http.Header       { return w.header }
func (w *errResponseWriter) WriteHeader(code int)      { w.status = code }
func (w *errResponseWriter) Write([]byte) (int, error) { return 0, errors.New("simulated write error") }

func TestHandleInspect_WriteError(t *testing.T) {
	server := NewADMSServer(WithEnableInspect())
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "/iclock/inspect", nil)
	w := newErrResponseWriter()
	server.HandleInspect(w, req)

	// The handler should not panic; the write error is logged and silently
	// absorbed. We just verify it didn't crash.
}

func TestHandleInspect_WriteError_WithDevices(t *testing.T) {
	server := NewADMSServer(WithEnableInspect())
	defer server.Close()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}
	server.updateDeviceActivity("DEV001")

	req := httptest.NewRequest(http.MethodGet, "/iclock/inspect", nil)
	w := newErrResponseWriter()
	server.HandleInspect(w, req)
}

// errOnNthWriteResponseWriter succeeds for the first (n-1) Write calls and
// returns an error on the nth call. This lets tests exercise the trailing-
// newline write-error path in HandleInspect.
type errOnNthWriteResponseWriter struct {
	header http.Header
	status int
	calls  int
	failOn int // 1-based: fail on the Nth Write call
}

func newErrOnNthWriteResponseWriter(n int) *errOnNthWriteResponseWriter {
	return &errOnNthWriteResponseWriter{header: make(http.Header), failOn: n}
}

func (w *errOnNthWriteResponseWriter) Header() http.Header  { return w.header }
func (w *errOnNthWriteResponseWriter) WriteHeader(code int) { w.status = code }
func (w *errOnNthWriteResponseWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls >= w.failOn {
		return 0, errors.New("simulated write error on call " + fmt.Sprintf("%d", w.calls))
	}
	return len(p), nil
}

func TestHandleInspect_TrailingNewlineWriteError(t *testing.T) {
	server := NewADMSServer(WithEnableInspect())
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "/iclock/inspect", nil)
	// First Write (JSON body) succeeds; second Write (trailing newline) fails.
	w := newErrOnNthWriteResponseWriter(2)
	server.HandleInspect(w, req)
}

// ---------------------------------------------------------------------------
// HandleCData coverage — method not allowed, missing SN, GET with commands
// ---------------------------------------------------------------------------

func TestHandleCData_MethodNotAllowed(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	req := httptest.NewRequest(http.MethodDelete, "/iclock/cdata?SN=DEV001", nil)
	w := httptest.NewRecorder()
	server.HandleCData(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestHandleCData_DefaultTable_GET_WithPendingCommands(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}
	if _, err := server.QueueCommand("DEV001", "CHECK"); err != nil {
		t.Fatalf("QueueCommand: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/iclock/cdata?SN=DEV001", nil)
	w := httptest.NewRecorder()
	server.HandleCData(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "C:1:CHECK") {
		t.Errorf("expected response to contain C:1:CHECK, got %q", w.Body.String())
	}
}

func TestHandleCData_DefaultTable_GET_NoCommands(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/iclock/cdata?SN=DEV001", nil)
	w := httptest.NewRecorder()
	server.HandleCData(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "OK" {
		t.Errorf("expected OK, got %q", w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// HandleGetRequest coverage — method not allowed, missing SN
// ---------------------------------------------------------------------------

func TestHandleGetRequest_MethodNotAllowed(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	req := httptest.NewRequest(http.MethodPost, "/iclock/getrequest?SN=DEV001", nil)
	w := httptest.NewRecorder()
	server.HandleGetRequest(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestHandleGetRequest_MissingSN(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "/iclock/getrequest", nil)
	w := httptest.NewRecorder()
	server.HandleGetRequest(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing SN, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// ServeHTTP routing coverage — unknown path
// ---------------------------------------------------------------------------

func TestServeHTTP_UnknownPath(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "/iclock/unknown", nil)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown path, got %d", w.Code)
	}
}

func TestServeHTTP_InspectDisabled(t *testing.T) {
	// Inspect is disabled by default.
	server := NewADMSServer()
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "/iclock/inspect", nil)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 when inspect disabled, got %d", w.Code)
	}
}

func TestServeHTTP_InspectEnabled(t *testing.T) {
	server := NewADMSServer(WithEnableInspect())
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "/iclock/inspect", nil)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 when inspect enabled, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// dispatchCommandResult — no callback configured (short-circuit)
// ---------------------------------------------------------------------------

func TestDispatchCommandResult_NilCallback(t *testing.T) {
	server := NewADMSServer() // no WithOnCommandResult
	defer server.Close()

	ok := server.dispatchCommandResult(CommandResult{ID: 1, Command: "INFO"})
	if !ok {
		t.Error("dispatchCommandResult should return true when no callback configured")
	}
}

// ---------------------------------------------------------------------------
// dispatchCallback — closed server
// ---------------------------------------------------------------------------

func TestDispatchCallback_AfterClose(t *testing.T) {
	server := NewADMSServer()
	server.Close()

	ok := server.dispatchCallback(func() {})
	if ok {
		t.Error("dispatchCallback should return false after server close")
	}
}

// ---------------------------------------------------------------------------
// requireDevice — invalid SN in query
// ---------------------------------------------------------------------------

func TestRequireDevice_InvalidSN(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "/iclock/cdata?SN=!!!", nil)
	w := httptest.NewRecorder()
	sn, ok := server.requireDevice(w, req)
	if ok {
		t.Error("expected requireDevice to return false for invalid SN")
	}
	if sn != "" {
		t.Errorf("expected empty SN, got %q", sn)
	}
}

// ---------------------------------------------------------------------------
// VerifyModeName coverage
// ---------------------------------------------------------------------------

func TestVerifyModeName_AllModes(t *testing.T) {
	tests := []struct {
		mode int
		want string
	}{
		{0, "Password"},
		{1, "Fingerprint"},
		{2, "Card"},
		{9, "Other"},
		{15, "Face"},
		{25, "Palm"},
		{99, "Unknown (99)"},
		{-1, "Unknown (-1)"},
	}
	for _, tt := range tests {
		got := VerifyModeName(tt.mode)
		if got != tt.want {
			t.Errorf("VerifyModeName(%d) = %q, want %q", tt.mode, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Multiple commands in writeCommandsOrOK
// ---------------------------------------------------------------------------

func TestWriteCommandsOrOK_MultipleCommands(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}

	for _, cmd := range []string{"INFO", "CHECK", "REBOOT"} {
		if _, err := server.QueueCommand("DEV001", cmd); err != nil {
			t.Fatalf("QueueCommand(%q): %v", cmd, err)
		}
	}

	w := httptest.NewRecorder()
	server.writeCommandsOrOK(w, "DEV001")

	body := w.Body.String()
	if !strings.Contains(body, "C:1:INFO\n") {
		t.Errorf("expected C:1:INFO in response, got %q", body)
	}
	if !strings.Contains(body, "C:2:CHECK\n") {
		t.Errorf("expected C:2:CHECK in response, got %q", body)
	}
	if !strings.Contains(body, "C:3:REBOOT\n") {
		t.Errorf("expected C:3:REBOOT in response, got %q", body)
	}
}

// ---------------------------------------------------------------------------
// Concurrent dispatch and close
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// dispatchCallback slow-path: channel full, then space freed before timeout
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// updateDeviceActivity — offline-to-online transition
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// New tests for command-result correlation and USERINFO query ingestion
// ---------------------------------------------------------------------------

func TestQueueCommand_ReturnsIncrementingIDs(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	ids := make(map[int64]bool)
	var prev int64
	for i := range 10 {
		id, err := server.QueueCommand("DEV001", fmt.Sprintf("CMD_%d", i))
		if err != nil {
			t.Fatalf("QueueCommand(%d) failed: %v", i, err)
		}
		if id <= 0 {
			t.Errorf("expected positive ID, got %d", id)
		}
		if ids[id] {
			t.Errorf("duplicate ID %d on iteration %d", id, i)
		}
		ids[id] = true
		if i > 0 && id <= prev {
			t.Errorf("expected strictly increasing IDs: prev=%d, current=%d", prev, id)
		}
		prev = id
	}
}

func TestCommandResult_QueuedCommandPopulated(t *testing.T) {
	received := make(chan CommandResult, 1)
	server := NewADMSServer(
		WithOnCommandResult(func(_ context.Context, result CommandResult) {
			received <- result
		}),
	)
	defer server.Close()

	// Queue a command so pendingCommands has the mapping.
	id, err := server.QueueCommand("CORR001", "DATA UPDATE USERINFO PIN=1\tName=Alice")
	if err != nil {
		t.Fatalf("QueueCommand failed: %v", err)
	}

	// Simulate the device confirming execution via POST /iclock/devicecmd.
	body := fmt.Sprintf("ID=%d&Return=0&CMD=DATA", id)
	req := httptest.NewRequest(http.MethodPost, "/iclock/devicecmd?SN=CORR001", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	server.HandleDeviceCmd(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	select {
	case result := <-received:
		if result.QueuedCommand != "DATA UPDATE USERINFO PIN=1\tName=Alice" {
			t.Errorf("expected QueuedCommand %q, got %q",
				"DATA UPDATE USERINFO PIN=1\tName=Alice", result.QueuedCommand)
		}
		if result.Command != "DATA" {
			t.Errorf("expected Command %q, got %q", "DATA", result.Command)
		}
		if result.ID != id {
			t.Errorf("expected ID %d, got %d", id, result.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for command result callback")
	}

	// Verify the pending entry was consumed (not leaked).
	server.queueMutex.Lock()
	_, stillPending := server.pendingCommands[id]
	server.queueMutex.Unlock()
	if stillPending {
		t.Error("expected pending command to be removed after confirmation")
	}
}

func TestCommandResult_QueuedCommandUnknownID(t *testing.T) {
	received := make(chan CommandResult, 1)
	server := NewADMSServer(
		WithOnCommandResult(func(_ context.Context, result CommandResult) {
			received <- result
		}),
	)
	defer server.Close()

	// Post a confirmation with an ID that was never queued.
	body := "ID=999999&Return=0&CMD=DATA"
	req := httptest.NewRequest(http.MethodPost, "/iclock/devicecmd?SN=UNK001", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	server.HandleDeviceCmd(w, req)

	select {
	case result := <-received:
		if result.QueuedCommand != "" {
			t.Errorf("expected empty QueuedCommand for unknown ID, got %q", result.QueuedCommand)
		}
		if result.ID != 999999 {
			t.Errorf("expected ID 999999, got %d", result.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for command result callback")
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

	// Queue commands to populate pendingCommands.
	id1, err := server.QueueCommand("EVICT01", "CMD_A")
	if err != nil {
		t.Fatalf("QueueCommand A failed: %v", err)
	}
	id2, err := server.QueueCommand("EVICT01", "CMD_B")
	if err != nil {
		t.Fatalf("QueueCommand B failed: %v", err)
	}

	// Verify entries exist before eviction.
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

func TestParseUserRecords(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	tests := []struct {
		name    string
		data    string
		want    []UserRecord
		wantLen int
	}{
		{
			name: "single record",
			data: "PIN=1\tName=Alice\tPrivilege=14\tCard=00112233\tPassword=secret",
			want: []UserRecord{
				{PIN: "1", Name: "Alice", Privilege: 14, Card: "00112233", Password: "secret"},
			},
			wantLen: 1,
		},
		{
			name: "multiple records",
			data: "PIN=1\tName=Alice\tPrivilege=0\nPIN=2\tName=Bob\tPrivilege=14\n",
			want: []UserRecord{
				{PIN: "1", Name: "Alice", Privilege: 0},
				{PIN: "2", Name: "Bob", Privilege: 14},
			},
			wantLen: 2,
		},
		{
			name:    "empty input",
			data:    "",
			want:    nil,
			wantLen: 0,
		},
		{
			name:    "blank lines only",
			data:    "\n\n\n",
			want:    nil,
			wantLen: 0,
		},
		{
			name:    "missing PIN is skipped",
			data:    "Name=NoPinUser\tPrivilege=0\nPIN=3\tName=HasPin",
			want:    []UserRecord{{PIN: "3", Name: "HasPin"}},
			wantLen: 1,
		},
		{
			name: "CRLF line endings",
			data: "PIN=10\tName=CRLFUser\r\nPIN=11\tName=Another\r\n",
			want: []UserRecord{
				{PIN: "10", Name: "CRLFUser"},
				{PIN: "11", Name: "Another"},
			},
			wantLen: 2,
		},
		{
			name:    "invalid privilege defaults to zero",
			data:    "PIN=5\tPrivilege=notanumber",
			want:    []UserRecord{{PIN: "5", Privilege: 0}},
			wantLen: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := server.parseUserRecords(tc.data, "PARSETEST")
			if len(got) != tc.wantLen {
				t.Fatalf("expected %d records, got %d: %+v", tc.wantLen, len(got), got)
			}
			for i, want := range tc.want {
				if got[i] != want {
					t.Errorf("record[%d]: expected %+v, got %+v", i, want, got[i])
				}
			}
		})
	}
}

func TestHandleCData_USERINFO(t *testing.T) {
	received := make(chan []UserRecord, 1)
	server := NewADMSServer(
		WithOnQueryUsers(func(_ context.Context, sn string, users []UserRecord) {
			if sn != "UINFO001" {
				// Don't block the channel on unexpected serial numbers.
				return
			}
			received <- users
		}),
	)
	defer server.Close()

	userData := "PIN=100\tName=Charlie\tPrivilege=0\tCard=AABB\tPassword=pw1\nPIN=200\tName=Diana\tPrivilege=14\n"
	req := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN=UINFO001&table=USERINFO", bytes.NewBufferString(userData))
	w := httptest.NewRecorder()

	server.HandleCData(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "OK" {
		t.Errorf("expected OK response body, got %q", w.Body.String())
	}

	select {
	case users := <-received:
		if len(users) != 2 {
			t.Fatalf("expected 2 users, got %d", len(users))
		}
		if users[0].PIN != "100" || users[0].Name != "Charlie" || users[0].Card != "AABB" || users[0].Password != "pw1" {
			t.Errorf("unexpected first user: %+v", users[0])
		}
		if users[1].PIN != "200" || users[1].Name != "Diana" || users[1].Privilege != 14 {
			t.Errorf("unexpected second user: %+v", users[1])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for query users callback")
	}
}

func TestHandleCData_USERINFO_NoCallback(t *testing.T) {
	// No WithOnQueryUsers option — should still return 200 OK without error.
	server := NewADMSServer()
	defer server.Close()

	userData := "PIN=1\tName=Solo\n"
	req := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN=NOCB001&table=USERINFO", bytes.NewBufferString(userData))
	w := httptest.NewRecorder()

	server.HandleCData(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "OK" {
		t.Errorf("expected OK response body, got %q", w.Body.String())
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

func TestSendQueryUsersCommand_ReturnsID(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	id, err := server.SendQueryUsersCommand("QUSER001")
	if err != nil {
		t.Fatalf("SendQueryUsersCommand failed: %v", err)
	}
	if id <= 0 {
		t.Errorf("expected positive ID, got %d", id)
	}

	// Verify the queued command is correct.
	commands := server.DrainCommands("QUSER001")
	if len(commands) != 1 {
		t.Fatalf("expected 1 command, got %d", len(commands))
	}
	if commands[0].cmd != "DATA QUERY USERINFO" {
		t.Errorf("expected %q, got %q", "DATA QUERY USERINFO", commands[0].cmd)
	}
	if commands[0].id != id {
		t.Errorf("expected drained command ID %d to match returned ID %d", commands[0].id, id)
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

func TestHandleCData_USERINFO_BodyTooLarge(t *testing.T) {
	server := NewADMSServer(WithMaxBodySize(10))
	defer server.Close()

	body := strings.Repeat("x", 100)
	req := httptest.NewRequest(http.MethodPost,
		"/iclock/cdata?SN=DEV001&table=USERINFO", strings.NewReader(body))
	w := httptest.NewRecorder()
	server.HandleCData(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413, got %d", w.Code)
	}
}

func TestHandleCData_USERINFO_CallbackQueueFull(t *testing.T) {
	blocker := make(chan struct{})
	server := NewADMSServer(
		WithCallbackBufferSize(1),
		WithDispatchTimeout(10*time.Millisecond),
		WithOnQueryUsers(func(_ context.Context, _ string, _ []UserRecord) {
			<-blocker
		}),
	)
	defer func() {
		close(blocker)
		server.Close()
	}()

	userData := "PIN=1\tName=Alice\n"

	// Fill the callback channel.
	req1 := httptest.NewRequest(http.MethodPost,
		"/iclock/cdata?SN=DEV001&table=USERINFO", strings.NewReader(userData))
	w1 := httptest.NewRecorder()
	server.HandleCData(w1, req1)

	time.Sleep(50 * time.Millisecond) // let worker pick up and block

	// Fill remaining channel capacity.
	req2 := httptest.NewRequest(http.MethodPost,
		"/iclock/cdata?SN=DEV001&table=USERINFO", strings.NewReader(userData))
	w2 := httptest.NewRecorder()
	server.HandleCData(w2, req2)

	// Now queue should be full — dispatch will fail but handler still returns 200.
	req3 := httptest.NewRequest(http.MethodPost,
		"/iclock/cdata?SN=DEV001&table=USERINFO", strings.NewReader(userData))
	w3 := httptest.NewRecorder()
	server.HandleCData(w3, req3)

	// USERINFO handler always returns 200 OK even when callback queue is full
	// (it logs a warning instead of returning 503).
	if w3.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w3.Code)
	}
}

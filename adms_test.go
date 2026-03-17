package zkadms

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	if err := server.QueueCommand(serialNumber, "INFO"); err != nil {
		t.Fatalf("QueueCommand failed: %v", err)
	}
	if err := server.QueueCommand(serialNumber, "DATA QUERY USER"); err != nil {
		t.Fatalf("QueueCommand failed: %v", err)
	}

	commands := server.GetCommands(serialNumber)
	if len(commands) != 2 {
		t.Errorf("Expected 2 commands, got %d", len(commands))
	}
	if commands[0] != "INFO" {
		t.Errorf("Expected first command to be INFO, got %s", commands[0])
	}
	if commands[1] != "DATA QUERY USER" {
		t.Errorf("Expected second command to be 'DATA QUERY USER', got %s", commands[1])
	}

	// Queue should be cleared after retrieval
	commands = server.GetCommands(serialNumber)
	if len(commands) != 0 {
		t.Errorf("Expected empty queue after retrieval, got %d commands", len(commands))
	}
}

func TestGetCommandsDeletesKey(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if err := server.QueueCommand("DEL001", "INFO"); err != nil {
		t.Fatalf("QueueCommand failed: %v", err)
	}
	_ = server.GetCommands("DEL001")

	server.queueMutex.RLock()
	_, exists := server.commandQueue["DEL001"]
	server.queueMutex.RUnlock()

	if exists {
		t.Error("Expected map key to be deleted after GetCommands, but it still exists")
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
	server := NewADMSServer()
	defer server.Close()

	received := make(chan AttendanceRecord, 1)
	server.OnAttendance = func(record AttendanceRecord) {
		received <- record
	}

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
	server := NewADMSServer()
	defer server.Close()

	received := make(chan AttendanceRecord, 2)
	server.OnAttendance = func(record AttendanceRecord) {
		received <- record
	}

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

	if err := server.QueueCommand(serialNumber, "INFO"); err != nil {
		t.Fatalf("QueueCommand failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN="+serialNumber, nil)
	w := httptest.NewRecorder()

	server.HandleCData(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "C:INFO") {
		t.Errorf("Expected command in response, got: %s", w.Body.String())
	}
}

func TestHandleGetRequest(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()
	serialNumber := "TEST001"

	if err := server.QueueCommand(serialNumber, "DATA QUERY USER"); err != nil {
		t.Fatalf("QueueCommand failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/iclock/getrequest?SN="+serialNumber, nil)
	w := httptest.NewRecorder()

	server.HandleGetRequest(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "C:DATA QUERY USER") {
		t.Errorf("Expected command in response, got: %s", w.Body.String())
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
	server := NewADMSServer()
	defer server.Close()

	received := make(chan map[string]string, 1)
	server.OnRegistry = func(sn string, info map[string]string) {
		received <- info
	}

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
		t.Fatal("Timed out waiting for OnRegistry callback")
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

func TestParseRegistryBody(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()
	body := "Key1=Val1, ~Key2=Val2,~Key3=Val3"
	info := server.parseRegistryBody(body)
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

func TestParseDeviceInfo(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	data := "DeviceName=ZKDevice\nSerialNumber=TEST001\nFirmwareVersion=1.0.0"
	info := server.parseDeviceInfo(data)

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

func TestSendCommand(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()
	serialNumber := "TEST001"

	server.SendCommand(serialNumber, "INFO")

	commands := server.GetCommands(serialNumber)
	if len(commands) != 1 {
		t.Errorf("Expected 1 command, got %d", len(commands))
	}
	if commands[0] != "INFO" {
		t.Errorf("Expected INFO command, got %s", commands[0])
	}
}

func TestSendDataCommand(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()
	serialNumber := "TEST001"

	if err := server.SendDataCommand(serialNumber, "USER", "user data"); err != nil {
		t.Fatalf("SendDataCommand failed: %v", err)
	}

	commands := server.GetCommands(serialNumber)
	if len(commands) != 1 {
		t.Errorf("Expected 1 command, got %d", len(commands))
	}
	if !strings.Contains(commands[0], "DATA QUERY USER") {
		t.Errorf("Expected DATA QUERY USER in command, got %s", commands[0])
	}
	if !strings.Contains(commands[0], "user data") {
		t.Errorf("Expected user data in command, got %s", commands[0])
	}
}

func TestSendInfoCommand(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()
	serialNumber := "TEST001"

	if err := server.SendInfoCommand(serialNumber); err != nil {
		t.Fatalf("SendInfoCommand failed: %v", err)
	}

	commands := server.GetCommands(serialNumber)
	if len(commands) != 1 {
		t.Errorf("Expected 1 command, got %d", len(commands))
	}
	if commands[0] != "INFO" {
		t.Errorf("Expected INFO command, got %s", commands[0])
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
			_ = server.QueueCommand(serialNumber, "INFO")
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
	commands := server.GetCommands(serialNumber)
	if len(commands) != 10 {
		t.Errorf("Expected 10 commands, got %d", len(commands))
	}
}

func TestAttendanceRecordSerialNumber(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()
	serialNumber := "TEST001"

	received := make(chan AttendanceRecord, 1)
	server.OnAttendance = func(record AttendanceRecord) {
		received <- record
	}

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

func TestCallbackNilAfterEnqueue(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	received := make(chan AttendanceRecord, 1)
	server.OnAttendance = func(record AttendanceRecord) {
		received <- record
	}

	attendanceData := "123\t2024-01-01 08:00:00\t0\t1\t0"
	req := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN=TEST001&table=ATTLOG", bytes.NewBufferString(attendanceData))
	w := httptest.NewRecorder()
	server.HandleCData(w, req)

	// Nil the callback after the request was handled (closure already enqueued).
	// With the captured-local fix this must not panic.
	server.OnAttendance = nil

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
	server := NewADMSServer()
	defer server.Close()

	received := make(chan string, 1)

	// First callback panics.
	server.OnAttendance = func(record AttendanceRecord) {
		panic("boom")
	}
	req1 := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN=TEST001&table=ATTLOG",
		bytes.NewBufferString("100\t2024-01-01 08:00:00\t0\t1\t0"))
	w1 := httptest.NewRecorder()
	server.HandleCData(w1, req1)

	// Replace with a well-behaved callback.
	server.OnAttendance = func(record AttendanceRecord) {
		received <- record.UserID
	}
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
	server := NewADMSServer()
	defer server.Close()

	var mu sync.Mutex
	var received []AttendanceRecord
	done := make(chan struct{})

	server.OnAttendance = func(record AttendanceRecord) {
		mu.Lock()
		received = append(received, record)
		if len(received) == 3 {
			close(done)
		}
		mu.Unlock()
	}

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
	server := NewADMSServer()
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

	server.OnAttendance = func(record AttendanceRecord) {}

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
	server := NewADMSServer()

	received := make(chan AttendanceRecord, 1)
	server.OnAttendance = func(record AttendanceRecord) {
		received <- record
	}

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

	if err := server.QueueCommand("DRAIN001", "CMD1"); err != nil {
		t.Fatalf("QueueCommand failed: %v", err)
	}
	if err := server.QueueCommand("DRAIN001", "CMD2"); err != nil {
		t.Fatalf("QueueCommand failed: %v", err)
	}

	commands := server.DrainCommands("DRAIN001")
	if len(commands) != 2 {
		t.Fatalf("expected 2 commands, got %d", len(commands))
	}
	if commands[0] != "CMD1" || commands[1] != "CMD2" {
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

	if err := server.QueueCommand("DRAIN_DEL", "INFO"); err != nil {
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

func TestGetCommands_BackwardCompat(t *testing.T) {
	// GetCommands should still work (it's deprecated but not removed).
	server := NewADMSServer()
	defer server.Close()

	if err := server.QueueCommand("COMPAT001", "INFO"); err != nil {
		t.Fatalf("QueueCommand failed: %v", err)
	}
	commands := server.GetCommands("COMPAT001")
	if len(commands) != 1 || commands[0] != "INFO" {
		t.Errorf("GetCommands backward compat broken: %v", commands)
	}
}

func TestFunctionalOption_OverridesDeprecatedField(t *testing.T) {
	// When both the deprecated field and functional option are set,
	// the functional option should take precedence.
	oldReceived := make(chan struct{}, 1)
	newReceived := make(chan AttendanceRecord, 1)

	server := NewADMSServer(
		WithOnAttendance(func(ctx context.Context, record AttendanceRecord) {
			newReceived <- record
		}),
	)
	defer server.Close()

	// Also set the deprecated field — it should NOT be called.
	server.OnAttendance = func(record AttendanceRecord) {
		oldReceived <- struct{}{}
	}

	data := "123\t2024-01-01 08:00:00\t0\t1\t0"
	req := httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN=TEST001&table=ATTLOG", bytes.NewBufferString(data))
	w := httptest.NewRecorder()
	server.HandleCData(w, req)

	select {
	case rec := <-newReceived:
		if rec.UserID != "123" {
			t.Errorf("expected UserID 123, got %s", rec.UserID)
		}
	case <-oldReceived:
		t.Error("deprecated OnAttendance field was called instead of WithOnAttendance")
	case <-time.After(2 * time.Second):
		t.Fatal("Timed out waiting for callback")
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
		if err := server.QueueCommand(sn, "CMD"+string(rune('1'+i))); err != nil {
			t.Fatalf("QueueCommand %d failed: %v", i+1, err)
		}
	}

	// Fourth should fail.
	err := server.QueueCommand(sn, "CMD4")
	if !errors.Is(err, ErrCommandQueueFull) {
		t.Errorf("expected ErrCommandQueueFull, got %v", err)
	}

	// Draining should free up space.
	_ = server.DrainCommands(sn)
	if err := server.QueueCommand(sn, "CMD5"); err != nil {
		t.Errorf("after drain, QueueCommand should succeed: %v", err)
	}
}

func TestWithMaxCommandsPerDevice_ZeroMeansUnlimited(t *testing.T) {
	server := NewADMSServer(WithMaxCommandsPerDevice(0))
	defer server.Close()

	for i := range 100 {
		if err := server.QueueCommand("DEV1", "CMD"+string(rune(i))); err != nil {
			t.Fatalf("QueueCommand %d failed: %v", i, err)
		}
	}
}

func TestWithMaxDevices_NegativeIgnored(t *testing.T) {
	server := NewADMSServer(WithMaxDevices(-5))
	defer server.Close()

	if server.maxDevices != 0 {
		t.Errorf("expected negative to be ignored (0), got %d", server.maxDevices)
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

func TestSendDataCommand_ReturnsError(t *testing.T) {
	server := NewADMSServer(WithMaxCommandsPerDevice(1))
	defer server.Close()
	sn := "TEST001"

	// First command should succeed.
	if err := server.SendDataCommand(sn, "USER", "data"); err != nil {
		t.Fatalf("SendDataCommand failed: %v", err)
	}

	// Second should fail due to queue limit.
	err := server.SendDataCommand(sn, "USER", "more data")
	if !errors.Is(err, ErrCommandQueueFull) {
		t.Errorf("expected ErrCommandQueueFull, got %v", err)
	}
}

func TestSendInfoCommand_ReturnsError(t *testing.T) {
	server := NewADMSServer(WithMaxCommandsPerDevice(1))
	defer server.Close()
	sn := "TEST001"

	// First command should succeed.
	if err := server.SendInfoCommand(sn); err != nil {
		t.Fatalf("SendInfoCommand failed: %v", err)
	}

	// Second should fail due to queue limit.
	err := server.SendInfoCommand(sn)
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

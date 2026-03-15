package zkdevicesync

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewIClockServer(t *testing.T) {
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

	server.RegisterDevice(serialNumber)

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

	server.RegisterDevice("COPY001")

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

	server.RegisterDevice("LD001")

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

	server.QueueCommand(serialNumber, "INFO")
	server.QueueCommand(serialNumber, "DATA QUERY USER")

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

	server.QueueCommand("DEL001", "INFO")
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

	req := httptest.NewRequest("GET", "/iclock/cdata", nil)
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
	req := httptest.NewRequest("POST", "/iclock/cdata?SN=TEST001&table=ATTLOG", bytes.NewBufferString(attendanceData))
	w := httptest.NewRecorder()

	server.HandleCData(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "OK") {
		t.Errorf("Expected OK response, got: %s", w.Body.String())
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
	req := httptest.NewRequest("POST", "/iclock/cdata?SN=TEST001&table=ATTLOG", bytes.NewBufferString(attendanceData))
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

	req := httptest.NewRequest("POST", "/iclock/cdata?SN=TEST001&table=OPERLOG", bytes.NewBufferString("operation data"))
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

	server.QueueCommand(serialNumber, "INFO")

	req := httptest.NewRequest("POST", "/iclock/cdata?SN="+serialNumber, nil)
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

	server.QueueCommand(serialNumber, "DATA QUERY USER")

	req := httptest.NewRequest("GET", "/iclock/getrequest?SN="+serialNumber, nil)
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

	req := httptest.NewRequest("GET", "/iclock/getrequest?SN=TEST001", nil)
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

	req := httptest.NewRequest("POST", "/iclock/devicecmd?SN=TEST001", bytes.NewBufferString("command result"))
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
	req := httptest.NewRequest("POST", "/iclock/devicecmd?SN=NEW001", bytes.NewBufferString("result"))
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
		{"inspect GET", "GET", "/iclock/inspect", http.StatusOK},
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
	server := NewADMSServer()
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
	req := httptest.NewRequest("POST", "/iclock/registry?SN=REG001", bytes.NewBufferString(body))
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
	req := httptest.NewRequest("POST", "/iclock/registry?SN=REG002", bytes.NewBufferString(body))
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
	server.RegisterDevice("A1")
	// Mark old activity to test offline
	server.devicesMutex.Lock()
	server.devices["A1"].LastActivity = time.Now().Add(-3 * time.Minute)
	server.devicesMutex.Unlock()

	req := httptest.NewRequest("GET", "/iclock/inspect", nil)
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

	server.RegisterDevice("ONLINE001")
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

	server.SendDataCommand(serialNumber, "USER", "user data")

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

	server.SendInfoCommand(serialNumber)

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

	server.RegisterDevice("TEST001")
	server.RegisterDevice("TEST002")
	server.RegisterDevice("TEST003")

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

	server.RegisterDevice(serialNumber)

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
			server.RegisterDevice(serialNumber)
		})
	}

	// Concurrent command queuing
	for range 10 {
		wg.Go(func() {
			server.QueueCommand(serialNumber, "INFO")
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
	req := httptest.NewRequest("POST", "/iclock/cdata?SN="+serialNumber+"&table=ATTLOG", bytes.NewBufferString(attendanceData))
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
	req := httptest.NewRequest("POST", "/iclock/cdata?SN=TEST001&table=ATTLOG", bytes.NewBufferString(attendanceData))
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
	req1 := httptest.NewRequest("POST", "/iclock/cdata?SN=TEST001&table=ATTLOG",
		bytes.NewBufferString("100\t2024-01-01 08:00:00\t0\t1\t0"))
	w1 := httptest.NewRecorder()
	server.HandleCData(w1, req1)

	// Replace with a well-behaved callback.
	server.OnAttendance = func(record AttendanceRecord) {
		received <- record.UserID
	}
	req2 := httptest.NewRequest("POST", "/iclock/cdata?SN=TEST001&table=ATTLOG",
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
		req := httptest.NewRequest("POST", "/iclock/cdata?SN=TEST001&table=ATTLOG", bytes.NewBufferString(attendanceData))
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
	req := httptest.NewRequest("POST", "/iclock/cdata?SN=BATCH001&table=ATTLOG", bytes.NewBufferString(data))
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
	req := httptest.NewRequest("POST", "/iclock/cdata?SN=FULL001&table=ATTLOG", bytes.NewBufferString(data))
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
	req1 := httptest.NewRequest("POST", "/iclock/cdata?SN=RETRY001&table=ATTLOG", bytes.NewBufferString(data))
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
	req2 := httptest.NewRequest("POST", "/iclock/cdata?SN=RETRY001&table=ATTLOG", bytes.NewBufferString(data))
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

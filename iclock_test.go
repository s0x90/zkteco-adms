package zkdevicesync

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewIClockServer(t *testing.T) {
	server := NewIClockServer()
	defer server.Close()
	if server == nil {
		t.Fatal("NewIClockServer returned nil")
	}
	if server.devices == nil {
		t.Error("devices map not initialized")
	}
	if server.commandQueue == nil {
		t.Error("commandQueue map not initialized")
	}
}

func TestRegisterDevice(t *testing.T) {
	server := NewIClockServer()
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
	server := NewIClockServer()
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
	server := NewIClockServer()
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
	server := NewIClockServer()
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
	server := NewIClockServer()
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
	server := NewIClockServer()
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
	server := NewIClockServer()
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
	server := NewIClockServer()
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

	for i := 0; i < 2; i++ {
		select {
		case <-received:
		case <-time.After(2 * time.Second):
			t.Fatalf("Timed out waiting for record %d", i+1)
		}
	}
}

func TestHandleCData_OperationLog(t *testing.T) {
	server := NewIClockServer()
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
	server := NewIClockServer()
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
	server := NewIClockServer()
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
	server := NewIClockServer()
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
	server := NewIClockServer()
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
	server := NewIClockServer()
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
	server := NewIClockServer()
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
	server := NewIClockServer()
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
	server := NewIClockServer()
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
	server := NewIClockServer()
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
	server := NewIClockServer()
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
	server := NewIClockServer()
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
	server := NewIClockServer()
	defer server.Close()
	body := "Key1=Val1, ~Key2=Val2,~Key3=Val3"
	info := server.parseRegistryBody(body)
	if info["Key1"] != "Val1" || info["Key2"] != "Val2" || info["Key3"] != "Val3" {
		t.Errorf("unexpected parse result: %#v", info)
	}
}

func TestParseAttendanceRecords(t *testing.T) {
	server := NewIClockServer()
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
	server := NewIClockServer()
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
	server := NewIClockServer()
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
	server := NewIClockServer()
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
	server := NewIClockServer()
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
	server := NewIClockServer()
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
	server := NewIClockServer()
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
	server := NewIClockServer()
	defer server.Close()
	serialNumber := "TEST001"

	done := make(chan bool)

	// Concurrent device registration
	for i := 0; i < 10; i++ {
		go func(id int) {
			server.RegisterDevice(serialNumber)
			done <- true
		}(i)
	}

	// Concurrent command queuing
	for i := 0; i < 10; i++ {
		go func(id int) {
			server.QueueCommand(serialNumber, "INFO")
			done <- true
		}(i)
	}

	// Wait for all goroutines
	for i := 0; i < 20; i++ {
		<-done
	}

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
	server := NewIClockServer()
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

func BenchmarkHandleCData(b *testing.B) {
	server := NewIClockServer()
	defer server.Close()
	attendanceData := "123\t2024-01-01 08:00:00\t0\t1\t0"

	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest("POST", "/iclock/cdata?SN=TEST001&table=ATTLOG", bytes.NewBufferString(attendanceData))
		w := httptest.NewRecorder()
		server.HandleCData(w, req)
	}
}

func BenchmarkParseAttendanceRecords(b *testing.B) {
	server := NewIClockServer()
	defer server.Close()
	data := "123\t2024-01-01 08:00:00\t0\t1\t0\n456\t2024-01-01 17:00:00\t1\t1\t0\n789\t2024-01-01 12:00:00\t2\t1\t0"

	for i := 0; i < b.N; i++ {
		server.parseAttendanceRecords(data, "TEST001")
	}
}

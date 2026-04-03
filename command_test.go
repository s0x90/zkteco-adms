package zkadms

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestQueueAndGetCommands(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()
	serialNumber := "TEST001"

	if err := server.RegisterDevice(serialNumber); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

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
	if commands[0].Command != "INFO" {
		t.Errorf("Expected first command to be INFO, got %s", commands[0].Command)
	}
	if commands[1].Command != "DATA UPDATE USERINFO PIN=1001\tName=Test\tPrivilege=0\tCard=" {
		t.Errorf("Expected second command to be DATA UPDATE USERINFO, got %s", commands[1].Command)
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

	if err := server.RegisterDevice("DEL001"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

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

func TestQueueCommand(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()
	serialNumber := "TEST001"

	if err := server.RegisterDevice(serialNumber); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	if _, err := server.QueueCommand(serialNumber, "INFO"); err != nil {
		t.Fatalf("QueueCommand failed: %v", err)
	}

	commands := server.DrainCommands(serialNumber)
	if len(commands) != 1 {
		t.Errorf("Expected 1 command, got %d", len(commands))
	}
	if commands[0].Command != "INFO" {
		t.Errorf("Expected INFO command, got %s", commands[0].Command)
	}
}

func TestDrainCommands(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if err := server.RegisterDevice("DRAIN001"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

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
	if commands[0].Command != "CMD1" || commands[1].Command != "CMD2" {
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

	if err := server.RegisterDevice("DRAIN_DEL"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

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

	if err := server.RegisterDevice("COUNT001"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
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

func TestSendUserAddCommand(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()
	serialNumber := "TEST001"

	if err := server.RegisterDevice(serialNumber); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	if _, err := server.SendUserAddCommand(serialNumber, "1001", "John Doe", 0, "12345678"); err != nil {
		t.Fatalf("SendUserAddCommand failed: %v", err)
	}

	commands := server.DrainCommands(serialNumber)
	if len(commands) != 1 {
		t.Errorf("Expected 1 command, got %d", len(commands))
	}
	want := "DATA UPDATE USERINFO PIN=1001\tName=John Doe\tPrivilege=0\tCard=12345678"
	if commands[0].Command != want {
		t.Errorf("Expected command %q, got %q", want, commands[0].Command)
	}
}

func TestSendUserDeleteCommand(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()
	serialNumber := "TEST001"

	if err := server.RegisterDevice(serialNumber); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	if _, err := server.SendUserDeleteCommand(serialNumber, "1001"); err != nil {
		t.Fatalf("SendUserDeleteCommand failed: %v", err)
	}

	commands := server.DrainCommands(serialNumber)
	if len(commands) != 1 {
		t.Errorf("Expected 1 command, got %d", len(commands))
	}
	want := "DATA DELETE USERINFO PIN=1001"
	if commands[0].Command != want {
		t.Errorf("Expected command %q, got %q", want, commands[0].Command)
	}
}

func TestSendInfoCommand(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()
	serialNumber := "TEST001"

	if err := server.RegisterDevice(serialNumber); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	if _, err := server.SendInfoCommand(serialNumber); err != nil {
		t.Fatalf("SendInfoCommand failed: %v", err)
	}

	commands := server.DrainCommands(serialNumber)
	if len(commands) != 1 {
		t.Errorf("Expected 1 command, got %d", len(commands))
	}
	if commands[0].Command != "INFO" {
		t.Errorf("Expected INFO command, got %s", commands[0].Command)
	}
}

func TestSendCheckCommand(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if err := server.RegisterDevice("TEST001"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	if _, err := server.SendCheckCommand("TEST001"); err != nil {
		t.Fatalf("SendCheckCommand failed: %v", err)
	}

	commands := server.DrainCommands("TEST001")
	if len(commands) != 1 {
		t.Fatalf("Expected 1 command, got %d", len(commands))
	}
	if commands[0].Command != "CHECK" {
		t.Errorf("Expected CHECK command, got %s", commands[0].Command)
	}
}

func TestSendGetOptionCommand(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if err := server.RegisterDevice("TEST001"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	if _, err := server.SendGetOptionCommand("TEST001", "DeviceName"); err != nil {
		t.Fatalf("SendGetOptionCommand failed: %v", err)
	}

	commands := server.DrainCommands("TEST001")
	if len(commands) != 1 {
		t.Fatalf("Expected 1 command, got %d", len(commands))
	}
	want := "GET OPTION FROM DeviceName"
	if commands[0].Command != want {
		t.Errorf("Expected command %q, got %q", want, commands[0].Command)
	}
}

func TestSendShellCommand(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if err := server.RegisterDevice("TEST001"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	if _, err := server.SendShellCommand("TEST001", "date"); err != nil {
		t.Fatalf("SendShellCommand failed: %v", err)
	}

	commands := server.DrainCommands("TEST001")
	if len(commands) != 1 {
		t.Fatalf("Expected 1 command, got %d", len(commands))
	}
	want := "Shell date"
	if commands[0].Command != want {
		t.Errorf("Expected command %q, got %q", want, commands[0].Command)
	}
}

func TestSendQueryUsersCommand(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if err := server.RegisterDevice("TEST001"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	if _, err := server.SendQueryUsersCommand("TEST001"); err != nil {
		t.Fatalf("SendQueryUsersCommand failed: %v", err)
	}

	commands := server.DrainCommands("TEST001")
	if len(commands) != 1 {
		t.Fatalf("Expected 1 command, got %d", len(commands))
	}
	want := "DATA QUERY USERINFO"
	if commands[0].Command != want {
		t.Errorf("Expected command %q, got %q", want, commands[0].Command)
	}
}

func TestSendLogCommand(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if err := server.RegisterDevice("TEST001"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	if _, err := server.SendLogCommand("TEST001"); err != nil {
		t.Fatalf("SendLogCommand failed: %v", err)
	}

	commands := server.DrainCommands("TEST001")
	if len(commands) != 1 {
		t.Fatalf("Expected 1 command, got %d", len(commands))
	}
	if commands[0].Command != "LOG" {
		t.Errorf("Expected LOG command, got %s", commands[0].Command)
	}
}

func TestCommandIDIncrement(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()
	serialNumber := "IDTEST001"
	if err := server.RegisterDevice(serialNumber); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

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

	if err := server.RegisterDevice("DEV1"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

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

	for _, sn := range []string{"DEVA", "DEVB"} {
		if err := server.RegisterDevice(sn); err != nil {
			t.Fatalf("RegisterDevice(%s) failed: %v", sn, err)
		}
	}

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

func TestQueueCommand_ReturnsIncrementingIDs(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

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

	if err := server.RegisterDevice("CORR001"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	// Queue a command and drain it so pendingCommands has the mapping.
	id, err := server.QueueCommand("CORR001", "DATA UPDATE USERINFO PIN=1\tName=Alice")
	if err != nil {
		t.Fatalf("QueueCommand failed: %v", err)
	}
	_ = server.DrainCommands("CORR001")

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

func TestSendUserAddCommand_ReturnsError(t *testing.T) {
	server := NewADMSServer(WithMaxCommandsPerDevice(1))
	defer server.Close()
	sn := "TEST001"

	if err := server.RegisterDevice(sn); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

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

	if err := server.RegisterDevice(sn); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

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

	if err := server.RegisterDevice(sn); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

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

	if err := server.RegisterDevice(sn); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

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

	if err := server.RegisterDevice(sn); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

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

	if err := server.RegisterDevice(sn); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

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

	if err := server.RegisterDevice(sn); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

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

	if err := server.RegisterDevice(sn); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	if _, err := server.SendLogCommand(sn); err != nil {
		t.Fatalf("SendLogCommand failed: %v", err)
	}

	_, err := server.SendLogCommand(sn)
	if !errors.Is(err, ErrCommandQueueFull) {
		t.Errorf("expected ErrCommandQueueFull, got %v", err)
	}
}

func TestSendQueryUsersCommand_ReturnsID(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if err := server.RegisterDevice("QUSER001"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

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
	if commands[0].Command != "DATA QUERY USERINFO" {
		t.Errorf("expected %q, got %q", "DATA QUERY USERINFO", commands[0].Command)
	}
	if commands[0].ID != id {
		t.Errorf("expected drained command ID %d to match returned ID %d", commands[0].ID, id)
	}
}

func TestQueueCommand_ReturnsErrDeviceNotFound(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	_, err := server.QueueCommand("NONEXISTENT", "INFO")
	if !errors.Is(err, ErrDeviceNotFound) {
		t.Errorf("expected ErrDeviceNotFound, got %v", err)
	}
}

func TestSendInfoCommand_ReturnsErrDeviceNotFound(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	_, err := server.SendInfoCommand("NONEXISTENT")
	if !errors.Is(err, ErrDeviceNotFound) {
		t.Errorf("expected ErrDeviceNotFound, got %v", err)
	}
}

func TestValidateCommandField_RejectsCRLF(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"newline", "CMD\nINJECTED"},
		{"carriage return", "CMD\rINJECTED"},
		{"crlf", "CMD\r\nINJECTED"},
		{"trailing newline", "CMD\n"},
		{"leading newline", "\nCMD"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCommandField("command", tc.value)
			if !errors.Is(err, ErrInvalidCommandField) {
				t.Errorf("expected ErrInvalidCommandField for %q, got %v", tc.value, err)
			}
		})
	}
}

func TestValidateCommandField_AcceptsValid(t *testing.T) {
	valid := []string{
		"INFO",
		"DATA UPDATE USERINFO PIN=1\tName=Alice",
		"CHECK",
		"SHELL echo hello",
		"DATA QUERY USERINFO",
	}

	for _, v := range valid {
		if err := validateCommandField("command", v); err != nil {
			t.Errorf("expected nil for %q, got %v", v, err)
		}
	}
}

func TestQueueCommand_RejectsNewlineInCommand(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	_, err := server.QueueCommand("DEV001", "INFO\nINJECTED")
	if !errors.Is(err, ErrInvalidCommandField) {
		t.Errorf("expected ErrInvalidCommandField, got %v", err)
	}
}

func TestSendUserAddCommand_RejectsNewlineInFields(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	_, err := server.SendUserAddCommand("DEV001", "1", "Alice\nINJECTED", 0, "")
	if !errors.Is(err, ErrInvalidCommandField) {
		t.Errorf("expected ErrInvalidCommandField for name with newline, got %v", err)
	}

	_, err = server.SendUserAddCommand("DEV001", "1\n2", "Alice", 0, "")
	if !errors.Is(err, ErrInvalidCommandField) {
		t.Errorf("expected ErrInvalidCommandField for pin with newline, got %v", err)
	}
}

func TestSendShellCommand_RejectsNewline(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	_, err := server.SendShellCommand("DEV001", "reboot\nrm -rf /")
	if !errors.Is(err, ErrInvalidCommandField) {
		t.Errorf("expected ErrInvalidCommandField, got %v", err)
	}
}

func TestPendingCommands_CountedTowardLimit(t *testing.T) {
	server := NewADMSServer(WithMaxCommandsPerDevice(2))
	defer server.Close()

	sn := "PENDING01"
	if err := server.RegisterDevice(sn); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	// Queue 2 commands (hits the limit).
	for i := range 2 {
		if _, err := server.QueueCommand(sn, fmt.Sprintf("CMD_%d", i)); err != nil {
			t.Fatalf("QueueCommand %d failed: %v", i, err)
		}
	}

	// Drain moves them to pendingCommands.
	_ = server.DrainCommands(sn)

	// Queue is empty, but 2 pending commands exist — still at the limit.
	_, err := server.QueueCommand(sn, "CMD_NEW")
	if !errors.Is(err, ErrCommandQueueFull) {
		t.Errorf("expected ErrCommandQueueFull (pending counted toward limit), got %v", err)
	}
}

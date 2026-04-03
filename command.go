package zkadms

import (
	"fmt"
	"strings"
)

// QueueCommand adds a command to be sent to the device on its next poll.
// It assigns a monotonically increasing command ID at queue time and returns
// the ID so callers can correlate the command with its [CommandResult].
// It returns [ErrDeviceNotFound] if the device is not registered with the server.
// It returns [ErrInvalidCommandField] if the command contains control characters
// that could cause injection on the ADMS wire protocol.
// It returns [ErrCommandQueueFull] (with ID 0) if the per-device limit
// configured via [WithMaxCommandsPerDevice] has been reached.
func (s *ADMSServer) QueueCommand(serialNumber, command string) (int64, error) {
	if err := validateCommandField("command", command); err != nil {
		return 0, err
	}

	s.devicesMutex.RLock()
	_, exists := s.devices[serialNumber]
	s.devicesMutex.RUnlock()
	if !exists {
		return 0, ErrDeviceNotFound
	}

	id := s.cmdID.Add(1)

	s.queueMutex.Lock()
	defer s.queueMutex.Unlock()

	if s.maxCommandsPerDevice > 0 {
		queued := len(s.commandQueue[serialNumber])
		pending := s.pendingCountLocked(serialNumber)
		if queued+pending >= s.maxCommandsPerDevice {
			return 0, ErrCommandQueueFull
		}
	}
	s.commandQueue[serialNumber] = append(s.commandQueue[serialNumber], CommandEntry{ID: id, Command: command})
	return id, nil
}

// pendingCountLocked returns the number of pending (drained but unconfirmed)
// commands for the given serial number. The caller must hold s.queueMutex.
func (s *ADMSServer) pendingCountLocked(serialNumber string) int {
	n := 0
	for _, entry := range s.pendingCommands {
		if entry.sn == serialNumber {
			n++
		}
	}
	return n
}

// DrainCommands retrieves and removes all queued commands for a device.
// After this call, the device's command queue is empty. Drained commands
// are tracked as pending until the device confirms execution via
// /iclock/devicecmd.
func (s *ADMSServer) DrainCommands(serialNumber string) []CommandEntry {
	s.queueMutex.Lock()
	defer s.queueMutex.Unlock()

	commands := s.commandQueue[serialNumber]
	delete(s.commandQueue, serialNumber)
	for _, cmd := range commands {
		s.pendingCommands[cmd.ID] = pendingEntry{sn: serialNumber, cmd: cmd.Command}
	}
	return commands
}

// PendingCommandsCount returns the number of queued commands for a device
// without modifying the queue.
func (s *ADMSServer) PendingCommandsCount(serialNumber string) int {
	s.queueMutex.RLock()
	defer s.queueMutex.RUnlock()
	return len(s.commandQueue[serialNumber])
}

// validateCommandField checks that a command field value does not contain
// control characters that could cause injection on the ADMS wire protocol.
// The wire format uses "C:<ID>:<CMD>\n", so newlines and carriage returns
// in field values would allow injecting extra command lines.
func validateCommandField(name, value string) error {
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("%w: %s", ErrInvalidCommandField, name)
	}
	return nil
}

// SendInfoCommand queues an INFO command to request device information.
// It returns the assigned command ID and an error if the command queue is full
// (see [WithMaxCommandsPerDevice]).
func (s *ADMSServer) SendInfoCommand(serialNumber string) (int64, error) {
	return s.QueueCommand(serialNumber, "INFO")
}

// SendUserAddCommand queues a DATA UPDATE USERINFO command to add or update
// a user on the device. The wire format uses tab-separated key=value pairs:
//
//	DATA UPDATE USERINFO PIN=<pin>\tName=<name>\tPrivilege=<privilege>\tCard=<card>
//
// Privilege values: 0 = normal user, 14 = admin.
//
// Note: the ADMS datasheet documents this as "USER ADD", but real devices
// (e.g. ZAM180-NF firmware) require the DATA UPDATE USERINFO prefix instead.
// The device confirms execution by POSTing to /iclock/devicecmd with CMD=DATA.
//
// It returns the assigned command ID and an error if the command queue is full
// (see [WithMaxCommandsPerDevice]).
func (s *ADMSServer) SendUserAddCommand(serialNumber, pin, name string, privilege int, card string) (int64, error) {
	for _, f := range []struct{ name, value string }{
		{"pin", pin}, {"name", name}, {"card", card},
	} {
		if err := validateCommandField(f.name, f.value); err != nil {
			return 0, err
		}
	}
	cmd := fmt.Sprintf("DATA UPDATE USERINFO PIN=%s\tName=%s\tPrivilege=%d\tCard=%s", pin, name, privilege, card)
	return s.QueueCommand(serialNumber, cmd)
}

// SendUserDeleteCommand queues a DATA DELETE USERINFO command to remove a user
// from the device. The wire format is:
//
//	DATA DELETE USERINFO PIN=<pin>
//
// Note: the ADMS datasheet documents this as "USER DEL", but real devices
// (e.g. ZAM180-NF firmware) require the DATA DELETE USERINFO prefix instead.
// The device confirms execution by POSTing to /iclock/devicecmd with CMD=DATA.
//
// It returns the assigned command ID and an error if the command queue is full
// (see [WithMaxCommandsPerDevice]).
func (s *ADMSServer) SendUserDeleteCommand(serialNumber, pin string) (int64, error) {
	if err := validateCommandField("pin", pin); err != nil {
		return 0, err
	}
	cmd := fmt.Sprintf("DATA DELETE USERINFO PIN=%s", pin)
	return s.QueueCommand(serialNumber, cmd)
}

// SendCheckCommand queues a CHECK (heartbeat) command to verify device
// responsiveness. The device confirms by POSTing to /iclock/devicecmd
// with CMD=CHECK and Return=0.
//
// It returns the assigned command ID and an error if the command queue is full
// (see [WithMaxCommandsPerDevice]).
func (s *ADMSServer) SendCheckCommand(serialNumber string) (int64, error) {
	return s.QueueCommand(serialNumber, "CHECK")
}

// SendGetOptionCommand queues a GET OPTION command to retrieve a device
// configuration value. The device confirms by POSTing to /iclock/devicecmd
// with CMD=GET OPTION.
//
// Confirmed keys on ZAM180-NF firmware: DeviceName, FWVersion, IPAddress,
// MACAddress, Platform, WorkCode, LockCount, UserCount, FPCount,
// AttLogCount, FaceCount, TransactionCount, MaxUserCount, MaxAttLogCount,
// MaxFingerCount, MaxFaceCount.
//
// The option value is typically delivered via the device info push
// (POST /iclock/cdata) rather than in the command confirmation body.
//
// It returns the assigned command ID and an error if the command queue is full
// (see [WithMaxCommandsPerDevice]).
func (s *ADMSServer) SendGetOptionCommand(serialNumber, key string) (int64, error) {
	if err := validateCommandField("key", key); err != nil {
		return 0, err
	}
	cmd := fmt.Sprintf("GET OPTION FROM %s", key)
	return s.QueueCommand(serialNumber, cmd)
}

// SendShellCommand queues a Shell command for execution on the device.
// The device executes the command and confirms by POSTing to /iclock/devicecmd
// with CMD=Shell, Return=0, and the output in the Content field.
//
// WARNING: This executes arbitrary commands on the device's Linux OS.
// Use with extreme caution — incorrect commands can brick the device.
//
// Example:
//
//	server.SendShellCommand("SERIAL001", "date")
//	// Device responds: ID=1&Return=0&CMD=Shell\nContent=Tue Mar 24 16:12:26 GMT 2026
//
// It returns the assigned command ID and an error if the command queue is full
// (see [WithMaxCommandsPerDevice]).
func (s *ADMSServer) SendShellCommand(serialNumber, command string) (int64, error) {
	if err := validateCommandField("command", command); err != nil {
		return 0, err
	}
	cmd := fmt.Sprintf("Shell %s", command)
	return s.QueueCommand(serialNumber, cmd)
}

// SendQueryUsersCommand queues a DATA QUERY USERINFO command to request
// all user records from the device. The device responds by pushing user data
// via POST /iclock/cdata with table=USERINFO, which is parsed into
// [UserRecord] values and dispatched via the [WithOnQueryUsers] callback.
// The device also sends a command confirmation by POSTing to /iclock/devicecmd
// with CMD=DATA and Return=0.
//
// To query a specific user by PIN, use [ADMSServer.QueueCommand] directly:
//
//	server.QueueCommand(serialNumber, "DATA QUERY USERINFO PIN=1")
//
// It returns the assigned command ID and an error if the command queue is full
// (see [WithMaxCommandsPerDevice]).
func (s *ADMSServer) SendQueryUsersCommand(serialNumber string) (int64, error) {
	return s.QueueCommand(serialNumber, "DATA QUERY USERINFO")
}

// SendLogCommand queues a LOG command to request log data from the device.
// The device confirms by POSTing to /iclock/devicecmd with CMD=LOG
// and Return=0.
//
// It returns the assigned command ID and an error if the command queue is full
// (see [WithMaxCommandsPerDevice]).
func (s *ADMSServer) SendLogCommand(serialNumber string) (int64, error) {
	return s.QueueCommand(serialNumber, "LOG")
}

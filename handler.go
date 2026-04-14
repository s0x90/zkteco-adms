package zkadms

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// readBody reads the request body with a size limit enforced by http.MaxBytesReader.
// It returns the body bytes or writes an HTTP error and returns a non-nil error.
// Oversized requests receive a 413 status; other read failures receive a 400.
func (s *ADMSServer) readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodySize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			s.logger.Warn("request body too large", "limit", s.maxBodySize)
			http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
			return nil, fmt.Errorf("readBody: %w", err)
		}
		s.logger.Warn("failed to read body", "error", err)
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return nil, fmt.Errorf("readBody: %w", err)
	}
	return body, nil
}

// requireMethod checks that the request method is one of the allowed methods.
// On failure it writes a 405 response and returns false.
func requireMethod(w http.ResponseWriter, r *http.Request, methods ...string) bool {
	if slices.Contains(methods, r.Method) {
		return true
	}
	http.Error(w, respMethodNotAllowed, http.StatusMethodNotAllowed)
	return false
}

// requireDevice validates the SN parameter, registers the device, and updates
// its activity timestamp. On failure it writes an HTTP error and returns ("", false).
func (s *ADMSServer) requireDevice(w http.ResponseWriter, r *http.Request) (string, bool) {
	sn, ok := s.requireSerialNumber(w, r)
	if !ok {
		return "", false
	}
	if !s.registerOrReject(w, sn) {
		return "", false
	}
	s.updateDeviceActivity(sn)
	return sn, true
}

// requireSerialNumber extracts and validates the SN query parameter.
// On failure it writes an HTTP error response and returns ("", false).
func (s *ADMSServer) requireSerialNumber(w http.ResponseWriter, r *http.Request) (string, bool) {
	sn := r.URL.Query().Get(paramSN)
	if sn == "" {
		http.Error(w, respMissingSN, http.StatusBadRequest)
		return "", false
	}
	if err := validateSerialNumber(sn); err != nil {
		s.logger.Warn("invalid serial number", "sn", sn, "error", err)
		http.Error(w, respInvalidSN, http.StatusBadRequest)
		return "", false
	}
	return sn, true
}

// registerOrReject calls RegisterDevice and writes an appropriate HTTP error
// if registration fails (e.g. device limit reached). Returns true on success.
func (s *ADMSServer) registerOrReject(w http.ResponseWriter, serialNumber string) bool {
	if err := s.RegisterDevice(serialNumber); err != nil {
		if errors.Is(err, ErrMaxDevicesReached) {
			s.logger.Warn("device limit reached, rejecting registration",
				"device", serialNumber, "limit", s.maxDevices)
			http.Error(w, respDeviceLimitMsg, http.StatusServiceUnavailable)
		} else {
			s.logger.Warn("device registration failed",
				"device", serialNumber, "error", err)
			http.Error(w, respInvalidSN, http.StatusBadRequest)
		}
		return false
	}
	return true
}

// writeCommandsOrOK drains pending commands for a device and writes them as
// "C:<id>:<cmd>\n" lines using the IDs assigned at queue time. The device uses
// the ID to confirm execution via /iclock/devicecmd.
// If no commands are pending, it writes "OK".
func (s *ADMSServer) writeCommandsOrOK(w http.ResponseWriter, serialNumber string) {
	commands := s.DrainCommands(serialNumber)
	w.WriteHeader(http.StatusOK)
	if len(commands) > 0 {
		for _, entry := range commands {
			fmt.Fprintf(w, cmdFormat, entry.ID, entry.Command)
		}
	} else {
		fmt.Fprint(w, respOK)
	}
}

// logRequest emits a debug log line with standard request metadata.
func (s *ADMSServer) logRequest(label string, r *http.Request, serialNumber string) {
	s.logger.Debug(label,
		"method", r.Method, "path", r.URL.Path, "device", serialNumber)
}

// logQueryParams logs the values of common iclock query parameters that are present.
func (s *ADMSServer) logQueryParams(label string, query url.Values, keys ...string) {
	for _, k := range keys {
		if v := query.Get(k); v != "" {
			s.logger.Debug(label, "key", k, "value", v)
		}
	}
}

// HandleCData handles the /iclock/cdata endpoint for attendance data.
func (s *ADMSServer) HandleCData(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet, http.MethodPost) {
		return
	}

	serialNumber, ok := s.requireDevice(w, r)
	if !ok {
		return
	}

	query := r.URL.Query()
	s.logRequest("cdata request", r, serialNumber)
	s.logQueryParams("cdata param", query, paramOptions, paramPushVer, paramPushOptionsFlag, paramTable)

	// Handle different table types
	table := query.Get(paramTable)

	switch table {
	case tableATTLOG:
		// Parse attendance log data
		body, err := s.readBody(w, r)
		if err != nil {
			return
		}

		// Log truncated body size/content
		if len(body) > 0 {
			s.logger.Debug("ATTLOG body", "preview", bodyPreview(body))
		}

		// Resolve the device's timezone for timestamp interpretation.
		s.devicesMutex.RLock()
		loc := s.deviceLocationLocked(serialNumber)
		s.devicesMutex.RUnlock()

		records := s.parseAttendanceRecords(string(body), serialNumber, loc)
		if !s.dispatchAttendance(records) {
			s.logger.Error("callback queue full, records not processed",
				"count", len(records), "device", serialNumber)
			http.Error(w,
				fmt.Sprintf("FAIL: callback queue full, %d records not processed", len(records)),
				http.StatusServiceUnavailable)
			return
		}

		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "OK: %d", len(records))

	case tableOPERLOG:
		// Operation log - acknowledge
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, respOK)

	case tableUSERINFO:
		// User records pushed in response to DATA QUERY USERINFO.
		body, err := s.readBody(w, r)
		if err != nil {
			return
		}

		if len(body) > 0 {
			users := s.parseUserRecords(string(body), serialNumber)
			s.logger.Debug("USERINFO records processed",
				"count", len(users), "device", serialNumber)
			if !s.dispatchQueryUsers(serialNumber, users) {
				s.logger.Error("callback queue full, user records dropped",
					"count", len(users), "device", serialNumber)
				http.Error(w, fmt.Sprintf("FAIL: callback queue full, %d user records not processed", len(users)),
					http.StatusServiceUnavailable)
				return
			}
		}

		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, respOK)

	default:
		// Handle INFO or other requests
		if r.Method == http.MethodPost {
			body, err := s.readBody(w, r)
			if err != nil {
				return
			}
			if len(body) > 0 {
				info := s.parseKVPairs(string(body), "\n", nil)
				if !s.dispatchDeviceInfo(serialNumber, info) {
					s.logger.Error("callback queue full, device info dropped",
						"device", serialNumber)
					http.Error(w, "FAIL: callback queue full, device info not processed",
						http.StatusServiceUnavailable)
					return
				}
				s.logger.Debug("INFO body", "preview", bodyPreview(body))
			}
		}

		// Check for pending commands
		s.writeCommandsOrOK(w, serialNumber)
	}
}

// HandleGetRequest handles the /iclock/getrequest endpoint.
func (s *ADMSServer) HandleGetRequest(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}

	serialNumber, ok := s.requireDevice(w, r)
	if !ok {
		return
	}

	s.logRequest("getrequest", r, serialNumber)

	s.writeCommandsOrOK(w, serialNumber)
}

// HandleDeviceCmd handles the /iclock/devicecmd endpoint.
//
// After the server sends "C:<ID>:<CMD>\n" via /iclock/getrequest, the device
// executes the command and POSTs the result here. The body typically contains
// key=value pairs separated by "&", for example:
//
//	ID=1&Return=0&CMD=USER ADD
//
// A Return value of 0 indicates success. The parsed result is dispatched to
// the callback registered via [WithOnCommandResult].
func (s *ADMSServer) HandleDeviceCmd(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}

	serialNumber, ok := s.requireDevice(w, r)
	if !ok {
		return
	}

	s.logRequest("devicecmd", r, serialNumber)

	body, err := s.readBody(w, r)
	if err != nil {
		return
	}

	if len(body) > 0 {
		s.logger.Debug("devicecmd body", "device", serialNumber, "preview", bodyPreview(body))
	}

	results := s.parseCommandResults(string(body), serialNumber)

	for _, result := range results {
		// Populate QueuedCommand from the pending commands map.
		// Deletion is deferred until dispatch succeeds so that a device
		// retry (after a 503) can still correlate the command.
		s.queueMutex.Lock()
		if entry, ok := s.pendingCommands[result.ID]; ok {
			result.QueuedCommand = entry.cmd
		}
		s.queueMutex.Unlock()

		s.logger.Info("command result",
			"device", serialNumber,
			"id", result.ID,
			"return", result.ReturnCode,
			"cmd", result.Command)
		s.logger.Debug("command result detail",
			"device", serialNumber,
			"id", result.ID,
			"queued_cmd", result.QueuedCommand)

		if !s.dispatchCommandResult(result) {
			s.logger.Error("callback queue full, command result dropped",
				"device", serialNumber, "id", result.ID)
			http.Error(w, fmt.Sprintf("FAIL: callback queue full, command result %d not processed", result.ID),
				http.StatusServiceUnavailable)
			return
		}

		// Dispatch succeeded — remove the pending entry.
		s.queueMutex.Lock()
		delete(s.pendingCommands, result.ID)
		s.queueMutex.Unlock()
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, respOK)
}

// ServeHTTP implements http.Handler interface for convenient routing.
func (s *ADMSServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, routeCData):
		s.HandleCData(w, r)
	case strings.HasSuffix(path, routeGetRequest):
		s.HandleGetRequest(w, r)
	case strings.HasSuffix(path, routeDeviceCmd):
		s.HandleDeviceCmd(w, r)
	case strings.HasSuffix(path, routeRegistry):
		s.HandleRegistry(w, r)
	case strings.HasSuffix(path, routeInspect):
		if !s.enableInspect {
			http.NotFound(w, r)
			return
		}
		s.HandleInspect(w, r)
	default:
		http.NotFound(w, r)
	}
}

// HandleRegistry processes /iclock/registry requests for device registration & capabilities.
func (s *ADMSServer) HandleRegistry(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet, http.MethodPost) {
		return
	}

	serialNumber, ok := s.requireDevice(w, r)
	if !ok {
		return
	}

	query := r.URL.Query()
	s.logRequest("registry request", r, serialNumber)
	s.logQueryParams("registry param", query, paramOptions, paramPushVer, paramPushOptionsFlag)

	body, err := s.readBody(w, r)
	if err != nil {
		return
	}
	if len(body) > 0 {
		s.logger.Debug("registry body", "preview", bodyPreview(body))
		info := s.parseKVPairs(string(body), ",", trimTildePrefix)
		s.devicesMutex.Lock()
		if dev := s.devices[serialNumber]; dev != nil {
			maps.Copy(dev.Options, info)
		}
		s.devicesMutex.Unlock()
		if !s.dispatchRegistry(serialNumber, info) {
			s.logger.Error("callback queue full, registry info dropped",
				"device", serialNumber)
			http.Error(w, "FAIL: callback queue full, registry info not processed",
				http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, respOK)
}

// HandleInspect serves /iclock/inspect returning JSON device snapshot.
func (s *ADMSServer) HandleInspect(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}

	snapshot := struct {
		Devices []DeviceSnapshot `json:"devices"`
		Count   int              `json:"count"`
		Time    time.Time        `json:"time"`
	}{Devices: []DeviceSnapshot{}, Time: time.Now()}

	s.devicesMutex.RLock()
	for _, d := range s.devices {
		dc := d.copy()
		snap := DeviceSnapshot{
			Serial:       dc.SerialNumber,
			LastActivity: dc.LastActivity.Format(time.RFC3339),
			Online:       s.isDeviceOnline(d),
			Options:      dc.Options,
			Timezone:     s.deviceLocationLocked(dc.SerialNumber).String(),
		}
		snapshot.Devices = append(snapshot.Devices, snap)
	}
	snapshot.Count = len(snapshot.Devices)
	s.devicesMutex.RUnlock()

	data, err := json.Marshal(snapshot)
	if err != nil {
		http.Error(w, "failed to encode response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err = w.Write(data); err != nil {
		s.logger.Warn("failed to write inspect response", "error", err)
		return
	}
	if _, err = w.Write([]byte("\n")); err != nil {
		s.logger.Warn("failed to write inspect trailing newline", "error", err)
	}
}

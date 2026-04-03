package zkadms

import (
	"time"
)

// RegisterDevice registers a new device or updates an existing one.
// It returns [ErrMaxDevicesReached] if the server's device limit (configured
// via [WithMaxDevices]) has been reached and the device is not already known.
// It returns [ErrInvalidSerialNumber] if the serial number is malformed.
//
// Use [DeviceOption] values to configure the device:
//
//	loc, _ := time.LoadLocation("Europe/Istanbul")
//	server.RegisterDevice("SN001", WithDeviceTimezone(loc))
func (s *ADMSServer) RegisterDevice(serialNumber string, opts ...DeviceOption) error {
	if err := validateSerialNumber(serialNumber); err != nil {
		return err
	}

	s.devicesMutex.Lock()
	defer s.devicesMutex.Unlock()

	dev, exists := s.devices[serialNumber]
	if !exists {
		if s.maxDevices > 0 && len(s.devices) >= s.maxDevices {
			return ErrMaxDevicesReached
		}
		dev = &Device{
			SerialNumber: serialNumber,
			LastActivity: time.Now(),
			Options:      make(map[string]string),
		}
		s.devices[serialNumber] = dev
	}
	for _, opt := range opts {
		opt(dev)
	}
	return nil
}

// GetDevice retrieves a copy of device information.
// Returns nil if the device is not registered.
func (s *ADMSServer) GetDevice(serialNumber string) *Device {
	s.devicesMutex.RLock()
	defer s.devicesMutex.RUnlock()
	d := s.devices[serialNumber]
	if d == nil {
		return nil
	}
	return d.copy()
}

// IsDeviceOnline reports whether the device has been active within the
// configured online threshold (default 2 minutes).
func (s *ADMSServer) IsDeviceOnline(serialNumber string) bool {
	s.devicesMutex.RLock()
	defer s.devicesMutex.RUnlock()
	d := s.devices[serialNumber]
	return s.isDeviceOnline(d)
}

// SetDeviceTimezone sets the timezone for a registered device. Attendance
// timestamps from this device will be interpreted in the given location.
// Pass nil to clear the device-specific timezone and fall back to the
// server default (see [WithDefaultTimezone]).
// Returns [ErrDeviceNotFound] if the device is not registered.
func (s *ADMSServer) SetDeviceTimezone(serialNumber string, loc *time.Location) error {
	s.devicesMutex.Lock()
	defer s.devicesMutex.Unlock()
	d := s.devices[serialNumber]
	if d == nil {
		return ErrDeviceNotFound
	}
	d.Timezone = loc
	return nil
}

// GetDeviceTimezone returns the effective timezone for a device.
// Resolution order: device-specific timezone, server default
// ([WithDefaultTimezone]), then [time.UTC].
// Returns nil if the device is not registered.
func (s *ADMSServer) GetDeviceTimezone(serialNumber string) *time.Location {
	s.devicesMutex.RLock()
	defer s.devicesMutex.RUnlock()
	if s.devices[serialNumber] == nil {
		return nil
	}
	return s.deviceLocationLocked(serialNumber)
}

// ListDevices returns copies of all registered devices.
// The returned slice and Device values are safe to use without additional locking.
func (s *ADMSServer) ListDevices() []*Device {
	s.devicesMutex.RLock()
	defer s.devicesMutex.RUnlock()

	devices := make([]*Device, 0, len(s.devices))
	for _, device := range s.devices {
		devices = append(devices, device.copy())
	}

	return devices
}

// updateDeviceActivity updates the last activity timestamp for a device.
func (s *ADMSServer) updateDeviceActivity(serialNumber string) {
	s.devicesMutex.Lock()
	defer s.devicesMutex.Unlock()

	if device, exists := s.devices[serialNumber]; exists {
		wasOnline := s.isDeviceOnline(device)
		device.LastActivity = time.Now()
		if !wasOnline {
			s.logger.Info("device online", "device", serialNumber)
		} else {
			s.logger.Debug("device activity", "device", serialNumber)
		}
	}
}

func (s *ADMSServer) isDeviceOnline(device *Device) bool {
	if device == nil {
		return false
	}
	return time.Since(device.LastActivity) <= s.onlineThreshold
}

// deviceLocationLocked returns the timezone to use when parsing attendance
// timestamps for the given device. Resolution order: device-specific timezone,
// server default (defaultTimezone), then time.UTC.
//
// The caller must hold devicesMutex (at least RLock).
func (s *ADMSServer) deviceLocationLocked(serialNumber string) *time.Location {
	if d := s.devices[serialNumber]; d != nil && d.Timezone != nil {
		return d.Timezone
	}
	if s.defaultTimezone != nil {
		return s.defaultTimezone
	}
	return time.UTC
}

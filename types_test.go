package zkadms

import (
	"errors"
	"strings"
	"testing"
	"time"
)

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

func TestSentinelErrors(t *testing.T) {
	// Verify sentinel errors exist and have expected messages.
	if ErrServerClosed.Error() != "zkadms: server closed" {
		t.Errorf("unexpected ErrServerClosed message: %s", ErrServerClosed.Error())
	}
	if ErrCallbackQueueFull.Error() != "zkadms: callback queue full" {
		t.Errorf("unexpected ErrCallbackQueueFull message: %s", ErrCallbackQueueFull.Error())
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

func TestDeviceCopy_IncludesTimezone(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	if err := server.RegisterDevice("DEV001", WithDeviceTimezone(tokyo)); err != nil {
		t.Fatalf("RegisterDevice failed: %v", err)
	}

	dev := server.GetDevice("DEV001")
	if dev.Timezone == nil || dev.Timezone.String() != "Asia/Tokyo" {
		t.Errorf("expected copied device to have Asia/Tokyo, got %v", dev.Timezone)
	}
}

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

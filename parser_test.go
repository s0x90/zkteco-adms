package zkadms

import (
	"strings"
	"testing"
	"time"
)

func TestParseKVPairs_Registry(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()
	body := "Key1=Val1, ~Key2=Val2,~Key3=Val3"
	info := server.parseKVPairs(body, ",", trimTildePrefix)
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
			records := server.parseAttendanceRecords(tc.data, "TEST001", time.UTC)
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

func TestParseAttendance_DeviceTimezone(t *testing.T) {
	istanbul, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		t.Fatal(err)
	}

	server := NewADMSServer()
	defer server.Close()

	data := "123\t2024-06-15 09:00:00\t0\t15\t0"
	records := server.parseAttendanceRecords(data, "DEV001", istanbul)
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}

	rec := records[0]
	// 09:00 Istanbul (UTC+3) = 06:00 UTC
	expectedUTC := time.Date(2024, 6, 15, 6, 0, 0, 0, time.UTC)
	if !rec.Timestamp.Equal(expectedUTC) {
		t.Errorf("expected timestamp %v, got %v", expectedUTC, rec.Timestamp.UTC())
	}
}

func TestParseAttendance_DefaultTimezone(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}

	server := NewADMSServer()
	defer server.Close()

	data := "123\t2024-06-15 09:00:00\t0\t15\t0"
	records := server.parseAttendanceRecords(data, "DEV001", berlin)
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}

	rec := records[0]
	// 09:00 Berlin (UTC+2 in summer CEST) = 07:00 UTC
	expectedUTC := time.Date(2024, 6, 15, 7, 0, 0, 0, time.UTC)
	if !rec.Timestamp.Equal(expectedUTC) {
		t.Errorf("expected timestamp %v, got %v", expectedUTC, rec.Timestamp.UTC())
	}
}

func TestParseAttendance_NilTimezone(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	data := "123\t2024-06-15 09:00:00\t0\t15\t0"
	records := server.parseAttendanceRecords(data, "DEV001", nil)
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}

	rec := records[0]

	//  09:00 UTC
	expectedUTC := time.Date(2024, 6, 15, 9, 0, 0, 0, time.UTC)
	if !rec.Timestamp.Equal(expectedUTC) {
		t.Errorf("expected timestamp %v, got %v", expectedUTC, rec.Timestamp.UTC())
	}

	berlinTZ, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	server.defaultTimezone = berlinTZ
	records = server.parseAttendanceRecords(data, "DEV001", nil)
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}

	rec = records[0]

	// 09:00 Berlin (UTC+2 in summer CEST) = 07:00 UTC
	expectedBerlin := time.Date(2024, 6, 15, 9, 0, 0, 0, berlinTZ)
	if !rec.Timestamp.Equal(expectedBerlin) {
		t.Errorf("expected timestamp %v, got %v", expectedBerlin, rec.Timestamp.UTC())
	}

	server.defaultTimezone = nil
	data = "123\t2024-06-15 09:00:00\t0\t15\t0"
	records = server.parseAttendanceRecords(data, "DEV001", nil)
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	// With both nil, should fall back to UTC.
	expectedUTC = time.Date(2024, 6, 15, 9, 0, 0, 0, time.UTC)
	if !records[0].Timestamp.Equal(expectedUTC) {
		t.Errorf("expected %v, got %v", expectedUTC, records[0].Timestamp)
	}
}

func TestParseAttendance_UTC_Fallback(t *testing.T) {
	server := NewADMSServer()
	defer server.Close()

	data := "123\t2024-06-15 09:00:00\t0\t15\t0"
	records := server.parseAttendanceRecords(data, "DEV001", time.UTC)
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}

	rec := records[0]
	expectedUTC := time.Date(2024, 6, 15, 9, 0, 0, 0, time.UTC)
	if !rec.Timestamp.Equal(expectedUTC) {
		t.Errorf("expected timestamp %v, got %v", expectedUTC, rec.Timestamp.UTC())
	}
}

func TestParseAttendance_UnixEpoch_IgnoresLocation(t *testing.T) {
	// Unix epoch timestamps are inherently UTC — location should not affect them.
	istanbul, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		t.Fatal(err)
	}

	server := NewADMSServer()
	defer server.Close()

	// 1718442000 = 2024-06-15 09:00:00 UTC
	data := "123\t1718442000\t0\t15\t0"
	records := server.parseAttendanceRecords(data, "DEV001", istanbul)
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}

	rec := records[0]
	expectedUTC := time.Date(2024, 6, 15, 9, 0, 0, 0, time.UTC)
	if !rec.Timestamp.Equal(expectedUTC) {
		t.Errorf("expected timestamp %v (unchanged by location), got %v", expectedUTC, rec.Timestamp.UTC())
	}
}

func TestParseAttendance_WinterSummerTime(t *testing.T) {
	// Verify DST transitions are handled correctly.
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}

	server := NewADMSServer()
	defer server.Close()

	// Winter time (CET = UTC+1): 2024-01-15 09:00:00 → 08:00 UTC
	winter := "123\t2024-01-15 09:00:00\t0\t15\t0"
	winterRecords := server.parseAttendanceRecords(winter, "DEV001", berlin)
	if len(winterRecords) != 1 {
		t.Fatalf("expected 1 record, got %d", len(winterRecords))
	}
	winterExpected := time.Date(2024, 1, 15, 8, 0, 0, 0, time.UTC)
	if !winterRecords[0].Timestamp.Equal(winterExpected) {
		t.Errorf("winter: expected %v, got %v", winterExpected, winterRecords[0].Timestamp.UTC())
	}

	// Summer time (CEST = UTC+2): 2024-07-15 09:00:00 → 07:00 UTC
	summer := "123\t2024-07-15 09:00:00\t0\t15\t0"
	summerRecords := server.parseAttendanceRecords(summer, "DEV001", berlin)
	if len(summerRecords) != 1 {
		t.Fatalf("expected 1 record, got %d", len(summerRecords))
	}
	summerExpected := time.Date(2024, 7, 15, 7, 0, 0, 0, time.UTC)
	if !summerRecords[0].Timestamp.Equal(summerExpected) {
		t.Errorf("summer: expected %v, got %v", summerExpected, summerRecords[0].Timestamp.UTC())
	}
}

func BenchmarkParseAttendanceRecords(b *testing.B) {
	server := NewADMSServer()
	defer server.Close()
	data := "123\t2024-01-01 08:00:00\t0\t1\t0\n456\t2024-01-01 17:00:00\t1\t1\t0\n789\t2024-01-01 12:00:00\t2\t1\t0"

	for b.Loop() {
		server.parseAttendanceRecords(data, "TEST001", time.UTC)
	}
}

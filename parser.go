package zkadms

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// bodyPreview returns a truncated preview of body for logging (max maxBodyPreviewLen bytes).
func bodyPreview(body []byte) string {
	if len(body) > maxBodyPreviewLen {
		return string(body[:maxBodyPreviewLen]) + "..."
	}
	return string(body)
}

// validateSerialNumber checks that a serial number is non-empty and matches
// the expected format (1–64 alphanumeric characters, hyphens, or underscores).
func validateSerialNumber(sn string) error {
	if sn == "" {
		return fmt.Errorf("%w: empty serial number", ErrInvalidSerialNumber)
	}
	if len(sn) > maxSerialNumberLength {
		return fmt.Errorf("%w: exceeds %d characters", ErrInvalidSerialNumber, maxSerialNumberLength)
	}
	if !serialNumberRe.MatchString(sn) {
		return fmt.Errorf("%w: contains invalid characters", ErrInvalidSerialNumber)
	}
	return nil
}

// parseAttendanceRecords parses attendance records from the device ATTLOG data.
// Each line must have at least a UserID and a parseable timestamp (either
// "2006-01-02 15:04:05" format or a Unix epoch integer). Malformed lines are
// skipped and logged so downstream systems never receive zero-value timestamps.
//
// The loc parameter specifies the timezone in which device-local timestamps
// (the "2006-01-02 15:04:05" format) are interpreted. Unix epoch timestamps
// are inherently UTC and are not affected by loc.
func (s *ADMSServer) parseAttendanceRecords(data string, serialNumber string, loc *time.Location) []AttendanceRecord {
	var records []AttendanceRecord
	var skipped int

	if loc == nil {
		if s.defaultTimezone != nil {
			loc = s.defaultTimezone
		} else {
			loc = time.UTC
		}
	}

	for line := range strings.SplitSeq(strings.TrimSpace(data), "\n") {
		line = strings.TrimRight(line, "\r") // handle \r\n line endings
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < attMinFields {
			skipped++
			s.logger.Warn("skipping malformed ATTLOG line",
				"device", serialNumber, "fields", len(parts), "line", line)
			continue
		}

		userID := strings.TrimSpace(parts[attFieldUserID])
		if userID == "" {
			skipped++
			s.logger.Warn("skipping ATTLOG line with empty UserID",
				"device", serialNumber, "line", line)
			continue
		}

		var ts time.Time
		if parsed, err := time.ParseInLocation(timestampFormat, parts[attFieldTimestamp], loc); err == nil {
			ts = parsed
		} else if epoch, err := strconv.ParseInt(parts[attFieldTimestamp], 10, 64); err == nil {
			ts = time.Unix(epoch, 0)
		} else {
			skipped++
			s.logger.Warn("skipping ATTLOG line with unparseable timestamp",
				"device", serialNumber, "timestamp", parts[attFieldTimestamp], "line", line)
			continue
		}

		record := AttendanceRecord{
			SerialNumber: serialNumber,
			UserID:       userID,
			Timestamp:    ts,
		}
		if len(parts) > attFieldStatus {
			if v, err := strconv.Atoi(parts[attFieldStatus]); err == nil {
				record.Status = v
			} else {
				s.logger.Warn("non-integer Status field, defaulting to 0",
					"device", serialNumber, "value", parts[attFieldStatus])
			}
		}
		if len(parts) > attFieldVerifyMode {
			if v, err := strconv.Atoi(parts[attFieldVerifyMode]); err == nil {
				record.VerifyMode = v
			} else {
				s.logger.Warn("non-integer VerifyMode field, defaulting to 0",
					"device", serialNumber, "value", parts[attFieldVerifyMode])
			}
		}
		if len(parts) > attFieldWorkCode {
			record.WorkCode = parts[attFieldWorkCode]
		}
		records = append(records, record)
	}
	if skipped > 0 {
		s.logger.Warn("skipped malformed ATTLOG lines",
			"device", serialNumber, "skipped", skipped, "total", len(records)+skipped)
	}
	return records
}

// trimTildePrefix removes a leading "~" from s.
func trimTildePrefix(s string) string {
	return strings.TrimPrefix(s, "~")
}

// parseKVPairs parses key=value pairs separated by sep. Each pair is split on
// "=". If transformKey is non-nil it is applied to each key before insertion.
// This generalises the two device parsers:
//   - Device info: sep="\n", transformKey=nil
//   - Registry:    sep=",",  transformKey=trimTildePrefix
func (s *ADMSServer) parseKVPairs(data, sep string, transformKey func(string) string) map[string]string {
	info := make(map[string]string)
	for part := range strings.SplitSeq(strings.TrimSpace(data), sep) {
		part = strings.TrimSpace(part)
		if key, value, ok := strings.Cut(part, "="); ok {
			k := strings.TrimSpace(key)
			if transformKey != nil {
				k = transformKey(k)
			}
			info[k] = strings.TrimSpace(value)
		}
	}
	return info
}

// parseUserRecords parses tab-separated USERINFO lines pushed by a device in
// response to a DATA QUERY USERINFO command. Each line has the form:
//
//	PIN=1\tName=John\tPrivilege=0\tCard=\tPassword=
//
// Lines that lack a PIN field are skipped with a warning.
func (s *ADMSServer) parseUserRecords(data, serialNumber string) []UserRecord {
	var records []UserRecord
	var skipped int
	for line := range strings.SplitSeq(strings.TrimSpace(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		fields := make(map[string]string)
		for part := range strings.SplitSeq(line, "\t") {
			if key, value, ok := strings.Cut(part, "="); ok {
				fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
			}
		}
		pin := fields["PIN"]
		if pin == "" {
			skipped++
			s.logger.Warn("skipping USERINFO line without PIN",
				"device", serialNumber, "line_len", len(line))
			continue
		}
		privilege, _ := strconv.Atoi(fields["Privilege"])
		records = append(records, UserRecord{
			PIN:       pin,
			Name:      fields["Name"],
			Privilege: privilege,
			Card:      fields["Card"],
			Password:  fields["Password"],
		})
	}
	if skipped > 0 {
		s.logger.Warn("skipped malformed USERINFO lines",
			"device", serialNumber, "skipped", skipped, "total", len(records)+skipped)
	}
	return records
}

// parseCommandResults parses a devicecmd confirmation body into one or more
// [CommandResult] values. The device uses two different formats:
//
// Batched format (ampersand-separated KV pairs, one result per line):
//
//	ID=1&Return=0&CMD=INFO\nID=2&Return=0&CMD=CHECK\n
//
// Shell/multiline format (newline-separated KV pairs, single result):
//
//	ID=32\nReturn=0\nCMD=Shell\nContent=output\n
//
// The parser accumulates key=value pairs into the current result. When a new
// ID= is encountered, it flushes the previous result and starts a new one.
// This handles both formats transparently.
func (s *ADMSServer) parseCommandResults(body, serialNumber string) []CommandResult {
	var results []CommandResult
	current := CommandResult{SerialNumber: serialNumber}
	hasID := false

	flush := func() {
		if hasID {
			results = append(results, current)
		}
		current = CommandResult{SerialNumber: serialNumber}
		hasID = false
	}

	// Normalize: treat both \n and & as delimiters between KV pairs.
	body = strings.ReplaceAll(body, "\n", "&")
	for part := range strings.SplitSeq(body, "&") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		switch strings.ToUpper(strings.TrimSpace(key)) {
		case "ID":
			id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil {
				s.logger.Warn("devicecmd: unparseable ID", "device", serialNumber, "value", value)
				continue
			}
			// New ID means new result — flush the previous one.
			if hasID {
				flush()
			}
			current.ID = id
			hasID = true
		case "RETURN":
			if code, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
				current.ReturnCode = code
			} else {
				s.logger.Warn("devicecmd: unparseable Return", "device", serialNumber, "value", value)
			}
		case "CMD":
			current.Command = strings.TrimSpace(value)
		}
	}
	flush() // flush the last result
	return results
}

// ParseQueryParams parses URL query parameters commonly used in iclock protocol.
func ParseQueryParams(urlStr string) (map[string]string, error) {
	u, err := url.Parse(urlStr)
	if err != nil {
		return nil, fmt.Errorf("parse URL: %w", err)
	}

	params := make(map[string]string)
	for key, values := range u.Query() {
		if len(values) > 0 {
			params[key] = values[0]
		}
	}

	return params, nil
}

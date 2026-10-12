package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	zkadms "github.com/s0x90/zkteco-adms"
)

// fixedNow makes the handlers' clock return 2026-03-20 02:00:00 UTC.
func fixedNow(t *testing.T) {
	t.Helper()
	old := now
	now = func() time.Time { return time.Date(2026, 3, 20, 2, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { now = old })
}

func putTimezone(mux http.Handler, sn, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, "/api/devices/"+sn+"/timezone", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func syncTimeCommand(t *testing.T, mux http.Handler, sn string) string {
	t.Helper()
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/devices/"+sn+"/sync-time", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("sync-time: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp commandResponse
	decodeResponse(t, w, &resp)
	return resp.Command
}

// ---------- sync-time uses the device's timezone ----------

func TestSyncTime_UsesDeviceTimezone(t *testing.T) {
	fixedNow(t)
	wib, _ := time.LoadLocation("Asia/Jakarta")
	wita, _ := time.LoadLocation("Asia/Makassar")

	server := newTestServer(t)
	if err := server.RegisterDevice("JKT", zkadms.WithDeviceTimezone(wib)); err != nil {
		t.Fatal(err)
	}
	if err := server.RegisterDevice("BALI", zkadms.WithDeviceTimezone(wita)); err != nil {
		t.Fatal(err)
	}
	mux := newMux(server)

	// 02:00 UTC is 09:00 in UTC+7 and 10:00 in UTC+8: each device gets its own.
	if got, want := syncTimeCommand(t, mux, "JKT"), "SET OPTION ServerLocalTime=2026-03-20 09:00:00"; got != want {
		t.Errorf("Jakarta: got %q, want %q", got, want)
	}
	if got, want := syncTimeCommand(t, mux, "BALI"), "SET OPTION ServerLocalTime=2026-03-20 10:00:00"; got != want {
		t.Errorf("Makassar: got %q, want %q", got, want)
	}
}

func TestSyncTime_FallsBackToServerDefaultTimezone(t *testing.T) {
	fixedNow(t)
	wib, _ := time.LoadLocation("Asia/Jakarta")

	server := zkadms.NewADMSServer(zkadms.WithDefaultTimezone(wib))
	t.Cleanup(server.Close)
	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatal(err)
	}

	got := syncTimeCommand(t, newMux(server), "DEV001")
	if want := "SET OPTION ServerLocalTime=2026-03-20 09:00:00"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// ---------- PUT /api/devices/{sn}/timezone ----------

func TestSetTimezone_Success(t *testing.T) {
	mux := newTestMux(t, "DEV001")

	w := putTimezone(mux, "DEV001", `{"timezone":"Asia/Jakarta"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp timezoneResponse
	decodeResponse(t, w, &resp)
	if resp.Device != "DEV001" || resp.Timezone != "Asia/Jakarta" || resp.Source != "device" {
		t.Errorf("unexpected response: %+v", resp)
	}

	// The device list reports the new zone.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/devices/DEV001", nil))
	var entry deviceEntry
	decodeResponse(t, rec, &entry)
	if entry.Timezone != "Asia/Jakarta" {
		t.Errorf("expected device detail to show Asia/Jakarta, got %q", entry.Timezone)
	}
}

func TestSetTimezone_EmptyResetsToServerDefault(t *testing.T) {
	mux := newTestMux(t, "DEV001")
	if w := putTimezone(mux, "DEV001", `{"timezone":"Asia/Jakarta"}`); w.Code != http.StatusOK {
		t.Fatalf("setup failed: %d", w.Code)
	}

	w := putTimezone(mux, "DEV001", `{"timezone":""}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp timezoneResponse
	decodeResponse(t, w, &resp)
	if resp.Source != "default" || resp.Timezone != "UTC" {
		t.Errorf("expected reset to the UTC default, got %+v", resp)
	}
}

func TestSetTimezone_Rejects(t *testing.T) {
	mux := newTestMux(t, "DEV001")

	for name, body := range map[string]string{
		"unknown zone":     `{"timezone":"Mars/Olympus"}`,
		"offset notation":  `{"timezone":"UTC+7"}`,
		"Local is unclear": `{"timezone":"Local"}`,
		"malformed JSON":   `{"timezone":`,
	} {
		w := putTimezone(mux, "DEV001", body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d: %s", name, w.Code, w.Body.String())
		}
	}

	// A rejected request must not change the device's zone.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/devices/DEV001", nil))
	var entry deviceEntry
	decodeResponse(t, rec, &entry)
	if entry.Timezone != "UTC" {
		t.Errorf("zone changed by a rejected request: %q", entry.Timezone)
	}
}

func TestSetTimezone_ErrorMessageHelpsUser(t *testing.T) {
	mux := newTestMux(t, "DEV001")

	w := putTimezone(mux, "DEV001", `{"timezone":"UTC+7"}`)
	var resp apiError
	decodeResponse(t, w, &resp)
	if !strings.Contains(resp.Error, "Asia/Jakarta") {
		t.Errorf("error should suggest an IANA name, got %q", resp.Error)
	}
}

func TestSetTimezone_DeviceNotFound(t *testing.T) {
	mux := newTestMux(t)

	w := putTimezone(mux, "NOSUCH", `{"timezone":"Asia/Jakarta"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestSetTimezone_RejectsOtherMethods(t *testing.T) {
	mux := newTestMux(t, "DEV001")

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/devices/DEV001/timezone", strings.NewReader(`{}`)))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for POST, got %d", w.Code)
	}
}

func TestSetTimezone_AffectsSyncTime(t *testing.T) {
	fixedNow(t)
	mux := newTestMux(t, "DEV001")

	if got := syncTimeCommand(t, mux, "DEV001"); !strings.HasSuffix(got, "2026-03-20 02:00:00") {
		t.Fatalf("before: expected UTC time, got %q", got)
	}
	putTimezone(mux, "DEV001", `{"timezone":"Asia/Jakarta"}`)
	if got := syncTimeCommand(t, mux, "DEV001"); !strings.HasSuffix(got, "2026-03-20 09:00:00") {
		t.Errorf("after: expected Jakarta time, got %q", got)
	}
}

// ---------- parsing ----------

func TestParseDeviceSpec(t *testing.T) {
	tests := []struct {
		spec, sn, zone string
		wantErr        bool
	}{
		{spec: "ABC123", sn: "ABC123"},
		{spec: "  ABC123  ", sn: "ABC123"},
		{spec: "ABC123=Asia/Jakarta", sn: "ABC123", zone: "Asia/Jakarta"},
		{spec: " ABC123 = Asia/Makassar ", sn: "ABC123", zone: "Asia/Makassar"},
		{spec: "", sn: ""},
		{spec: "ABC123=", wantErr: true},
		{spec: "ABC123=Nowhere/Land", wantErr: true},
		{spec: "ABC123=Local", wantErr: true},
	}
	for _, tt := range tests {
		sn, loc, err := parseDeviceSpec(tt.spec)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseDeviceSpec(%q): expected an error", tt.spec)
			} else if !strings.Contains(err.Error(), "ABC123") {
				t.Errorf("parseDeviceSpec(%q): error should name the device, got %v", tt.spec, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseDeviceSpec(%q): unexpected error %v", tt.spec, err)
			continue
		}
		if sn != tt.sn {
			t.Errorf("parseDeviceSpec(%q): sn = %q, want %q", tt.spec, sn, tt.sn)
		}
		switch {
		case tt.zone == "" && loc != nil:
			t.Errorf("parseDeviceSpec(%q): expected no zone, got %v", tt.spec, loc)
		case tt.zone != "" && (loc == nil || loc.String() != tt.zone):
			t.Errorf("parseDeviceSpec(%q): zone = %v, want %s", tt.spec, loc, tt.zone)
		}
	}
}

func TestLoadTimezone(t *testing.T) {
	for _, ok := range []string{"UTC", "Asia/Jakarta", "Europe/London"} {
		if _, err := loadTimezone(ok); err != nil {
			t.Errorf("loadTimezone(%q): unexpected error %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Local", "WIB", "UTC+7", "+07:00"} {
		if _, err := loadTimezone(bad); err == nil {
			t.Errorf("loadTimezone(%q): expected an error", bad)
		}
	}
}

// ---------- run() ----------

func TestRun_InvalidDeviceTimezone(t *testing.T) {
	err := run(t.Context(), ":0", []string{"DEV001=Not/AZone"})
	if err == nil || !strings.Contains(err.Error(), "DEV001") {
		t.Fatalf("expected an error naming DEV001, got %v", err)
	}
}

// TestRun_PerDeviceAndDefaultTimezones starts a real server and checks that
// -devices SN=Zone and the server default decide how attendance times are read.
func TestRun_PerDeviceAndDefaultTimezones(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	wita, _ := time.LoadLocation("Asia/Makassar")
	ctx, cancel := contextWithCancel(t)
	errCh := make(chan error, 1)
	go func() {
		errCh <- run(ctx, addr, []string{"JKT=Asia/Jakarta", "PLAIN"}, zkadms.WithDefaultTimezone(wita))
	}()
	base := "http://" + addr
	waitReady(t, base+"/ui/state")

	// The same wall-clock time from two devices in two situations.
	post := func(sn string) {
		resp, err := http.Post(base+"/iclock/cdata?SN="+sn+"&table=ATTLOG", "text/plain",
			strings.NewReader("1001\t2026-03-20 09:00:00\t0\t1\t0"))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	post("JKT")   // its own zone: UTC+7
	post("PLAIN") // server default: UTC+8

	wantOffset := map[string]string{"JKT": "+07:00", "PLAIN": "+08:00"}
	found := map[string]bool{}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(found) < len(wantOffset) {
		resp, err := http.Get(base + "/ui/state")
		if err != nil {
			t.Fatal(err)
		}
		var st stateResponse
		err = json.NewDecoder(resp.Body).Decode(&st)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range st.Events {
			if off, ok := wantOffset[e.Device]; ok && e.Kind == "attendance" {
				found[e.Device] = true
				if !strings.Contains(e.Message, "2026-03-20T09:00:00"+off) {
					t.Errorf("%s: expected 09:00 at %s, got %q", e.Device, off, e.Message)
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(found) != len(wantOffset) {
		t.Errorf("did not see attendance events for every device: %v", found)
	}

	// The dashboard data reports the effective zones.
	resp, err := http.Get(base + "/api/devices")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list devicesResponse
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	zones := map[string]string{}
	for _, d := range list.Devices {
		zones[d.SerialNumber] = d.Timezone
	}
	if zones["JKT"] != "Asia/Jakarta" || zones["PLAIN"] != "Asia/Makassar" {
		t.Errorf("unexpected zones: %v", zones)
	}

	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("run returned %v", err)
	}
}

// ---------- dashboard page ----------

func TestDashboard_PageHasTimezoneControls(t *testing.T) {
	mux, _ := newDashboardMux(t)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	body := w.Body.String()
	for _, want := range []string{"Set on server", "Apply to device", "Device timezone", `id="zones"`, "Asia/Jakarta"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard page is missing %q", want)
		}
	}
}

// ---------- apply timezone to the device ----------

func postApplyTimezone(mux http.Handler, sn string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/devices/"+sn+"/timezone/apply", nil))
	return w
}

func drainedCommands(server *zkadms.ADMSServer, sn string) []string {
	var cmds []string
	for _, e := range server.DrainCommands(sn) {
		cmds = append(cmds, e.Command)
	}
	return cmds
}

func TestApplyTimezone_QueuesSetThenReadBack(t *testing.T) {
	fixedNow(t)
	wib, _ := time.LoadLocation("Asia/Jakarta")
	server := newTestServer(t)
	if err := server.RegisterDevice("DEV001", zkadms.WithDeviceTimezone(wib)); err != nil {
		t.Fatal(err)
	}

	w := postApplyTimezone(newMux(server), "DEV001")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp applyTimezoneResponse
	decodeResponse(t, w, &resp)
	if resp.OffsetHours != 7 || resp.Timezone != "Asia/Jakarta" || resp.Status != "queued" {
		t.Errorf("unexpected response: %+v", resp)
	}

	got := drainedCommands(server, "DEV001")
	want := []string{"SET OPTION TimeZone=7", "GET OPTION FROM TimeZone"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("queued %q, want %q (set first, then read back)", got, want)
	}
}

func TestApplyTimezone_OffsetSigns(t *testing.T) {
	fixedNow(t)
	for zone, want := range map[string]string{
		"UTC":               "SET OPTION TimeZone=0",
		"America/Sao_Paulo": "SET OPTION TimeZone=-3",
		"Asia/Jayapura":     "SET OPTION TimeZone=9",
	} {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Fatal(err)
		}
		server := newTestServer(t)
		if err := server.RegisterDevice("DEV001", zkadms.WithDeviceTimezone(loc)); err != nil {
			t.Fatal(err)
		}
		if w := postApplyTimezone(newMux(server), "DEV001"); w.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d", zone, w.Code)
		}
		if got := drainedCommands(server, "DEV001"); len(got) == 0 || got[0] != want {
			t.Errorf("%s: queued %q, want first command %q", zone, got, want)
		}
	}
}

func TestApplyTimezone_RefusesFractionalHourOffsets(t *testing.T) {
	fixedNow(t)
	ist, _ := time.LoadLocation("Asia/Kolkata") // UTC+5:30
	server := newTestServer(t)
	if err := server.RegisterDevice("DEV001", zkadms.WithDeviceTimezone(ist)); err != nil {
		t.Fatal(err)
	}

	w := postApplyTimezone(newMux(server), "DEV001")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if got := drainedCommands(server, "DEV001"); len(got) != 0 {
		t.Errorf("nothing should be queued for an unsupported offset, got %q", got)
	}
}

func TestApplyTimezone_DeviceNotFound(t *testing.T) {
	w := postApplyTimezone(newTestMux(t), "NOSUCH")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestApplyTimezone_QueueFull(t *testing.T) {
	server := zkadms.NewADMSServer(zkadms.WithMaxCommandsPerDevice(1))
	t.Cleanup(server.Close)
	if err := server.RegisterDevice("DEV001"); err != nil {
		t.Fatal(err)
	}
	// One command fits, so the read-back that follows the set is rejected.
	if w := postApplyTimezone(newMux(server), "DEV001"); w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when the queue fills, got %d", w.Code)
	}
}

// ---------- device reply capture ----------

func TestCaptureDeviceReplies_RecordsReplyAndPassesBodyThrough(t *testing.T) {
	events := newEventLog(10)
	var seen string
	h := captureDeviceReplies(events, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = string(b)
	}))

	body := "ID=5\nReturn=0\nCMD=Shell\nContent=Thu Mar 20 09:00:00 WIB 2026\n"
	req := httptest.NewRequest(http.MethodPost, "/iclock/devicecmd?SN=DEV001", strings.NewReader(body))
	h.ServeHTTP(httptest.NewRecorder(), req)

	if seen != body {
		t.Errorf("handler must receive the whole body unchanged, got %q", seen)
	}
	got := events.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	if got[0].Kind != "device-reply" || got[0].Device != "DEV001" {
		t.Errorf("unexpected event: %+v", got[0])
	}
	if !strings.Contains(got[0].Message, "Content=Thu Mar 20 09:00:00 WIB 2026") ||
		strings.Contains(got[0].Message, "\n") {
		t.Errorf("expected the device's output on one line, got %q", got[0].Message)
	}
}

func TestCaptureDeviceReplies_TruncatesButStillPassesFullBody(t *testing.T) {
	events := newEventLog(10)
	var seenLen int
	h := captureDeviceReplies(events, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seenLen = len(b)
	}))

	big := bytes.Repeat([]byte("x"), maxReplyBody*3)
	req := httptest.NewRequest(http.MethodPost, "/iclock/devicecmd?SN=DEV001", bytes.NewReader(big))
	h.ServeHTTP(httptest.NewRecorder(), req)

	if seenLen != len(big) {
		t.Errorf("handler saw %d bytes, want the full %d", seenLen, len(big))
	}
	if msg := events.snapshot()[0].Message; !strings.HasSuffix(msg, "…") {
		t.Errorf("expected a truncated reply to end with an ellipsis")
	}
}

func TestCaptureDeviceReplies_IgnoresOtherRequests(t *testing.T) {
	events := newEventLog(10)
	h := captureDeviceReplies(events, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/iclock/cdata?SN=DEV001", strings.NewReader("x")),
		httptest.NewRequest(http.MethodGet, "/iclock/devicecmd?SN=DEV001", nil),
		httptest.NewRequest(http.MethodPost, "/iclock/devicecmd?SN=DEV001", strings.NewReader("")),
	} {
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	if n := len(events.snapshot()); n != 0 {
		t.Errorf("expected no events, got %d", n)
	}
}

func TestDescribeResult(t *testing.T) {
	for code, want := range map[int]string{
		0:     "OK",
		-1:    "not supported",
		-2:    "file operation",
		-1002: "invalid command syntax",
		-1004: "not supported on this model",
		-77:   "code -77",
	} {
		if got := describeResult(code); !strings.Contains(got, want) {
			t.Errorf("describeResult(%d) = %q, want it to contain %q", code, got, want)
		}
	}
	if strings.Contains(describeResult(0), "FAILED") {
		t.Error("success must not be reported as a failure")
	}
}

// TestRun_FailedCommandAndDeviceReplyReachDashboard drives a real run() with a
// device that rejects a command, and checks the dashboard feed shows both the
// explained failure and the device's own words.
func TestRun_FailedCommandAndDeviceReplyReachDashboard(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	ctx, cancel := contextWithCancel(t)
	errCh := make(chan error, 1)
	go func() { errCh <- run(ctx, addr, []string{"DEV001"}) }()
	base := "http://" + addr
	waitReady(t, base+"/ui/state")

	// Queue a command, let the device fetch it, then reject it with -1002.
	resp, err := http.Post(base+"/api/devices/DEV001/command", "application/json",
		strings.NewReader(`{"command":"SET OPTION TimeZone=7"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	resp, err = http.Get(base + "/iclock/getrequest?SN=DEV001")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	resp, err = http.Post(base+"/iclock/devicecmd?SN=DEV001", "text/plain",
		strings.NewReader("ID=1&Return=-1002&CMD=SET OPTION"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	var sawFailure, sawReply bool
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && (!sawFailure || !sawReply) {
		resp, err := http.Get(base + "/ui/state")
		if err != nil {
			t.Fatal(err)
		}
		var st stateResponse
		err = json.NewDecoder(resp.Body).Decode(&st)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range st.Events {
			switch e.Kind {
			case "command-result":
				sawFailure = e.Level == "error" &&
					strings.Contains(e.Message, "SET OPTION TimeZone=7") &&
					strings.Contains(e.Message, "invalid command syntax")
			case "device-reply":
				sawReply = strings.Contains(e.Message, "Return=-1002")
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !sawFailure {
		t.Error("no explained failure event for the rejected command")
	}
	if !sawReply {
		t.Error("the device's raw reply did not reach the dashboard feed")
	}

	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("run returned %v", err)
	}
}

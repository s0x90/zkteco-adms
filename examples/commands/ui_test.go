package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	zkadms "github.com/s0x90/zkteco-adms"
)

func newDashboardMux(t *testing.T, devices ...string) (*http.ServeMux, *eventLog) {
	t.Helper()
	server := newTestServer(t, devices...)
	events := newEventLog(maxEvents)
	mux := newMux(server)
	mountDashboard(mux, server, events)
	return mux, events
}

func getState(t *testing.T, mux http.Handler) stateResponse {
	t.Helper()
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ui/state", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from /ui/state, got %d", w.Code)
	}
	var st stateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("invalid JSON from /ui/state: %v", err)
	}
	return st
}

// ---------- eventLog ----------

func TestEventLog_NewestFirstWithIncreasingIDs(t *testing.T) {
	l := newEventLog(10)
	l.add("a", "D1", "first")
	l.add("b", "D2", "second")

	got := l.snapshot()
	if len(got) != 2 {
		t.Fatalf("expected 2 events, got %d", len(got))
	}
	if got[0].Message != "second" || got[1].Message != "first" {
		t.Errorf("expected newest first, got %+v", got)
	}
	if got[0].ID <= got[1].ID {
		t.Errorf("expected increasing IDs, got %d then %d", got[1].ID, got[0].ID)
	}
	if _, err := time.Parse(time.RFC3339, got[0].Time); err != nil {
		t.Errorf("event time %q is not RFC3339: %v", got[0].Time, err)
	}
}

func TestEventLog_DropsOldestWhenFull(t *testing.T) {
	l := newEventLog(3)
	for _, m := range []string{"1", "2", "3", "4", "5"} {
		l.add("k", "D", m)
	}

	got := l.snapshot()
	if len(got) != 3 {
		t.Fatalf("expected 3 events kept, got %d", len(got))
	}
	if got[0].Message != "5" || got[2].Message != "3" {
		t.Errorf("expected events 5,4,3 kept, got %+v", got)
	}
}

func TestEventLog_TruncatesLongMessages(t *testing.T) {
	l := newEventLog(1)
	l.add("k", "D", strings.Repeat("é", maxEventMessage+50))

	msg := l.snapshot()[0].Message
	if r := []rune(msg); len(r) != maxEventMessage+1 || r[len(r)-1] != '…' {
		t.Errorf("expected %d runes plus an ellipsis, got %d runes", maxEventMessage, len(r))
	}
}

func TestEventLog_SnapshotIsACopy(t *testing.T) {
	l := newEventLog(5)
	l.add("k", "D", "original")
	l.snapshot()[0].Message = "changed"

	if got := l.snapshot()[0].Message; got != "original" {
		t.Errorf("snapshot must not alias internal state, got %q", got)
	}
}

func TestEventLog_ConcurrentAdds(t *testing.T) {
	l := newEventLog(1000)
	done := make(chan struct{})
	for range 10 {
		go func() {
			defer func() { done <- struct{}{} }()
			for range 50 {
				l.add("k", "D", "m")
				l.snapshot()
			}
		}()
	}
	for range 10 {
		<-done
	}
	if n := len(l.snapshot()); n != 500 {
		t.Errorf("expected 500 events, got %d", n)
	}
}

// ---------- dashboard handlers ----------

func TestDashboard_ServesIndexWithSecurityHeaders(t *testing.T) {
	mux, _ := newDashboardMux(t)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("expected text/html, got %q", ct)
	}
	if !strings.Contains(w.Body.String(), "ADMS Device Monitor") {
		t.Error("expected the dashboard page in the body")
	}
	csp := w.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "connect-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q is missing %q", csp, want)
		}
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("expected X-Content-Type-Options: nosniff")
	}
}

func TestDashboard_DoesNotSwallowOtherRoutes(t *testing.T) {
	mux, _ := newDashboardMux(t)

	for path, want := range map[string]int{
		"/nonexistent": http.StatusNotFound,
		"/ui":          http.StatusNotFound,
	} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != want {
			t.Errorf("GET %s: expected %d, got %d", path, want, w.Code)
		}
	}

	// The existing API and ADMS routes still work alongside the dashboard.
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/devices", nil))
	if w.Code != http.StatusOK {
		t.Errorf("GET /api/devices: expected 200, got %d", w.Code)
	}
}

func TestDashboard_IndexRejectsNonGet(t *testing.T) {
	mux, _ := newDashboardMux(t)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for POST /, got %d", w.Code)
	}
}

func TestDashboard_StateReportsDevicesAndEvents(t *testing.T) {
	mux, events := newDashboardMux(t, "DEV001", "DEV002")
	events.add("command-result", "DEV001", "#1 CHECK: OK")

	st := getState(t, mux)

	if st.Devices.Count != 2 || len(st.Devices.Devices) != 2 {
		t.Fatalf("expected 2 devices, got %+v", st.Devices)
	}
	for _, d := range st.Devices.Devices {
		if !d.Online {
			t.Errorf("device %s was just registered and should be online", d.SerialNumber)
		}
	}
	if len(st.Events) != 1 || st.Events[0].Device != "DEV001" {
		t.Errorf("expected the one event, got %+v", st.Events)
	}
	if _, err := time.Parse(time.RFC3339, st.ServerTime); err != nil {
		t.Errorf("server_time %q is not RFC3339: %v", st.ServerTime, err)
	}
}

func TestDashboard_StateEmptyIsArraysNotNull(t *testing.T) {
	mux, _ := newDashboardMux(t)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ui/state", nil))

	// The page calls .filter/.length on these, so null would break it.
	body := w.Body.String()
	if !strings.Contains(body, `"devices":[]`) || !strings.Contains(body, `"events":[]`) {
		t.Errorf("expected empty arrays, got %s", body)
	}
}

func TestDashboard_StateShowsQueuedCommandsAsPending(t *testing.T) {
	mux, _ := newDashboardMux(t, "DEV001")

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, postJSON(t, "/api/devices/DEV001/check", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("queueing a command failed: %d %s", w.Code, w.Body.String())
	}

	st := getState(t, mux)
	if got := st.Devices.Devices[0].PendingCmds; got != 1 {
		t.Errorf("expected 1 pending command, got %d", got)
	}
}

func TestDashboard_StateIsNotLogged(t *testing.T) {
	var dump strings.Builder
	old := dumpWriter
	dumpWriter = &dump
	t.Cleanup(func() { dumpWriter = old })

	mux, _ := newDashboardMux(t)
	getState(t, mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if dump.Len() != 0 {
		t.Errorf("polling endpoints must not dump HTTP exchanges, got: %s", dump.String())
	}
}

// ---------- helpers ----------

func TestFormatInfo_SortedAndStable(t *testing.T) {
	got := formatInfo(map[string]string{"b": "2", "a": "1", "c": ""})
	if got != "a=1, b=2, c=" {
		t.Errorf("unexpected format: %q", got)
	}
	if formatInfo(nil) != "" {
		t.Error("expected empty string for nil map")
	}
}

func TestFormatUsers_OmitsPasswords(t *testing.T) {
	got := formatUsers([]zkadms.UserRecord{
		{PIN: "1001", Name: "Ann", Password: "secret"},
		{PIN: "1002", Name: "Bob", Card: "123"},
	})
	if got != "2 user(s): 1001 (Ann), 1002 (Bob)" {
		t.Errorf("unexpected format: %q", got)
	}
	if strings.Contains(got, "secret") || strings.Contains(got, "123") {
		t.Errorf("password or card leaked into the event: %q", got)
	}
}

func TestDashboardPort(t *testing.T) {
	for addr, want := range map[string]string{
		":8080":          ":8080",
		"0.0.0.0:9000":   ":9000",
		"127.0.0.1:1234": ":1234",
		"[::1]:80":       ":80",
		"nonsense":       "",
		"":               "",
	} {
		if got := dashboardPort(addr); got != want {
			t.Errorf("dashboardPort(%q) = %q, want %q", addr, got, want)
		}
	}
}

// ---------- end to end ----------

// TestRun_RecordsDeviceEventsForDashboard drives a real run() with simulated
// device traffic and checks that the callbacks populate the dashboard feed.
func TestRun_RecordsDeviceEventsForDashboard(t *testing.T) {
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

	post := func(path, body string) {
		t.Helper()
		resp, err := http.Post(base+path, "text/plain", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	post("/iclock/cdata?SN=DEV001&table=ATTLOG", "1001\t2026-03-20 09:00:00\t0\t1\t0")
	post("/iclock/cdata?SN=DEV001", "DeviceName=ZK-F22\nFWVersion=6.6")
	post("/iclock/cdata?SN=DEV001&table=USERINFO", "PIN=1001\tName=Ann\tPrivilege=0\tCard=\tPassword=pw")
	post("/iclock/devicecmd?SN=DEV001", "ID=1&Return=0&CMD=CHECK")

	want := map[string]bool{"attendance": false, "device-info": false, "users": false, "command-result": false}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
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
			if _, ok := want[e.Kind]; ok {
				want[e.Kind] = true
				if e.Device != "DEV001" {
					t.Errorf("event %q has device %q", e.Kind, e.Device)
				}
				if e.Kind == "users" && strings.Contains(e.Message, "pw") {
					t.Errorf("password leaked into users event: %q", e.Message)
				}
			}
		}
		done := true
		for _, seen := range want {
			done = done && seen
		}
		if done {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	for kind, seen := range want {
		if !seen {
			t.Errorf("no %q event reached the dashboard feed", kind)
		}
	}

	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("run returned %v", err)
	}
}

func contextWithCancel(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	return ctx, cancel
}

// waitReady polls url until it answers, so tests do not race server startup.
func waitReady(t *testing.T, url string) {
	t.Helper()
	client := http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := client.Get(url); err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("server did not become ready at %s", url)
}

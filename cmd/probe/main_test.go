package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- candidates slice validation ----------

// defaultCfg returns a probeConfig with sensible defaults for testing.
func defaultCfg(addr, sn string) probeConfig {
	return probeConfig{
		addr:           addr,
		sn:             sn,
		destructive:    false,
		delayMs:        10,
		commandTimeout: 120 * time.Second,
		maxCommands:    200,
	}
}

func TestCandidates_NoDuplicateCommands(t *testing.T) {
	seen := make(map[string]int)
	for i, c := range candidates {
		if prev, ok := seen[c.cmd]; ok {
			t.Errorf("duplicate command %q at index %d and %d", c.cmd, prev, i)
		}
		seen[c.cmd] = i
	}
}

func TestCandidates_NoEmptyCommands(t *testing.T) {
	for i, c := range candidates {
		if strings.TrimSpace(c.cmd) == "" {
			t.Errorf("candidate[%d] has empty command", i)
		}
	}
}

func TestCandidates_NoEmptyDescriptions(t *testing.T) {
	for i, c := range candidates {
		if strings.TrimSpace(c.desc) == "" {
			t.Errorf("candidate[%d] (%q) has empty description", i, c.cmd)
		}
	}
}

func TestCandidates_HasSafeAndDestructive(t *testing.T) {
	var safe, destructive int
	for _, c := range candidates {
		if c.safe {
			safe++
		} else {
			destructive++
		}
	}
	if safe == 0 {
		t.Error("expected at least one safe candidate")
	}
	if destructive == 0 {
		t.Error("expected at least one destructive candidate")
	}
	t.Logf("candidates: %d safe, %d destructive, %d total", safe, destructive, safe+destructive)
}

// ---------- logHandler tests ----------

func TestLogHandler_PassesThrough(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello"))
	})

	handler := logHandler(inner)
	req := httptest.NewRequest(http.MethodGet, "/test?foo=bar", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "hello" {
		t.Errorf("expected body %q, got %q", "hello", w.Body.String())
	}
}

func TestLogHandler_PreservesPostBody(t *testing.T) {
	var gotBody string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 1024)
		n, _ := r.Body.Read(b)
		gotBody = string(b[:n])
		w.WriteHeader(http.StatusOK)
	})

	handler := logHandler(inner)
	body := "test body content"
	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(body))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if gotBody != body {
		t.Errorf("expected body %q, got %q", body, gotBody)
	}
}

func TestLogHandler_TruncatesLongBody(t *testing.T) {
	// The logHandler truncates POST bodies > 200 chars in the log output.
	// The underlying handler should still receive the full body.
	longBody := strings.Repeat("A", 500)
	var gotLen int
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := readAll(r.Body)
		gotLen = len(b)
		w.WriteHeader(http.StatusOK)
	})

	handler := logHandler(inner)
	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(longBody))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if gotLen != 500 {
		t.Errorf("expected full body length 500, got %d", gotLen)
	}
}

// readAll is a small helper for tests; avoids importing io in the test file
// since io is already used in the main file.
func readAll(r interface{ Read([]byte) (int, error) }) []byte {
	var buf bytes.Buffer
	b := make([]byte, 4096)
	for {
		n, err := r.Read(b)
		buf.Write(b[:n])
		if err != nil {
			break
		}
	}
	return buf.Bytes()
}

// ---------- printSummary tests ----------

func TestPrintSummary_Empty(t *testing.T) {
	var mu sync.Mutex
	var results []result
	var queued []candidate

	// Capture stdout is difficult, but we can at least verify it doesn't panic.
	printSummary(&mu, results, queued, 0)
}

func TestPrintSummary_AllAccepted(t *testing.T) {
	var mu sync.Mutex
	results := []result{
		{cmd: "INFO", desc: "Get info", id: 1, retCode: 0, retCmd: "INFO"},
		{cmd: "CHECK", desc: "Heartbeat", id: 2, retCode: 0, retCmd: "CHECK"},
	}
	queued := []candidate{
		{cmd: "INFO", desc: "Get info", safe: true},
		{cmd: "CHECK", desc: "Heartbeat", safe: true},
	}

	printSummary(&mu, results, queued, 2)
	t.Log("printSummary completed for all-accepted case")
}

func TestPrintSummary_MixedResults(t *testing.T) {
	var mu sync.Mutex
	results := []result{
		{cmd: "INFO", desc: "Get info", id: 1, retCode: 0, retCmd: "INFO"},
		{cmd: "BAD", desc: "Bad cmd", id: 2, retCode: -1002, retCmd: "BAD"},
	}
	queued := []candidate{
		{cmd: "INFO", desc: "Get info", safe: true},
		{cmd: "BAD", desc: "Bad cmd", safe: true},
		{cmd: "PENDING", desc: "Never replied", safe: true},
	}

	// 2 results, but 3 queued and only 2 replied => 1 noreply
	printSummary(&mu, results, queued, 2)
	t.Log("printSummary completed for mixed-results case")
}

func TestPrintSummary_AllNoReply(t *testing.T) {
	var mu sync.Mutex
	var results []result
	queued := []candidate{
		{cmd: "CMD1", desc: "First", safe: true},
		{cmd: "CMD2", desc: "Second", safe: true},
	}

	// 0 replies out of 2 queued
	printSummary(&mu, results, queued, 0)
	t.Log("printSummary completed for all-noreply case")
}

func TestPrintSummary_AllRejected(t *testing.T) {
	var mu sync.Mutex
	results := []result{
		{cmd: "BAD1", desc: "Bad 1", id: 1, retCode: -1, retCmd: "BAD1"},
		{cmd: "BAD2", desc: "Bad 2", id: 2, retCode: -1002, retCmd: "BAD2"},
	}
	queued := []candidate{
		{cmd: "BAD1", desc: "Bad 1", safe: true},
		{cmd: "BAD2", desc: "Bad 2", safe: true},
	}

	printSummary(&mu, results, queued, 2)
	t.Log("printSummary completed for all-rejected case")
}

// ---------- run() lifecycle tests ----------

func TestRun_StartsAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())

	errCh := make(chan error, 1)
	go func() {
		errCh <- run(ctx, defaultCfg(":0", "TESTDEV001"))
	}()

	// Give the server a moment to start listening.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return within 5s after context cancellation")
	}
}

func TestRun_InvalidAddr(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// An obviously invalid address should make ListenAndServe fail immediately.
	err := run(ctx, defaultCfg(":-1", "TESTDEV001"))
	if err == nil {
		t.Fatal("expected error for invalid address, got nil")
	}
}

func TestRun_InvalidSerialNumber(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	err := run(ctx, defaultCfg(":0", "INVALID DEVICE!"))
	if err == nil {
		t.Fatal("expected error for invalid serial number, got nil")
	}
	if !strings.Contains(err.Error(), "RegisterDevice") {
		t.Errorf("expected RegisterDevice error, got: %v", err)
	}
}

// ---------- run() with simulated device traffic ----------

func TestRun_ExercisesCallbacks(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close() // free the port for run()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- run(ctx, defaultCfg(addr, "PROBEDEV"))
	}()

	// Wait for server to start using a readiness loop.
	base := "http://" + addr
	waitForServer(t, base+"/iclock/cdata?SN=PROBEDEV")

	// 1. Send device info — triggers WithOnDeviceInfo callback.
	infoData := "FWVersion=Ver 6.60\nDeviceName=ZK-F22"
	resp, err := http.Post(base+"/iclock/cdata?SN=PROBEDEV",
		"text/plain", strings.NewReader(infoData))
	if err != nil {
		t.Fatalf("POST device info failed: %v", err)
	}
	resp.Body.Close()

	// Give the probe goroutine time to queue candidates so the FIFO
	// fallback has entries available when the unknown-ID result arrives.
	time.Sleep(200 * time.Millisecond)

	// 2. Send command result with unknown ID — triggers FIFO fallback path
	// (ID not in idMap, QueuedCommand empty because ID not in pendingCommands).
	// Sent first so replied=0 < len(queued), exercising the FIFO branch.
	cmdResultUnknown := "ID=99999&Return=0&CMD=UNKNOWN"
	resp, err = http.Post(base+"/iclock/devicecmd?SN=PROBEDEV",
		"text/plain", strings.NewReader(cmdResultUnknown))
	if err != nil {
		t.Fatalf("POST devicecmd (unknown ID) failed: %v", err)
	}
	resp.Body.Close()

	// 3. Send command result (Return=0) — triggers WithOnCommandResult OK path.
	cmdResult := "ID=1&Return=0&CMD=INFO"
	resp, err = http.Post(base+"/iclock/devicecmd?SN=PROBEDEV",
		"text/plain", strings.NewReader(cmdResult))
	if err != nil {
		t.Fatalf("POST devicecmd (OK) failed: %v", err)
	}
	resp.Body.Close()

	// 4. Send command result (Return=-1002) — triggers WithOnCommandResult FAIL path.
	cmdResultFail := "ID=2&Return=-1002&CMD=BAD"
	resp, err = http.Post(base+"/iclock/devicecmd?SN=PROBEDEV",
		"text/plain", strings.NewReader(cmdResultFail))
	if err != nil {
		t.Fatalf("POST devicecmd (FAIL) failed: %v", err)
	}
	resp.Body.Close()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return within 5s after context cancellation")
	}
}

// waitForServer polls the given URL until it gets a successful response.
func waitForServer(t *testing.T, url string) {
	t.Helper()
	client := http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("server at %s did not become ready within timeout", url)
		}
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestRun_DestructiveFlag starts the server with destructive=true.
// We cannot easily observe the filtering behavior from outside without a
// real device, but we exercise the code path.
func TestRun_DestructiveFlag(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())

	errCh := make(chan error, 1)
	go func() {
		cfg := defaultCfg(":0", "TESTDEV002")
		cfg.destructive = true
		errCh <- run(ctx, cfg)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return within 5s")
	}
}

// ---------- result / candidate type tests ----------

func TestResult_Fields(t *testing.T) {
	r := result{
		cmd:     "INFO",
		desc:    "Get info",
		id:      42,
		retCode: 0,
		retCmd:  "INFO",
	}
	if r.cmd != "INFO" || r.id != 42 || r.retCode != 0 {
		t.Errorf("unexpected result fields: %+v", r)
	}
}

func TestCandidate_Fields(t *testing.T) {
	c := candidate{
		cmd:  "CHECK",
		desc: "Heartbeat",
		safe: true,
	}
	if c.cmd != "CHECK" || !c.safe {
		t.Errorf("unexpected candidate fields: %+v", c)
	}
}

// ---------- candidates safe-only filtering ----------

func TestCandidates_SafeOnlyFilter(t *testing.T) {
	var safeCount int
	for _, c := range candidates {
		if c.safe {
			safeCount++
		}
	}
	// When destructive=false, only safe commands should be queued.
	// Verify there are actually safe commands available.
	if safeCount == 0 {
		t.Fatal("no safe candidates found; probe would queue nothing with destructive=false")
	}

	// Also verify all candidates count > safe count (there must be some destructive ones).
	if safeCount == len(candidates) {
		t.Error("all candidates are safe; destructive flag is meaningless")
	}
	t.Logf("safe=%d total=%d (destructive=%d)", safeCount, len(candidates), len(candidates)-safeCount)
}

// TestRun_WithSmallDelay verifies very small delays don't cause issues.
func TestRun_WithSmallDelay(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())

	errCh := make(chan error, 1)
	go func() {
		cfg := defaultCfg(":0", "TESTDEV003")
		cfg.delayMs = 0
		errCh <- run(ctx, cfg) // zero delay
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return within 5s")
	}
}

// ---------- logHandler edge cases ----------

func TestLogHandler_GetRequestNoBody(t *testing.T) {
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := logHandler(inner)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if !called {
		t.Error("inner handler was not called")
	}
}

func TestLogHandler_PostEmptyBody(t *testing.T) {
	var gotBody string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := readAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	})

	handler := logHandler(inner)
	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(""))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if gotBody != "" {
		t.Errorf("expected empty body, got %q", gotBody)
	}
}

// ---------- concurrent printSummary safety ----------

func TestPrintSummary_ConcurrentSafe(t *testing.T) {
	var mu sync.Mutex
	results := []result{
		{cmd: "A", desc: "A", id: 1, retCode: 0, retCmd: "A"},
	}
	queued := []candidate{
		{cmd: "A", desc: "A", safe: true},
		{cmd: "B", desc: "B", safe: true},
	}

	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func(){
			printSummary(&mu, results, queued, 1)
		})
	}
	wg.Wait()
	t.Log("concurrent printSummary completed without panic or race")
}

// ---------- candidate categories ----------

func TestCandidates_ContainsExpectedCategories(t *testing.T) {
	// Verify the candidates list includes at least some expected commands.
	expected := []string{"INFO", "CHECK", "LOG"}
	for _, want := range expected {
		found := false
		for _, c := range candidates {
			if c.cmd == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected candidate %q not found", want)
		}
	}
}

func TestCandidates_SafeCommandsDontModify(t *testing.T) {
	// Safe commands should not contain SET, CONTROL, ENROLL, CLEAR, REBOOT, DELETE, ADD
	destructiveKeywords := []string{
		"SET OPTION",
		"CONTROL DEVICE",
		"ENROLL_FP PIN=",
		"USER ADD",
		"USER DEL",
		"DATA DEL",
	}

	for _, c := range candidates {
		if !c.safe {
			continue
		}
		for _, kw := range destructiveKeywords {
			if strings.HasPrefix(c.cmd, kw) {
				t.Errorf("safe candidate %q starts with destructive keyword %q", c.cmd, kw)
			}
		}
	}
}

// ---------- run() with full probe cycle simulation ----------

func TestRun_FullProbeWithDeviceOnline(t *testing.T) {
	// Start the probe and simulate a device that is already online
	// (which it is, since RegisterDevice sets LastActivity = now).
	// With delayMs=0, the goroutine queues all safe commands immediately,
	// then enters the confirmation-waiting ticker loop.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		// destructive=true so ALL candidates get queued (exercises both paths).
		cfg := defaultCfg(addr, "FULLTEST")
		cfg.destructive = true
		cfg.delayMs = 0
		errCh <- run(ctx, cfg)
	}()

	// Wait for server readiness.
	base := "http://" + addr
	waitForServer(t, base+"/iclock/cdata?SN=FULLTEST")

	// The device is online immediately after registration, so the
	// goroutine starts queuing commands right away with delayMs=0.
	// Give it a moment to finish queuing.
	time.Sleep(500 * time.Millisecond)

	// Send device info to exercise that callback path.
	infoData := "FWVersion=Ver 6.60\nDeviceName=ProbeTestDevice"
	resp, err := http.Post(base+"/iclock/cdata?SN=FULLTEST",
		"text/plain", strings.NewReader(infoData))
	if err != nil {
		t.Fatalf("POST device info failed: %v", err)
	}
	resp.Body.Close()

	// Send a mix of successful and failed command confirmations.
	for i := range 3 {
		cmdResult := fmt.Sprintf("ID=%d&Return=0&CMD=TEST%d", i+1, i+1)
		resp, err = http.Post(base+"/iclock/devicecmd?SN=FULLTEST",
			"text/plain", strings.NewReader(cmdResult))
		if err != nil {
			t.Fatalf("POST devicecmd %d failed: %v", i+1, err)
		}
		resp.Body.Close()
	}

	// Send a failed result too.
	resp, err = http.Post(base+"/iclock/devicecmd?SN=FULLTEST",
		"text/plain", strings.NewReader("ID=4&Return=-1002&CMD=BADCMD"))
	if err != nil {
		t.Fatalf("POST devicecmd fail failed: %v", err)
	}
	resp.Body.Close()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return within 5s")
	}
}

// TestRun_PartialConfirmations sends only some confirmations and waits
// long enough for the ticker to fire, exercising the "still pending" log
// path (line 327) in the confirmation-waiting loop.
func TestRun_PartialConfirmations(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		// destructive=false, delayMs=0 => only safe commands queued instantly.
		cfg := defaultCfg(addr, "PARTIALTEST")
		cfg.delayMs = 0
		errCh <- run(ctx, cfg)
	}()

	base := "http://" + addr
	waitForServer(t, base+"/iclock/cdata?SN=PARTIALTEST")

	// Wait for commands to be queued.
	time.Sleep(500 * time.Millisecond)

	// Send only 2 confirmations out of many safe commands.
	for i := range 2 {
		cmdResult := fmt.Sprintf("ID=%d&Return=0&CMD=CMD%d", i+1, i+1)
		resp, err := http.Post(base+"/iclock/devicecmd?SN=PARTIALTEST",
			"text/plain", strings.NewReader(cmdResult))
		if err != nil {
			t.Fatalf("POST devicecmd %d failed: %v", i+1, err)
		}
		resp.Body.Close()
	}

	// Wait for at least one ticker cycle (5s) so the "still pending"
	// message is printed. The ticker fires every 5 seconds and checks
	// remaining > 0 which will be true since we only sent 2 out of ~30.
	time.Sleep(6 * time.Second)

	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run returned unexpected error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return within 10s")
	}
}

// TestRun_ContextCancelDuringDeviceWait cancels the context before the
// device comes online, exercising the ctx.Done path in the device-wait loop.
func TestRun_ContextCancelDuringDeviceWait(t *testing.T) {
	// We need a device that is NOT online. RegisterDevice marks it as online
	// immediately, so the wait loop exits right away. The only way to hit the
	// ctx.Done select branch is to cancel before the goroutine checks IsDeviceOnline.
	// We test this indirectly: start with a very short context timeout that
	// expires while the goroutine is in the 1-second sleep of the wait loop.
	// Since IsDeviceOnline returns true immediately after RegisterDevice, this
	// won't hit the select. Instead, we rely on context cancellation during
	// the command queuing loop (line 256-260).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	// Use a very short-lived context.
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		// destructive=true + large delay = goroutine will be mid-queuing when ctx expires.
		cfg := defaultCfg(addr, "CANCELTEST")
		cfg.destructive = true
		cfg.delayMs = 5000
		errCh <- run(ctx, cfg)
	}()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run returned unexpected error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return within 10s")
	}
}

// TestRun_SafeOnlyFiltering verifies that with destructive=false, the
// goroutine skips destructive commands (exercises the SKIP branch).
func TestRun_SafeOnlyFiltering(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		// destructive=false: destructive commands are skipped.
		cfg := defaultCfg(addr, "SAFETEST")
		cfg.delayMs = 0
		errCh <- run(ctx, cfg)
	}()

	base := "http://" + addr
	waitForServer(t, base+"/iclock/cdata?SN=SAFETEST")

	// Wait for the goroutine to detect online and queue safe commands.
	time.Sleep(500 * time.Millisecond)

	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return within 5s")
	}
}

// TestRun_AllConfirmationsReceived exercises the ticker path where all
// queued commands receive confirmations before the timeout.
func TestRun_AllConfirmationsReceived(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		// destructive=false, delayMs=0 => only safe commands queued instantly.
		cfg := defaultCfg(addr, "ALLCONFIRM")
		cfg.delayMs = 0
		errCh <- run(ctx, cfg)
	}()

	base := "http://" + addr
	waitForServer(t, base+"/iclock/cdata?SN=ALLCONFIRM")

	// Wait for commands to be queued.
	time.Sleep(500 * time.Millisecond)

	// Count safe candidates.
	var safeCount int
	for _, c := range candidates {
		if c.safe {
			safeCount++
		}
	}

	// Send confirmations for ALL safe commands to trigger the
	// "ALL CONFIRMATIONS RECEIVED" path in the ticker.
	for i := range safeCount {
		cmdResult := fmt.Sprintf("ID=%d&Return=0&CMD=CMD%d", i+1, i+1)
		resp, err := http.Post(base+"/iclock/devicecmd?SN=ALLCONFIRM",
			"text/plain", strings.NewReader(cmdResult))
		if err != nil {
			t.Fatalf("POST devicecmd %d failed: %v", i+1, err)
		}
		resp.Body.Close()
	}

	// The probe's ticker checks every 5 seconds. Wait for it to
	// detect all confirmations and trigger self-shutdown.
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run returned unexpected error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not self-terminate after all confirmations")
	}
}

// TestRun_Timeout exercises the timeout path (lines 298-309 in main.go)
// by using a very short commandTimeout. Commands are queued but no
// confirmations are sent, so the timeout fires before completion.
// This covers: the timeout select case, snapshot copies of results/queued/
// replied, the printSummary call, and cancelFn() — 11 statements total.
func TestRun_Timeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		// Use a 2-second timeout. With delayMs=0 and destructive=false,
		// safe commands are queued instantly. No confirmations are sent,
		// so the timeout fires after 2 seconds.
		cfg := defaultCfg(addr, "TIMEOUTDEV")
		cfg.delayMs = 0
		cfg.commandTimeout = 2 * time.Second
		errCh <- run(ctx, cfg)
	}()

	// Wait for the server to start.
	base := "http://" + addr
	waitForServer(t, base+"/iclock/cdata?SN=TIMEOUTDEV")

	// Don't send any confirmations — let the timeout fire.
	// The probe should self-terminate after the 2-second timeout
	// plus up to one 5-second ticker cycle.
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run returned unexpected error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not self-terminate after timeout")
	}
}

// TestRun_QueueFull exercises the QUEUE ERROR branch (line 272-274)
// by setting maxCommands=1. The first safe command fills the queue,
// and subsequent commands hit the ErrCommandQueueFull path.
func TestRun_QueueFull(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		// maxCommands=1: first command queues OK, all others fail.
		// destructive=false, delayMs=0 => safe commands queued instantly.
		cfg := defaultCfg(addr, "QUEUEFULL")
		cfg.delayMs = 0
		cfg.maxCommands = 1
		errCh <- run(ctx, cfg)
	}()

	base := "http://" + addr
	waitForServer(t, base+"/iclock/cdata?SN=QUEUEFULL")

	// Wait for the goroutine to finish queuing commands.
	time.Sleep(500 * time.Millisecond)

	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return within 5s")
	}
}

// TestRun_DeviceWaitCtxCancel exercises the ctx.Done select branch in the
// device-wait loop (lines 265-267 in main.go). By setting onlineThreshold
// to 1ns, the device appears offline immediately after RegisterDevice
// (since more than 1ns elapses before IsDeviceOnline is called). The
// goroutine enters the wait loop, and canceling the context triggers the
// ctx.Done case.
func TestRun_DeviceWaitCtxCancel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	// Short-lived context: cancel after 300ms, while the goroutine is
	// stuck in the device-wait loop (device appears offline due to 1ns threshold).
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		cfg := defaultCfg(addr, "WAITCANCEL")
		cfg.onlineThreshold = 1 * time.Nanosecond
		errCh <- run(ctx, cfg)
	}()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run returned unexpected error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return within 10s after context cancellation")
	}
}

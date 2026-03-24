// Command probe queues candidate ADMS commands to a real device and reports
// which ones succeed (Return=0) vs fail (Return=-1002 or other).
//
// Usage:
//
//	go run ./cmd/probe -addr 192.168.1.201:8080 -sn CMZS220260005
//
// The probe starts a temporary ADMS server, registers the device, queues all
// candidate commands with a 2-second delay between each, then waits for the
// device to poll and confirm. Results are printed as they arrive.
//
// NOTE: Some commands may have side effects (REBOOT, CLEAR DATA, etc.).
// Commands marked [SAFE] are read-only. Commands marked [CAUTION] may modify
// device state. The probe skips destructive commands by default unless
// -destructive is set.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	zkadms "github.com/s0x90/zkteco-adms"
)

type candidate struct {
	cmd  string
	desc string
	safe bool // false = potentially destructive
}

type result struct {
	cmd     string
	desc    string
	id      int64
	retCode int
	retCmd  string
}

// Candidate commands to probe, organized by category.
// These are gathered from:
// - ADMS datasheet
// - ZKTeco BioTime/ZKBioSecurity protocol sniffing
// - Community reverse-engineering (StackOverflow, GitHub projects)
// - ZKTeco Standalone SDK documentation (UDP commands translated to ADMS)
var candidates = []candidate{
	// === DEVICE INFO / STATUS (SAFE) ===
	{"INFO", "Request device info (firmware, serial, etc.)", true},
	{"CHECK", "Heartbeat/ping check", true},
	{"GET OPTION FROM DeviceName", "Get device name option", true},
	{"GET OPTION FROM FWVersion", "Get firmware version", true},
	{"GET OPTION FROM IPAddress", "Get IP address", true},
	{"GET OPTION FROM MACAddress", "Get MAC address", true},
	{"GET OPTION FROM Platform", "Get platform info", true},
	{"GET OPTION FROM WorkCode", "Get work code setting", true},
	{"GET OPTION FROM LockCount", "Get door lock count", true},
	{"GET OPTION FROM UserCount", "Get user count", true},
	{"GET OPTION FROM FPCount", "Get fingerprint template count", true},
	{"GET OPTION FROM AttLogCount", "Get attendance log count", true},
	{"GET OPTION FROM FaceCount", "Get face template count", true},
	{"GET OPTION FROM TransactionCount", "Get transaction count", true},
	{"GET OPTION FROM MaxUserCount", "Get max user capacity", true},
	{"GET OPTION FROM MaxAttLogCount", "Get max attendance log capacity", true},
	{"GET OPTION FROM MaxFingerCount", "Get max fingerprint capacity", true},
	{"GET OPTION FROM MaxFaceCount", "Get max face template capacity", true},

	// === DATA QUERY COMMANDS (SAFE) ===
	{"DATA QUERY USERINFO PIN=1", "Query user info for PIN 1", true},
	{"DATA QUERY USERINFO", "Query all users", true},
	{"DATA QUERY ATTLOG", "Query attendance logs", true},
	{"DATA QUERY FIRST_AND_LAST_LOG", "Query first and last attendance log", true},
	{"DATA QUERY OPERLOG", "Query operation logs", true},
	{"DATA COUNT USERINFO", "Count users", true},
	{"DATA COUNT ATTLOG", "Count attendance logs", true},

	// === SET OPTION COMMANDS (CAUTION - modifies settings) ===
	{"SET OPTION ServerLocalTime=" + time.Now().Format("2006-01-02 15:04:05"), "Sync device clock", false},
	{"SET OPTION LockOpenDelay=5", "Set door open delay to 5 seconds", false},

	// === USER MANAGEMENT (CAUTION) ===
	// Already confirmed working:
	// DATA UPDATE USERINFO PIN=9999\tName=ProbeTest\tPrivilege=0\tCard=
	// DATA DELETE USERINFO PIN=9999

	// === CONTROL COMMANDS (CAUTION) ===
	{"CONTROL DEVICE 1 1", "Trigger door relay (open door)", false},
	{"CONTROL DEVICE 1 0", "Close door relay", false},
	{"CONTROL DEVICE 2 1", "Trigger aux output 1", false},
	{"CONTROL DEVICE 3 1", "Trigger alarm", false},
	{"CONTROL DEVICE 4 1", "Disable normal open", false},

	// === CLEAR / LOG COMMANDS (CAUTION) ===
	// NOTE: Skipping CLEAR DATA and CLEAR LOG — too destructive for a probe.

	// === REBOOT (CAUTION) ===
	// {"REBOOT", "Reboot device", false},
	// Skipping — would interrupt the probe.

	// === ENROLL COMMANDS (CAUTION) ===
	{"ENROLL_FP PIN=9999 FID=0", "Start fingerprint enrollment for user 9999 finger 0", false},
	{"ENROLL_FP CANCEL", "Cancel fingerprint enrollment", false},

	// === BIODATA / TEMPLATE COMMANDS (SAFE reads) ===
	{"DATA QUERY BIODATA PIN=1 Type=9", "Query face template for user 1", true},
	{"DATA QUERY BIODATA PIN=1 Type=0", "Query fingerprint template for user 1 (type 0)", true},
	{"DATA QUERY FINGERTMP PIN=1", "Query fingerprint template (alternate syntax)", true},
	{"DATA QUERY USERPIC PIN=1", "Query user photo for PIN 1", true},

	// === MISC DOCUMENTED COMMANDS (SAFE) ===
	{"PutFile FileName=options.cfg", "Request device to upload options.cfg", true},
	{"GetFile FileName=options.cfg", "Request device to send options.cfg", true},
	{"Shell date", "Execute shell command (unlikely to work)", true},
	{"LOG", "Request log data", true},
	{"DATA QUERY OPTIONS", "Query all device options", true},

	// === ACCESS CONTROL COMMANDS (SAFE reads) ===
	{"DATA QUERY TIMEZONE", "Query timezone schedules", true},
	{"DATA QUERY FIRSTCARD", "Query first-card unlock config", true},
	{"DATA QUERY MULTIMCARD", "Query multi-card open config", true},
	{"DATA QUERY INOUTFUN", "Query in/out function config", true},

	// === ALTERNATE COMMAND SYNTAX (testing if device accepts these) ===
	{"USER ADD PIN=9998\tName=SyntaxTest\tPrivilege=0\tCard=", "Datasheet USER ADD (likely fails)", false},
	{"USER DEL PIN=9998", "Datasheet USER DEL (likely fails)", false},
	{"DATA DEL USERINFO PIN=9998", "DATA DEL (truncated DELETE, likely fails)", false},
}

func main() {
	addr := flag.String("addr", ":8080", "Listen address for ADMS server")
	sn := flag.String("sn", "CMZS220260005", "Device serial number")
	destructive := flag.Bool("destructive", false, "Include potentially destructive commands")
	delayMs := flag.Int("delay", 1500, "Delay between commands in milliseconds")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	if err := run(ctx, *addr, *sn, *destructive, *delayMs); err != nil {
		stop()
		log.Fatal(err)
	}
	stop()
}

// run is the core probe logic, extracted from main for testability.
func run(ctx context.Context, addr, sn string, destructive bool, delayMs int) error {
	// Track results.
	// Commands are queued FIFO and assigned IDs at write-time, so we
	// correlate results by maintaining a queue of queued candidates in
	// order. When a confirmation arrives, the first unmatched candidate
	// with a matching command prefix is paired.
	var (
		mu      sync.Mutex
		results []result
		queued  []candidate // commands queued in order
		replied int         // how many confirmations received
	)

	server := zkadms.NewADMSServer(
		zkadms.WithEnableInspect(),
		zkadms.WithMaxCommandsPerDevice(200),
		zkadms.WithOnCommandResult(func(_ context.Context, cr zkadms.CommandResult) {
			mu.Lock()
			defer mu.Unlock()

			// Match against queued commands in order.
			desc := "(unknown)"
			cmd := cr.Command
			if replied < len(queued) {
				c := queued[replied]
				desc = c.desc
				cmd = c.cmd
			}
			replied++

			r := result{
				cmd:     cmd,
				desc:    desc,
				id:      cr.ID,
				retCode: cr.ReturnCode,
				retCmd:  cr.Command,
			}
			results = append(results, r)

			status := "OK"
			if cr.ReturnCode != 0 {
				status = fmt.Sprintf("FAIL(%d)", cr.ReturnCode)
			}
			fmt.Printf("  [ID=%d] %-7s  Return=%-6d CMD=%-30s  | %s → %s\n",
				cr.ID, status, cr.ReturnCode, cr.Command, cmd, desc)
		}),
		zkadms.WithOnDeviceInfo(func(_ context.Context, serial string, info map[string]string) {
			fmt.Printf("\n--- Device Info from %s ---\n", serial)
			for k, v := range info {
				fmt.Printf("  %s = %s\n", k, v)
			}
			fmt.Println("---")
		}),
	)
	defer server.Close()

	if err := server.RegisterDevice(sn); err != nil {
		return fmt.Errorf("RegisterDevice: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/iclock/", server)

	handler := logHandler(mux)

	srv := &http.Server{Addr: addr, Handler: handler}

	// cancelFn is used by the probe goroutine to trigger shutdown when done.
	ctx, cancelFn := context.WithCancel(ctx)
	defer cancelFn()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("HTTP server shutdown error: %v", err)
		}
	}()

	// Queue commands in a goroutine with delays.
	go func() {
		// Wait for device to connect first.
		fmt.Println("Waiting for device to connect...")
		for {
			if server.IsDeviceOnline(sn) {
				fmt.Printf("Device %s is online!\n\n", sn)
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(1 * time.Second):
			}
		}

		delay := time.Duration(delayMs) * time.Millisecond

		fmt.Println("=== PROBING COMMANDS ===")
		fmt.Printf("Delay between commands: %v\n", delay)
		fmt.Printf("Destructive mode: %v\n\n", destructive)

		for _, c := range candidates {
			select {
			case <-ctx.Done():
				return
			default:
			}

			if !c.safe && !destructive {
				fmt.Printf("  SKIP (destructive): %s  | %s\n", c.cmd, c.desc)
				continue
			}

			mu.Lock()
			queued = append(queued, c)
			queuedCount := len(queued)
			mu.Unlock()

			if err := server.QueueCommand(sn, c.cmd); err != nil {
				fmt.Printf("  QUEUE ERROR: %s: %v\n", c.cmd, err)
				continue
			}
			tag := "[SAFE]"
			if !c.safe {
				tag = "[CAUTION]"
			}
			fmt.Printf("  QUEUED [#%d] %s %s  | %s\n", queuedCount, tag, c.cmd, c.desc)

			time.Sleep(delay)
		}

		fmt.Println("\n=== ALL COMMANDS QUEUED ===")
		fmt.Println("Waiting for device confirmations (Ctrl+C to stop)...")
		fmt.Println("Device polls every 3-8 seconds, so results may take a minute.")
		fmt.Println()

		// Wait for all queued commands to be confirmed or timeout.
		timeout := time.After(120 * time.Second)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timeout:
				fmt.Println("\n=== TIMEOUT — printing summary ===")
				printSummary(&mu, results, queued, replied)
				cancelFn()
				return
			case <-ticker.C:
				mu.Lock()
				remaining := len(queued) - replied
				mu.Unlock()
				if remaining <= 0 {
					fmt.Println("\n=== ALL CONFIRMATIONS RECEIVED ===")
					printSummary(&mu, results, queued, replied)
					cancelFn()
					return
				}
				fmt.Printf("  ... %d commands still pending confirmation\n", remaining)
			}
		}
	}()

	fmt.Printf("ADMS Probe Server listening on %s\n", addr)
	fmt.Printf("Device: %s\n", sn)
	fmt.Println("Endpoints: /iclock/* (ADMS protocol)")
	fmt.Println()

	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}

	// Final summary.
	printSummary(&mu, results, queued, replied)
	return nil
}

// logHandler wraps an http.Handler with request logging middleware.
func logHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Capture the body for POST requests.
		var bodyStr string
		if r.Method == http.MethodPost && r.Body != nil {
			bodyBytes, err := io.ReadAll(r.Body)
			if err == nil {
				bodyStr = string(bodyBytes)
				// Put the body back for the actual handler.
				r.Body = io.NopCloser(strings.NewReader(bodyStr))
			}
		}
		query := r.URL.RawQuery
		if len(bodyStr) > 200 {
			bodyStr = bodyStr[:200] + "...(truncated)"
		}
		fmt.Printf("  >>> %s %s?%s  body=%q\n", r.Method, r.URL.Path, query, bodyStr)
		next.ServeHTTP(w, r)
	})
}

func printSummary(mu *sync.Mutex, results []result, queued []candidate, replied int) {
	mu.Lock()
	defer mu.Unlock()

	fmt.Println()
	fmt.Println(strings.Repeat("=", 80))
	fmt.Println("PROBE RESULTS SUMMARY")
	fmt.Println(strings.Repeat("=", 80))

	var accepted, rejected, noreply []string

	for _, r := range results {
		line := fmt.Sprintf("  [ID=%d] Return=%-6d CMD=%-25s  | %s → %s",
			r.id, r.retCode, r.retCmd, r.cmd, r.desc)
		if r.retCode == 0 {
			accepted = append(accepted, line)
		} else {
			rejected = append(rejected, line)
		}
	}

	// Any queued commands beyond what we got replies for.
	for i := replied; i < len(queued); i++ {
		c := queued[i]
		noreply = append(noreply, fmt.Sprintf("  [#%d] %-50s | %s", i+1, c.cmd, c.desc))
	}

	fmt.Printf("\nACCEPTED (Return=0): %d commands\n", len(accepted))
	for _, s := range accepted {
		fmt.Println(s)
	}

	fmt.Printf("\nREJECTED (Return≠0): %d commands\n", len(rejected))
	for _, s := range rejected {
		fmt.Println(s)
	}

	fmt.Printf("\nNO REPLY: %d commands\n", len(noreply))
	for _, s := range noreply {
		fmt.Println(s)
	}

	fmt.Println(strings.Repeat("=", 80))
}

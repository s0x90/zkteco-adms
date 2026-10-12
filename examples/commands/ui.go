package main

import (
	"bytes"
	_ "embed"
	"io"
	"net/http"
	"strings"
	"time"

	zkadms "github.com/s0x90/zkteco-adms"
)

// indexHTML is the single-page dashboard. It has no external dependencies
// (no CDN scripts, fonts or stylesheets), so it works on an offline network.
//
//go:embed web/index.html
var indexHTML []byte

// stateResponse is what the dashboard polls: every device plus recent events,
// in one request so the page never shows the two out of step.
type stateResponse struct {
	ServerTime string          `json:"server_time"`
	Devices    devicesResponse `json:"devices"`
	Events     []event         `json:"events"`
}

// handleIndex serves the dashboard page.
func handleIndex() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("X-Content-Type-Options", "nosniff")
		// The page needs only its own inline script and style and same-origin
		// requests; refuse everything else (framing, forms, remote loads).
		h.Set("Content-Security-Policy",
			"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; "+
				"connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		h.Set("Cache-Control", "no-store")
		_, _ = w.Write(indexHTML)
	})
}

// handleState returns the device list and recent events for the dashboard.
func handleState(server *zkadms.ADMSServer, events *eventLog) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, stateResponse{
			ServerTime: time.Now().Format(time.RFC3339),
			Devices:    listDevicesResponse(server),
			Events:     events.snapshot(),
		})
	})
}

// maxReplyBody caps how much of a device's command reply is recorded.
const maxReplyBody = 4096

// captureDeviceReplies records the raw body of every command reply a device
// POSTs to /iclock/devicecmd as a "device-reply" event, so the dashboard shows
// exactly what the device said (including Content= output that the library's
// parsed CommandResult does not carry) without watching the terminal.
func captureDeviceReplies(events *eventLog, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/iclock/devicecmd") || r.Body == nil {
			next.ServeHTTP(w, r)
			return
		}
		// Read a bounded prefix for the event, then put it back in front of the
		// rest of the stream so the library still sees the whole body.
		prefix, _ := io.ReadAll(io.LimitReader(r.Body, maxReplyBody+1))
		r.Body = readCloser{io.MultiReader(bytes.NewReader(prefix), r.Body), r.Body}
		next.ServeHTTP(w, r)

		reply := strings.TrimSpace(string(prefix))
		if reply == "" {
			return
		}
		if len(prefix) > maxReplyBody {
			reply = strings.TrimSpace(string(prefix[:maxReplyBody])) + "…"
		}
		reply = strings.ReplaceAll(strings.ReplaceAll(reply, "\r", ""), "\n", " ⏎ ")
		events.add("device-reply", r.URL.Query().Get("SN"), reply)
	})
}

// readCloser reads from one reader and closes another.
type readCloser struct {
	io.Reader
	io.Closer
}

// mountDashboard adds the web dashboard to mux. The polling endpoint is
// deliberately not wrapped in logMiddleware: the page polls every few seconds
// and each request would otherwise dump a full HTTP exchange to stderr,
// burying the device traffic you are trying to watch.
//
// SECURITY: like the rest of this example, the dashboard has no
// authentication, and it can send commands to every device.
func mountDashboard(mux *http.ServeMux, server *zkadms.ADMSServer, events *eventLog) {
	mux.Handle("GET /{$}", handleIndex())
	mux.Handle("GET /ui/state", handleState(server, events))
}

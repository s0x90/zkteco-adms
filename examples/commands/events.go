package main

import (
	"sync"
	"time"
)

// maxEvents is how many recent events the dashboard keeps in memory.
const maxEvents = 100

// maxEventMessage caps a single event message so a large device push (for
// example a full user list) cannot bloat the dashboard response.
const maxEventMessage = 500

// event is one thing that happened on a device, shown in the dashboard log.
type event struct {
	ID   int64  `json:"id"`
	Time string `json:"time"`
	Kind string `json:"kind"`
	// Level is "ok", "error" or "info"; the dashboard colors rows by it.
	Level   string `json:"level"`
	Device  string `json:"device"`
	Message string `json:"message"`
}

// eventLog is a fixed-size, concurrency-safe record of recent device events.
// The ADMS callbacks append to it; the dashboard reads it. It lives in memory
// only, so it is empty after a restart.
type eventLog struct {
	mu     sync.Mutex
	nextID int64
	items  []event // oldest first, at most limit entries
	limit  int
}

func newEventLog(limit int) *eventLog {
	return &eventLog{limit: limit}
}

// add appends an informational event.
func (l *eventLog) add(kind, device, message string) {
	l.addLevel(kind, "info", device, message)
}

// addLevel appends an event, dropping the oldest once the log is full.
func (l *eventLog) addLevel(kind, level, device, message string) {
	if r := []rune(message); len(r) > maxEventMessage {
		message = string(r[:maxEventMessage]) + "…"
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.nextID++
	l.items = append(l.items, event{
		ID:      l.nextID,
		Time:    time.Now().Format(time.RFC3339),
		Kind:    kind,
		Level:   level,
		Device:  device,
		Message: message,
	})
	if len(l.items) > l.limit {
		l.items = l.items[len(l.items)-l.limit:]
	}
}

// snapshot returns a copy of the events, newest first.
func (l *eventLog) snapshot() []event {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]event, len(l.items))
	for i, e := range l.items {
		out[len(l.items)-1-i] = e
	}
	return out
}

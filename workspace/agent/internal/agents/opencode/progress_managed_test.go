package opencode

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
)

func progressTestHandle(t *testing.T, v2 bool) *threadHandle {
	t.Helper()
	if v2 {
		detectedMajor.Store(int32(apiV2))
	} else {
		detectedMajor.Store(int32(apiV1))
	}
	t.Cleanup(func() { detectedMajor.Store(0) })
	h := &threadHandle{name: "prog", ses: "ses_p", alive: true, events: make(chan agents.Event, 8)}
	handlesMu.Lock()
	handles[h.name] = h
	handlesMu.Unlock()
	t.Cleanup(func() { handlesMu.Lock(); delete(handles, h.name); handlesMu.Unlock() })
	return h
}

func feed(typ, id string) {
	noteProgress(typ, json.RawMessage(`{"sessionID":"ses_p","id":"`+id+`"}`))
}

// The sequence measured on opencode 2.0.32 (`sleep 120`): after session.tool.progress nothing
// arrives for the whole sleep, then session.tool.success and session.step.ended.
func TestSilentToolStaysAliveUntilItsEnd(t *testing.T) {
	h := progressTestHandle(t, true)
	feed("session.tool.called", "call_1")
	feed("session.tool.progress", "call_1")
	at, _ := h.act.At()
	now := at.Add(2 * time.Hour)

	got, ok := h.act.Progress(now, h.tools.any)
	if !ok || !got.Equal(now) {
		t.Fatalf("silent tool in flight: got %v, %v; want now", got, ok)
	}
	feed("session.tool.success", "call_1")
	at, _ = h.act.At()
	got, ok = h.act.Progress(now, h.tools.any)
	if !ok || !got.Equal(at) {
		t.Fatalf("after the tool ended: got %v, %v; want the last event, so the bound can lapse", got, ok)
	}
}

func TestParallelToolsAreTrackedByCallID(t *testing.T) {
	h := progressTestHandle(t, true)
	feed("session.tool.called", "call_1")
	feed("session.tool.called", "call_2")
	feed("session.tool.success", "call_1")
	if !h.tools.any() {
		t.Fatal("the second call was dropped when the first ended")
	}
	feed("session.tool.failed", "call_2")
	if h.tools.any() {
		t.Fatal("a failed call stayed in flight")
	}
}

// A missed end event must not pin the row beyond the turn.
func TestStepEndClearsALostToolEnd(t *testing.T) {
	for _, end := range []string{"session.step.ended", "session.execution.succeeded", "session.execution.interrupted", "session.idle"} {
		h := progressTestHandle(t, true)
		feed("session.tool.called", "call_1")
		feed(end, "")
		if h.tools.any() {
			t.Errorf("%s left a tool in flight", end)
		}
		handlesMu.Lock()
		delete(handles, h.name)
		handlesMu.Unlock()
	}
}

func TestEventsOfAnotherSessionDoNotStamp(t *testing.T) {
	h := progressTestHandle(t, true)
	noteProgress("session.tool.called", json.RawMessage(`{"sessionID":"ses_other","id":"c"}`))
	if _, ok := h.act.At(); ok || h.tools.any() {
		t.Fatal("another session's event reached this handle")
	}
}

// opencode 1.x was not measured: it reports nothing and keeps holding.
func TestV1StreamIsNotStamped(t *testing.T) {
	h := progressTestHandle(t, false)
	feed("session.tool.called", "call_1")
	if _, ok := (managedDriver{}).ManagedProgress(session.Meta{Name: h.name}); ok {
		t.Fatal("a v1 daemon reported progress")
	}
}

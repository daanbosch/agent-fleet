package codex

import (
	"encoding/json"
	"testing"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
)

func TestNotificationsNamingTheThreadStampItsHandle(t *testing.T) {
	h := &threadHandle{name: "prog-a", tid: "thr_a", alive: true, events: make(chan agents.Event, 8)}
	other := &threadHandle{name: "prog-b", tid: "thr_b", alive: true, events: make(chan agents.Event, 8)}
	registerCodexTestHandle(t, h)
	registerCodexTestHandle(t, other)

	// The account-wide notifications of the measured run (rate limits, MCP startup) name no
	// thread and must not make any session look alive.
	dispatchNotification(rpcMsg{Method: "account/rateLimits/updated", Params: json.RawMessage(`{"rateLimits":{}}`)})
	dispatchNotification(rpcMsg{Method: "mcpServer/startupStatus/updated", Params: json.RawMessage(`{"name":"x"}`)})
	if _, ok := h.act.At(); ok {
		t.Fatal("an account-wide notification stamped a session")
	}

	dispatchNotification(rpcMsg{Method: "item/completed", Params: json.RawMessage(`{"threadId":"thr_a","item":{"type":"commandExecution"}}`)})
	if _, ok := h.act.At(); !ok {
		t.Fatal("a notification naming the thread did not stamp its handle")
	}
	if _, ok := other.act.At(); ok {
		t.Fatal("a notification stamped another session's handle")
	}
}

func TestManagedProgressReportsTheLastNotification(t *testing.T) {
	m := session.Meta{Name: "prog-c"}
	if _, ok := (managedDriver{}).ManagedProgress(m); ok {
		t.Fatal("answered without a handle")
	}
	h := &threadHandle{name: m.Name, tid: "thr_c", alive: true, events: make(chan agents.Event, 8)}
	registerCodexTestHandle(t, h)
	if _, ok := (managedDriver{}).ManagedProgress(m); ok {
		t.Fatal("answered for a thread never heard from")
	}
	dispatchNotification(rpcMsg{Method: "item/started", Params: json.RawMessage(`{"threadId":"thr_c"}`)})
	if _, ok := (managedDriver{}).ManagedProgress(m); !ok {
		t.Fatal("no answer after a notification")
	}
}

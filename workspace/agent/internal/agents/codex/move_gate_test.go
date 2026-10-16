package codex

// The pump's own lease on the session's lifecycle gate (ADR 0115 decision 11.1). The input path
// that queued an entry let go of its lease at the accept, so the pump is what a move has to wait
// for: an entry whose turn/start is out is in the thread before the move closes input, and one
// still queued stays there, where the move's snapshot reads it.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/lifegate"
)

func TestPumpStartsBeforeAMoveOrLeavesTheEntryQueued(t *testing.T) {
	m, cl := newMockCodexServer(t)
	hold := make(chan struct{})
	m.holdStart = hold
	h := newCodexTestHandle(t, cl, "codex-move-1")
	registerCodexTestHandle(t, h)
	t.Cleanup(func() { lifegate.Reopen(h.name) }) // the gate's memory is process-wide
	held := true
	releaseStart := func() {
		if held {
			held = false
			close(hold)
		}
	}
	t.Cleanup(releaseStart) // a failed assertion must not leave the mock parked in turn/start

	if err := h.Send(memberInput("one", "af_one")); err != nil {
		t.Fatal(err)
	}
	waitCodexCalls(t, m, "turn/start", 1) // committed and sent, not yet answered

	locked := make(chan *lifegate.Exclusive, 1)
	go func() {
		x, err := lifegate.Lock(context.Background(), h.name)
		if err != nil {
			t.Errorf("Lock: %v", err)
		}
		locked <- x
	}()
	deadline := time.Now().Add(waitBackstop)
	// Or the move already has the gate, which the select below reports.
	for lifegate.WritersWaiting(h.name) == 0 && len(locked) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the move never started waiting")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case x := <-locked:
		x.Unlock()
		t.Fatal("the move got the gate before turn/start was answered")
	case <-time.After(50 * time.Millisecond):
	}
	releaseStart()
	var x *lifegate.Exclusive
	select {
	case x = <-locked:
	case <-time.After(waitBackstop):
		t.Fatal("the pump kept its lease past turn/start; a running turn must not hold a move off")
	}
	x.SetClosed("codex-move-1-target", false)
	x.Unlock()

	// Accepted after input closed (the HTTP gate sits above this layer): it must stay queued.
	if err := h.Send(memberInput("two", "af_two")); err != nil {
		t.Fatal(err)
	}
	completeTurn(t, m, h, 1)
	waitCodexState(t, h, agents.TurnCompleted)
	time.Sleep(100 * time.Millisecond) // room for a wrongly started second turn/start
	if got := m.callCount("turn/start"); got != 1 {
		t.Fatalf("turn/start count while closed = %d, want 1", got)
	}
	h.mu.Lock()
	items := h.tq().Items()
	h.mu.Unlock()
	if len(items) != 1 || items[0].ID != "af_two" {
		t.Fatalf("queue while closed = %+v, want af_two still queued for the snapshot", items)
	}

	// The move rolls back: the parked pump starts again.
	lifegate.Reopen(h.name)
	completeTurn(t, m, h, 2)
	waitPumpDone(t, h)
	if got := fmt.Sprint(sentTurns(m)); got != "[one two]" {
		t.Fatalf("turns = %s, want one then two, each once", got)
	}
}

package opencode

// The pump's own lease on the session's lifecycle gate (ADR 0115 decision 11.1). The input path
// that queued an entry let go of its lease at the accept, so the pump is what a move has to wait
// for: an entry held behind another client's turn (waitIdle) goes to serve before the move
// closes input, and one still queued stays there, where the move's snapshot reads it.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/lifegate"
)

// pumpLease takes the lease the pump would hold, for a test that calls runTurn itself.
func pumpLease(t *testing.T, h *threadHandle) *lifegate.Lease {
	t.Helper()
	l, err := lifegate.Enter(context.Background(), h.name)
	if err != nil {
		t.Fatalf("lease on %s: %v", h.name, err)
	}
	t.Cleanup(l.Release)
	return l
}

func TestPumpSendsBeforeAMoveOrLeavesTheEntryQueued(t *testing.T) {
	m, srv := newMockServe(t)
	m.turnDelay = 30 * time.Second // runs until endTurn
	h := newTestHandle(t, srv)
	h.name = "oc-move-1"
	t.Cleanup(func() { lifegate.Reopen(h.name) }) // the gate's memory is process-wide
	// Runs before newTestHandle's wait for the pump: a failed assertion must not leave the pump
	// parked in waitIdle or in a turn serve never ends.
	t.Cleanup(func() { endForeignTurn(m); endTurn(m) })

	heldBehindAForeignTurn(t, m, h, memberInput("one", "msg_one"))

	locked := make(chan *lifegate.Exclusive, 1)
	go func() {
		x, err := lifegate.Lock(context.Background(), h.name)
		if err != nil {
			t.Errorf("Lock: %v", err)
		}
		locked <- x
	}()
	deadline := time.Now().Add(10 * time.Second)
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
		t.Fatal("the move got the gate while the pump held an entry behind another client's turn")
	case <-time.After(50 * time.Millisecond):
	}
	endForeignTurn(m)
	var x *lifegate.Exclusive
	select {
	case x = <-locked:
	case <-time.After(10 * time.Second):
		t.Fatal("the pump kept its lease past /message; a running turn must not hold a move off")
	}
	waitTurns(t, m, 1)
	x.SetClosed("oc-move-1-target", false)
	x.Unlock()

	// Accepted after input closed (the HTTP gate sits above this layer): it must stay queued.
	if err := h.Send(memberInput("two", "msg_two")); err != nil {
		t.Fatal(err)
	}
	endTurn(m)
	waitPumpIdle(t, h)
	if got := fmt.Sprint(sentTurns(m)); got != "[one]" {
		t.Fatalf("turns while closed = %s, want only one", got)
	}
	h.mu.Lock()
	items := h.tq().Items()
	h.mu.Unlock()
	if len(items) != 1 || items[0].ID != "msg_two" {
		t.Fatalf("queue while closed = %+v, want msg_two still queued for the snapshot", items)
	}

	// The move rolls back: the parked pump starts again.
	lifegate.Reopen(h.name)
	waitTurns(t, m, 2)
	endTurn(m)
	waitPumpIdle(t, h)
	if got := fmt.Sprint(sentTurns(m)); got != "[one two]" {
		t.Fatalf("turns = %s, want one then two, each once", got)
	}
	waitState(t, h, agents.TurnCompleted)
}

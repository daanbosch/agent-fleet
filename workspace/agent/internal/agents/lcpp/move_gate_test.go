package lcpp

// The pump's own lease on the session's lifecycle gate (ADR 0115 decision 11.1). The input path
// that queued an entry let go of its lease at the accept, so the pump is what a move has to wait
// for: an entry about to be committed is in the store before the move closes input, and one
// still queued stays there, where the move's snapshot reads it.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/lifegate"
)

func TestPumpStoresBeforeAMoveOrLeavesTheEntryQueued(t *testing.T) {
	h, c, _ := startHandle(t, "lcpp-move-1")
	t.Cleanup(func() { lifegate.Reopen(h.name) }) // the gate's memory is process-wide
	taken, rel := holdAtCommit(h)
	var once sync.Once
	release := func() { once.Do(rel) }
	t.Cleanup(release) // a failed assertion must not leave the pump parked before its commit

	mustSend(t, h, member("m1", "one"))
	<-taken // taken, not committed: the pump holds its lease

	locked := make(chan *lifegate.Exclusive, 1)
	go func() {
		x, err := lifegate.Lock(context.Background(), h.name)
		if err != nil {
			t.Errorf("Lock: %v", err)
		}
		locked <- x
	}()
	deadline := time.Now().Add(hangGuard)
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
		t.Fatal("the move got the gate while the pump was about to commit an entry")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	expectStarted(t, c, "one") // in the store and at the engine
	var x *lifegate.Exclusive
	select {
	case x = <-locked:
	case <-time.After(hangGuard):
		t.Fatal("the pump kept its lease past the store; a running turn must not hold a move off")
	}
	x.SetClosed("lcpp-move-1-target", false)
	x.Unlock()

	// Accepted after input closed (the HTTP gate sits above this layer): it must stay queued.
	mustSend(t, h, member("m2", "two"))
	c.answer(t)
	expectNoStart(t, h, c)
	h.mu.Lock()
	items := h.q.Items()
	h.mu.Unlock()
	if !equal(ids(items), []string{"m2"}) {
		t.Fatalf("queue while closed = %v, want m2 still queued for the snapshot", ids(items))
	}

	// The move rolls back: the parked pump starts again.
	lifegate.Reopen(h.name)
	expectStarted(t, c, "two")
	c.answer(t)
	if items, _, _ := settled(t, h); len(items) != 0 {
		t.Fatalf("queue after the reopen = %v, want empty", ids(items))
	}
	if got := c.seen(); !equal(got, []string{"one", "two"}) {
		t.Fatalf("engine saw %q, want one then two, each once", got)
	}
}

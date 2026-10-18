package memreview

import (
	"context"
	"errors"
	"testing"
)

// A run that leaves proposals in the queue says so once, with counts; a run that queues nothing
// says nothing.
func TestAReviewTellsTheQueueWhenItLeavesProposals(t *testing.T) {
	type call struct {
		run             string
		queued, pending int
	}
	var calls []call
	OnQueued = func(run string, queued, pending int) error {
		calls = append(calls, call{run, queued, pending})
		return nil
	}
	t.Cleanup(func() { OnQueued = nil })

	h := newHarness(t)
	res, err := h.reviewer().Review(context.Background(), h.req(TriggerAuto, turns(4)))
	if err != nil || res.Queued != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if len(calls) != 1 || calls[0].run != res.RunID || calls[0].queued != 1 || calls[0].pending != 1 {
		t.Fatalf("calls = %+v", calls)
	}

	// A window with nothing to propose: no call.
	h2 := newHarness(t)
	h2.replies = []func() (string, error){say(`{"ops":[]}`)}
	calls = nil
	if res, err := h2.reviewer().Review(context.Background(), h2.req(TriggerAuto, turns(4))); err != nil || res.Queued != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if len(calls) != 0 {
		t.Fatalf("calls = %+v", calls)
	}
}

// A stop after the run was saved as done and before its notification was made must not lose the
// notification: the pending mark is in the write that ends the run, and the reconcile finishes it
// once, even after more restarts.
func TestAStopBeforeTheNotificationDoesNotLoseIt(t *testing.T) {
	var calls []string
	OnQueued = func(run string, queued, pending int) error { calls = append(calls, run); return nil }
	t.Cleanup(func() { OnQueued = nil })

	h := newHarness(t)
	if !h.reviewUntilCrash(h.reviewer(), "ended", h.req(TriggerAuto, turns(4))) {
		t.Fatal("no crash at the ended stage")
	}
	if len(calls) != 0 || len(Pending()) != 1 {
		t.Fatalf("setup: calls %v, pending %d", calls, len(Pending()))
	}
	// Positive control: the crashed run is done, so nothing else would announce it.
	rs := runs()
	if len(rs) != 1 || !rs[0].Terminal() || !rs[0].NotifyPending {
		t.Fatalf("runs = %+v", rs)
	}
	if len(rs[0].Ops) != 0 {
		t.Fatalf("a finished run still carries its operations: %+v", rs[0])
	}

	ReconcileNotices(h.now)
	if len(calls) != 1 || calls[0] != rs[0].ID {
		t.Fatalf("calls after the reconcile = %v", calls)
	}
	if runs()[0].NotifyPending {
		t.Fatal("the mark was not cleared")
	}
	ReconcileNotices(h.now)
	if len(calls) != 1 {
		t.Fatalf("announced again: %v", calls)
	}
}

// The notification's outbox failing keeps the mark and retries; a run whose proposals were all
// decided meanwhile announces nothing.
func TestAFailedNotificationIsRetriedAndNothingIsAnnouncedWhenAllWasDecided(t *testing.T) {
	failing := true
	var calls int
	OnQueued = func(string, int, int) error {
		calls++
		if failing {
			return errors.New("outbox full")
		}
		return nil
	}
	t.Cleanup(func() { OnQueued = nil })

	h := newHarness(t)
	if _, err := h.reviewer().Review(context.Background(), h.req(TriggerAuto, turns(4))); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !runs()[0].NotifyPending {
		t.Fatalf("calls %d, mark %v", calls, runs()[0].NotifyPending)
	}
	ReconcileNotices(h.now)
	if calls != 2 || !runs()[0].NotifyPending {
		t.Fatalf("calls %d, mark %v", calls, runs()[0].NotifyPending)
	}
	failing = false
	ReconcileNotices(h.now)
	if calls != 3 || runs()[0].NotifyPending {
		t.Fatalf("calls %d, mark %v", calls, runs()[0].NotifyPending)
	}

	// Decided before the notification could be made: nothing to say, and the mark goes.
	h2 := newHarness(t)
	failing = true
	calls = 0
	if _, err := h2.reviewer().Review(context.Background(), h2.req(TriggerAuto, turns(4))); err != nil {
		t.Fatal(err)
	}
	for _, p := range Pending() {
		if err := Reject(p.ID, h2.now); err != nil {
			t.Fatal(err)
		}
	}
	before := calls
	ReconcileNotices(h2.now)
	if calls != before || runs()[0].NotifyPending {
		t.Fatalf("calls %d (was %d), mark %v", calls, before, runs()[0].NotifyPending)
	}
}

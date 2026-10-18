package memreview

import (
	"context"
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
	OnQueued = func(run string, queued, pending int) { calls = append(calls, call{run, queued, pending}) }
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

package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/notice"
)

// The notification of a run that left proposals is about the workspace and carries counts only,
// once per run.
func TestMemoryReviewNoticeCarriesCountsOnlyAndIsRaisedOncePerRun(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	before := len(notice.List())
	notifyMemoryReview("0123456789abcdef0123", 2, 5)
	notifyMemoryReview("0123456789abcdef0123", 2, 5) // a replayed run
	notifyMemoryReview("aaaaaaaaaaaaaaaaaaaa", 1, 6)
	evs := notice.List()
	if len(evs) != before+2 {
		t.Fatalf("events = %d, want %d", len(evs), before+2)
	}
	var mine []notice.Event
	for _, e := range evs {
		if e.Kind == "memory-review" {
			mine = append(mine, e)
		}
	}
	if len(mine) != 2 {
		t.Fatalf("memory-review events = %d", len(mine))
	}
	e := mine[0]
	if e.TargetType != "workspace" || e.SessionName != "" {
		t.Fatalf("event = %+v", e)
	}
	b, _ := json.Marshal(e.Payload)
	if string(b) != `{"pending":5,"queued":2}` && string(b) != `{"pending":6,"queued":1}` {
		t.Fatalf("payload = %s", b)
	}
	if strings.Contains(string(b), "0123456789") {
		t.Fatal("the payload names the run")
	}
}

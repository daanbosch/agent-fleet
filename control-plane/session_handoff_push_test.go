package main

// "Push, then send" (#2072) through the CP's offer handler, with the owner's Agent replaced by
// the lifecycle stub: the offer is stored only on the verdict the Agent builds after its push,
// and a refused push leaves no offer behind.

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

const pushedSha = "fedcba9876543210"

// pushClears makes the stub Agent behave like a push that succeeded: the next handoff-context
// reports the gate open on a new HEAD.
func (f *handoffLife) pushClears() {
	f.push = func() (int, map[string]any) {
		delete(f.agentCtx, "blocked")
		f.agentCtx["headSha"] = pushedSha
		return http.StatusOK, map[string]any{"pushed": true, "remote": "origin", "branch": "feature/x"}
	}
}

func (f *handoffLife) offerRows() int {
	f.t.Helper()
	rows, err := f.st.ListSessionHandoffOffersByOwner(context.Background(), f.owner.mv.MembershipID)
	if err != nil {
		f.t.Fatal(err)
	}
	return len(rows)
}

func (f *handoffLife) pushHits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits["/sessions/session-1/handoff-push"]
}

func TestHandoffPushThenSendStoresThePushedHead(t *testing.T) {
	for _, reason := range []string{"no_upstream", "unpushed_commits"} {
		t.Run(reason, func(t *testing.T) {
			f := newHandoffLife(t)
			f.setAgent(map[string]any{"blocked": reason})
			f.pushClears()
			o := f.mustOffer(map[string]any{"pushFirst": true})
			if o.HeadSha != pushedSha || f.row(o.ID).HeadSha != pushedSha {
				t.Fatalf("offer head = %s (row %s), want the pushed %s", o.HeadSha, f.row(o.ID).HeadSha, pushedSha)
			}
			if f.pushHits() != 1 {
				t.Fatalf("push hits = %d, want 1", f.pushHits())
			}
		})
	}
}

// TestHandoffPushFailureStoresNoOffer: a rejected push (non-fast-forward, protected branch,
// pre-push hook) reaches the owner as git reported it, and nothing is sent.
func TestHandoffPushFailureStoresNoOffer(t *testing.T) {
	f := newHandoffLife(t)
	f.setAgent(map[string]any{"blocked": "unpushed_commits"})
	const gitSaid = " ! [rejected]        feature/x -> feature/x (non-fast-forward)"
	f.push = func() (int, map[string]any) {
		return http.StatusConflict, map[string]any{"error": map[string]string{"code": "push_rejected", "message": gitSaid}}
	}
	rec := f.offer(map[string]any{"pushFirst": true})
	f.requireConflict(rec, "handoff_push_failed", "")
	if e := errorOf(t, rec); e["reason"] != "push_rejected" || !strings.Contains(e["message"].(string), "non-fast-forward") {
		t.Fatalf("push failure answered %v, want reason push_rejected with git's message", e)
	}
	if n := f.offerRows(); n != 0 {
		t.Fatalf("%d offers stored after a failed push", n)
	}
}

// The push is opt-in and only for the reasons it can clear; anything that would refuse the
// offer anyway is refused before the push, so such a click publishes nothing.
func TestHandoffPushOnlyWhenItCanLeadToAnOffer(t *testing.T) {
	f := newHandoffLife(t)
	f.pushClears()

	f.setAgent(map[string]any{"blocked": "no_upstream"})
	f.requireConflict(f.offer(nil), "handoff_blocked", "") // not asked for

	f.setAgent(map[string]any{"blocked": "detached_head"})
	f.requireConflict(f.offer(map[string]any{"pushFirst": true, "ackWarning": true}), "handoff_blocked", "")

	f.setAgent(map[string]any{"blocked": "no_upstream", "warning": "uncommitted_changes"})
	f.requireConflict(f.offer(map[string]any{"pushFirst": true}), "handoff_warning", "")
	if f.pushHits() != 0 {
		t.Fatalf("push hits = %d before any offer could be sent, want 0", f.pushHits())
	}

	f.setAgent(map[string]any{"blocked": nil, "warning": nil})
	f.mustOffer(nil)
	f.setAgent(map[string]any{"blocked": "unpushed_commits"})
	f.requireConflict(f.offer(map[string]any{"pushFirst": true}), "handoff_already_pending", "")
	if f.pushHits() != 0 {
		t.Fatalf("push hits = %d with an offer already pending, want 0", f.pushHits())
	}
}

// After the push the verdict is the Agent's again: a gate that still blocks (a commit landed
// meanwhile) stops the offer.
func TestHandoffPushStillBlockedStoresNoOffer(t *testing.T) {
	f := newHandoffLife(t)
	f.setAgent(map[string]any{"blocked": "unpushed_commits"})
	f.push = func() (int, map[string]any) {
		return http.StatusOK, map[string]any{"pushed": true}
	}
	rec := f.offer(map[string]any{"pushFirst": true})
	f.requireConflict(rec, "handoff_blocked", "")
	if n := f.offerRows(); n != 0 || f.pushHits() != 1 {
		t.Fatalf("rows=%d pushHits=%d, want 0 and 1", n, f.pushHits())
	}
}

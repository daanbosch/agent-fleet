package opencode

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
)

const regionMsg = "Upstream request failed: This Go model requires Global regions. Select Global in your workspace's Privacy settings to use it."

func TestPermanentRetry(t *testing.T) {
	for msg, want := range map[string]bool{
		regionMsg: true,
		"You exceeded your current quota, please check your plan and billing details.": true,
		"Insufficient balance. Manage your billing here.":                              true,
		"Incorrect API key provided: sk-xxx.":                                          true,
		// measured transient: these keep retrying and recover
		"stub upstream exploded":                    false,
		"Rate limit reached for requests":           false,
		"Service unavailable":                       false,
		"Provider is overloaded":                    false,
		"Upstream request failed: connection reset": false,
		"": false,
	} {
		if got := permanentRetry(msg); got != want {
			t.Errorf("permanentRetry(%q) = %v, want %v", msg, got, want)
		}
	}
}

func sessionStatusEvent(status string) []byte {
	return []byte(fmt.Sprintf(`{"payload":{"type":"session.status","properties":{"sessionID":"ses_test","status":%s}}}`, status))
}

func retryStatus(attempt int, msg string) []byte {
	return sessionStatusEvent(fmt.Sprintf(`{"type":"retry","attempt":%d,"message":%q,"next":1791949793161}`, attempt, msg))
}

func enriched(h *threadHandle) *agents.RetryInfo {
	td := agents.TranscriptData{}
	managedEnrich(session.Meta{Name: h.name, Dir: h.dir, Kind: session.KindOpencode, Driver: session.DriverManaged}, &td)
	return td.Retry
}

// A transient provider error shows while it is retried and is gone once output arrives.
func TestRetryIsShownWhileItLastsAndClearsOnOutput(t *testing.T) {
	m, srv := newMockServe(t)
	h := newTestHandle(t, srv)
	registerTestHandle(t, h)
	startBlocking(t, m, h)

	handleServeEvent(retryStatus(5, "stub upstream exploded"))
	got := enriched(h)
	if got == nil || got.Attempt != 5 || got.Message != "stub upstream exploded" || got.Failed || got.NextMs != 1791949793161 {
		t.Fatalf("Retry = %+v, want attempt 5 with the upstream message", got)
	}
	// opencode goes busy between two attempts: that is not the end of the retry.
	handleServeEvent(sessionStatusEvent(`{"type":"busy"}`))
	if enriched(h) == nil {
		t.Fatal("a busy status between attempts cleared the retry")
	}
	handleServeEvent([]byte(`{"payload":{"type":"message.part.updated","properties":{"part":{"sessionID":"ses_test","type":"text"}}}}`))
	if got := enriched(h); got != nil {
		t.Fatalf("Retry = %+v after output, want none", got)
	}
	if m.aborts != 0 {
		t.Fatalf("aborts = %d, a transient error must keep retrying", m.aborts)
	}
	endTurn(m)
	waitState(t, h, agents.TurnCompleted)
}

func TestRetryClearsWhenTheTurnEnds(t *testing.T) {
	m, srv := newMockServe(t)
	h := newTestHandle(t, srv)
	registerTestHandle(t, h)
	startBlocking(t, m, h)
	handleServeEvent(retryStatus(3, "Service unavailable"))
	endTurn(m)
	waitState(t, h, agents.TurnCompleted)
	if got := enriched(h); got != nil {
		t.Fatalf("Retry = %+v after the turn ended, want none", got)
	}
}

// A message that can never succeed ends the turn as failed, with that message, instead of
// leaving the session retrying for good.
func TestPermanentRetryFailsTheTurn(t *testing.T) {
	m, srv := newMockServe(t)
	h := newTestHandle(t, srv)
	registerTestHandle(t, h)
	failed := make(chan string, 4)
	agents.SetStateNotifier(func(sid, previous, state, excerpt string) {
		if state == agents.StateFailed {
			failed <- excerpt
		}
	})
	t.Cleanup(func() { agents.SetStateNotifier(nil) })
	startBlocking(t, m, h)

	// Seen once it could still be a flaky gateway.
	handleServeEvent(retryStatus(1, regionMsg))
	time.Sleep(50 * time.Millisecond)
	m.mu.Lock()
	aborts := m.aborts
	m.mu.Unlock()
	if aborts != 0 {
		t.Fatalf("aborts = %d after the first attempt, want 0", aborts)
	}

	handleServeEvent(retryStatus(2, regionMsg))
	waitState(t, h, agents.TurnFailed)
	select {
	case excerpt := <-failed:
		if !strings.Contains(excerpt, "requires Global regions") {
			t.Errorf("failure = %q, want the upstream message", excerpt)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the failed turn was not reported")
	}
	got := enriched(h)
	if got == nil || !got.Failed || got.Message != regionMsg {
		t.Fatalf("Retry = %+v, want the failed retry kept for the mirror", got)
	}

	// The next input starts clean.
	m.turnDelay = 0
	if err := h.Send(memberInput("again", "msg_again")); err != nil {
		t.Fatal(err)
	}
	waitState(t, h, agents.TurnCompleted)
	if got := enriched(h); got != nil {
		t.Fatalf("Retry = %+v after the next turn, want none", got)
	}
}

// The user's own stop is a cancellation even when a permanent message was seen first.
func TestStopWinsOverAPermanentRetry(t *testing.T) {
	m, srv := newMockServe(t)
	h := newTestHandle(t, srv)
	registerTestHandle(t, h)
	m.holdAbort = true
	startBlocking(t, m, h)
	if _, err := h.Interrupt(agents.InterruptOpts{}); err != nil {
		t.Fatal(err)
	}
	handleServeEvent(retryStatus(2, regionMsg))
	m.mu.Lock()
	m.holdAbort = false
	m.mu.Unlock()
	endTurn(m)
	waitState(t, h, agents.TurnCancelled)
}

// A retry on a session this handle is not running (an attached TUI's turn) is not ours to fail.
func TestRetryOutsideOurTurnIsIgnored(t *testing.T) {
	m, srv := newMockServe(t)
	h := newTestHandle(t, srv)
	registerTestHandle(t, h)
	handleServeEvent(retryStatus(9, regionMsg))
	if got := enriched(h); got != nil {
		t.Fatalf("Retry = %+v with no turn running", got)
	}
	if m.aborts != 0 {
		t.Fatalf("aborts = %d, want 0", m.aborts)
	}
}

// v2 announces the next attempt as session.retry.scheduled (testdata/v2/fail-server-error).
func TestV2RetryScheduled(t *testing.T) {
	m, srv := newMockServe(t)
	h := newTestHandle(t, srv)
	registerTestHandle(t, h)
	startBlocking(t, m, h)
	handleServeEvent([]byte(`{"type":"session.retry.scheduled","data":{"sessionID":"ses_test","attempt":4,"at":1791931612994,"error":{"type":"provider.internal","message":"stub upstream exploded","status":500}}}`))
	got := enriched(h)
	if got == nil || got.Attempt != 4 || got.NextMs != 1791931612994 || got.Message != "stub upstream exploded" {
		t.Fatalf("Retry = %+v, want attempt 4", got)
	}
	handleServeEvent([]byte(`{"type":"session.step.ended","data":{"sessionID":"ses_test"}}`))
	if enriched(h) != nil {
		t.Fatal("a finished step must clear the retry")
	}
	handleServeEvent([]byte(`{"type":"session.retry.scheduled","data":{"sessionID":"ses_test","attempt":2,"at":1,"error":{"type":"provider.internal","message":"` + regionMsg + `","status":500}}}`))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		n := m.aborts
		m.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("a permanent message on v2 must abort the turn too")
}

// The /global/event line exactly as 1.19.0 sent it while retrying (loopback stub answering 500).
func TestRetryFromTheMeasuredGlobalEvent(t *testing.T) {
	m, srv := newMockServe(t)
	h := newTestHandle(t, srv)
	registerTestHandle(t, h)
	startBlocking(t, m, h)
	handleServeEvent([]byte(`{"directory":"/work","project":"global","payload":{"id":"evt_1388b229000174j6X2BlRro2E4","type":"session.status","properties":{"sessionID":"ses_test","status":{"type":"retry","attempt":2,"message":"Upstream request failed: connection reset","next":1791950009293}}}}`))
	got := enriched(h)
	if got == nil || got.Attempt != 2 || got.Message != "Upstream request failed: connection reset" {
		t.Fatalf("Retry = %+v, want the measured retry", got)
	}
	endTurn(m)
}

// While the pump waits behind another client's turn (an attached TUI's), h.running is true but
// no /message of ours is out: that client's retry is not ours to show or to abort.
func TestRetryBehindAnotherClientsTurnIsNotOurs(t *testing.T) {
	m, srv := newMockServe(t)
	h := newTestHandle(t, srv)
	registerTestHandle(t, h)
	m.mu.Lock()
	m.busy = true // the foreign turn
	m.mu.Unlock()
	if err := h.Send(memberInput("queued", "msg_q")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		polls := m.statusPolls
		m.mu.Unlock()
		if polls > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	handleServeEvent(retryStatus(2, regionMsg))
	m.mu.Lock()
	aborts, turns := m.aborts, len(m.turns)
	m.mu.Unlock()
	if aborts != 0 || turns != 0 {
		t.Fatalf("aborts=%d own messages=%d, want neither: the retry is the other client's", aborts, turns)
	}
	if got := enriched(h); got != nil {
		t.Fatalf("Retry = %+v, want none for another client's turn", got)
	}
	m.mu.Lock()
	m.busy = false
	m.mu.Unlock()
	waitState(t, h, agents.TurnCompleted)
}

// A stop leaves no retry note behind, whichever came first.
func TestStopLeavesNoRetryNoteStopFirst(t *testing.T)  { stopLeavesNoRetryNote(t, true) }
func TestStopLeavesNoRetryNoteRetryFirst(t *testing.T) { stopLeavesNoRetryNote(t, false) }

// (not a subtest: the handle's sid is built from t.Name(), which must not contain a slash)
func stopLeavesNoRetryNote(t *testing.T, stopFirst bool) {
	{
		{
			m, srv := newMockServe(t)
			h := newTestHandle(t, srv)
			registerTestHandle(t, h)
			m.holdAbort = true
			startBlocking(t, m, h)
			if stopFirst {
				if _, err := h.Interrupt(agents.InterruptOpts{}); err != nil {
					t.Fatal(err)
				}
				handleServeEvent(retryStatus(2, regionMsg))
			} else {
				handleServeEvent(retryStatus(2, regionMsg))
				if _, err := h.Interrupt(agents.InterruptOpts{}); err != nil {
					t.Fatal(err)
				}
			}
			if got := enriched(h); got != nil {
				t.Fatalf("Retry = %+v right after the stop, want none", got)
			}
			m.mu.Lock()
			m.holdAbort = false
			m.mu.Unlock()
			endTurn(m)
			waitState(t, h, agents.TurnCancelled)
			if got := enriched(h); got != nil {
				t.Fatalf("Retry = %+v after the cancelled turn, want none", got)
			}
		}
	}
}

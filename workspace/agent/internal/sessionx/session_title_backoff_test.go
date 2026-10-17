package sessionx

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/transcript"
)

func titleTurns(n int) []transcript.Turn {
	out := make([]transcript.Turn, n)
	for i := range out {
		out[i] = transcript.Turn{Role: []string{"user", "assistant"}[i%2], Text: "turn"}
	}
	return out
}

func resetTitleGen(t *testing.T, name string) {
	t.Helper()
	titleGenMu.Lock()
	delete(titleGenState, name)
	titleGenMu.Unlock()
	t.Cleanup(func() {
		titleGenMu.Lock()
		delete(titleGenState, name)
		titleGenMu.Unlock()
	})
}

func stubTitleFn(t *testing.T, fn func(context.Context, []transcript.Turn) (string, error)) {
	t.Helper()
	prev := titleSuggestLLM
	titleSuggestLLM = fn
	t.Cleanup(func() { titleSuggestLLM = prev })
}

// A failed automatic attempt must not lock the user's button out (#2061: it answered 409 "already
// in progress" for the whole backoff).
func TestManualTitleBypassesAutoBackoff(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AF_SESSIONS_DIR", t.TempDir())
	const name = "backoff-manual"
	resetTitleGen(t, name)
	calls := 0
	stubTitleFn(t, func(context.Context, []transcript.Turn) (string, error) {
		calls++
		if calls == 1 {
			return "", errors.New("boom")
		}
		return "Real title", nil
	})
	turns := titleTurns(4)
	generateSessionTitle(name, turns, 4) // auto fails -> backoff
	if titleGenReady(name, 4) {
		t.Fatal("precondition: the automatic trigger should be backing off")
	}
	got, err := generateTitleNow(context.Background(), name, turns, nil)
	if err != nil || got != "Real title" || calls != 2 {
		t.Fatalf("got %q err=%v calls=%d, want the manual press to reach the model", got, err, calls)
	}
	if !titleGenReady(name, 4) {
		t.Error("a successful manual generation should clear the backoff")
	}
}

func TestManualTitleWhileInFlightIsBusy(t *testing.T) {
	const name = "backoff-busy"
	resetTitleGen(t, name)
	started, release := make(chan struct{}), make(chan struct{})
	stubTitleFn(t, func(context.Context, []transcript.Turn) (string, error) {
		close(started)
		<-release
		return "T", nil
	})
	turns := titleTurns(4)
	if !titleGenClaim(name, 4) {
		t.Fatal("claim failed")
	}
	go func() { _, _ = titleSuggestLLM(context.Background(), turns) }()
	<-started
	_, err := generateTitleNow(context.Background(), name, turns, nil)
	rr := httptest.NewRecorder()
	writeTitleGenErr(rr, err)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "busy") {
		t.Fatalf("status %d body %s, want 409 busy", rr.Code, rr.Body.String())
	}
	close(release)
	titleGenDone(name, true, 4)
}

func TestManualTitleEmptyReplyHasItsOwnCode(t *testing.T) {
	const name = "backoff-empty"
	resetTitleGen(t, name)
	stubTitleLLM(t, "")
	_, err := generateTitleNow(context.Background(), name, titleTurns(4), nil)
	rr := httptest.NewRecorder()
	writeTitleGenErr(rr, err)
	if rr.Code != http.StatusUnprocessableEntity || !strings.Contains(rr.Body.String(), "title_empty") {
		t.Fatalf("status %d body %s, want 422 title_empty", rr.Code, rr.Body.String())
	}
	// A real CLI failure keeps the old code.
	stubTitleFn(t, func(context.Context, []transcript.Turn) (string, error) { return "", errors.New("x") })
	_, err = generateTitleNow(context.Background(), name, titleTurns(4), nil)
	rr = httptest.NewRecorder()
	writeTitleGenErr(rr, err)
	if rr.Code != http.StatusInternalServerError || !strings.Contains(rr.Body.String(), "generation_failed") {
		t.Fatalf("status %d body %s, want 500 generation_failed", rr.Code, rr.Body.String())
	}
}

func TestAutoTitleBackoffGrowsCapsAndResetsOnNewTurns(t *testing.T) {
	const name = "backoff-grow"
	resetTitleGen(t, name)
	wait := func() time.Duration {
		titleGenMu.Lock()
		defer titleGenMu.Unlock()
		return time.Until(titleGenState[name].nextAttempt)
	}
	expire := func() {
		titleGenMu.Lock()
		e := titleGenState[name]
		e.nextAttempt = time.Time{}
		titleGenState[name] = e
		titleGenMu.Unlock()
	}
	var prev time.Duration
	for i := 0; i < 12; i++ {
		if !titleGenClaim(name, 4) {
			t.Fatalf("attempt %d refused", i)
		}
		titleGenDone(name, false, 4)
		w := wait()
		if w > titleGenBackoffMax {
			t.Fatalf("wait %v exceeds the cap", w)
		}
		if i > 0 && i < 4 && w < prev+prev/2 {
			t.Fatalf("attempt %d: wait %v did not grow from %v", i, w, prev)
		}
		prev = w
		expire()
	}
	if prev < titleGenBackoffMax-time.Minute {
		t.Errorf("wait %v never reached the cap %v", prev, titleGenBackoffMax)
	}
	// New turns restart the schedule from the base.
	if !titleGenClaim(name, 6) {
		t.Fatal("claim refused after growth")
	}
	titleGenDone(name, false, 6)
	if w := wait(); w > titleGenBackoff {
		t.Errorf("wait after growth = %v, want the base %v", w, titleGenBackoff)
	}
}

// Growth must lift a deadline that is still in the future (not only shorten the next one), and
// polling the same input again must keep the wait.
func TestAutoTitleGrowthLiftsTheRunningDeadline(t *testing.T) {
	const name = "backoff-grow-deadline"
	resetTitleGen(t, name)
	for i := 0; i < 8; i++ { // same 4-turn input, failing: wait climbs to the cap
		titleGenMu.Lock()
		e := titleGenState[name]
		e.nextAttempt = time.Time{}
		titleGenState[name] = e
		titleGenMu.Unlock()
		if !titleGenClaim(name, 4) {
			t.Fatalf("attempt %d refused", i)
		}
		titleGenDone(name, false, 4)
	}
	for i := 0; i < 3; i++ { // repeated polls of the unchanged input stay refused
		if titleGenReady(name, 4) || titleGenClaim(name, 4) {
			t.Fatal("unchanged input must stay inside the backoff")
		}
	}
	if !titleGenReady(name, 6) {
		t.Fatal("a grown transcript must pass the cheap pre-check inside the deadline")
	}
	if !titleGenClaim(name, 6) {
		t.Fatal("a grown transcript must be claimable inside the deadline")
	}
	titleGenDone(name, false, 6)
	titleGenMu.Lock()
	w := time.Until(titleGenState[name].nextAttempt)
	titleGenMu.Unlock()
	if w > titleGenBackoff {
		t.Errorf("wait after growth = %v, want the base %v", w, titleGenBackoff)
	}
	if titleGenReady(name, 6) {
		t.Error("the same grown input must not reset again")
	}
}

// Provider errors can carry the model's reply or CLI stderr; the log must name only a class.
func TestAutoTitleFailureLogCarriesNoErrorText(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AF_SESSIONS_DIR", t.TempDir())
	const name = "backoff-log"
	resetTitleGen(t, name)
	stubTitleFn(t, func(context.Context, []transcript.Turn) (string, error) {
		return "", errors.New("cursor execution failed: PRIVATE_REPLY_SENTINEL")
	})
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	generateSessionTitle(name, titleTurns(4), 4)
	if !strings.Contains(buf.String(), "failed") {
		t.Fatalf("expected a failure log line, got %q", buf.String())
	}
	if strings.Contains(buf.String(), "PRIVATE_REPLY_SENTINEL") {
		t.Errorf("log leaked provider error text: %q", buf.String())
	}
}

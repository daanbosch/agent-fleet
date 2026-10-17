package copilot

import (
	"testing"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/limitobs"
)

func resetPublished(t *testing.T) {
	t.Helper()
	clear := func() { tokenPublished.Store(nil); usagePublished.Store(nil) }
	clear()
	t.Cleanup(clear)
}

func publishToken(tok string) { tokenPublished.Store(&tok) }

// Review finding (#2017): Token holds tokenMu across a gh run and HandleUsage holds usageMu
// across the HTTP fetch; the snapshot must not wait on either.
func TestLimitReadingDoesNotWaitOnTokenOrUsageFetch(t *testing.T) {
	resetPublished(t)
	publishToken("t1")
	usagePublished.Store(&usagePub{token: "t1", at: time.Now(),
		res: &usageResult{ResetsAt: "2026-11-01T00:00:00Z", Quotas: []QuotaSnap{{ID: "chat", RemainingPct: 70}}}})
	tokenMu.Lock()
	usageMu.Lock()
	defer tokenMu.Unlock()
	defer usageMu.Unlock()
	done := make(chan limitobs.Reading, 1)
	go func() { done <- LimitReading() }()
	select {
	case r := <-done:
		if len(r.Windows) != 1 || r.Windows[0].UsedPct != 30 {
			t.Fatalf("reading = %+v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("LimitReading waited on tokenMu/usageMu")
	}
}

// A gh login to another account (another token) reports the account with no pools, so the
// CP drops the previous account's pools before anything of the new one was fetched.
func TestLimitReadingTokenChangeReportsTheNewAccountEmpty(t *testing.T) {
	resetPublished(t)
	if r := LimitReading(); !r.NoIdentity {
		t.Fatalf("before gh was ever asked: %+v, want NoIdentity", r)
	}
	publishToken("t1")
	usagePublished.Store(&usagePub{token: "t1", at: time.Now(), res: &usageResult{Quotas: []QuotaSnap{{ID: "chat", RemainingPct: 70}}}})
	before := LimitReading()
	publishToken("t2")
	after := LimitReading()
	if len(before.Windows) != 1 || len(after.Windows) != 0 || after.NoIdentity || after.Identity == before.Identity {
		t.Fatalf("before %+v after %+v", before, after)
	}
}

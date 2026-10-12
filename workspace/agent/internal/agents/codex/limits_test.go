package codex

import (
	"context"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/limitobs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeCodexAuth(t *testing.T, account string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("HOME"), ".codex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	auth := `{"auth_mode":"chatgpt","tokens":{"access_token":"t","account_id":"` + account + `"}}`
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(auth), 0o600); err != nil {
		t.Fatal(err)
	}
}

func resetAccountCache(t *testing.T) {
	t.Helper()
	c := &accountUsageCache
	clear := func() {
		c.Lock()
		c.val, c.ok, c.fetched, c.observed, c.account = usage{}, false, time.Time{}, time.Time{}, ""
		c.Unlock()
		accountPublished.Store(nil)
	}
	clear()
	t.Cleanup(clear)
}

// ADR 0111 decision 6 / issue #1962 acceptance: a failed refresh must not make an old value look
// fresh. The cache used to stamp its age from the last ATTEMPT, so a refresh that failed served
// an hour-old reading as "0 seconds old" for the next five minutes.
func TestAccountUsageFailedRefreshKeepsTheOldObservationTime(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	resetAccountCache(t)
	writeCodexAuth(t, "a")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	defer srv.Close()
	old := accountUsageURL
	accountUsageURL = srv.URL
	t.Cleanup(func() { accountUsageURL = old })

	observed := time.Now().Add(-time.Hour).Truncate(time.Second)
	c := &accountUsageCache
	c.Lock()
	c.val = usage{OK: true, FiveHour: &usageWindow{Pct: 40}}
	c.ok, c.observed, c.account = true, observed, "a"
	c.fetched = time.Now().Add(-2 * accountUsageTTL) // due for a refresh
	c.Unlock()

	u, ok := accountUsage(context.Background())
	if !ok || !u.OK {
		t.Fatalf("a failed refresh dropped the cached reading: ok=%v %+v", ok, u)
	}
	if !u.ObservedAt.Equal(observed) {
		t.Errorf("observedAt = %v, want the last real observation %v", u.ObservedAt, observed)
	}
	if u.AgeSec < 3500 {
		t.Errorf("ageSec = %d: a failed refresh made an hour-old reading look fresh", u.AgeSec)
	}
}

// A re-login drops the account view's cache rather than serving the previous account's quota.
func TestAccountUsageCacheIsPerAccount(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	resetAccountCache(t)
	writeCodexAuth(t, "b")
	c := &accountUsageCache
	c.Lock()
	c.val, c.ok, c.observed, c.fetched, c.account = usage{OK: true}, true, time.Now(), time.Now(), "a"
	c.Unlock()
	accountPublished.Store(&accountPub{val: usage{OK: true}, observed: time.Now(), account: "a"})
	if _, ok := peekAccountUsage("a"); !ok {
		t.Fatal("peek lost account a's own reading")
	}
	if _, ok := peekAccountUsage("b"); ok {
		t.Fatal("peek served account a's cache under account b")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	defer srv.Close()
	old := accountUsageURL
	accountUsageURL = srv.URL
	t.Cleanup(func() { accountUsageURL = old })
	if u, ok := accountUsage(context.Background()); ok || u.OK {
		t.Fatalf("accountUsage served account a's cache under account b: %+v", u)
	}
}

// A window past its reset is rolled forward for the chip, and flagged: its 0% is a guess. The
// snapshot reads the raw values instead.
func TestAdjustWindowFlagsTheRolledWindowAndKeepsTheRawReading(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Minute).Unix()
	w := adjustWindow(97, 300, past, now)
	if !w.Stale || !w.Derived || w.Pct != 0 {
		t.Fatalf("rolled window = %+v, want stale+derived at 0%%", w)
	}
	if w.raw.UsedPercent != 97 || w.raw.ResetsAt != past || w.raw.WindowMinutes != 300 {
		t.Fatalf("raw reading lost: %+v", w.raw)
	}
	if f := adjustWindow(12, 300, now.Add(time.Hour).Unix(), now); f.Stale || f.Derived || f.Pct != 12 {
		t.Fatalf("an open window was flagged: %+v", f)
	}
}

// LimitReading reports the newest real observation per window, with its own time, and the
// recorded values even when the chip would roll them.
func TestLimitReadingReportsRawObservations(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	resetAccountCache(t)
	resetObservedRateLimits()
	t.Cleanup(resetObservedRateLimits)
	writeCodexAuth(t, "a")
	pastReset := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	SetObservedRateLimits(&RateLimitWindow{UsedPercent: 88, WindowMinutes: 300, ResetsAt: pastReset.Unix()}, nil, "plus")

	r := LimitReading()
	if r.Identity != "a" {
		t.Errorf("identity = %q, want the auth.json account id", r.Identity)
	}
	if len(r.Windows) != 1 {
		t.Fatalf("windows = %+v, want the one pushed window", r.Windows)
	}
	w := r.Windows[0]
	if w.Unit != "5h" || w.UsedPct != 88 || !w.ResetsAt.Equal(pastReset) || w.ObservedAt.IsZero() {
		t.Fatalf("window = %+v, want the recorded 88%% with its own reset and an observation time", w)
	}
}

// Review finding (#2017): the snapshot used to take accountUsageCache's lock, which accountUsage
// holds across the HTTP fetch. A hung account endpoint then held up GET /sessions?limits=1 and,
// through it, the reaper's serial sweep. The snapshot must answer from what was published.
func TestLimitReadingDoesNotWaitOnAnAccountFetch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	resetAccountCache(t)
	resetObservedRateLimits()
	t.Cleanup(resetObservedRateLimits)
	writeCodexAuth(t, "a")
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		http.Error(w, "late", http.StatusBadGateway)
	}))
	defer srv.Close()
	defer close(release)
	old := accountUsageURL
	accountUsageURL = srv.URL
	t.Cleanup(func() { accountUsageURL = old })
	reset := time.Now().Add(time.Hour).Unix()
	accountPublished.Store(&accountPub{account: "a", observed: time.Now().Add(-time.Minute),
		val: usage{OK: true, FiveHour: adjustWindow(30, 300, reset, time.Now())}})

	fetching := make(chan struct{})
	go func() {
		close(fetching)
		_, _ = accountUsage(context.Background()) // holds the cache lock until the server answers
	}()
	<-fetching
	time.Sleep(50 * time.Millisecond)

	done := make(chan limitsReading, 1)
	go func() { done <- limitsReading{LimitReading()} }()
	select {
	case got := <-done:
		if len(got.r.Windows) != 1 || got.r.Windows[0].UsedPct != 30 {
			t.Fatalf("reading = %+v, want the published 30%%", got.r)
		}
	case <-time.After(time.Second):
		t.Fatal("LimitReading waited on an account fetch in flight")
	}
}

type limitsReading struct{ r limitobs.Reading }

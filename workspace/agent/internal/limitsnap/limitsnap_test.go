package limitsnap

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/limitobs"
)

func stubSources(t *testing.T, m map[string]func() limitobs.Reading) {
	t.Helper()
	old := sources
	sources = m
	t.Cleanup(func() { sources = old })
}

// Issue #1962 acceptance: re-login under another provider account hides the previous
// account's values. The collector still holds the old account's capture; the snapshot must not
// report it under the new account.
func TestBuildDropsReadingsFromBeforeARelogin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	reading := limitobs.Reading{Identity: "a", Windows: []limitobs.Window{{
		Unit: "5h", UsedPct: 70, ResetsAt: t0.Add(3 * time.Hour), WindowMins: 300, ObservedAt: t0,
	}}}
	stubSources(t, map[string]func() limitobs.Reading{"claude": func() limitobs.Reading { return reading }})

	s1 := Build(t0.Add(time.Minute))
	k1, ok := s1.Kinds["claude"]
	if !ok || len(k1.Units) != 1 || k1.Units[0].UsedPct != 70 {
		t.Fatalf("first build = %+v, want the reading", s1)
	}
	// Signed in to account b; the capture on disk is still a's.
	reading.Identity = "b"
	s2 := Build(t0.Add(2 * time.Minute))
	if k, ok := s2.Kinds["claude"]; ok {
		t.Fatalf("re-login: the previous account's reading was reported as the new one's: %+v", k)
	}
	// b's own first reading comes through, under a different epoch.
	reading.Windows[0].ObservedAt = t0.Add(3 * time.Minute)
	reading.Windows[0].UsedPct = 5
	s3 := Build(t0.Add(4 * time.Minute))
	k3, ok := s3.Kinds["claude"]
	if !ok || k3.Units[0].UsedPct != 5 || k3.Epoch == k1.Epoch || k3.Epoch == "" {
		t.Fatalf("after re-login = %+v (old epoch %q), want b's reading under a new epoch", s3, k1.Epoch)
	}
}

// Decision 6's P1 coverage: only the covered kinds can appear, a kind with nothing captured is
// absent (unknown), and a reading without an observation time is never reported.
func TestBuildReportsOnlyObservedUnits(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	stubSources(t, map[string]func() limitobs.Reading{
		"codex": func() limitobs.Reading { return limitobs.Reading{Identity: "x"} },
		"copilot": func() limitobs.Reading {
			return limitobs.Reading{Identity: "y", Windows: []limitobs.Window{{Unit: "chat", UsedPct: 10}}}
		},
		"muse": func() limitobs.Reading {
			return limitobs.Reading{Identity: "z", Windows: []limitobs.Window{{Unit: "weekly", UsedPct: 20, ObservedAt: now}}}
		},
	})
	s := Build(now)
	if _, ok := s.Kinds["codex"]; ok {
		t.Error("a kind with no reading was reported")
	}
	if _, ok := s.Kinds["copilot"]; ok {
		t.Error("a reading without an observation time was reported")
	}
	if k := s.Kinds["muse"]; len(k.Units) != 1 || k.Units[0].ObservedAt != "2026-10-01T12:00:00Z" {
		t.Errorf("muse = %+v", k)
	}
	b, _ := json.Marshal(s)
	var back map[string]any
	if json.Unmarshal(b, &back) != nil || back["v"] != float64(Version) {
		t.Errorf("wire = %s", b)
	}
}

func TestCachedReusesABuildWithinTheTTL(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	n := 0
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	stubSources(t, map[string]func() limitobs.Reading{"claude": func() limitobs.Reading { n++; return limitobs.Reading{} }})
	cache.Lock()
	cache.at = time.Time{}
	cache.Unlock()
	Cached(now)
	Cached(now.Add(cacheTTL / 2))
	if n != 1 {
		t.Fatalf("collectors read %d times within the TTL, want 1", n)
	}
	Cached(now.Add(cacheTTL))
	if n != 2 {
		t.Fatalf("collectors read %d times after the TTL, want 2", n)
	}
}

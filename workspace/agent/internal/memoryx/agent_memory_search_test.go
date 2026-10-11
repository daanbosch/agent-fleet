package memoryx

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func memSearchNames(hits []agentMemHit) []string {
	var out []string
	for _, h := range hits {
		out = append(out, h.Name)
	}
	return out
}

func memSearchSave(t *testing.T, c agentMemCaller, name, desc, body string, at time.Time) {
	t.Helper()
	if _, err := agentMemSave(c, agentMemSaveReq{Name: name, Description: desc, Body: body}, at); err != nil {
		t.Fatal(err)
	}
}

// Recency and relevance disagree on this fixture: the newest memory only mentions the word in
// its body, the oldest has it in the name.
func TestAgentMemorySearchRanksByFieldThenTerms(t *testing.T) {
	agentMemTestEnv(t)
	c := agentMemCallerT(t, "claude-main")
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	memSearchSave(t, c, "deploy-notes", "how the rollout goes", "nothing special", now)
	memSearchSave(t, c, "misc-a", "deploy checklist", "x", now.Add(time.Hour))
	memSearchSave(t, c, "misc-b", "other", "mentions deploy once", now.Add(2*time.Hour))
	hits, fb, err := agentMemSearchRanked(c, "deploy", 0)
	if err != nil || fb != "" {
		t.Fatalf("err=%v fallback=%q", err, fb)
	}
	if got, want := memSearchNames(hits), []string{"deploy-notes", "misc-a", "misc-b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v (name > description > body)", got, want)
	}
	// A cut at the limit keeps the best, not the newest.
	hits, _, _ = agentMemSearchRanked(c, "deploy", 1)
	if len(hits) != 1 || hits[0].Name != "deploy-notes" {
		t.Fatalf("limit 1 = %v", memSearchNames(hits))
	}
}

func TestAgentMemorySearchTiesUsePinThenUsesThenRecency(t *testing.T) {
	agentMemTestEnv(t)
	c := agentMemCallerT(t, "claude-main")
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	memSearchSave(t, c, "a-old", "tie word", "x", now)
	memSearchSave(t, c, "b-used", "tie word", "x", now.Add(time.Hour))
	memSearchSave(t, c, "c-mid", "tie word", "x", now.Add(2*time.Hour))
	memSearchSave(t, c, "d-new", "tie word", "x", now.Add(3*time.Hour))
	if _, err := agentMemPin(agentMemPinReq{Scope: "project", Project: c.Project.ID, Name: "a-old", Pinned: true}, now.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	agentMemRecordUses(c, agentMemEntry{Name: "b-used", Scope: "project"})
	hits, _, err := agentMemSearchRanked(c, "tie", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := memSearchNames(hits), []string{"a-old", "b-used", "d-new", "c-mid"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v (pinned, used, then newest first)", got, want)
	}
}

func TestAgentMemorySearchFallsBackToAnyTerm(t *testing.T) {
	agentMemTestEnv(t)
	c := agentMemCallerT(t, "claude-main")
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	memSearchSave(t, c, "one", "alpha only", "x", now)
	memSearchSave(t, c, "two", "alpha beta", "line with gamma", now.Add(time.Hour))
	// Every word present in one memory: no fallback.
	hits, fb, _ := agentMemSearchRanked(c, "alpha beta", 0)
	if fb != "" || len(hits) != 1 || hits[0].Name != "two" || hits[0].MatchedTerms != nil {
		t.Fatalf("and answer: fb=%q hits=%+v", fb, hits)
	}
	// "zeta" is nowhere: the AND is empty, the OR answers, best-matching first.
	hits, fb, err := agentMemSearchRanked(c, "alpha beta zeta", 0)
	if err != nil || fb != "any" {
		t.Fatalf("fallback = %q, err=%v", fb, err)
	}
	if got := memSearchNames(hits); !reflect.DeepEqual(got, []string{"two", "one"}) {
		t.Fatalf("order = %v", got)
	}
	if !reflect.DeepEqual(hits[0].MatchedTerms, []string{"alpha", "beta"}) || !reflect.DeepEqual(hits[1].MatchedTerms, []string{"alpha"}) {
		t.Fatalf("matchedTerms = %v / %v", hits[0].MatchedTerms, hits[1].MatchedTerms)
	}
	// A single unmatched word has nothing to fall back to.
	if hits, fb, _ = agentMemSearchRanked(c, "zeta", 0); len(hits) != 0 || fb != "" {
		t.Fatalf("zeta: fb=%q hits=%v", fb, hits)
	}
}

func TestAgentMemorySearchJapaneseWithoutSpaces(t *testing.T) {
	agentMemTestEnv(t)
	c := agentMemCallerT(t, "claude-main")
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	memSearchSave(t, c, "jp", "ビルドの注意", "メモリが少ないので並列数を絞る", now)
	hits, _, _ := agentMemSearchRanked(c, "並列数", 0)
	if len(hits) != 1 || hits[0].Name != "jp" || len(hits[0].Snippets) != 1 {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestAgentMemorySearchCountsUsesOnlyForReturnedHits(t *testing.T) {
	agentMemTestEnv(t)
	c := agentMemCallerT(t, "claude-main")
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	memSearchSave(t, c, "wanted", "needle", "x", now)
	memSearchSave(t, c, "other", "needle", "x", now.Add(time.Hour))
	if hits, _, _ := agentMemSearchRanked(c, "needle", 1); len(hits) != 1 || hits[0].Name != "other" {
		t.Fatalf("hits = %v", hits)
	}
	rel, err := agentMemScopeDir(agentMemScopeProject, c)
	if err != nil {
		t.Fatal(err)
	}
	if u := agentMemLoadUsage(rel); u["other"] != 1 || u["wanted"] != 0 {
		t.Errorf("uses = %v, want only the returned hit counted", u)
	}
}

// TestAgentMemorySearchLatency states the cost at the size ADR 0108 measured (469 files). It
// fails only if a search of 500 memories takes more than a second, and logs the figure.
func TestAgentMemorySearchLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("writes 500 memories")
	}
	agentMemTestEnv(t)
	c := agentMemCallerT(t, "claude-main")
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	body := strings.Repeat("line of ordinary memory text about builds and tests\n", 40)
	for i := 0; i < 500; i++ {
		memSearchSave(t, c, fmt.Sprintf("mem-%03d", i), fmt.Sprintf("memory %d about builds", i), body+fmt.Sprintf("unique%d", i), now.Add(time.Duration(i)*time.Minute))
	}
	// The first read after 500 writes is slow whatever it does (measured: the index, which
	// search does not touch, takes the same ~2 s), so it goes first and is not charged to search.
	t0 := time.Now()
	if _, err := agentMemListIndex(c, 0); err != nil {
		t.Fatal(err)
	}
	t.Logf("index of 500 (untouched path, first read): %v", time.Since(t0))
	for _, q := range []string{"builds", "builds", "builds tests ordinary", "builds nosuchword"} {
		start := time.Now()
		hits, fb, err := agentMemSearchRanked(c, q, 0)
		d := time.Since(start)
		if err != nil || len(hits) != 20 {
			t.Fatalf("%q: %d hits, err=%v", q, len(hits), err)
		}
		t.Logf("500 memories, %q: %v (fallback=%q)", q, d, fb)
		if d > time.Second {
			t.Errorf("%q took %v", q, d)
		}
	}
}

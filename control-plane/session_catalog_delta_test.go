package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/k-k1/agent-fleet/control-plane/internal/store"
)

func deltaFixture(t *testing.T, n int) syncBenchFixture {
	t.Helper()
	f := newSyncBenchFixture(t, n)
	f.agent.catalogDelay, f.agent.reposDelay = 0, 0
	f.agent.delta = true
	return f
}

func (f syncBenchFixture) sync(t *testing.T) {
	t.Helper()
	if _, err := f.mgr.syncOwnerCatalog(context.Background(), f.res, catalogSyncNeed{exact: true}); err != nil {
		t.Fatal(err)
	}
}

// stored is the owner's catalog as the database holds it: name -> "title|archived".
func (f syncBenchFixture) stored(t *testing.T) map[string]string {
	t.Helper()
	rows, err := f.st.ListSharedSessionCatalogByOwner(context.Background(), f.ownerView.MembershipID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, r := range rows {
		out[r.Name] = fmt.Sprintf("%s|%v", r.Title, r.Archived)
	}
	return out
}

// want is what the stored catalog must equal: the fake Agent's sessions.
func (f syncBenchFixture) want() map[string]string {
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	out := map[string]string{}
	for _, s := range f.agent.sessions {
		out[s["name"].(string)] = fmt.Sprintf("%v|%v", s["title"], s["archived"])
	}
	return out
}

func (f syncBenchFixture) assertSynced(t *testing.T, what string) {
	t.Helper()
	got, want := f.stored(t), f.want()
	if len(got) != len(want) {
		t.Fatalf("%s: stored %d rows, Agent has %d", what, len(got), len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: %s stored as %q, Agent says %q", what, k, got[k], v)
		}
	}
}

func (f syncBenchFixture) lastSince() string {
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	return f.agent.sinceSeen[len(f.agent.sinceSeen)-1]
}

// The first sync reads the whole catalog; later ones send the cursor and get only what
// changed, and the stored catalog still ends up equal to the Agent's after edits and deletions.
func TestCatalogSyncUsesDeltasAndStaysEqual(t *testing.T) {
	f := deltaFixture(t, 300)
	f.sync(t)
	if got := f.lastSince(); got != "" {
		t.Fatalf("first sync sent since=%q, want none", got)
	}
	f.assertSynced(t, "first")

	full := f.agent.bytesSent.Load()
	f.sync(t)
	if got := f.lastSince(); got == "" {
		t.Fatal("second sync did not send the cursor")
	}
	if one := f.agent.bytesSent.Load() - full; one > 200 {
		t.Fatalf("an unchanged catalog cost %d bytes on the wire (the whole catalog was %d)", one, full)
	}
	f.assertSynced(t, "unchanged")

	f.agent.change("s-0007", func(s map[string]any) { s["title"] = "renamed"; s["archived"] = true })
	f.agent.remove("s-0100")
	f.sync(t)
	f.assertSynced(t, "edit + delete")
	if _, ok := f.stored(t)["s-0100"]; ok {
		t.Fatal("a deleted session is still in the stored catalog")
	}
	f.agent.change("s-0008", func(s map[string]any) { s["title"] = "again" })
	f.sync(t)
	f.assertSynced(t, "second edit")
}

// An Agent restart resets its counter. The old cursor must not be trusted: the CP is sent the
// whole catalog and, because the sessions are gone from it, deletions made while the CP could
// not see them are applied too.
func TestCatalogSyncRestartedAgentForcesFullRead(t *testing.T) {
	f := deltaFixture(t, 20)
	f.sync(t)
	f.sync(t)
	f.agent.remove("s-0003") // the CP will never be told by name: the restart loses the tombstone
	f.agent.restart()
	f.sync(t)
	f.assertSynced(t, "after restart")
	if _, ok := f.stored(t)["s-0003"]; ok {
		t.Fatal("a session deleted before the restart survived it")
	}
	f.agent.change("s-0004", func(s map[string]any) { s["title"] = "post" })
	f.sync(t)
	if f.lastSince() == "" {
		t.Fatal("deltas did not resume after the full read")
	}
	f.assertSynced(t, "after resume")
}

// An Agent from before deltas ignores ?since and sends the whole catalog without a cursor: it
// must keep working, every time as a full replace.
func TestCatalogSyncWithAnOldAgentIsAlwaysFull(t *testing.T) {
	f := newSyncBenchFixture(t, 30)
	f.agent.catalogDelay, f.agent.reposDelay = 0, 0
	f.sync(t)
	f.agent.mu.Lock()
	f.agent.sessions = f.agent.sessions[:25]
	f.agent.mu.Unlock()
	f.sync(t)
	f.assertSynced(t, "old agent, rows removed")
	if o := f.mgr.catalogSyncer().owners[f.ownerView.MembershipID]; o.cursor != "" {
		t.Fatalf("a cursor %q was kept for an Agent that sent none", o.cursor)
	}
}

// A new Agent answering a CP that sends no cursor (an old CP) gets the full body; that side
// is the Agent's own test. Here: the CP never trusts a delta flag it did not ask for.
func TestCatalogSyncIgnoresADeltaItDidNotAskFor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []map[string]any{{"name": "only"}}, "delta": true, "cursor": "x.1"})
	}))
	defer srv.Close()
	got, err := readAgentCatalog(context.Background(), stubRuntime{endpoint: srv.URL, token: "t"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.delta {
		t.Fatal("a delta answer to a request without a cursor was taken as a delta")
	}
}

// Deltas assume the stored catalog still equals the last write. After the full-resync interval
// the CP reads everything again, which also repairs a row changed behind its back.
func TestCatalogSyncResyncsFullyAfterTheInterval(t *testing.T) {
	f := deltaFixture(t, 10)
	f.sync(t)
	f.sync(t)
	// Damage the stored catalog the way another replica or an operator could.
	if err := f.st.ApplySharedSessionCatalogDelta(context.Background(), f.res.ws.ID, f.ownerView.MembershipID, nil, []string{"s-0002"}); err != nil {
		t.Fatal(err)
	}
	f.sync(t)
	if _, ok := f.stored(t)["s-0002"]; ok {
		t.Fatal("test setup: the row should still be missing under deltas")
	}
	cs := f.mgr.catalogSyncer()
	cs.mu.Lock()
	cs.owners[f.ownerView.MembershipID].fullAt = time.Now().Add(-catalogFullResync - time.Second)
	cs.mu.Unlock()
	f.sync(t)
	f.assertSynced(t, "after the interval")
}

// A changed working-copy inventory changes stored columns (branch, parent) of rows whose
// session did not change; the delta cannot carry that, so the CP reads the whole catalog.
func TestCatalogSyncRepoInventoryChangeForcesFullRead(t *testing.T) {
	f := deltaFixture(t, 14)
	f.sync(t)
	f.sync(t)
	f.agent.mu.Lock()
	f.agent.repos[3]["branch"] = "feature/x"
	f.agent.mu.Unlock()
	if _, err := f.mgr.syncOwnerCatalog(context.Background(), f.res, catalogSyncNeed{exact: true, liveRepos: true}); err != nil {
		t.Fatal(err)
	}
	rows, _ := f.st.ListSharedSessionCatalogByOwner(context.Background(), f.ownerView.MembershipID)
	n := 0
	for _, r := range rows {
		if r.WorkingCopyID == "wc-3" {
			n++
			if r.Branch != "feature/x" {
				t.Fatalf("%s kept branch %q although its working copy moved", r.Name, r.Branch)
			}
		}
	}
	if n == 0 {
		t.Fatal("test setup: no session in wc-3")
	}
}

func TestApplySharedSessionCatalogDelta(t *testing.T) {
	ctx := context.Background()
	f := newSyncBenchFixture(t, 3)
	f.agent.catalogDelay, f.agent.reposDelay = 0, 0
	f.sync(t)
	owner, ws := f.ownerView.MembershipID, f.res.ws.ID
	before, _ := f.st.ListSharedSessionCatalogByOwner(ctx, owner)
	ids := map[string]string{}
	for _, r := range before {
		ids[r.Name] = r.ID
	}
	// s-0001 carries a session-scoped share rule (the fixture's); deleting it must drop the rule.
	err := f.st.ApplySharedSessionCatalogDelta(ctx, ws, owner,
		[]store.SharedSessionCatalog{{ID: "fresh-id", WorkspaceID: ws, OwnerMembershipID: owner, Name: "s-0002", Kind: "claude", Title: "edited", CreatedAt: "2026-10-01T00:00:02Z", LastSeen: store.NowTS()},
			{ID: store.NewID(), WorkspaceID: ws, OwnerMembershipID: owner, Name: "s-new", Kind: "claude", CreatedAt: "2026-10-02T00:00:00Z", LastSeen: store.NowTS()}},
		[]string{"s-0001"})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := f.st.ListSharedSessionCatalogByOwner(ctx, owner)
	var got []string
	byName := map[string]store.SharedSessionCatalog{}
	for _, r := range after {
		got = append(got, r.Name)
		byName[r.Name] = r
	}
	sort.Strings(got)
	if strings.Join(got, ",") != "s-0000,s-0002,s-new" {
		t.Fatalf("catalog after the delta = %v, want s-0000 untouched, s-0002 edited, s-new added, s-0001 gone", got)
	}
	if byName["s-0002"].ID != ids["s-0002"] || byName["s-0002"].Title != "edited" {
		t.Fatalf("an upserted row must keep its id and take the new columns: %+v (id was %s)", byName["s-0002"], ids["s-0002"])
	}
	shares, _ := f.st.ListSessionSharesByOwner(ctx, owner)
	for _, s := range shares {
		if s.ScopeType == "session" && s.ScopeKey == "s-0001" {
			t.Fatal("the share rule of a deleted session survived")
		}
	}
}

// A cursor belongs to one workspace's Agent: after the owner's workspace is replaced, the old
// cursor is not sent even if the new Agent happened to share the epoch.
func TestCatalogSyncCursorDoesNotCrossWorkspaces(t *testing.T) {
	f := deltaFixture(t, 6)
	f.sync(t)
	f.sync(t)
	if f.lastSince() == "" {
		t.Fatal("test setup: deltas should be running")
	}
	cs := f.mgr.catalogSyncer()
	cs.mu.Lock()
	cs.owners[f.ownerView.MembershipID].cursorWS = "a-previous-workspace"
	cs.mu.Unlock()
	f.sync(t)
	if got := f.lastSince(); got != "" {
		t.Fatalf("sent cursor %q recorded for another workspace", got)
	}
}

// Logged for the PR: a warm sync of a 1,200-session catalog, whole versus delta, CP side only
// (the fake Agent answers instantly, so this is decode + store).
func TestCatalogSyncWarmCost(t *testing.T) {
	for _, delta := range []bool{false, true} {
		f := newSyncBenchFixture(t, 1200)
		f.agent.catalogDelay, f.agent.reposDelay = 0, 0
		f.agent.delta = delta
		f.sync(t)
		f.agent.change("s-0007", func(s map[string]any) { s["title"] = "x" })
		before := f.agent.bytesSent.Load()
		start := time.Now()
		for i := 0; i < 5; i++ {
			f.sync(t)
		}
		t.Logf("delta=%v: %6.1f ms per warm sync, %d bytes per warm sync", delta,
			float64(time.Since(start).Microseconds())/5000, (f.agent.bytesSent.Load()-before)/5)
	}
}

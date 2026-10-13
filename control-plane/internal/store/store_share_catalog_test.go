package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// catalogFixtureRows is the shape of the member behind issue #2074: 1,200 sessions, most of
// them archived.
func catalogFixtureRows(ws, owner string, n int) []SharedSessionCatalog {
	rows := make([]SharedSessionCatalog, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, SharedSessionCatalog{ID: NewID(), WorkspaceID: ws, OwnerMembershipID: owner,
			Name: fmt.Sprintf("s-%04d", i), Kind: "claude", Dir: "/home/dev/repos/app", Repo: "app",
			WorkingCopyID: fmt.Sprintf("wc-%d", i%7), Title: fmt.Sprintf("Session %d", i),
			CreatedAt: fmt.Sprintf("2026-10-01T00:%02d:%02dZ", (i/60)%60, i%60), State: "stopped",
			Archived: i >= 56, LastSeen: NowTS()})
	}
	return rows
}

func catalogByName(t *testing.T, st *SQL, owner string) map[string]SharedSessionCatalog {
	t.Helper()
	rows, err := st.ListSharedSessionCatalogByOwner(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]SharedSessionCatalog{}
	for _, r := range rows {
		out[r.Name] = r
	}
	return out
}

// A catalog sync for a member with ~1,200 sessions used to cost one SELECT and one upsert
// per session (about 2,400 statements) inside the transaction that holds the owner's
// membership row lock, which every share change also takes. Pinned here: an unchanged
// inventory costs a constant handful of statements, a small change costs a small number,
// row ids are stable, and the stale sweep still removes vanished sessions and their
// session-scoped rules.
func TestReplaceSharedSessionCatalogWritesOnlyTheDiff(t *testing.T) {
	for dialect, st := range inboxStores(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			f := newInboxFix(t, st, "owner", "peer")
			owner := f.members["owner"]
			ws := Workspace{ID: NewID(), TenantID: f.tenant, MembershipID: owner, ContainerName: "c", Network: "n",
				DataDir: "d", AgentPort: "1", AgentToken: "t", State: "running", CreatedAt: NowTS()}
			if err := st.CreateWorkspace(ctx, ws); err != nil {
				t.Fatal(err)
			}
			const n = 1200
			rows := catalogFixtureRows(ws.ID, owner, n)

			before := st.StatementCount()
			start := time.Now()
			if err := st.ReplaceSharedSessionCatalog(ctx, ws.ID, owner, rows); err != nil {
				t.Fatal(err)
			}
			t.Logf("first sync (%d new rows): %d statements, %v", n, st.StatementCount()-before, time.Since(start))
			first := catalogByName(t, st, owner)
			if len(first) != n {
				t.Fatalf("rows=%d", len(first))
			}

			// The Agent's inventory is unchanged; only the sync's own clock moved.
			time.Sleep(2 * time.Millisecond)
			again := catalogFixtureRows(ws.ID, owner, n)
			before = st.StatementCount()
			start = time.Now()
			if err := st.ReplaceSharedSessionCatalog(ctx, ws.ID, owner, again); err != nil {
				t.Fatal(err)
			}
			steady := st.StatementCount() - before
			t.Logf("unchanged sync (%d rows): %d statements, %v", n, steady, time.Since(start))
			if steady > 6 {
				t.Fatalf("an unchanged inventory of %d rows cost %d statements; want a constant handful", n, steady)
			}
			for name, r := range catalogByName(t, st, owner) {
				if r.ID != first[name].ID || r.LastSeen != first[name].LastSeen {
					t.Fatalf("%s rewritten by an unchanged sync: %+v -> %+v", name, first[name], r)
				}
			}

			// A session-scoped rule on a session that is about to vanish, and one that stays.
			for _, key := range []string{"s-0001", "s-0002"} {
				if err := st.PutSessionShare(ctx, SessionShare{ID: NewID(), TenantID: f.tenant, OwnerMembershipID: owner,
					RecipientMembershipID: f.members["peer"], ScopeType: "session", ScopeKey: key, Permission: "ro",
					CreatedAt: NowTS(), UpdatedAt: NowTS()}); err != nil {
					t.Fatal(err)
				}
			}
			changed := catalogFixtureRows(ws.ID, owner, n)[2:] // s-0000 and s-0001 are gone
			const later = "2030-01-01T00:00:00Z"               // NowTS has whole-second resolution
			for i := range changed {
				changed[i].LastSeen = later
			}
			changed[0].Title = "Renamed" // s-0002
			changed[1].State, changed[1].Activity = "running", "working"
			changed = append(changed, SharedSessionCatalog{ID: NewID(), WorkspaceID: ws.ID, OwnerMembershipID: owner,
				Name: "s-new", Kind: "codex", Dir: "/d", Repo: "app", CreatedAt: NowTS(), State: "running", LastSeen: NowTS()})
			before = st.StatementCount()
			start = time.Now()
			if err := st.ReplaceSharedSessionCatalog(ctx, ws.ID, owner, changed); err != nil {
				t.Fatal(err)
			}
			small := st.StatementCount() - before
			t.Logf("sync with 3 changed/new and 2 vanished rows: %d statements, %v", small, time.Since(start))
			if small > 10 {
				t.Fatalf("a 5-row change cost %d statements", small)
			}
			got := catalogByName(t, st, owner)
			if len(got) != n-1 {
				t.Fatalf("rows=%d want %d", len(got), n-1)
			}
			if _, ok := got["s-0000"]; ok {
				t.Fatal("vanished session kept its row")
			}
			if got["s-0002"].Title != "Renamed" || got["s-0002"].ID != first["s-0002"].ID {
				t.Fatalf("changed row=%+v", got["s-0002"])
			}
			if got["s-0003"].State != "running" || got["s-0003"].Activity != "working" {
				t.Fatalf("state change lost: %+v", got["s-0003"])
			}
			if got["s-0002"].LastSeen != later || got["s-0004"].LastSeen != first["s-0004"].LastSeen {
				t.Fatalf("last_seen must move on the changed row only: %q / %q", got["s-0002"].LastSeen, got["s-0004"].LastSeen)
			}
			if got["s-new"].Kind != "codex" {
				t.Fatalf("new row=%+v", got["s-new"])
			}
			shares, err := st.ListSessionSharesByOwner(ctx, owner)
			if err != nil {
				t.Fatal(err)
			}
			if len(shares) != 1 || shares[0].ScopeKey != "s-0002" {
				t.Fatalf("shares after sweep=%+v", shares)
			}
		})
	}
}

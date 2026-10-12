package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/k-k1/agent-fleet/control-plane/internal/pgtest"
)

// limitBoardStores runs f against SQLite and, when AF_TEST_DATABASE_URL is set, Postgres. The
// upserts use ON CONFLICT … WHERE, which is exactly the kind of statement that works on one
// dialect and not the other.
func limitBoardStores(t *testing.T, f func(t *testing.T, st *SQL)) {
	t.Run("sqlite", func(t *testing.T) {
		st, err := OpenSQLite(filepath.Join(t.TempDir(), "cp.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		if err := st.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		f(t, st)
	})
	t.Run("postgres", func(t *testing.T) {
		url, ok := pgtest.Schema(t)
		if !ok {
			t.Skip("set AF_TEST_DATABASE_URL to run the Postgres half")
		}
		st, err := OpenPostgres(url)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		if err := st.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		f(t, st)
	})
}

func limitMembers(t *testing.T, st *SQL, keys ...string) (Tenant, []Membership) {
	t.Helper()
	ctx := context.Background()
	tn, err := st.EnsureDefaultTenant(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var out []Membership
	for _, k := range keys {
		id, err := st.UpsertIdentity(ctx, k+"@example.com", k, "")
		if err != nil {
			t.Fatal(err)
		}
		m, err := st.EnsureMembership(ctx, id.ID, tn.ID, "member")
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return tn, out
}

func TestLimitObservationsNewerWinsAndEpochDrops(t *testing.T) {
	limitBoardStores(t, func(t *testing.T, st *SQL) {
		ctx := context.Background()
		_, ms := limitMembers(t, st, "alice")
		m := ms[0].ID
		obs := func(pct float64, observed, source string) []LimitObservation {
			return []LimitObservation{{Unit: "5h", UsedPct: pct, ResetsAt: "2026-10-01T15:00:00Z", WindowMins: 300,
				ObservedAt: observed, ReceivedAt: "2026-10-01T12:30:00Z", Source: source}}
		}
		get := func() []LimitObservation {
			t.Helper()
			rows, err := st.ListLimitObservations(ctx, []string{m})
			if err != nil {
				t.Fatal(err)
			}
			return rows
		}
		if err := st.RecordLimitObservations(ctx, m, "claude", "e1", "t", obs(40, "2026-10-01T12:10:00Z", "agent")); err != nil {
			t.Fatal(err)
		}
		// An older observation never overwrites a newer one.
		if err := st.RecordLimitObservations(ctx, m, "claude", "e1", "t", obs(20, "2026-10-01T12:00:00Z", "browser")); err != nil {
			t.Fatal(err)
		}
		if r := get(); len(r) != 1 || r[0].UsedPct != 40 || r[0].Source != "agent" {
			t.Fatalf("older observation overwrote a newer one: %+v", r)
		}
		// The identical observation from the other source is a duplicate: stored once, first kept.
		if err := st.RecordLimitObservations(ctx, m, "claude", "e1", "t", obs(40, "2026-10-01T12:10:00Z", "browser")); err != nil {
			t.Fatal(err)
		}
		if r := get(); len(r) != 1 || r[0].Source != "agent" {
			t.Fatalf("duplicate was not deduplicated: %+v", r)
		}
		if err := st.RecordLimitObservations(ctx, m, "claude", "e1", "t", obs(55, "2026-10-01T12:20:00Z", "browser")); err != nil {
			t.Fatal(err)
		}
		if r := get(); r[0].UsedPct != 55 || r[0].ObservedAt != "2026-10-01T12:20:00Z" {
			t.Fatalf("newer observation not taken: %+v", r)
		}
		// Another login: the previous epoch's rows are gone, even with no new reading yet.
		if err := st.RecordLimitObservations(ctx, m, "claude", "e2", "t2", nil); err != nil {
			t.Fatal(err)
		}
		if r := get(); len(r) != 0 {
			t.Fatalf("previous account's values survived a new epoch: %+v", r)
		}
		// A late post under the old epoch flips back, and still never shows the other's rows.
		if err := st.RecordLimitObservations(ctx, m, "claude", "e2", "t3", obs(3, "2026-10-01T12:40:00Z", "agent")); err != nil {
			t.Fatal(err)
		}
		if r := get(); len(r) != 1 || r[0].Epoch != "e2" || r[0].UsedPct != 3 {
			t.Fatalf("new account's reading = %+v", r)
		}
	})
}

func TestLimitPublicationAndCascade(t *testing.T) {
	limitBoardStores(t, func(t *testing.T, st *SQL) {
		ctx := context.Background()
		tn, ms := limitMembers(t, st, "alice", "bob", "carol")
		a, b, c := ms[0].ID, ms[1].ID, ms[2].ID
		if _, found, err := st.GetLimitPublication(ctx, a); err != nil || found {
			t.Fatalf("default must be hidden (no row): found=%v err=%v", found, err)
		}
		if err := st.PutLimitPublication(ctx, LimitPublication{MembershipID: a, Audience: "members", Level: "band",
			UpdatedAt: "t", Viewers: []string{b, c}}); err != nil {
			t.Fatal(err)
		}
		p, found, err := st.GetLimitPublication(ctx, a)
		if err != nil || !found || p.Audience != "members" || p.Level != "band" || len(p.Viewers) != 2 {
			t.Fatalf("publication = %+v found=%v err=%v", p, found, err)
		}
		if err := st.PutLimitPublication(ctx, LimitPublication{MembershipID: a, Audience: "members", Level: "exact",
			UpdatedAt: "t", Viewers: []string{c}}); err != nil {
			t.Fatal(err)
		}
		if p, _, _ := st.GetLimitPublication(ctx, a); len(p.Viewers) != 1 || p.Viewers[0] != c {
			t.Fatalf("viewers not replaced: %+v", p)
		}
		if err := st.RecordLimitObservations(ctx, c, "codex", "e", "t", []LimitObservation{{Unit: "7d", ObservedAt: "x", ReceivedAt: "x", Source: "agent"}}); err != nil {
			t.Fatal(err)
		}
		if err := st.PutLimitSessionStates(ctx, LimitSessionStates{MembershipID: c, States: "[]", ObservedAt: "t"}); err != nil {
			t.Fatal(err)
		}
		// Removing a named viewer takes them out of every audience; removing the publisher
		// takes everything of theirs.
		for _, id := range []string{c} {
			if err := st.SetMembershipStatus(ctx, id, "inactive"); err != nil {
				t.Fatal(err)
			}
			if err := st.DeleteMembership(ctx, id); err != nil {
				t.Fatalf("cascade: %v", err)
			}
		}
		if p, _, _ := st.GetLimitPublication(ctx, a); len(p.Viewers) != 0 {
			t.Fatalf("a removed member is still in an audience: %+v", p)
		}
		for _, table := range []string{"limit_observation", "limit_epoch", "limit_session_state"} {
			if n := countRows(t, st, table); n != 0 {
				t.Errorf("%s: %d rows left after the member was deleted", table, n)
			}
		}
		// The tenant switch defaults on.
		if on, err := st.LimitBoardEnabled(ctx, tn.ID); err != nil || !on {
			t.Fatalf("tenant switch default = %v err=%v, want on", on, err)
		}
		if err := st.SetLimitBoardEnabled(ctx, tn.ID, false, "t"); err != nil {
			t.Fatal(err)
		}
		if on, _ := st.LimitBoardEnabled(ctx, tn.ID); on {
			t.Fatal("tenant switch did not turn off")
		}
		// The roster pages by user key.
		page, err := st.ListMembersByTenantPage(ctx, tn.ID, "", 1)
		if err != nil || len(page) != 1 || page[0].UserKey != "alice" {
			t.Fatalf("page 1 = %+v err=%v", page, err)
		}
		page, _ = st.ListMembersByTenantPage(ctx, tn.ID, "alice", 10)
		if len(page) != 1 || page[0].MembershipID != b {
			t.Fatalf("page 2 = %+v (carol was removed)", page)
		}
	})
}

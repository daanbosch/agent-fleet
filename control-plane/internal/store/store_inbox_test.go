package store

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/k-k1/agent-fleet/control-plane/internal/pgtest"
)

// inboxStores is the SQLite store, and the Postgres one where AF_TEST_DATABASE_URL is set.
// The claim race and the decline race behave differently on the two (one connection versus
// row locks), so each test runs on both.
func inboxStores(t *testing.T) map[string]*SQL {
	t.Helper()
	ctx := context.Background()
	out := map[string]*SQL{}
	lite, err := OpenSQLite(filepath.Join(t.TempDir(), "cp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lite.Close() })
	out["sqlite"] = lite
	if url, ok := pgtest.Schema(t); ok {
		pg, err := OpenPostgres(url)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { pg.Close() })
		out["postgres"] = pg
	} else {
		t.Log("AF_TEST_DATABASE_URL unset: Postgres half skipped")
	}
	for name, st := range out {
		if err := st.Migrate(ctx); err != nil {
			t.Fatalf("%s: migrate: %v", name, err)
		}
	}
	return out
}

type inboxFix struct {
	st      *SQL
	tenant  string
	members map[string]string // name -> membership id
}

func newInboxFix(t *testing.T, st *SQL, names ...string) inboxFix {
	t.Helper()
	ctx := context.Background()
	tn, err := st.EnsureDefaultTenant(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f := inboxFix{st: st, tenant: tn.ID, members: map[string]string{}}
	for _, n := range names {
		id, err := st.UpsertIdentity(ctx, "", n+"-"+NewID()[:6], "")
		if err != nil {
			t.Fatal(err)
		}
		m, err := st.EnsureMembership(ctx, id.ID, tn.ID, "member")
		if err != nil {
			t.Fatal(err)
		}
		f.members[n] = m.ID
	}
	return f
}

var inboxTestLimits = InboxSendLimits{PerHour: 1000, HourStart: "0000", PendingPerPair: 1000}

func (f inboxFix) send(t *testing.T, kind, from string, to ...string) string {
	t.Helper()
	state := ""
	if kind == "request" {
		state = "open"
	}
	now := NowTS()
	it := InboxItem{ID: NewID(), TenantID: f.tenant, SenderMembershipID: f.members[from], Kind: kind,
		State: state, Ciphertext: "Ym9keQ==", CreatedAt: now}
	var rs []string
	for _, n := range to {
		rs = append(rs, f.members[n])
	}
	res, err := f.st.CreateInboxItem(context.Background(), it, rs, inboxTestLimits)
	if err != nil || !res.Created {
		t.Fatalf("send: %+v %v", res, err)
	}
	return it.ID
}

func (f inboxFix) item(t *testing.T, id string) InboxItem {
	t.Helper()
	it, ok, err := f.st.GetInboxItem(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("get %s: ok=%v err=%v", id, ok, err)
	}
	return it
}

func (f inboxFix) reconcile(t *testing.T) {
	t.Helper()
	if _, err := f.st.ReconcileInbox(context.Background(), f.tenant, NowTS()); err != nil {
		t.Fatal(err)
	}
}

func (f inboxFix) remove(t *testing.T, name string) {
	t.Helper()
	if err := f.st.SetMembershipStatus(context.Background(), f.members[name], "inactive"); err != nil {
		t.Fatal(err)
	}
}

var noRetention = InboxRetention{DMBefore: "0000", FinalBefore: "0000"}

// claimRace starts n claims of one fresh open request at the same moment, by n different
// recipients, and returns how many of them won.
func claimRace(t *testing.T, st *SQL, n int) (winners int, f inboxFix, id string) {
	t.Helper()
	names := []string{"a"}
	var rs []string
	for i := 0; i < n; i++ {
		nm := "r" + string(rune('A'+i/26)) + string(rune('a'+i%26))
		names = append(names, nm)
		rs = append(rs, nm)
	}
	f = newInboxFix(t, st, names...)
	id = f.send(t, "request", "a", rs[:20]...)
	// The item is frozen to 20 recipients by the API, but the race is about the update,
	// not the limit, so the store is given everyone.
	ctx := context.Background()
	for _, r := range rs[20:] {
		if _, err := st.DB().ExecContext(ctx, `INSERT INTO inbox_participant(item_id,membership_id,role) VALUES(?,?,'recipient')`,
			id, f.members[r]); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, r := range rs {
		wg.Add(1)
		go func(mid string) {
			defer wg.Done()
			<-start
			ok, err := st.ClaimInboxItem(ctx, id, mid, NowTS())
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			if ok {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}(f.members[r])
	}
	close(start)
	wg.Wait()
	return winners, f, id
}

// TestInboxClaimRaceHasOneWinner is docs/log/130 R-1: 50 recipients claim one request at the
// same moment and exactly one of them becomes the assignee, on both dialects.
func TestInboxClaimRaceHasOneWinner(t *testing.T) {
	for name, st := range inboxStores(t) {
		t.Run(name, func(t *testing.T) {
			winners, f, id := claimRace(t, st, 50)
			if winners != 1 {
				t.Fatalf("%d claims won, want exactly 1", winners)
			}
			it := f.item(t, id)
			if it.State != "claimed" || it.AssigneeMembershipID == "" {
				t.Fatalf("item after the race: %+v", it)
			}
		})
	}
}

// TestInboxClaimRacePositiveControl removes the state condition from the claim and shows
// that the race test then sees more than one winner. Without it, a race test that never
// actually raced would pass just the same.
func TestInboxClaimRacePositiveControl(t *testing.T) {
	saved := inboxClaimGuard
	inboxClaimGuard = ""
	t.Cleanup(func() { inboxClaimGuard = saved })
	for name, st := range inboxStores(t) {
		t.Run(name, func(t *testing.T) {
			if winners, _, _ := claimRace(t, st, 50); winners < 2 {
				t.Fatalf("an unconditional claim had %d winner(s): the race test cannot tell it from the real one", winners)
			}
		})
	}
}

// TestInboxRequestStateTable walks decision 2's table, including every operation on every
// final state (docs/log/130 R-3, R-4, R-5).
func TestInboxRequestStateTable(t *testing.T) {
	ctx := context.Background()
	for name, st := range inboxStores(t) {
		t.Run(name, func(t *testing.T) {
			f := newInboxFix(t, st, "a", "b", "c")
			a, b, c := f.members["a"], f.members["b"], f.members["c"]
			id := f.send(t, "request", "a", "b", "c")
			must := func(want bool, ok bool, err error, what string) {
				t.Helper()
				if err != nil || ok != want {
					t.Fatalf("%s: ok=%v err=%v, want %v", what, ok, err, want)
				}
			}
			ok, err := st.ReleaseInboxItem(ctx, id, b, NowTS())
			must(false, ok, err, "release while open")
			ok, err = st.CompleteInboxItem(ctx, id, b, NowTS())
			must(false, ok, err, "done while open")
			ok, err = st.ClaimInboxItem(ctx, id, a, NowTS())
			must(false, ok, err, "the sender claims")
			ok, err = st.ClaimInboxItem(ctx, id, b, NowTS())
			must(true, ok, err, "b claims")
			ok, err = st.ClaimInboxItem(ctx, id, b, NowTS())
			must(false, ok, err, "b claims again (the handler answers this one as idempotent)")
			ok, err = st.ClaimInboxItem(ctx, id, c, NowTS())
			must(false, ok, err, "c claims a claimed request")
			ok, err = st.DeclineInboxItem(ctx, id, c, NowTS())
			must(false, ok, err, "c declines a claimed request")
			ok, err = st.ReleaseInboxItem(ctx, id, c, NowTS())
			must(false, ok, err, "c releases b's claim")
			ok, err = st.ReleaseInboxItem(ctx, id, b, NowTS())
			must(true, ok, err, "b releases")
			if it := f.item(t, id); it.State != "open" || it.AssigneeMembershipID != "" {
				t.Fatalf("after release: %+v", it)
			}
			ok, err = st.ClaimInboxItem(ctx, id, c, NowTS())
			must(true, ok, err, "c claims")
			ok, err = st.WithdrawInboxItem(ctx, id, c, NowTS())
			must(false, ok, err, "a recipient withdraws")
			ok, err = st.CompleteInboxItem(ctx, id, c, NowTS())
			must(true, ok, err, "c done")
			if it := f.item(t, id); it.State != "done" || it.AssigneeMembershipID != c || it.FinalAt == "" {
				t.Fatalf("after done: %+v", it)
			}
			for _, op := range []struct {
				name string
				fn   func() (bool, error)
			}{
				{"claim", func() (bool, error) { return st.ClaimInboxItem(ctx, id, b, NowTS()) }},
				{"decline", func() (bool, error) { return st.DeclineInboxItem(ctx, id, b, NowTS()) }},
				{"release", func() (bool, error) { return st.ReleaseInboxItem(ctx, id, c, NowTS()) }},
				{"done", func() (bool, error) { return st.CompleteInboxItem(ctx, id, c, NowTS()) }},
				{"withdraw", func() (bool, error) { return st.WithdrawInboxItem(ctx, id, a, NowTS()) }},
			} {
				ok, err := op.fn()
				must(false, ok, err, op.name+" on done")
			}
			if it := f.item(t, id); it.State != "done" {
				t.Fatalf("a final state moved: %+v", it)
			}

			// Withdraw from claimed keeps the assignee on record.
			id2 := f.send(t, "request", "a", "b")
			ok, err = st.ClaimInboxItem(ctx, id2, b, NowTS())
			must(true, ok, err, "b claims #2")
			ok, err = st.WithdrawInboxItem(ctx, id2, a, NowTS())
			must(true, ok, err, "a withdraws #2")
			if it := f.item(t, id2); it.State != "withdrawn" || it.AssigneeMembershipID != b {
				t.Fatalf("withdrawn from claimed: %+v", it)
			}
			ok, err = st.CompleteInboxItem(ctx, id2, b, NowTS())
			must(false, ok, err, "done after withdraw")
		})
	}
}

// TestInboxDeclineByEveryRecipientDeclinesItem: the item is declined only when every
// recipient has declined, and two recipients declining at once still get there.
func TestInboxDeclineByEveryRecipientDeclinesItem(t *testing.T) {
	ctx := context.Background()
	for name, st := range inboxStores(t) {
		t.Run(name, func(t *testing.T) {
			f := newInboxFix(t, st, "a", "b", "c")
			id := f.send(t, "request", "a", "b", "c")
			if ok, err := st.DeclineInboxItem(ctx, id, f.members["b"], NowTS()); err != nil || !ok {
				t.Fatalf("b declines: %v %v", ok, err)
			}
			if it := f.item(t, id); it.State != "open" {
				t.Fatalf("one of two declined, state=%s, want open", it.State)
			}
			if ok, _ := st.DeclineInboxItem(ctx, id, f.members["b"], NowTS()); ok {
				t.Fatal("a second decline by b changed something")
			}
			if ok, err := st.DeclineInboxItem(ctx, id, f.members["c"], NowTS()); err != nil || !ok {
				t.Fatalf("c declines: %v %v", ok, err)
			}
			if it := f.item(t, id); it.State != "declined" || it.FinalAt == "" {
				t.Fatalf("both declined: %+v", it)
			}

			for round := 0; round < 10; round++ {
				id := f.send(t, "request", "a", "b", "c")
				var wg sync.WaitGroup
				for _, n := range []string{"b", "c"} {
					wg.Add(1)
					go func(mid string) {
						defer wg.Done()
						if _, err := st.DeclineInboxItem(ctx, id, mid, NowTS()); err != nil {
							t.Errorf("decline: %v", err)
						}
					}(f.members[n])
				}
				wg.Wait()
				if it := f.item(t, id); it.State != "declined" {
					t.Fatalf("round %d: concurrent declines left state=%s", round, it.State)
				}
			}
		})
	}
}

// TestInboxClaimWithdrawRace is docs/log/130 R-2 at the store: whichever lands first, the
// final state is withdrawn.
func TestInboxClaimWithdrawRace(t *testing.T) {
	ctx := context.Background()
	for name, st := range inboxStores(t) {
		t.Run(name, func(t *testing.T) {
			f := newInboxFix(t, st, "a", "b")
			for round := 0; round < 10; round++ {
				id := f.send(t, "request", "a", "b")
				var wg sync.WaitGroup
				wg.Add(2)
				go func() { defer wg.Done(); _, _ = st.ClaimInboxItem(ctx, id, f.members["b"], NowTS()) }()
				go func() { defer wg.Done(); _, _ = st.WithdrawInboxItem(ctx, id, f.members["a"], NowTS()) }()
				wg.Wait()
				if it := f.item(t, id); it.State != "withdrawn" {
					t.Fatalf("round %d: state=%s, want withdrawn", round, it.State)
				}
			}
		})
	}
}

// TestInboxDoneOrReleaseRaceWithdraw is docs/log/130 R-3 at the store: done against withdraw
// ends in exactly one of the two with the other refused, and release against withdraw always
// ends withdrawn.
func TestInboxDoneOrReleaseRaceWithdraw(t *testing.T) {
	ctx := context.Background()
	for name, st := range inboxStores(t) {
		t.Run(name, func(t *testing.T) {
			f := newInboxFix(t, st, "a", "b")
			a, b := f.members["a"], f.members["b"]
			for round := 0; round < 10; round++ {
				for _, op := range []string{"done", "release"} {
					id := f.send(t, "request", "a", "b")
					if ok, _ := st.ClaimInboxItem(ctx, id, b, NowTS()); !ok {
						t.Fatal("claim")
					}
					var wg sync.WaitGroup
					var opOK, wdOK bool
					wg.Add(2)
					go func() {
						defer wg.Done()
						if op == "done" {
							opOK, _ = st.CompleteInboxItem(ctx, id, b, NowTS())
						} else {
							opOK, _ = st.ReleaseInboxItem(ctx, id, b, NowTS())
						}
					}()
					go func() { defer wg.Done(); wdOK, _ = st.WithdrawInboxItem(ctx, id, a, NowTS()) }()
					wg.Wait()
					got := f.item(t, id).State
					switch op {
					case "done":
						if opOK == wdOK || (opOK && got != "done") || (wdOK && got != "withdrawn") {
							t.Fatalf("round %d done x withdraw: done=%v withdraw=%v state=%s", round, opOK, wdOK, got)
						}
					case "release":
						if !wdOK || got != "withdrawn" {
							t.Fatalf("round %d release x withdraw: release=%v withdraw=%v state=%s", round, opOK, wdOK, got)
						}
					}
				}
			}
		})
	}
}

// TestInboxRemovalAndBlockSystemRows covers the system rows of the state table: docs/log/130
// M-1, M-2, M-3, M-4 and M-6.
func TestInboxRemovalAndBlockSystemRows(t *testing.T) {
	ctx := context.Background()
	for name, st := range inboxStores(t) {
		t.Run(name, func(t *testing.T) {
			// M-1 (a): the assignee is removed, C can still take it -> open, no assignee.
			f := newInboxFix(t, st, "a", "b", "c")
			id := f.send(t, "request", "a", "b", "c")
			if ok, _ := st.ClaimInboxItem(ctx, id, f.members["b"], NowTS()); !ok {
				t.Fatal("claim")
			}
			f.remove(t, "b")
			f.reconcile(t)
			if it := f.item(t, id); it.State != "open" || it.AssigneeMembershipID != "" {
				t.Fatalf("M-1(a): %+v", it)
			}
			if el, _ := st.InboxEligibleRecipients(ctx, id); !el[f.members["c"]] || el[f.members["b"]] {
				t.Fatalf("M-1(a): eligible=%v, want c only", el)
			}
			// M-6 (a): B is re-invited, the item stays open and B can take it again.
			if err := st.SetMembershipStatus(ctx, f.members["b"], "active"); err != nil {
				t.Fatal(err)
			}
			f.reconcile(t)
			if el, _ := st.InboxEligibleRecipients(ctx, id); !el[f.members["b"]] {
				t.Fatalf("M-6(a): b not eligible after re-invite: %v", el)
			}

			// M-1 (b): the only recipient is the removed assignee -> declined.
			f = newInboxFix(t, st, "a", "b")
			id = f.send(t, "request", "a", "b")
			_, _ = st.ClaimInboxItem(ctx, id, f.members["b"], NowTS())
			f.remove(t, "b")
			f.reconcile(t)
			if it := f.item(t, id); it.State != "declined" {
				t.Fatalf("M-1(b): %+v", it)
			}
			// M-6 (b): re-inviting does not roll it back.
			_ = st.SetMembershipStatus(ctx, f.members["b"], "active")
			f.reconcile(t)
			if it := f.item(t, id); it.State != "declined" {
				t.Fatalf("M-6(b): %+v", it)
			}

			// M-2: C declined, then B is removed -> declined.
			f = newInboxFix(t, st, "a", "b", "c")
			id = f.send(t, "request", "a", "b", "c")
			_, _ = st.DeclineInboxItem(ctx, id, f.members["c"], NowTS())
			f.remove(t, "b")
			f.reconcile(t)
			if it := f.item(t, id); it.State != "declined" {
				t.Fatalf("M-2: %+v", it)
			}

			// M-3: the sender is removed -> open and claimed both become withdrawn.
			f = newInboxFix(t, st, "a", "b")
			open := f.send(t, "request", "a", "b")
			claimed := f.send(t, "request", "a", "b")
			_, _ = st.ClaimInboxItem(ctx, claimed, f.members["b"], NowTS())
			f.remove(t, "a")
			f.reconcile(t)
			for _, id := range []string{open, claimed} {
				if it := f.item(t, id); it.State != "withdrawn" {
					t.Fatalf("M-3: %+v", it)
				}
			}

			// M-4 (a)/(b): the assignee blocks the sender.
			f = newInboxFix(t, st, "a", "b", "c")
			two := f.send(t, "request", "a", "b", "c")
			one := f.send(t, "request", "a", "b")
			_, _ = st.ClaimInboxItem(ctx, two, f.members["b"], NowTS())
			_, _ = st.ClaimInboxItem(ctx, one, f.members["b"], NowTS())
			if err := st.SetInboxBlock(ctx, f.members["b"], f.members["a"], true); err != nil {
				t.Fatal(err)
			}
			f.reconcile(t)
			if it := f.item(t, two); it.State != "open" || it.AssigneeMembershipID != "" {
				t.Fatalf("M-4(a): %+v", it)
			}
			if el, _ := st.InboxEligibleRecipients(ctx, two); el[f.members["b"]] || !el[f.members["c"]] {
				t.Fatalf("M-4(a): eligible=%v", el)
			}
			if it := f.item(t, one); it.State != "declined" {
				t.Fatalf("M-4(b): %+v", it)
			}
			// A second reconcile changes nothing.
			if ch, err := st.ReconcileInbox(ctx, f.tenant, NowTS()); err != nil || len(ch) != 0 {
				t.Fatalf("second reconcile changed %v (%v)", ch, err)
			}
		})
	}
}

// TestInboxHardDeleteAndPurge covers docs/log/130 M-7, M-8 and M-9: the hard delete through
// membershipCascade is not stopped by the inbox, leaves the items others still take part in,
// and a purge run twice deletes nothing the second time and leaves no orphan.
func TestInboxHardDeleteAndPurge(t *testing.T) {
	ctx := context.Background()
	for name, st := range inboxStores(t) {
		t.Run(name, func(t *testing.T) {
			f := newInboxFix(t, st, "a", "b", "c")
			ab := f.send(t, "dm", "a", "b")
			abc := f.send(t, "request", "a", "b", "c")
			if err := st.SetInboxBlock(ctx, f.members["c"], f.members["a"], false); err != nil {
				t.Fatal(err)
			}
			_ = st.SetInboxBlock(ctx, f.members["c"], f.members["b"], true)

			// M-9: A and B removed logically only. The A-B item's body ends with the second
			// removal itself, before any purge, and the item C still takes part in stays.
			f.remove(t, "a")
			f.remove(t, "b")
			if _, ok, _ := st.GetInboxItem(ctx, ab); ok {
				t.Fatal("M-9: the item of two removed members survived their removal")
			}
			if n, err := st.PurgeInbox(ctx, noRetention); err != nil || n != 0 {
				t.Fatalf("M-9 purge after the removals: n=%d err=%v, want 0", n, err)
			}
			if _, ok, _ := st.GetInboxItem(ctx, abc); !ok {
				t.Fatal("M-9: an item with an active participant was purged")
			}

			// M-7: hard delete A and B through the real cascade.
			for _, m := range []string{"a", "b"} {
				if err := st.DeleteMembership(ctx, f.members[m]); err != nil {
					t.Fatalf("M-7: delete %s: %v", m, err)
				}
			}
			var left int
			_ = st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM inbox_participant WHERE membership_id IN (?,?)`,
				f.members["a"], f.members["b"]).Scan(&left)
			if left != 0 {
				t.Fatalf("M-7: %d participant rows of deleted members left", left)
			}
			if es, err := st.ListInboxEntries(ctx, f.members["c"], noRetention); err != nil || len(es) != 1 || es[0].Item.ID != abc {
				t.Fatalf("M-7: c's view after the delete: %+v %v", es, err)
			}
			if rules, _ := st.ListInboxSenderRules(ctx, f.members["c"]); len(rules) != 0 {
				t.Fatalf("M-7: rules about deleted members survived: %v", rules)
			}

			// M-8: two purges, the second deletes nothing, and nothing is orphaned.
			if _, err := st.PurgeInbox(ctx, noRetention); err != nil {
				t.Fatal(err)
			}
			if n, err := st.PurgeInbox(ctx, noRetention); err != nil || n != 0 {
				t.Fatalf("M-8: second purge deleted %d (%v)", n, err)
			}
			for _, q := range []string{
				`SELECT COUNT(*) FROM inbox_participant p WHERE NOT EXISTS (SELECT 1 FROM membership m WHERE m.id=p.membership_id)`,
				`SELECT COUNT(*) FROM inbox_participant p WHERE NOT EXISTS (SELECT 1 FROM inbox_item i WHERE i.id=p.item_id)`,
				`SELECT COUNT(*) FROM inbox_item i WHERE NOT EXISTS (SELECT 1 FROM inbox_participant p WHERE p.item_id=i.id)`,
				`SELECT COUNT(*) FROM inbox_mailbox b WHERE NOT EXISTS (SELECT 1 FROM membership m WHERE m.id=b.membership_id)`,
			} {
				var n int
				if err := st.DB().QueryRowContext(ctx, q).Scan(&n); err != nil || n != 0 {
					t.Fatalf("M-8 orphan check %q: %d (%v)", q, n, err)
				}
			}

			// C leaves too: hard delete removes the last item with it.
			f.remove(t, "c")
			if err := st.DeleteMembership(ctx, f.members["c"]); err != nil {
				t.Fatal(err)
			}
			if got := countRows(t, st, "inbox_item"); got != 0 {
				t.Fatalf("%d items left with no participant", got)
			}
		})
	}
}

// TestInboxPurgeOrphansFromOldCP is docs/log/130 V-6: a CP that predates the inbox
// hard-deletes a member with a cascade that does not name these tables. The delete must not
// be stopped by a foreign key, and the purge then removes what it left.
func TestInboxPurgeOrphansFromOldCP(t *testing.T) {
	ctx := context.Background()
	for name, st := range inboxStores(t) {
		t.Run(name, func(t *testing.T) {
			f := newInboxFix(t, st, "a", "b")
			f.send(t, "dm", "a", "b")
			_ = st.SetInboxBlock(ctx, f.members["a"], f.members["b"], true)
			f.remove(t, "b")
			// The old cascade: everything but the inbox tables.
			for _, stmt := range membershipCascade(f.members["b"]) {
				if strings.Contains(stmt.sql, "inbox_") {
					continue
				}
				if _, err := st.DB().ExecContext(ctx, stmt.sql, stmt.args...); err != nil {
					t.Fatalf("old cascade %q: %v", stmt.sql, err)
				}
			}
			if _, err := st.PurgeInbox(ctx, noRetention); err != nil {
				t.Fatal(err)
			}
			var n int
			_ = st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM inbox_participant WHERE membership_id=?`, f.members["b"]).Scan(&n)
			if n != 0 {
				t.Fatalf("%d orphan participant rows survived the purge", n)
			}
			if rules, _ := st.ListInboxSenderRules(ctx, f.members["a"]); len(rules) != 0 {
				t.Fatalf("a rule about a deleted member survived: %v", rules)
			}
		})
	}
}

// TestInboxRetention: reads and the purge drop a DM by its creation and a request by its
// final state. An open request never expires.
func TestInboxRetention(t *testing.T) {
	ctx := context.Background()
	for name, st := range inboxStores(t) {
		t.Run(name, func(t *testing.T) {
			f := newInboxFix(t, st, "a", "b")
			old := time.Now().UTC().Add(-100 * 24 * time.Hour).Format(time.RFC3339)
			dm := f.send(t, "dm", "a", "b")
			openReq := f.send(t, "request", "a", "b")
			doneReq := f.send(t, "request", "a", "b")
			_, _ = st.ClaimInboxItem(ctx, doneReq, f.members["b"], NowTS())
			_, _ = st.CompleteInboxItem(ctx, doneReq, f.members["b"], old)
			freshDone := f.send(t, "request", "a", "b")
			_, _ = st.WithdrawInboxItem(ctx, freshDone, f.members["a"], NowTS())
			for _, id := range []string{dm, openReq, doneReq, freshDone} {
				_, _ = st.DB().ExecContext(ctx, `UPDATE inbox_item SET created_at=? WHERE id=?`, old, id)
			}
			cut := time.Now().UTC().Add(-90 * 24 * time.Hour).Format(time.RFC3339)
			ret := InboxRetention{DMBefore: cut, FinalBefore: cut}
			es, err := st.ListInboxEntries(ctx, f.members["b"], ret)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]bool{}
			for _, e := range es {
				got[e.Item.ID] = true
			}
			if got[dm] || got[doneReq] || !got[openReq] || !got[freshDone] {
				t.Fatalf("visible after the cut: %v", got)
			}
			if n, err := st.PurgeInbox(ctx, ret); err != nil || n != 2 {
				t.Fatalf("purge n=%d err=%v, want 2", n, err)
			}
			if n, _ := st.PurgeInbox(ctx, ret); n != 0 {
				t.Fatalf("second purge deleted %d", n)
			}
		})
	}
}

// TestInboxSendLimits checks the limits on both sides of each boundary (docs/log/130 A-8 at
// the store; the recipient count and the body size are the handler's).
func TestInboxSendLimits(t *testing.T) {
	ctx := context.Background()
	for name, st := range inboxStores(t) {
		t.Run(name, func(t *testing.T) {
			f := newInboxFix(t, st, "a", "b", "c")
			lim := InboxSendLimits{PerHour: 30, HourStart: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339), PendingPerPair: 10}
			send := func(to ...string) InboxSendResult {
				var rs []string
				for _, n := range to {
					rs = append(rs, f.members[n])
				}
				res, err := st.CreateInboxItem(ctx, InboxItem{ID: NewID(), TenantID: f.tenant,
					SenderMembershipID: f.members["a"], Kind: "request", State: "open", CreatedAt: NowTS()}, rs, lim)
				if err != nil {
					t.Fatal(err)
				}
				return res
			}
			for i := 0; i < 10; i++ {
				if r := send("b"); !r.Created {
					t.Fatalf("pending #%d refused: %+v", i+1, r)
				}
			}
			r := send("b", "c")
			if r.Created || len(r.PendingFull) != 1 || r.PendingFull[0] != f.members["b"] {
				t.Fatalf("11th pending to b: %+v", r)
			}
			if got := countRows(t, st, "inbox_item"); got != 10 {
				t.Fatalf("a refused send left a row: %d items", got)
			}
			for i := 0; i < 20; i++ {
				if r := send("c"); !r.Created && i < 10 {
					t.Fatalf("send to c #%d refused: %+v", i+1, r)
				}
			}
			// 10 to b and 10 to c went through; c is at its pair limit now. Make room on b
			// and count up to the hourly limit.
			es, _ := st.ListInboxEntries(ctx, f.members["b"], noRetention)
			for _, e := range es {
				_, _ = st.WithdrawInboxItem(ctx, e.Item.ID, f.members["a"], NowTS())
			}
			for i := 20; i < 30; i++ {
				if r := send("b"); !r.Created {
					t.Fatalf("send #%d in the hour refused: %+v", i+1, r)
				}
			}
			if r := send("b"); r.Created || !r.RateLimited {
				t.Fatalf("31st send in the hour: %+v", r)
			}
		})
	}
}

// TestInboxSequenceAndCursor: every change moves each participant's sequence, the badge
// counts only other people's changes after the cursor, and the cursor never goes back.
func TestInboxSequenceAndCursor(t *testing.T) {
	ctx := context.Background()
	for name, st := range inboxStores(t) {
		t.Run(name, func(t *testing.T) {
			f := newInboxFix(t, st, "a", "b")
			a, b := f.members["a"], f.members["b"]
			id := f.send(t, "request", "a", "b")
			if n, _ := st.CountInboxUnseen(ctx, b, noRetention); n != 1 {
				t.Fatalf("b unseen=%d, want 1", n)
			}
			if n, _ := st.CountInboxUnseen(ctx, a, noRetention); n != 0 {
				t.Fatalf("a's own send counts for a: %d", n)
			}
			mb, _ := st.GetInboxMailbox(ctx, b)
			if _, err := st.SetInboxReadCursor(ctx, b, mb.LastSeq+100); err != nil {
				t.Fatal(err)
			}
			mb2, _ := st.GetInboxMailbox(ctx, b)
			if mb2.ReadSeq != mb.LastSeq {
				t.Fatalf("cursor went past the counter: %+v", mb2)
			}
			if n, _ := st.CountInboxUnseen(ctx, b, noRetention); n != 0 {
				t.Fatalf("unseen after the cursor: %d", n)
			}
			if _, err := st.SetInboxReadCursor(ctx, b, 0); err != nil {
				t.Fatal(err)
			}
			if mb3, _ := st.GetInboxMailbox(ctx, b); mb3.ReadSeq != mb2.ReadSeq {
				t.Fatalf("cursor went back: %+v", mb3)
			}
			if ok, _ := st.MarkInboxRead(ctx, id, b, NowTS()); !ok {
				t.Fatal("mark read")
			}
			if n, _ := st.CountInboxUnseen(ctx, b, noRetention); n != 0 {
				t.Fatalf("reading made the item unseen again: %d", n)
			}
			_, _ = st.ClaimInboxItem(ctx, id, b, NowTS())
			if n, _ := st.CountInboxUnseen(ctx, a, noRetention); n != 1 {
				t.Fatalf("a does not see b's claim: %d", n)
			}
			if n, _ := st.CountInboxUnseen(ctx, b, noRetention); n != 0 {
				t.Fatalf("b's own claim lit b's badge: %d", n)
			}
		})
	}
}

// TestInboxLaunchOnce: one session per assignee, refused for anyone else, and a reservation
// that loses its claim before it is confirmed never reaches the Agent (docs/log/130 R-6,
// R-7, R-8 at the store).
func TestInboxLaunchOnce(t *testing.T) {
	ctx := context.Background()
	for name, st := range inboxStores(t) {
		t.Run(name, func(t *testing.T) {
			f := newInboxFix(t, st, "a", "b", "c")
			a, b, c := f.members["a"], f.members["b"], f.members["c"]
			id := f.send(t, "request", "a", "b", "c")
			until := time.Now().UTC().Add(time.Minute).Format(time.RFC3339)
			if ok, _ := st.BeginInboxLaunch(ctx, id, b, "t0", NowTS(), until); ok {
				t.Fatal("launch of an open request")
			}
			_, _ = st.ClaimInboxItem(ctx, id, b, NowTS())
			if ok, _ := st.BeginInboxLaunch(ctx, id, c, "tc", NowTS(), until); ok {
				t.Fatal("R-7: a non-assignee launched")
			}
			if ok, _ := st.BeginInboxLaunch(ctx, id, b, "t1", NowTS(), until); !ok {
				t.Fatal("assignee launch refused")
			}
			if ok, _ := st.BeginInboxLaunch(ctx, id, b, "t2", NowTS(), until); ok {
				t.Fatal("R-8: a second launch while the first is reserved")
			}
			if ok, _ := st.ConfirmInboxLaunch(ctx, id, b, "t2"); ok {
				t.Fatal("confirmed under a token that holds no reservation")
			}
			if ok, _ := st.ConfirmInboxLaunch(ctx, id, b, "t1"); !ok {
				t.Fatal("confirm refused")
			}
			// Creating: the outcome may be a session, so nothing aborts it but a known refusal.
			if ok, _ := st.BeginInboxLaunch(ctx, id, b, "t3", NowTS(), "9999"); ok {
				t.Fatal("R-8: a second launch while the first is creating")
			}
			if ok, err := st.FinishInboxLaunch(ctx, id, b, "t1", "s1", "/repos/x", "feat", NowTS()); err != nil || !ok {
				t.Fatalf("finish: %v %v", ok, err)
			}
			if ok, _ := st.BeginInboxLaunch(ctx, id, b, "t4", NowTS(), until); ok {
				t.Fatal("R-8: a second launch after the session was recorded")
			}
			it := f.item(t, id)
			if it.SessionName != "s1" || it.SessionMembershipID != b || it.LaunchState != "" || it.LaunchToken != "" {
				t.Fatalf("launch record: %+v", it)
			}
			if ok, _ := st.SetInboxPRLink(ctx, id, c, "https://example.com/pr/1", "", NowTS()); ok {
				t.Fatal("a non-assignee set the PR link")
			}
			if ok, _ := st.SetInboxPRLink(ctx, id, b, "https://example.com/pr/1", "feat", NowTS()); !ok {
				t.Fatal("assignee PR link refused")
			}

			// R-6: a withdraw or a release between reserving and confirming wins.
			for _, op := range []string{"withdraw", "release"} {
				id := f.send(t, "request", "a", "b")
				_, _ = st.ClaimInboxItem(ctx, id, b, NowTS())
				if ok, _ := st.BeginInboxLaunch(ctx, id, b, "r-"+op, NowTS(), until); !ok {
					t.Fatal("reserve")
				}
				if op == "withdraw" {
					_, _ = st.WithdrawInboxItem(ctx, id, a, NowTS())
				} else {
					_, _ = st.ReleaseInboxItem(ctx, id, b, NowTS())
					// Even a re-claim by the same member does not revive the old reservation.
					_, _ = st.ClaimInboxItem(ctx, id, b, NowTS())
				}
				if ok, _ := st.ConfirmInboxLaunch(ctx, id, b, "r-"+op); ok {
					t.Fatalf("R-6: confirmed after a %s", op)
				}
			}
			// A withdraw after the confirm leaves the creating launch, so its session is still
			// recorded on the card.
			id3 := f.send(t, "request", "a", "b")
			_, _ = st.ClaimInboxItem(ctx, id3, b, NowTS())
			_, _ = st.BeginInboxLaunch(ctx, id3, b, "late", NowTS(), until)
			_, _ = st.ConfirmInboxLaunch(ctx, id3, b, "late")
			_, _ = st.WithdrawInboxItem(ctx, id3, a, NowTS())
			if ok, _ := st.FinishInboxLaunch(ctx, id3, b, "late", "s3", "/d", "", NowTS()); !ok {
				t.Fatal("a session confirmed before the withdraw was not recorded")
			}
			// Abort clears only its own launch.
			id4 := f.send(t, "request", "a", "b")
			_, _ = st.ClaimInboxItem(ctx, id4, b, NowTS())
			_, _ = st.BeginInboxLaunch(ctx, id4, b, "mine", NowTS(), until)
			_ = st.AbortInboxLaunch(ctx, id4, "other")
			if it := f.item(t, id4); it.LaunchState != "reserved" {
				t.Fatalf("another token's abort cleared the reservation: %+v", it)
			}
			_ = st.AbortInboxLaunch(ctx, id4, "mine")
			if ok, _ := st.BeginInboxLaunch(ctx, id4, b, "again", NowTS(), until); !ok {
				t.Fatal("no launch possible after an abort")
			}
		})
	}
}

// TestInboxRemovalSurvivesReinvite is decision 9 and the state table's system rows when a
// member is removed and re-invited before any sweep or read: the removal's effects happen in
// the removal itself, so the re-invite cannot erase them.
func TestInboxRemovalSurvivesReinvite(t *testing.T) {
	ctx := context.Background()
	for name, st := range inboxStores(t) {
		t.Run(name, func(t *testing.T) {
			f := newInboxFix(t, st, "a", "b", "c")
			claimed := f.send(t, "request", "a", "b", "c")
			_, _ = st.ClaimInboxItem(ctx, claimed, f.members["b"], NowTS())
			f.remove(t, "b")
			_ = st.SetMembershipStatus(ctx, f.members["b"], "active")
			f.reconcile(t)
			if it := f.item(t, claimed); it.State != "open" || it.AssigneeMembershipID != "" {
				t.Fatalf("the removed assignee's claim survived a re-invite: %+v", it)
			}

			sent := f.send(t, "request", "a", "b")
			f.remove(t, "a")
			_ = st.SetMembershipStatus(ctx, f.members["a"], "active")
			f.reconcile(t)
			if it := f.item(t, sent); it.State != "withdrawn" {
				t.Fatalf("the removed sender's request survived a re-invite: %+v", it)
			}

			g := newInboxFix(t, st, "p", "q")
			dm := g.send(t, "dm", "p", "q")
			g.remove(t, "p")
			g.remove(t, "q")
			_ = st.SetMembershipStatus(ctx, g.members["q"], "active")
			if _, err := st.PurgeInbox(ctx, noRetention); err != nil {
				t.Fatal(err)
			}
			if _, ok, _ := st.GetInboxItem(ctx, dm); ok {
				t.Fatal("a body whose last participant was removed came back with a re-invite")
			}
		})
	}
}

// TestInboxSendRechecksUnderLock: CreateInboxItem decides who may receive inside its own
// transaction, so a block, a chosen-members setting or a removal saved after the handler's
// check still stops the item.
func TestInboxSendRechecksUnderLock(t *testing.T) {
	ctx := context.Background()
	for name, st := range inboxStores(t) {
		t.Run(name, func(t *testing.T) {
			f := newInboxFix(t, st, "a", "b", "c")
			send := func() InboxSendResult {
				res, err := st.CreateInboxItem(ctx, InboxItem{ID: NewID(), TenantID: f.tenant, SenderMembershipID: f.members["a"],
					Kind: "dm", CreatedAt: NowTS()}, []string{f.members["c"], f.members["b"]}, inboxTestLimits)
				if err != nil {
					t.Fatal(err)
				}
				return res
			}
			_ = st.SetInboxBlock(ctx, f.members["b"], f.members["a"], true)
			if r := send(); r.Created || len(r.NotAccepting) != 1 || r.NotAccepting[0] != f.members["b"] {
				t.Fatalf("block: %+v", r)
			}
			_ = st.SetInboxBlock(ctx, f.members["b"], f.members["a"], false)
			_ = st.PutInboxSettings(ctx, f.members["b"], "chosen", false, nil)
			if r := send(); r.Created || len(r.NotAccepting) != 1 {
				t.Fatalf("chosen: %+v", r)
			}
			_ = st.PutInboxSettings(ctx, f.members["b"], "tenant", false, nil)
			f.remove(t, "b")
			if r := send(); r.Created || !r.Invalid {
				t.Fatalf("inactive: %+v", r)
			}
			if got := countRows(t, st, "inbox_item"); got != 0 {
				t.Fatalf("%d item(s) created by refused sends", got)
			}
		})
	}
}

// TestInboxReadAndClaimDoNotDeadlock holds a read inside its participant update with an
// advisory-lock trigger and lets a claim of the same item start meanwhile. With the read
// locking the participant before the mailbox this deadlocked on Postgres (SQLSTATE 40P01);
// both have to finish now. Postgres only: SQLite runs one transaction at a time.
func TestInboxReadAndClaimDoNotDeadlock(t *testing.T) {
	dsn, ok := pgtest.Schema(t)
	if !ok {
		t.Skip("set AF_TEST_DATABASE_URL: the deadlock needs Postgres row locks")
	}
	st, err := OpenPostgres(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, other := range []string{"claim", "withdraw"} {
		t.Run(other, func(t *testing.T) {
			f := newInboxFix(t, st, "a", "b")
			id := f.send(t, "request", "a", "b")
			gate, err := st.db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer gate.Close()
			if _, err := gate.ExecContext(ctx, "SELECT pg_advisory_lock(hashtext($1))", id); err != nil {
				t.Fatal(err)
			}
			for _, q := range []string{
				`CREATE OR REPLACE FUNCTION inbox_test_gate() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN
				 PERFORM pg_advisory_lock(hashtext(NEW.item_id)); PERFORM pg_advisory_unlock(hashtext(NEW.item_id));
				 RETURN NEW; END$$`,
				`DROP TRIGGER IF EXISTS inbox_test_read_gate ON inbox_participant`,
				`CREATE TRIGGER inbox_test_read_gate AFTER UPDATE OF delivery ON inbox_participant
				 FOR EACH ROW WHEN (OLD.delivery='unread' AND NEW.delivery='read') EXECUTE FUNCTION inbox_test_gate()`,
			} {
				if _, err := st.db.ExecContext(ctx, q); err != nil {
					t.Fatal(err)
				}
			}
			readDone, otherDone := make(chan error, 1), make(chan error, 1)
			go func() { _, err := st.MarkInboxRead(ctx, id, f.members["b"], NowTS()); readDone <- err }()
			waitLock := func(kind, needle string) bool {
				for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
					var n int
					_ = st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pg_stat_activity WHERE pid<>pg_backend_pid()
						AND wait_event_type=? AND wait_event=? AND query LIKE ?`, "Lock", kind, "%"+needle+"%").Scan(&n)
					if n > 0 {
						return true
					}
				}
				return false
			}
			if !waitLock("advisory", "UPDATE inbox_participant SET delivery") {
				t.Fatal("the read never reached the gate: the test proves nothing")
			}
			go func() {
				var err error
				if other == "claim" {
					_, err = st.ClaimInboxItem(ctx, id, f.members["b"], NowTS())
				} else {
					_, err = st.WithdrawInboxItem(ctx, id, f.members["a"], NowTS())
				}
				otherDone <- err
			}()
			// The second transaction now waits on a row the read holds, whichever row that is.
			if !waitLock("transactionid", "") && !waitLock("tuple", "") {
				t.Fatalf("the %s never waited on the read: the test proves nothing", other)
			}
			if _, err := gate.ExecContext(ctx, "SELECT pg_advisory_unlock(hashtext($1))", id); err != nil {
				t.Fatal(err)
			}
			if err := <-readDone; err != nil {
				t.Fatalf("read: %v", err)
			}
			if err := <-otherDone; err != nil {
				t.Fatalf("%s: %v", other, err)
			}
			_, _ = st.db.ExecContext(ctx, `DROP TRIGGER IF EXISTS inbox_test_read_gate ON inbox_participant`)
		})
	}
}

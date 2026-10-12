package store

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestInboxTakeInRules: a request's recipe is taken in by its current assignee only, once per
// (item, member) however often or concurrently it is pressed, and the memo keeps its origin
// through an edit (ADR 0111 decisions 2 and 3, docs/log/130 T-2, T-3, V-2).
func TestInboxTakeInRules(t *testing.T) {
	ctx := context.Background()
	for name, st := range inboxStores(t) {
		t.Run(name, func(t *testing.T) {
			f := newInboxFix(t, st, "a", "b", "c")
			b, c := f.members["b"], f.members["c"]
			id := f.send(t, "request", "a", "b", "c")
			memo := func() Memo {
				return Memo{ID: NewID(), Kind: "text", Body: "fix the flaky test", CreatedAt: NowTS(),
					OriginSenderMembershipID: f.members["a"], OriginSenderKey: "a-key"}
			}
			if _, r, err := st.TakeInInboxRecipe(ctx, id, b, memo()); err != nil || r.Allowed {
				t.Fatalf("take-in of an open request: %+v %v", r, err)
			}
			_, _ = st.ClaimInboxItem(ctx, id, b, NowTS())
			if _, r, _ := st.TakeInInboxRecipe(ctx, id, c, memo()); r.Allowed {
				t.Fatal("T-3: a non-assignee took the recipe in")
			}
			var mu sync.Mutex
			ids := map[string]int{}
			created := 0
			var wg sync.WaitGroup
			for i := 0; i < 20; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					m, r, err := st.TakeInInboxRecipe(ctx, id, b, memo())
					if err != nil || !r.Allowed {
						t.Errorf("take-in: %+v %v", r, err)
						return
					}
					mu.Lock()
					ids[m.ID]++
					if r.Created {
						created++
					}
					mu.Unlock()
				}()
			}
			wg.Wait()
			if len(ids) != 1 || created != 1 {
				t.Fatalf("T-3: 20 take-ins made memos %v, created=%d", ids, created)
			}
			var mid string
			for k := range ids {
				mid = k
			}
			got, _, _ := st.GetMemo(ctx, mid)
			if got.OriginItemID != id || got.OriginSenderKey != "a-key" || got.MembershipID != b {
				t.Fatalf("origin: %+v", got)
			}
			got.Body = "edited"
			got.OriginItemID, got.OriginSenderKey = "", ""
			if err := st.UpdateMemo(ctx, got); err != nil {
				t.Fatal(err)
			}
			again, _, _ := st.GetMemo(ctx, mid)
			if again.Body != "edited" || again.OriginItemID != id || again.OriginSenderKey != "a-key" {
				t.Fatalf("V-2: an edit dropped the origin: %+v", again)
			}
			// Released: no longer the assignee, no longer allowed.
			_, _ = st.ReleaseInboxItem(ctx, id, b, NowTS())
			if _, r, _ := st.TakeInInboxRecipe(ctx, id, b, memo()); r.Allowed {
				t.Fatal("a released assignee took the recipe in")
			}
			// A DM: any recipient (the handler refuses the sender).
			dm := f.send(t, "dm", "a", "b", "c")
			for _, m := range []string{b, c} {
				if _, r, _ := st.TakeInInboxRecipe(ctx, dm, m, memo()); !r.Allowed || !r.Created {
					t.Fatalf("DM take-in by %s: %+v", m, r)
				}
			}
			// The memo was deleted: taking in again makes one again.
			_ = st.DeleteMemo(ctx, mid, b)
			_, _ = st.ClaimInboxItem(ctx, id, b, NowTS())
			if _, r, _ := st.TakeInInboxRecipe(ctx, id, b, memo()); !r.Created {
				t.Fatalf("take-in after the memo was deleted: %+v", r)
			}
		})
	}
}

// TestInboxSendWithShareIsAtomic: a session attachment's share and the item are created in one
// transaction, so a refused send keeps neither, and an existing read-write share is not
// narrowed by attaching (decision 5).
func TestInboxSendWithShareIsAtomic(t *testing.T) {
	ctx := context.Background()
	for name, st := range inboxStores(t) {
		t.Run(name, func(t *testing.T) {
			f := newInboxFix(t, st, "a", "b", "c")
			a, b, c := f.members["a"], f.members["b"], f.members["c"]
			now := NowTS()
			share := func(rid string) SessionShare {
				return SessionShare{ID: NewID(), TenantID: f.tenant, OwnerMembershipID: a, RecipientMembershipID: rid,
					ScopeType: "session", ScopeKey: "s1", Permission: "ro", CreatedAt: now, UpdatedAt: now}
			}
			if err := st.PutSessionShare(ctx, SessionShare{ID: NewID(), TenantID: f.tenant, OwnerMembershipID: a,
				RecipientMembershipID: c, ScopeType: "session", ScopeKey: "s1", Permission: "rw", CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatal(err)
			}
			extras := InboxSendExtras{
				Attachments: []InboxAttachment{{ID: NewID(), Kind: "session", CatalogID: "cat-1", Granted: true}},
				Shares:      []SessionShare{share(b), share(c)},
			}
			item := func() InboxItem {
				return InboxItem{ID: NewID(), TenantID: f.tenant, SenderMembershipID: a, Kind: "dm", CreatedAt: NowTS()}
			}
			// An approved Agent operation of the owner is in flight: the share is refused,
			// and so is the item with it.
			until := time.Now().UTC().Add(time.Minute).Format(time.RFC3339)
			if ok, err := st.AcquireSessionShareOwnerLease(ctx, a, "op-1", NowTS(), until); err != nil || !ok {
				t.Fatalf("lease: %v %v", ok, err)
			}
			if _, err := st.CreateInboxItemWith(ctx, item(), []string{b, c}, inboxTestLimits, extras); err == nil {
				t.Fatal("a send was created while its share was refused")
			}
			if n := countRows(t, st, "inbox_item") + countRows(t, st, "inbox_attachment"); n != 0 {
				t.Fatalf("%d row(s) of a refused send kept", n)
			}
			if rows, _ := st.ListSessionSharesByRecipient(ctx, b); len(rows) != 0 {
				t.Fatalf("the refused send's share was kept: %+v", rows)
			}
			_ = st.ReleaseSessionShareOwnerLease(ctx, a, "op-1")
			res, err := st.CreateInboxItemWith(ctx, item(), []string{b, c}, inboxTestLimits, extras)
			if err != nil || !res.Created {
				t.Fatalf("send: %+v %v", res, err)
			}
			for _, rid := range []string{b, c} {
				rows, _ := st.ListSessionSharesByRecipient(ctx, rid)
				if len(rows) != 1 {
					t.Fatalf("shares of %s: %+v", rid, rows)
				}
				if rid == c && rows[0].Permission != "rw" {
					t.Fatal("attaching narrowed a read-write share")
				}
			}
			if n := countRows(t, st, "inbox_attachment"); n != 1 {
				t.Fatalf("%d attachments", n)
			}
		})
	}
}

// TestInboxAttachmentsGoWithTheirItem: the purge and the hard delete remove the attachments
// of items they remove, and no attachment is left without its item.
func TestInboxAttachmentsGoWithTheirItem(t *testing.T) {
	ctx := context.Background()
	for name, st := range inboxStores(t) {
		t.Run(name, func(t *testing.T) {
			f := newInboxFix(t, st, "a", "b")
			res, err := st.CreateInboxItemWith(ctx, InboxItem{ID: NewID(), TenantID: f.tenant, SenderMembershipID: f.members["a"],
				Kind: "dm", CreatedAt: NowTS()}, []string{f.members["b"]}, inboxTestLimits,
				InboxSendExtras{Attachments: []InboxAttachment{{ID: NewID(), Kind: "recipe", Ciphertext: "eA=="}}})
			if err != nil || !res.Created {
				t.Fatal(err)
			}
			f.remove(t, "a")
			f.remove(t, "b")
			if n := countRows(t, st, "inbox_attachment"); n != 0 {
				t.Fatalf("%d attachment(s) outlived their item", n)
			}
		})
	}
}

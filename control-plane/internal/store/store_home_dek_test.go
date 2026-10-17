package store

import (
	"context"
	"testing"
	"time"
)

func TestHomeDEKInsertNeverReplaces(t *testing.T) {
	ctx := context.Background()
	for name, st := range homeOpStores(t) {
		ws := homeOpWorkspace(t, st)
		first, err := st.InsertHomeDEK(ctx, HomeDEK{MembershipID: ws.MembershipID, Ciphertext: "sealed-1", KeyRef: ws.TenantID})
		if err != nil || first.Ciphertext != "sealed-1" || first.Scheme != HomeDEKMigrating {
			t.Fatalf("%s: first insert = %+v, %v", name, first, err)
		}
		// A second start that raced the first gets the stored key back, not its own.
		second, err := st.InsertHomeDEK(ctx, HomeDEK{MembershipID: ws.MembershipID, Ciphertext: "sealed-2", KeyRef: ws.TenantID})
		if err != nil || second.Ciphertext != "sealed-1" {
			t.Fatalf("%s: second insert = %+v, %v; want the first key kept", name, second, err)
		}
		// Deleting the workspace row leaves the home's key, and so does finishing a Destroy.
		if err := st.DeleteWorkspace(ctx, ws.ID); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := st.GetHomeDEK(ctx, ws.MembershipID); err != nil || !ok {
			t.Fatalf("%s: the home key went with the workspace row (ok=%v, err=%v)", name, ok, err)
		}
		ws2 := homeOpWorkspace(t, st)
		if _, err := st.InsertHomeDEK(ctx, HomeDEK{MembershipID: ws2.MembershipID, Ciphertext: "sealed", KeyRef: ws2.TenantID}); err != nil {
			t.Fatal(err)
		}
		op := HomeOperation{ID: NewID(), WorkspaceID: ws2.ID, MembershipID: ws2.MembershipID, Kind: HomeOpDestroy, Op: "destroy"}
		if err := st.InsertHomeOperation(ctx, op); err != nil {
			t.Fatal(err)
		}
		if ok, err := st.FinishHomeOperation(ctx, op.ID, HomeOperationFinish{DeleteWorkspace: true}); err != nil || !ok {
			t.Fatalf("%s: finish = %v, %v", name, ok, err)
		}
		if _, ok, err := st.GetHomeDEK(ctx, ws2.MembershipID); err != nil || !ok {
			t.Fatalf("%s: a finished Destroy dropped the home key (ok=%v, err=%v)", name, ok, err)
		}
	}
}

func TestCountHomeDEKs(t *testing.T) {
	ctx := context.Background()
	for name, st := range homeOpStores(t) {
		with := homeOpWorkspace(t, st)
		homeOpWorkspace(t, st) // a second home with no key yet
		if _, err := st.InsertHomeDEK(ctx, HomeDEK{MembershipID: with.MembershipID, Ciphertext: "sealed", KeyRef: with.TenantID}); err != nil {
			t.Fatal(err)
		}
		c, err := st.CountHomeDEKs(ctx)
		if err != nil || c != (HomeDEKCounts{Migrating: 1, WithoutKey: 1}) {
			t.Fatalf("%s: counts = %+v, %v", name, c, err)
		}
	}
}

func TestConfirmAndRemigrateHomeDEK(t *testing.T) {
	ctx := context.Background()
	for name, st := range homeOpStores(t) {
		ws := homeOpWorkspace(t, st)
		if _, err := st.InsertHomeDEK(ctx, HomeDEK{MembershipID: ws.MembershipID, Ciphertext: "sealed", KeyRef: ws.TenantID}); err != nil {
			t.Fatal(err)
		}
		read, _, _ := st.GetHomeDEK(ctx, ws.MembershipID)
		other := read
		other.Ciphertext = "other"
		if ok, err := st.ConfirmHomeDEK(ctx, other); err != nil || ok {
			t.Fatalf("%s: confirm of another key = %v, %v", name, ok, err)
		}
		if ok, err := st.ConfirmHomeDEK(ctx, read); err != nil || !ok {
			t.Fatalf("%s: confirm = %v, %v", name, ok, err)
		}
		d, _, _ := st.GetHomeDEK(ctx, ws.MembershipID)
		if d.Scheme != HomeDEKRandom || d.MigratedAt == "" {
			t.Fatalf("%s: after confirm scheme %q migrated_at %q", name, d.Scheme, d.MigratedAt)
		}
		if ok, _ := st.ConfirmHomeDEK(ctx, read); ok {
			t.Fatalf("%s: a second confirm claimed the row", name)
		}
		if ok, err := st.RemigrateHomeDEK(ctx, ws.MembershipID); err != nil || !ok {
			t.Fatalf("%s: remigrate = %v, %v", name, ok, err)
		}
		after, _, _ := st.GetHomeDEK(ctx, ws.MembershipID)
		if after.Scheme != HomeDEKMigrating || after.MigratedAt != "" || after.Ciphertext != "sealed" || after.ConfirmEpoch != read.ConfirmEpoch+1 {
			t.Fatalf("%s: after remigrate %+v", name, after)
		}
		// The row read before the remigrate matches it again in key and scheme, not in epoch.
		if ok, _ := st.ConfirmHomeDEK(ctx, read); ok {
			t.Fatalf("%s: a confirm read before the remigrate claimed the row", name)
		}
		if ok, err := st.ConfirmHomeDEK(ctx, after); err != nil || !ok {
			t.Fatalf("%s: confirm with the current epoch = %v, %v", name, ok, err)
		}
		if ok, _ := st.RemigrateHomeDEK(ctx, ws.MembershipID); !ok {
			t.Fatalf("%s: remigrate again failed", name)
		}
		if ok, _ := st.RemigrateHomeDEK(ctx, ws.MembershipID); ok {
			t.Fatalf("%s: remigrate of a migrating home claimed the row", name)
		}
	}
}

func TestDeleteHomeDEKIsConditional(t *testing.T) {
	ctx := context.Background()
	for name, st := range homeOpStores(t) {
		seed := func(confirm bool) (Workspace, HomeDEK) {
			ws := homeOpWorkspace(t, st)
			d, err := st.InsertHomeDEK(ctx, HomeDEK{MembershipID: ws.MembershipID, Ciphertext: "sealed", KeyRef: ws.TenantID})
			if err != nil {
				t.Fatal(err)
			}
			if confirm {
				if ok, err := st.ConfirmHomeDEK(ctx, d); err != nil || !ok {
					t.Fatalf("%s: confirm = %v, %v", name, ok, err)
				}
				d, _, _ = st.GetHomeDEK(ctx, ws.MembershipID)
			}
			return ws, d
		}
		del := func(label string, d HomeDEK, want bool) {
			t.Helper()
			token := leaseOf(t, st, d.MembershipID)
			defer st.ReleaseSessionShareOwnerLease(ctx, d.MembershipID, token)
			if got, err := st.DeleteHomeDEK(ctx, d, token); err != nil || got != want {
				t.Fatalf("%s: %s: DeleteHomeDEK = %v, %v; want %v", name, label, got, err, want)
			}
			_, ok, _ := st.GetHomeDEK(ctx, d.MembershipID)
			if want && ok {
				t.Fatalf("%s: %s: row still present after a delete that reported success", name, label)
			}
			if !want && !ok && label != "already gone" {
				t.Fatalf("%s: %s: a refused delete removed the row", name, label)
			}
		}

		// A workspace row names the membership: the home is alive as far as the store knows.
		ws, d := seed(true)
		del("workspace exists", d, false)
		if err := st.DeleteWorkspace(ctx, ws.ID); err != nil {
			t.Fatal(err)
		}
		// A key read before a remigrate is stale afterwards: the epoch moved.
		if ok, err := st.RemigrateHomeDEK(ctx, ws.MembershipID); err != nil || !ok {
			t.Fatalf("%s: remigrate = %v, %v", name, ok, err)
		}
		del("migrating again", d, false)
		cur, _, _ := st.GetHomeDEK(ctx, ws.MembershipID)
		del("migrating", cur, false)
		if ok, err := st.ConfirmHomeDEK(ctx, cur); err != nil || !ok {
			t.Fatalf("%s: re-confirm = %v, %v", name, ok, err)
		}
		del("read before the remigrate", d, false)
		cur, _, _ = st.GetHomeDEK(ctx, ws.MembershipID)
		stale := cur
		stale.Ciphertext = "another"
		del("another key", stale, false)
		del("current, no workspace, confirmed", cur, true)
		del("already gone", cur, false)
	}
}

func TestOrphanHomeDEKs(t *testing.T) {
	ctx := context.Background()
	for name, st := range homeOpStores(t) {
		live := homeOpWorkspace(t, st)
		orphan := homeOpWorkspace(t, st)
		for _, ws := range []Workspace{live, orphan} {
			if _, err := st.InsertHomeDEK(ctx, HomeDEK{MembershipID: ws.MembershipID, Ciphertext: "sealed", KeyRef: ws.TenantID}); err != nil {
				t.Fatal(err)
			}
		}
		if err := st.DeleteWorkspace(ctx, orphan.ID); err != nil {
			t.Fatal(err)
		}
		got, err := st.ListOrphanHomeDEKs(ctx)
		if err != nil || len(got) != 1 || got[0].MembershipID != orphan.MembershipID || got[0].Scheme != HomeDEKMigrating || !got[0].MembershipExists {
			t.Fatalf("%s: orphans = %+v, %v", name, got, err)
		}
		if ok, err := st.DeleteOrphanHomeDEK(ctx, live.MembershipID); err != nil || ok {
			t.Fatalf("%s: removed the key a workspace names: %v, %v", name, ok, err)
		}
		if ok, err := st.DeleteOrphanHomeDEK(ctx, orphan.MembershipID); err != nil || !ok {
			t.Fatalf("%s: orphan removal = %v, %v", name, ok, err)
		}
		if got, _ := st.ListOrphanHomeDEKs(ctx); len(got) != 0 {
			t.Fatalf("%s: orphans after removal = %+v", name, got)
		}
	}
}

// leaseOf takes the membership's lifecycle lease the way a destroy does and returns its token.
func leaseOf(t *testing.T, st *SQL, membershipID string) string {
	t.Helper()
	token := NewID()
	now := time.Now().UTC()
	ts := func(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z07:00") }
	ok, err := st.AcquireSessionShareOwnerLease(context.Background(), membershipID, token, ts(now), ts(now.Add(time.Minute)))
	if err != nil || !ok {
		t.Fatalf("lease = %v, %v", ok, err)
	}
	return token
}

// The delete is the lease holder's: a token that is not the current one, a lease another CP
// took over, an expired one, and no token at all keep the key.
func TestDeleteHomeDEKNeedsTheLease(t *testing.T) {
	ctx := context.Background()
	for name, st := range homeOpStores(t) {
		ws := homeOpWorkspace(t, st)
		d, _ := st.InsertHomeDEK(ctx, HomeDEK{MembershipID: ws.MembershipID, Ciphertext: "sealed", KeyRef: ws.TenantID})
		if ok, err := st.ConfirmHomeDEK(ctx, d); err != nil || !ok {
			t.Fatal(ok, err)
		}
		d, _, _ = st.GetHomeDEK(ctx, ws.MembershipID)
		if err := st.DeleteWorkspace(ctx, ws.ID); err != nil {
			t.Fatal(err)
		}
		old := leaseOf(t, st, ws.MembershipID)
		if err := st.ReleaseSessionShareOwnerLease(ctx, ws.MembershipID, old); err != nil {
			t.Fatal(err)
		}
		fresh := leaseOf(t, st, ws.MembershipID) // another CP took the membership over
		for label, token := range map[string]string{"released lease": old, "no lease": "", "unknown token": "x"} {
			if ok, err := st.DeleteHomeDEK(ctx, d, token); err != nil || ok {
				t.Fatalf("%s: %s: deleted = %v, %v", name, label, ok, err)
			}
		}
		// The operator path refuses while an operation holds the membership.
		if ok, err := st.DeleteOrphanHomeDEK(ctx, ws.MembershipID); err != nil || ok {
			t.Fatalf("%s: orphan removal under a live lease = %v, %v", name, ok, err)
		}
		if err := st.ReleaseSessionShareOwnerLease(ctx, ws.MembershipID, fresh); err != nil {
			t.Fatal(err)
		}
		if ok, err := st.DeleteOrphanHomeDEK(ctx, ws.MembershipID); err != nil || !ok {
			t.Fatalf("%s: orphan removal after the lease ended = %v, %v", name, ok, err)
		}
	}
}

// Postgres only (READ COMMITTED): a workspace insert for the membership may not commit while a
// delete is waiting on the key row, or the delete would go on to drop the key the new workspace
// points at. The delete holds the membership row, which the insert's foreign key needs.
func TestDeleteHomeDEKBlocksAWorkspaceInsert(t *testing.T) {
	ctx := context.Background()
	st, ok := homeOpStores(t)["postgres"]
	if !ok {
		t.Skip("needs AF_TEST_DATABASE_URL")
	}
	ws := homeOpWorkspace(t, st)
	d, _ := st.InsertHomeDEK(ctx, HomeDEK{MembershipID: ws.MembershipID, Ciphertext: "sealed", KeyRef: ws.TenantID})
	if ok, err := st.ConfirmHomeDEK(ctx, d); err != nil || !ok {
		t.Fatal(ok, err)
	}
	d, _, _ = st.GetHomeDEK(ctx, ws.MembershipID)
	if err := st.DeleteWorkspace(ctx, ws.ID); err != nil {
		t.Fatal(err)
	}
	token := leaseOf(t, st, ws.MembershipID)

	// Hold the key row, as a concurrent reader of the key would.
	hold, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback()
	if _, err := hold.ExecContext(ctx, `SELECT 1 FROM home_dek WHERE membership_id=? FOR UPDATE`, ws.MembershipID); err != nil {
		t.Fatal(err)
	}
	deleted := make(chan bool, 1)
	go func() {
		ok, _ := st.DeleteHomeDEK(ctx, d, token)
		deleted <- ok
	}()
	time.Sleep(500 * time.Millisecond) // the delete is now waiting on the key row
	created := make(chan struct{})
	go func() {
		w := ws
		w.ID = NewID()
		w.ContainerName += "-2"
		_ = st.CreateWorkspace(ctx, w)
		close(created)
	}()
	select {
	case <-created:
		t.Fatal("a workspace for the membership committed while the delete waited: it would then lose its key")
	case <-time.After(700 * time.Millisecond):
	}
	hold.Rollback()
	if !<-deleted {
		t.Fatal("the delete should win once the key row is free")
	}
	<-created
}

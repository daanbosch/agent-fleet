package store

import (
	"context"
	"testing"
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
			if got, err := st.DeleteHomeDEK(ctx, d); err != nil || got != want {
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

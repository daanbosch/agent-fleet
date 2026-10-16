package store

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

// Both dialects: the single-use code is one conditional UPDATE, and its RowsAffected is the
// whole guarantee.
func TestDevIdPCodeIsSpentExactlyOnce(t *testing.T) {
	ctx := context.Background()
	for name, st := range homeOpStores(t) {
		g := DevIdPGrant{Hash: "h1", Kind: DevIdPGrantCode, ClientID: "c", WorkspaceID: "w", MembershipID: "m",
			RedirectURI: "http://localhost:3000/cb", ExpiresAt: 1100}
		if err := st.InsertDevIdPGrant(ctx, g, 1000); err != nil {
			t.Fatalf("%s: insert: %v", name, err)
		}
		var ok, replay, other atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, state, err := st.ConsumeDevIdPCode(ctx, "h1", 1010)
				switch {
				case err != nil:
					other.Add(1)
				case state == DevIdPCodeOK:
					ok.Add(1)
				case state == DevIdPCodeReplayed:
					replay.Add(1)
				default:
					other.Add(1)
				}
			}()
		}
		wg.Wait()
		if ok.Load() != 1 || replay.Load() != 7 || other.Load() != 0 {
			t.Errorf("%s: ok=%d replayed=%d other=%d, want 1/7/0", name, ok.Load(), replay.Load(), other.Load())
		}
		// Expired is "unknown", never redeemable.
		if err := st.InsertDevIdPGrant(ctx, DevIdPGrant{Hash: "h2", Kind: DevIdPGrantCode, ClientID: "c",
			WorkspaceID: "w", MembershipID: "m", ExpiresAt: 1100}, 1000); err != nil {
			t.Fatal(err)
		}
		if _, state, _ := st.ConsumeDevIdPCode(ctx, "h2", 1100); state != DevIdPCodeUnknown {
			t.Errorf("%s: an expired code was %v, want unknown", name, state)
		}
		// An access token is not a code.
		if err := st.InsertDevIdPGrant(ctx, DevIdPGrant{Hash: "t1", Kind: DevIdPGrantAccess, Parent: "h1", ClientID: "c",
			WorkspaceID: "w", MembershipID: "m", ExpiresAt: 2000}, 1000); err != nil {
			t.Fatal(err)
		}
		if _, state, _ := st.ConsumeDevIdPCode(ctx, "t1", 1010); state != DevIdPCodeUnknown {
			t.Errorf("%s: an access token redeemed as a code: %v", name, state)
		}
		if _, ok, _ := st.GetDevIdPAccess(ctx, "t1", 1010); !ok {
			t.Errorf("%s: the access token is missing", name)
		}
		if err := st.RevokeDevIdPCode(ctx, "h1", 5000); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := st.GetDevIdPAccess(ctx, "t1", 1010); ok {
			t.Errorf("%s: the access token survived its code's revocation", name)
		}
		// The revoked code stays as the mark, and a token issued from it afterwards is refused.
		if err := st.InsertDevIdPGrant(ctx, DevIdPGrant{Hash: "t2", Kind: DevIdPGrantAccess, Parent: "h1", ClientID: "c",
			WorkspaceID: "w", MembershipID: "m", ExpiresAt: 6000}, 1020); err != ErrDevIdPCodeRevoked {
			t.Errorf("%s: a token issued from a revoked code: %v", name, err)
		}
		if _, ok, _ := st.GetDevIdPAccess(ctx, "t2", 1020); ok {
			t.Errorf("%s: that token is readable", name)
		}
		// Inserting purges what has expired.
		if err := st.InsertDevIdPGrant(ctx, DevIdPGrant{Hash: "h3", Kind: DevIdPGrantCode, ClientID: "c",
			WorkspaceID: "w", MembershipID: "m", ExpiresAt: 5000}, 3000); err != nil {
			t.Fatal(err)
		}
		if _, state, _ := st.ConsumeDevIdPCode(ctx, "h2", 1010); state != DevIdPCodeUnknown {
			t.Errorf("%s: an expired grant was not purged by a later insert: %v", name, state)
		}
	}
}

func TestDevIdPClientRegistrationAndWorkspaceDeletion(t *testing.T) {
	ctx := context.Background()
	for name, st := range homeOpStores(t) {
		ws := homeOpWorkspace(t, st)
		c := DevIdPClient{WorkspaceID: ws.ID, MembershipID: ws.MembershipID, ClientID: "afdev-x", SecretEnc: "s", CreatedAt: 1}
		if created, err := st.CreateDevIdPClient(ctx, c); err != nil || !created {
			t.Fatalf("%s: create: %v %v", name, created, err)
		}
		c.ClientID = "afdev-y"
		if created, err := st.CreateDevIdPClient(ctx, c); err != nil || created {
			t.Fatalf("%s: a second registration for the workspace must not replace the first: %v %v", name, created, err)
		}
		got, ok, err := st.GetDevIdPClientByID(ctx, "afdev-x")
		if err != nil || !ok || got.WorkspaceID != ws.ID || got.MembershipID != ws.MembershipID {
			t.Fatalf("%s: lookup by id: %+v %v %v", name, got, ok, err)
		}
		if err := st.InsertDevIdPGrant(ctx, DevIdPGrant{Hash: "g", Kind: DevIdPGrantCode, ClientID: "afdev-x",
			WorkspaceID: ws.ID, MembershipID: ws.MembershipID, ExpiresAt: 1 << 40}, 1); err != nil {
			t.Fatal(err)
		}
		if err := st.DeleteWorkspace(ctx, ws.ID); err != nil {
			t.Fatalf("%s: delete workspace: %v", name, err)
		}
		if _, ok, _ := st.GetDevIdPClient(ctx, ws.ID); ok {
			t.Errorf("%s: the registration outlived its workspace", name)
		}
		if _, state, _ := st.ConsumeDevIdPCode(ctx, "g", 2); state != DevIdPCodeUnknown {
			t.Errorf("%s: a grant outlived its workspace: %v", name, state)
		}
	}
}

func TestDevIdPKeysRetireAndDrop(t *testing.T) {
	ctx := context.Background()
	for name, st := range homeOpStores(t) {
		for i, kid := range []string{"k1", "k2"} {
			if err := st.InsertDevIdPKey(ctx, DevIdPKey{KID: kid, PrivateEnc: "p", PublicKey: "q",
				State: DevIdPKeyActive, CreatedAt: int64(100 + i)}); err != nil {
				t.Fatal(err)
			}
		}
		if err := st.RetireDevIdPKeys(ctx, "k2", 200); err != nil {
			t.Fatal(err)
		}
		keys, _ := st.ListDevIdPKeys(ctx)
		if len(keys) != 2 || keys[0].KID != "k2" || keys[0].State != DevIdPKeyActive || keys[1].State != DevIdPKeyRetired {
			t.Fatalf("%s: keys after retire: %+v", name, keys)
		}
		if err := st.DeleteRetiredDevIdPKeys(ctx, 199); err != nil {
			t.Fatal(err)
		}
		if keys, _ = st.ListDevIdPKeys(ctx); len(keys) != 2 {
			t.Errorf("%s: a key retired after the cutoff was dropped", name)
		}
		if err := st.DeleteRetiredDevIdPKeys(ctx, 200); err != nil {
			t.Fatal(err)
		}
		if keys, _ = st.ListDevIdPKeys(ctx); len(keys) != 1 || keys[0].KID != "k2" {
			t.Errorf("%s: after the drop: %+v", name, keys)
		}
	}
}

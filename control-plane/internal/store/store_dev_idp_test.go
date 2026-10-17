package store

import (
	"context"
	"errors"
	"fmt"
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

// Both dialects: personas are scoped to their workspace, handles are unique per workspace, a
// consent ticket is spent once, and an address is "taken" by an identity or a linked sign-in
// in any letter case.
func TestDevIdPPersonasAndConsentTickets(t *testing.T) {
	ctx := context.Background()
	for name, st := range homeOpStores(t) {
		ws := homeOpWorkspace(t, st)
		p := DevIdPPersona{ID: "p1", WorkspaceID: ws.ID, Handle: "admin", Name: "Admin", CreatedAt: 1}
		if err := st.CreateDevIdPPersona(ctx, p, 20); err != nil {
			t.Fatalf("%s: create: %v", name, err)
		}
		dup := p
		dup.ID = "p2"
		if err := st.CreateDevIdPPersona(ctx, dup, 20); err != ErrDevIdPPersonaExists {
			t.Fatalf("%s: duplicate handle: %v", name, err)
		}
		other := p
		other.ID, other.WorkspaceID = "p3", homeOpWorkspace(t, st).ID
		if err := st.CreateDevIdPPersona(ctx, other, 20); err != nil {
			t.Fatalf("%s: the same handle in another workspace: %v", name, err)
		}
		if _, ok, _ := st.GetDevIdPPersona(ctx, ws.ID, "p3"); ok {
			t.Errorf("%s: another workspace's persona was readable", name)
		}
		if ok, _ := st.DeleteDevIdPPersona(ctx, ws.ID, "p3"); ok {
			t.Errorf("%s: another workspace's persona was deletable", name)
		}
		if list, _ := st.ListDevIdPPersonas(ctx, ws.ID); len(list) != 1 || list[0].ID != "p1" {
			t.Errorf("%s: list = %+v", name, list)
		}

		if _, err := st.UpsertIdentity(ctx, "Real.Person@Example.Test", "real-person", ""); err != nil {
			t.Fatal(err)
		}
		if taken, err := st.DevIdPEmailTaken(ctx, "real.person@example.test"); err != nil || !taken {
			t.Errorf("%s: an identity's address (other case) is not taken: %v %v", name, taken, err)
		}
		if taken, _ := st.DevIdPEmailTaken(ctx, "nobody@example.test"); taken {
			t.Errorf("%s: an unused address is taken", name)
		}

		g := DevIdPGrant{Hash: "t1", Kind: DevIdPGrantConsent, ClientID: "c", WorkspaceID: ws.ID, MembershipID: "m", ExpiresAt: 1100}
		if err := st.InsertDevIdPGrant(ctx, g, 1000); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := st.ConsumeDevIdPConsent(ctx, "t1", 1010); !ok {
			t.Errorf("%s: live ticket refused", name)
		}
		if _, ok, _ := st.ConsumeDevIdPConsent(ctx, "t1", 1010); ok {
			t.Errorf("%s: ticket spent twice", name)
		}
		// A ticket is not a code, and a code is not a ticket.
		if err := st.InsertDevIdPGrant(ctx, DevIdPGrant{Hash: "t2", Kind: DevIdPGrantConsent, ClientID: "c", WorkspaceID: ws.ID, MembershipID: "m", ExpiresAt: 1100}, 1000); err != nil {
			t.Fatal(err)
		}
		if _, state, _ := st.ConsumeDevIdPCode(ctx, "t2", 1010); state == DevIdPCodeOK {
			t.Errorf("%s: a consent ticket was redeemed as a code", name)
		}
		if err := st.InsertDevIdPGrant(ctx, DevIdPGrant{Hash: "c2", Kind: DevIdPGrantCode, ClientID: "c", WorkspaceID: ws.ID, MembershipID: "m", ExpiresAt: 1100}, 1000); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := st.ConsumeDevIdPConsent(ctx, "c2", 1010); ok {
			t.Errorf("%s: a code was accepted as a consent ticket", name)
		}
		if _, ok, _ := st.ConsumeDevIdPConsent(ctx, "t2", 1200); ok {
			t.Errorf("%s: an expired ticket was accepted", name)
		}

		if err := st.DeleteWorkspace(ctx, ws.ID); err != nil {
			t.Fatal(err)
		}
		if list, _ := st.ListDevIdPPersonas(ctx, ws.ID); len(list) != 0 {
			t.Errorf("%s: personas outlived their workspace", name)
		}
	}
}

// Both dialects: the count behind the admin modal's "workspaces opted in" follows
// registrations, and the deployment switch row reads back "" until it is written.
func TestDevIdPClientCountAndSwitchRow(t *testing.T) {
	ctx := context.Background()
	for name, st := range homeOpStores(t) {
		if v, err := st.GetSetting(ctx, "dev_idp"); err != nil || v != "" {
			t.Fatalf("%s: unset switch read %q, %v", name, v, err)
		}
		if n, err := st.CountDevIdPClients(ctx); err != nil || n != 0 {
			t.Fatalf("%s: empty count %d, %v", name, n, err)
		}
		for _, ws := range []string{"w1", "w2"} {
			if _, err := st.CreateDevIdPClient(ctx, DevIdPClient{WorkspaceID: ws, MembershipID: "m", ClientID: "c-" + ws,
				SecretEnc: "s", KeyRef: "k", CreatedAt: 1}); err != nil {
				t.Fatalf("%s: create: %v", name, err)
			}
		}
		if n, _ := st.CountDevIdPClients(ctx); n != 2 {
			t.Errorf("%s: count = %d, want 2", name, n)
		}
		if err := st.SetSetting(ctx, "dev_idp", "off"); err != nil {
			t.Fatal(err)
		}
		if err := st.SetSetting(ctx, "dev_idp", "on"); err != nil {
			t.Fatal(err)
		}
		if v, _ := st.GetSetting(ctx, "dev_idp"); v != "on" {
			t.Errorf("%s: switch row = %q", name, v)
		}
		// The switch never touches registrations.
		if n, _ := st.CountDevIdPClients(ctx); n != 2 {
			t.Errorf("%s: count after the switch = %d", name, n)
		}
		if err := st.DeleteDevIdPClient(ctx, "w1"); err != nil {
			t.Fatal(err)
		}
		if n, _ := st.CountDevIdPClients(ctx); n != 1 {
			t.Errorf("%s: count after a delete = %d, want 1", name, n)
		}
	}
}

// Both dialects: the per-workspace limit holds when many creates race, because the count and
// the insert share one transaction.
func TestDevIdPPersonaLimitHoldsUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	const max, racers = 5, 16
	for name, st := range homeOpStores(t) {
		ws := homeOpWorkspace(t, st)
		var ok, limit, other atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				err := st.CreateDevIdPPersona(ctx, DevIdPPersona{ID: fmt.Sprintf("p%d", i), WorkspaceID: ws.ID,
					Handle: fmt.Sprintf("h%d", i), Name: "n", CreatedAt: 1}, max)
				switch {
				case err == nil:
					ok.Add(1)
				case errors.Is(err, ErrDevIdPPersonaLimit):
					limit.Add(1)
				default:
					t.Errorf("%s: create: %v", name, err)
					other.Add(1)
				}
			}(i)
		}
		wg.Wait()
		list, _ := st.ListDevIdPPersonas(ctx, ws.ID)
		if len(list) != max || ok.Load() != max || limit.Load() != racers-max {
			t.Errorf("%s: rows=%d ok=%d limit=%d, want %d/%d/%d", name, len(list), ok.Load(), limit.Load(), max, max, racers-max)
		}
	}
}

// Both dialects: a workspace that does not exist (or was deleted first) takes no persona, so
// the limit cannot be bypassed by racing against a row that holds no lock.
func TestDevIdPPersonaRefusedForMissingWorkspace(t *testing.T) {
	ctx := context.Background()
	for name, st := range homeOpStores(t) {
		err := st.CreateDevIdPPersona(ctx, DevIdPPersona{ID: "p1", WorkspaceID: "no-such-ws", Handle: "a", CreatedAt: 1}, 5)
		if !errors.Is(err, ErrDevIdPWorkspaceGone) {
			t.Errorf("%s: missing workspace: %v, want ErrDevIdPWorkspaceGone", name, err)
		}
		if list, _ := st.ListDevIdPPersonas(ctx, "no-such-ws"); len(list) != 0 {
			t.Errorf("%s: %d orphan personas", name, len(list))
		}
	}
}

// Both dialects: replacing swaps the registration in place, and inserts when there is none.
func TestReplaceDevIdPClient(t *testing.T) {
	ctx := context.Background()
	for name, st := range homeOpStores(t) {
		ws := homeOpWorkspace(t, st)
		c := DevIdPClient{WorkspaceID: ws.ID, MembershipID: ws.MembershipID, ClientID: "afdev-a", SecretEnc: "s1", CreatedAt: 1}
		if err := st.ReplaceDevIdPClient(ctx, c); err != nil {
			t.Fatalf("%s: replace into empty: %v", name, err)
		}
		c.ClientID, c.SecretEnc = "afdev-b", "s2"
		if err := st.ReplaceDevIdPClient(ctx, c); err != nil {
			t.Fatalf("%s: replace: %v", name, err)
		}
		if _, ok, _ := st.GetDevIdPClientByID(ctx, "afdev-a"); ok {
			t.Errorf("%s: the old client id still resolves", name)
		}
		got, ok, _ := st.GetDevIdPClient(ctx, ws.ID)
		if !ok || got.ClientID != "afdev-b" || got.SecretEnc != "s2" {
			t.Errorf("%s: after replace: %+v", name, got)
		}
		if n, _ := st.CountDevIdPClients(ctx); n != 1 {
			t.Errorf("%s: count = %d, want 1", name, n)
		}
	}
}

package store

import (
	"context"
	"sync"
	"testing"
	"time"
)

// handoffTakeRace runs rounds of the sender's withdraw against the recipient's take on a
// fresh offer each round, both released at once, and returns how many rounds had a winner
// count other than one. Each round settles its offer so the next one can be created (one
// pending offer per session).
func handoffTakeRace(t *testing.T, st *SQL, rounds int) (bad int) {
	t.Helper()
	ctx := context.Background()
	f := newHandoffFixtureOn(t, st)
	for i := 0; i < rounds; i++ {
		o := f.offer(t, "")
		if created, err := st.CreateSessionHandoffOffer(ctx, o); err != nil || !created {
			t.Fatalf("round %d: create=%v err=%v", i, created, err)
		}
		var withdrew, took bool
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			ok, err := st.TransitionSessionHandoffOffer(ctx, o.ID, "pending", "withdrawn", NowTS(), "")
			if err != nil {
				t.Errorf("withdraw: %v", err)
			}
			withdrew = ok
		}()
		go func() {
			defer wg.Done()
			<-start
			ok, err := st.TakeSessionHandoffOffer(ctx, o.ID, f.recipient.ID, NewID(), NowTS())
			if err != nil {
				t.Errorf("take: %v", err)
			}
			took = ok
		}()
		close(start)
		wg.Wait()
		if withdrew == took {
			bad++
		}
		// Settle whatever is left so the next round can offer again.
		got, _, _ := st.GetSessionHandoffOffer(ctx, o.ID)
		if got.Status == "pending" {
			if _, err := st.DB().ExecContext(ctx, `UPDATE session_handoff_offer SET status='withdrawn' WHERE id=?`, o.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	return bad
}

// TestHandoffTakeRaceHasOneWinner is issue #2020's race: the sender withdraws (or launches
// it themselves, which withdraws first) while the recipient takes it to launch. Exactly one
// of the two may succeed, or the same work runs twice (ADR 0057 decision 6).
func TestHandoffTakeRaceHasOneWinner(t *testing.T) {
	for name, st := range inboxStores(t) {
		t.Run(name, func(t *testing.T) {
			if bad := handoffTakeRace(t, st, 60); bad != 0 {
				t.Fatalf("%d of 60 rounds did not have exactly one winner", bad)
			}
		})
	}
}

// TestHandoffTakeRacePositiveControl drops the take condition from the withdraw — the
// shape before #2020 — and shows the race test then sees rounds where both sides won.
// Without it, a race test that never raced would pass just the same.
func TestHandoffTakeRacePositiveControl(t *testing.T) {
	saved := handoffUntakenGuard
	handoffUntakenGuard = ""
	t.Cleanup(func() { handoffUntakenGuard = saved })
	for name, st := range inboxStores(t) {
		t.Run(name, func(t *testing.T) {
			if bad := handoffTakeRace(t, st, 60); bad == 0 {
				t.Fatal("an unguarded withdraw never lost to a take: the race test cannot tell it from the real one")
			}
		})
	}
}

// TestHandoffTakeStateTable walks the take through every decision on both dialects.
func TestHandoffTakeStateTable(t *testing.T) {
	ctx := context.Background()
	for name, st := range inboxStores(t) {
		t.Run(name, func(t *testing.T) {
			f := newHandoffFixtureOn(t, st)
			must := func(want, ok bool, err error, what string) {
				t.Helper()
				if err != nil || ok != want {
					t.Fatalf("%s: ok=%v err=%v, want %v", what, ok, err, want)
				}
			}
			o := f.offer(t, "")
			if created, err := st.CreateSessionHandoffOffer(ctx, o); err != nil || !created {
				t.Fatalf("create=%v err=%v", created, err)
			}
			ok, err := st.TakeSessionHandoffOffer(ctx, o.ID, f.other.ID, "tok-x", NowTS())
			must(false, ok, err, "a non-recipient takes")
			ok, err = st.TakeSessionHandoffOffer(ctx, o.ID, f.recipient.ID, "tok-1", NowTS())
			must(true, ok, err, "the recipient takes")
			if got, _, _ := st.GetSessionHandoffOffer(ctx, o.ID); got.Status != "pending" || got.TakenAt == "" {
				t.Fatalf("after take: status=%q takenAt=%q, want pending and set", got.Status, got.TakenAt)
			}
			ok, err = st.TakeSessionHandoffOffer(ctx, o.ID, f.recipient.ID, "tok-2", NowTS())
			must(false, ok, err, "a second take (another tab) while the first holds it")
			ok, err = st.TransitionSessionHandoffOffer(ctx, o.ID, "pending", "withdrawn", NowTS(), "")
			must(false, ok, err, "the sender withdraws a taken offer")
			ok, err = st.TransitionSessionHandoffOffer(ctx, o.ID, "pending", "declined", NowTS(), "")
			must(false, ok, err, "a decline of a taken offer")
			ok, err = st.TransitionSessionHandoffOffer(ctx, o.ID, "pending", "accepted", NowTS(), "s")
			must(false, ok, err, "a tokenless accept of a taken offer")
			ok, err = st.ReleaseSessionHandoffOffer(ctx, o.ID, f.recipient.ID, "tok-2")
			must(false, ok, err, "a release with the wrong token")
			ok, err = st.AcceptSessionHandoffOffer(ctx, o.ID, f.recipient.ID, "tok-2", NowTS(), "s")
			must(false, ok, err, "an accept with the wrong token")

			// A failed launch gives it back: pending, untaken, decidable again.
			ok, err = st.ReleaseSessionHandoffOffer(ctx, o.ID, f.recipient.ID, "tok-1")
			must(true, ok, err, "the take's own release")
			if got, _, _ := st.GetSessionHandoffOffer(ctx, o.ID); got.Status != "pending" || got.TakenAt != "" || got.Ciphertext == "" {
				t.Fatalf("after release: %+v", got)
			}
			ok, err = st.AcceptSessionHandoffOffer(ctx, o.ID, f.recipient.ID, "tok-1", NowTS(), "s")
			must(false, ok, err, "an accept after the take was released")

			// The retry takes it again and the launch succeeds.
			ok, err = st.TakeSessionHandoffOffer(ctx, o.ID, f.recipient.ID, "tok-3", NowTS())
			must(true, ok, err, "take again after a release")
			ok, err = st.AcceptSessionHandoffOffer(ctx, o.ID, f.recipient.ID, "tok-3", NowTS(), "taken-over")
			must(true, ok, err, "accept with the take's token")
			got, _, _ := st.GetSessionHandoffOffer(ctx, o.ID)
			if got.Status != "accepted" || got.AcceptedSessionName != "taken-over" || got.TakenAt != "" || got.Ciphertext == "" {
				t.Fatalf("after accept: %+v", got)
			}
			ok, err = st.TransitionSessionHandoffOffer(ctx, o.ID, "pending", "withdrawn", NowTS(), "")
			must(false, ok, err, "withdraw after accept")
			ok, err = st.TakeSessionHandoffOffer(ctx, o.ID, f.recipient.ID, "tok-4", NowTS())
			must(false, ok, err, "take after accept")
		})
	}
}

// A take whose Console went away mid-launch must not pin the offer for ever: past the take
// window the sender can withdraw and the recipient can take again. An offer past its own
// deadline cannot be taken at all, and expiry waits for a live take.
func TestHandoffTakeGoesStaleAndRespectsDeadline(t *testing.T) {
	ctx := context.Background()
	for name, st := range inboxStores(t) {
		t.Run(name, func(t *testing.T) {
			f := newHandoffFixtureOn(t, st)
			o := f.offer(t, "")
			if _, err := st.CreateSessionHandoffOffer(ctx, o); err != nil {
				t.Fatal(err)
			}
			old := time.Now().UTC().Add(-HandoffTakeWindow - time.Minute).Format(time.RFC3339)
			if ok, err := st.TakeSessionHandoffOffer(ctx, o.ID, f.recipient.ID, "abandoned", old); err != nil || !ok {
				t.Fatalf("take=%v err=%v", ok, err)
			}
			if ok, err := st.TakeSessionHandoffOffer(ctx, o.ID, f.recipient.ID, "fresh", NowTS()); err != nil || !ok {
				t.Fatalf("a stale take blocked a new take: ok=%v err=%v", ok, err)
			}
			if ok, err := st.ReleaseSessionHandoffOffer(ctx, o.ID, f.recipient.ID, "fresh"); err != nil || !ok {
				t.Fatal("release", ok, err)
			}
			if _, err := st.TakeSessionHandoffOffer(ctx, o.ID, f.recipient.ID, "abandoned", old); err != nil {
				t.Fatal(err)
			}
			if ok, err := st.TransitionSessionHandoffOffer(ctx, o.ID, "pending", "withdrawn", NowTS(), ""); err != nil || !ok {
				t.Fatalf("a stale take blocked the withdraw: ok=%v err=%v", ok, err)
			}

			due := f.offer(t, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339))
			if _, err := st.CreateSessionHandoffOffer(ctx, due); err != nil {
				t.Fatal(err)
			}
			if ok, err := st.TakeSessionHandoffOffer(ctx, due.ID, f.recipient.ID, "late", NowTS()); err != nil || ok {
				t.Fatalf("an overdue offer was taken: ok=%v err=%v", ok, err)
			}
			// A take made just before the deadline holds expiry off until it settles.
			if _, err := st.DB().ExecContext(ctx, `UPDATE session_handoff_offer SET take_token='live',taken_at=? WHERE id=?`, NowTS(), due.ID); err != nil {
				t.Fatal(err)
			}
			if expired, err := st.ExpireSessionHandoffOffers(ctx, NowTS()); err != nil || len(expired) != 0 {
				t.Fatalf("expiry overrode a live take: %v err=%v", expired, err)
			}
			if ok, err := st.ReleaseSessionHandoffOffer(ctx, due.ID, f.recipient.ID, "live"); err != nil || !ok {
				t.Fatal("release", ok, err)
			}
			if expired, err := st.ExpireSessionHandoffOffers(ctx, NowTS()); err != nil || len(expired) != 1 {
				t.Fatalf("expired=%v err=%v, want the released offer", expired, err)
			}
		})
	}
}

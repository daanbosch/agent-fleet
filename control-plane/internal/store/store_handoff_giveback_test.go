package store

// The recipient's give-back of a take whose launch outcome stays unknown (issue #2058): it
// releases exactly a confirmed take older than the take window, and it races the sender's
// withdraw and the recipient's own reconcile without either side winning twice.

import (
	"context"
	"sync"
	"testing"
	"time"
)

// confirmedTake takes and confirms o for the recipient under token, confirmed at `at`.
func (f handoffFixture) confirmedTake(t *testing.T, o SessionHandoffOffer, token, at string) {
	t.Helper()
	ctx := context.Background()
	if ok, err := f.st.TakeSessionHandoffOffer(ctx, o.ID, f.recipient.ID, token, at); err != nil || !ok {
		t.Fatalf("take: ok=%v err=%v", ok, err)
	}
	if ok, err := f.st.ConfirmSessionHandoffTake(ctx, o.ID, f.recipient.ID, token, at); err != nil || !ok {
		t.Fatalf("confirm: ok=%v err=%v", ok, err)
	}
}

func TestHandoffGiveBackReleasesExactlyAnUnknownTake(t *testing.T) {
	ctx := context.Background()
	for name, st := range inboxStores(t) {
		t.Run(name, func(t *testing.T) {
			f := newHandoffFixtureOn(t, st)
			now := time.Now().UTC()
			old := now.Add(-HandoffTakeWindow - time.Minute).Format(time.RFC3339)
			edge := now.Add(-HandoffTakeWindow).Format(time.RFC3339)
			fresh := now.Add(-HandoffTakeWindow + time.Minute).Format(time.RFC3339)
			must := func(want, ok bool, err error, what string) {
				t.Helper()
				if err != nil || ok != want {
					t.Fatalf("%s: ok=%v err=%v, want %v", what, ok, err, want)
				}
			}
			state := func(id string) string {
				t.Helper()
				o, _, err := st.GetSessionHandoffOffer(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				return HandoffTakeState(o, now)
			}
			o := f.offer(t, "")
			if _, err := st.CreateSessionHandoffOffer(ctx, o); err != nil {
				t.Fatal(err)
			}
			ok, err := st.GiveBackUnknownSessionHandoffTake(ctx, o.ID, f.recipient.ID, now)
			must(false, ok, err, "give back an untaken offer")
			if s := state(o.ID); s != "" {
				t.Fatalf("untaken offer state %q", s)
			}

			// Unconfirmed, fresh and stale: never an unknown launch.
			_, _ = st.TakeSessionHandoffOffer(ctx, o.ID, f.recipient.ID, "u", NowTS())
			ok, err = st.GiveBackUnknownSessionHandoffTake(ctx, o.ID, f.recipient.ID, now)
			must(false, ok, err, "give back an unconfirmed take")
			if s := state(o.ID); s != "starting" {
				t.Fatalf("unconfirmed take state %q, want starting", s)
			}
			if _, err := st.DB().ExecContext(ctx, `UPDATE session_handoff_offer SET taken_at=? WHERE id=?`, old, o.ID); err != nil {
				t.Fatal(err)
			}
			ok, err = st.GiveBackUnknownSessionHandoffTake(ctx, o.ID, f.recipient.ID, now)
			must(false, ok, err, "give back a stale unconfirmed take")
			if s := state(o.ID); s != "" {
				t.Fatalf("stale unconfirmed take state %q, want none", s)
			}
			ok, err = st.ReleaseSessionHandoffOffer(ctx, o.ID, f.recipient.ID, "u")
			must(true, ok, err, "release")

			// Confirmed within the window: its create may still be running.
			f.confirmedTake(t, o, "c", fresh)
			ok, err = st.GiveBackUnknownSessionHandoffTake(ctx, o.ID, f.recipient.ID, now)
			must(false, ok, err, "give back a take confirmed within the window")
			if s := state(o.ID); s != "confirming" {
				t.Fatalf("fresh confirmed take state %q, want confirming", s)
			}
			// Only the recipient, and only past the window (the edge counts as past).
			if _, err := st.DB().ExecContext(ctx, `UPDATE session_handoff_offer SET take_confirmed_at=? WHERE id=?`, edge, o.ID); err != nil {
				t.Fatal(err)
			}
			if s := state(o.ID); s != "unknown" {
				t.Fatalf("confirmed take at the window's edge: state %q, want unknown", s)
			}
			ok, err = st.GiveBackUnknownSessionHandoffTake(ctx, o.ID, f.other.ID, now)
			must(false, ok, err, "another member gives it back")
			ok, err = st.GiveBackUnknownSessionHandoffTake(ctx, o.ID, f.owner.ID, now)
			must(false, ok, err, "the sender gives it back")
			ok, err = st.GiveBackUnknownSessionHandoffTake(ctx, o.ID, f.recipient.ID, now)
			must(true, ok, err, "the recipient gives back the unknown take")
			got, _, _ := st.GetSessionHandoffOffer(ctx, o.ID)
			if got.Status != "pending" || got.TakenAt != "" || got.TakeConfirmedAt != "" || got.Ciphertext == "" {
				t.Fatalf("after the give-back: %+v, want pending, untaken, body kept", got)
			}
			ok, err = st.ConfirmSessionHandoffTake(ctx, o.ID, f.recipient.ID, "c", NowTS())
			must(false, ok, err, "a create under the given-back take")
			ok, err = st.AcceptSessionHandoffOffer(ctx, o.ID, f.recipient.ID, "c", NowTS(), "late")
			must(false, ok, err, "a late accept under the given-back take")
			ok, err = st.GiveBackUnknownSessionHandoffTake(ctx, o.ID, f.recipient.ID, now)
			must(false, ok, err, "a second give-back")

			// A decided offer is never touched.
			f.confirmedTake(t, o, "d", old)
			ok, err = st.AcceptSessionHandoffOffer(ctx, o.ID, f.recipient.ID, "d", NowTS(), "s")
			must(true, ok, err, "accept")
			ok, err = st.GiveBackUnknownSessionHandoffTake(ctx, o.ID, f.recipient.ID, now)
			must(false, ok, err, "give back an accepted offer")
			if got, _, _ := st.GetSessionHandoffOffer(ctx, o.ID); got.Status != "accepted" || got.AcceptedSessionName != "s" {
				t.Fatalf("the accepted offer moved: %+v", got)
			}

			// Unshared while the take was confirmed: the give-back expires it.
			gone := f.offer(t, "")
			if _, err := st.CreateSessionHandoffOffer(ctx, gone); err != nil {
				t.Fatal(err)
			}
			f.confirmedTake(t, gone, "g", old)
			if err := st.DeleteSessionShare(ctx, f.sessionShareRow.ID, f.owner.ID); err != nil {
				t.Fatal(err)
			}
			ok, err = st.GiveBackUnknownSessionHandoffTake(ctx, gone.ID, f.recipient.ID, now)
			must(true, ok, err, "give back an unshared unknown take")
			if got, _, _ := st.GetSessionHandoffOffer(ctx, gone.ID); got.Status != "expired" || got.DecidedAt == "" || got.TakenAt != "" {
				t.Fatalf("after giving back an unshared offer: %+v, want expired", got)
			}
		})
	}
}

// giveBackRace runs rounds of the recipient's give-back against `other` on a fresh offer
// whose take is unknown, both released at once, and counts the rounds whose outcome breaks
// `ok(gaveBack, otherWon)`.
func giveBackRace(t *testing.T, f handoffFixture, rounds int, other func(f handoffFixture, o SessionHandoffOffer) bool, ok func(gaveBack, otherWon bool) bool) (bad int) {
	t.Helper()
	ctx := context.Background()
	st := f.st
	old := time.Now().UTC().Add(-HandoffTakeWindow - time.Minute).Format(time.RFC3339)
	for i := 0; i < rounds; i++ {
		o := f.offer(t, "")
		if created, err := st.CreateSessionHandoffOffer(ctx, o); err != nil || !created {
			t.Fatalf("round %d: create=%v err=%v", i, created, err)
		}
		f.confirmedTake(t, o, "tok", old)
		var gave, won bool
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			r, err := st.GiveBackUnknownSessionHandoffTake(ctx, o.ID, f.recipient.ID, time.Now())
			if err != nil {
				t.Errorf("give back: %v", err)
			}
			gave = r
		}()
		go func() {
			defer wg.Done()
			<-start
			won = other(f, o)
		}()
		close(start)
		wg.Wait()
		if !ok(gave, won) {
			bad++
		}
		if _, err := st.DB().ExecContext(ctx, `UPDATE session_handoff_offer SET status='withdrawn' WHERE id=? AND status='pending'`, o.ID); err != nil {
			t.Fatal(err)
		}
	}
	return bad
}

// The sender's withdraw lands only once the give-back has: while the unknown take holds, the
// same work may be running on the recipient's side (ADR 0057 decision 6).
func withdrawVsGiveBack(t *testing.T, f handoffFixture) int {
	return giveBackRace(t, f, 60,
		func(f handoffFixture, o SessionHandoffOffer) bool {
			r, err := f.st.TransitionSessionHandoffOffer(context.Background(), o.ID, "pending", "withdrawn", NowTS(), "")
			if err != nil {
				t.Errorf("withdraw: %v", err)
			}
			return r
		},
		func(gaveBack, withdrew bool) bool { return !withdrew || gaveBack })
}

// The reconcile found the session and accepts under the take; the give-back must then
// change nothing, and an accept must fail once the give-back landed: exactly one wins.
func acceptVsGiveBack(t *testing.T, f handoffFixture) int {
	return giveBackRace(t, f, 60,
		func(f handoffFixture, o SessionHandoffOffer) bool {
			r, err := f.st.AcceptSessionHandoffOffer(context.Background(), o.ID, f.recipient.ID, "tok", NowTS(), "s-1")
			if err != nil {
				t.Errorf("accept: %v", err)
			}
			return r
		},
		func(gaveBack, accepted bool) bool { return gaveBack != accepted })
}

func TestHandoffGiveBackRaces(t *testing.T) {
	for name, st := range inboxStores(t) {
		t.Run(name, func(t *testing.T) {
			f := newHandoffFixtureOn(t, st)
			if bad := withdrawVsGiveBack(t, f); bad != 0 {
				t.Errorf("withdraw vs give-back: %d of 60 rounds let the withdraw past the unknown take", bad)
			}
			if bad := acceptVsGiveBack(t, f); bad != 0 {
				t.Errorf("reconcile accept vs give-back: %d of 60 rounds did not have exactly one winner", bad)
			}
		})
	}
}

// Positive controls: with the guards off, both races are seen to break, so the test above
// is not green merely because nothing raced.
func TestHandoffGiveBackRacesPositiveControl(t *testing.T) {
	for name, st := range inboxStores(t) {
		t.Run(name, func(t *testing.T) {
			f := newHandoffFixtureOn(t, st)
			handoffTakeGuards = false
			bad := withdrawVsGiveBack(t, f)
			handoffTakeGuards = true
			if bad == 0 {
				t.Error("a withdraw without the take guard never got past the unknown take: the race test cannot tell it from the real one")
			}
			handoffGiveBackGuards = false
			bad = acceptVsGiveBack(t, f)
			handoffGiveBackGuards = true
			if bad == 0 {
				t.Error("an unconditional give-back never won alongside the accept: the race test cannot tell it from the real one")
			}
		})
	}
	t.Cleanup(func() { handoffTakeGuards, handoffGiveBackGuards = true, true })
}

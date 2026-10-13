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
	handoffTakeGuards = false
	t.Cleanup(func() { handoffTakeGuards = true })
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

// A take whose Console went quiet past the take window loses the offer to the sender, and
// when that Console wakes up its create is fenced: the confirm fails, so it starts nothing.
// A confirmed take — its create has reached the Agent — never goes stale for the sender or
// for another take; only its holder settles it, or the recipient declines.
func TestHandoffTakeConfirmFencesAStaleTake(t *testing.T) {
	ctx := context.Background()
	for name, st := range inboxStores(t) {
		t.Run(name, func(t *testing.T) {
			f := newHandoffFixtureOn(t, st)
			old := time.Now().UTC().Add(-HandoffTakeWindow - time.Minute).Format(time.RFC3339)
			must := func(want, ok bool, err error, what string) {
				t.Helper()
				if err != nil || ok != want {
					t.Fatalf("%s: ok=%v err=%v, want %v", what, ok, err, want)
				}
			}

			slept := f.offer(t, "")
			if _, err := st.CreateSessionHandoffOffer(ctx, slept); err != nil {
				t.Fatal(err)
			}
			ok, err := st.TakeSessionHandoffOffer(ctx, slept.ID, f.recipient.ID, "slept", old)
			must(true, ok, err, "take, then the machine sleeps")
			ok, err = st.TransitionSessionHandoffOffer(ctx, slept.ID, "pending", "withdrawn", NowTS(), "")
			must(true, ok, err, "the sender withdraws past the window")
			ok, err = st.ConfirmSessionHandoffTake(ctx, slept.ID, f.recipient.ID, "slept", NowTS())
			must(false, ok, err, "the woken Console's create")

			held := f.offer(t, "")
			if _, err := st.CreateSessionHandoffOffer(ctx, held); err != nil {
				t.Fatal(err)
			}
			ok, err = st.TakeSessionHandoffOffer(ctx, held.ID, f.recipient.ID, "slow", NowTS())
			must(true, ok, err, "take")
			ok, err = st.ConfirmSessionHandoffTake(ctx, held.ID, f.recipient.ID, "other", NowTS())
			must(false, ok, err, "confirm with another token")
			ok, err = st.ConfirmSessionHandoffTake(ctx, held.ID, f.recipient.ID, "slow", NowTS())
			must(true, ok, err, "the create reaches the Agent")
			// The create is slow: far past the window, and past the offer's own deadline.
			if _, err := st.DB().ExecContext(ctx, `UPDATE session_handoff_offer SET taken_at=?, expires_at=? WHERE id=?`, old, old, held.ID); err != nil {
				t.Fatal(err)
			}
			ok, err = st.TransitionSessionHandoffOffer(ctx, held.ID, "pending", "withdrawn", NowTS(), "")
			must(false, ok, err, "the sender withdraws a confirmed take")
			if expired, err := st.ExpireSessionHandoffOffers(ctx, NowTS()); err != nil || len(expired) != 0 {
				t.Fatalf("expiry overrode a confirmed take: %v err=%v", expired, err)
			}
			ok, err = st.TakeSessionHandoffOffer(ctx, held.ID, f.recipient.ID, "second-tab", NowTS())
			must(false, ok, err, "a second tab takes a confirmed take")
			ok, err = st.TransitionSessionHandoffOffer(ctx, held.ID, "pending", "accepted", NowTS(), "s")
			must(false, ok, err, "a token-less accept of a confirmed take")
			ok, err = st.AcceptSessionHandoffOffer(ctx, held.ID, f.recipient.ID, "slow", NowTS(), "late-but-fine")
			must(true, ok, err, "the slow launch accepts")

			// Another tab of the recipient declining does not prove the confirmed create
			// stopped: a decline would let the sender launch without withdrawing.
			slow := f.offer(t, "")
			if _, err := st.CreateSessionHandoffOffer(ctx, slow); err != nil {
				t.Fatal(err)
			}
			if _, err := st.TakeSessionHandoffOffer(ctx, slow.ID, f.recipient.ID, "tab-1", old); err != nil {
				t.Fatal(err)
			}
			ok, err = st.ConfirmSessionHandoffTake(ctx, slow.ID, f.recipient.ID, "tab-1", old)
			must(true, ok, err, "confirm")
			ok, err = st.TransitionSessionHandoffOffer(ctx, slow.ID, "pending", "declined", NowTS(), "")
			must(false, ok, err, "tab 2 declines a confirmed take past the window")
			// An unconfirmed stale take does yield to a decline.
			ok, err = st.ReleaseSessionHandoffOffer(ctx, slow.ID, f.recipient.ID, "tab-1")
			must(true, ok, err, "release")
			if _, err := st.TakeSessionHandoffOffer(ctx, slow.ID, f.recipient.ID, "tab-3", old); err != nil {
				t.Fatal(err)
			}
			ok, err = st.TransitionSessionHandoffOffer(ctx, slow.ID, "pending", "declined", NowTS(), "")
			must(true, ok, err, "decline past an unconfirmed stale take")
		})
	}
}

// A Console from before the take reports its launch with a token-less accept. It must land
// only on an offer nobody has taken: a stale take still belongs to a newer tab, whose accept
// would otherwise be refused after its own launch.
func TestHandoffTokenlessAcceptNeverSettlesATake(t *testing.T) {
	ctx := context.Background()
	for name, st := range inboxStores(t) {
		t.Run(name, func(t *testing.T) {
			f := newHandoffFixtureOn(t, st)
			o := f.offer(t, "")
			if _, err := st.CreateSessionHandoffOffer(ctx, o); err != nil {
				t.Fatal(err)
			}
			old := time.Now().UTC().Add(-HandoffTakeWindow - time.Minute).Format(time.RFC3339)
			if ok, err := st.TakeSessionHandoffOffer(ctx, o.ID, f.recipient.ID, "new-tab", old); err != nil || !ok {
				t.Fatal("take", ok, err)
			}
			if ok, err := st.TransitionSessionHandoffOffer(ctx, o.ID, "pending", "accepted", NowTS(), "old-tab"); err != nil || ok {
				t.Fatalf("a token-less accept settled a stale take: ok=%v err=%v", ok, err)
			}
			if ok, err := st.AcceptSessionHandoffOffer(ctx, o.ID, f.recipient.ID, "new-tab", NowTS(), "new-tab-session"); err != nil || !ok {
				t.Fatalf("the take's own accept: ok=%v err=%v", ok, err)
			}
		})
	}
}

// Unsharing revokes at once: the body is erased and the offer leaves the inbox. An offer whose
// take is confirmed keeps its status and take, though: expiring it would let the sender's own
// launch skip the withdraw while the recipient's session starts. A fence that has not been
// passed yet loses to the unshare, as before.
func TestHandoffUnshareKeepsAConfirmedLaunchFenced(t *testing.T) {
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
			if _, err := st.CreateSessionHandoffOffer(ctx, o); err != nil {
				t.Fatal(err)
			}
			ok, err := st.TakeSessionHandoffOffer(ctx, o.ID, f.recipient.ID, "tok", NowTS())
			must(true, ok, err, "take")
			ok, err = st.ConfirmSessionHandoffTake(ctx, o.ID, f.recipient.ID, "tok", NowTS())
			must(true, ok, err, "confirm")
			if err := st.DeleteSessionShare(ctx, f.sessionShareRow.ID, f.owner.ID); err != nil {
				t.Fatal(err)
			}
			got, _, _ := st.GetSessionHandoffOffer(ctx, o.ID)
			if got.Status != "pending" || got.TakeConfirmedAt == "" || got.Ciphertext != "" {
				t.Fatalf("after the unshare during a confirmed launch: %+v, want pending, still taken, body erased", got)
			}
			if inbox, err := st.ListSessionHandoffOffersByRecipient(ctx, f.recipient.ID); err != nil || len(inbox) != 0 {
				t.Fatalf("inbox=%v err=%v, want empty after the unshare", inbox, err)
			}
			ok, err = st.TransitionSessionHandoffOffer(ctx, o.ID, "pending", "withdrawn", NowTS(), "")
			must(false, ok, err, "the sender withdraws the unshared, confirmed launch")
			// The launch failed after all: giving it back expires it, since the share is gone.
			ok, err = st.ReleaseSessionHandoffOffer(ctx, o.ID, f.recipient.ID, "tok")
			must(true, ok, err, "release")
			if got, _, _ := st.GetSessionHandoffOffer(ctx, o.ID); got.Status != "expired" || got.DecidedAt == "" {
				t.Fatalf("after release: %+v, want expired", got)
			}

			// Re-share, then unshare before the fence: the offer expires and the create is refused.
			if err := st.PutSessionShare(ctx, f.sessionShareRow); err != nil {
				t.Fatal(err)
			}
			early := f.offer(t, "")
			if _, err := st.CreateSessionHandoffOffer(ctx, early); err != nil {
				t.Fatal(err)
			}
			ok, err = st.TakeSessionHandoffOffer(ctx, early.ID, f.recipient.ID, "early", NowTS())
			must(true, ok, err, "take")
			if err := st.DeleteSessionShare(ctx, f.sessionShareRow.ID, f.owner.ID); err != nil {
				t.Fatal(err)
			}
			if got, _, _ := st.GetSessionHandoffOffer(ctx, early.ID); got.Status != "expired" {
				t.Fatalf("an unconfirmed take kept the offer through the unshare: %+v", got)
			}
			ok, err = st.ConfirmSessionHandoffTake(ctx, early.ID, f.recipient.ID, "early", NowTS())
			must(false, ok, err, "the create after the unshare")
		})
	}
}

// A take confirms once, so two creates can never share its Agent key (issue #2040); a Console
// can give back only an unconfirmed take; the CP reads a confirmed take's token back to ask
// the recipient's Agent about its create, and gives it back on the Agent's word.
func TestHandoffConfirmOnceAndConfirmedToken(t *testing.T) {
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
			if _, err := st.CreateSessionHandoffOffer(ctx, o); err != nil {
				t.Fatal(err)
			}
			_, held, err := st.ConfirmedSessionHandoffTake(ctx, o.ID, f.recipient.ID)
			must(false, held, err, "token of an untaken offer")
			ok, err := st.TakeSessionHandoffOffer(ctx, o.ID, f.recipient.ID, "tok", NowTS())
			must(true, ok, err, "take")
			_, held, err = st.ConfirmedSessionHandoffTake(ctx, o.ID, f.recipient.ID)
			must(false, held, err, "token of an unconfirmed take")
			ok, err = st.ConfirmSessionHandoffTake(ctx, o.ID, f.recipient.ID, "tok", NowTS())
			must(true, ok, err, "confirm")
			ok, err = st.ConfirmSessionHandoffTake(ctx, o.ID, f.recipient.ID, "tok", NowTS())
			must(false, ok, err, "a second create under the same take")
			token, held, err := st.ConfirmedSessionHandoffTake(ctx, o.ID, f.recipient.ID)
			if err != nil || !held || token != "tok" {
				t.Fatalf("confirmed token = %q held=%v err=%v", token, held, err)
			}
			_, held, err = st.ConfirmedSessionHandoffTake(ctx, o.ID, f.other.ID)
			must(false, held, err, "another member reads the token")
			ok, err = st.ReleaseUnconfirmedSessionHandoffTake(ctx, o.ID, f.recipient.ID, "tok")
			must(false, ok, err, "a Console releases a confirmed take")
			ok, err = st.TransitionSessionHandoffOffer(ctx, o.ID, "pending", "withdrawn", NowTS(), "")
			must(false, ok, err, "withdraw under the confirmed take")
			ok, err = st.ReleaseSessionHandoffOffer(ctx, o.ID, f.recipient.ID, "tok")
			must(true, ok, err, "the Agent refused: the CP gives it back")
			ok, err = st.ConfirmSessionHandoffTake(ctx, o.ID, f.recipient.ID, "tok", NowTS())
			must(false, ok, err, "a retry under the released take")
			ok, err = st.TransitionSessionHandoffOffer(ctx, o.ID, "pending", "withdrawn", NowTS(), "")
			must(true, ok, err, "withdraw after the release")

			// The Console's own release: unconfirmed only, and only with the take's token.
			o2 := f.offer(t, "")
			if _, err := st.CreateSessionHandoffOffer(ctx, o2); err != nil {
				t.Fatal(err)
			}
			ok, err = st.TakeSessionHandoffOffer(ctx, o2.ID, f.recipient.ID, "early", NowTS())
			must(true, ok, err, "take")
			ok, err = st.ReleaseUnconfirmedSessionHandoffTake(ctx, o2.ID, f.recipient.ID, "other")
			must(false, ok, err, "release with another token")
			ok, err = st.ReleaseUnconfirmedSessionHandoffTake(ctx, o2.ID, f.recipient.ID, "early")
			must(true, ok, err, "release the unconfirmed take")
			ok, err = st.ConfirmSessionHandoffTake(ctx, o2.ID, f.recipient.ID, "early", NowTS())
			must(false, ok, err, "the released take's late create")
		})
	}
}

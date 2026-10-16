package sessionx

// The state machine of the resume that waits for a PERSON (docs/log/47 §4-11). Detecting the
// login failure itself is held down by internal/agents/claude/abort_test.go; what is watched
// here is the one rule that makes this different from the retryable resume — nothing is sent
// until the credentials in force are newer than the turn they killed. There is no tmux, no
// claude and no credentials file here, so the side effects are replaced (newAbortFixture) and
// so is the login moment.

import (
	"testing"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents/claude"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/chatx"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
)

func authAbort(at time.Time) claude.Abort {
	return claude.Abort{
		Msg:  "Please run /login · API Error: 401 OAuth access token has expired. Re-authenticate to continue.",
		Auth: true,
		At:   at,
	}
}

// setLoginAt replaces the "when was the login in force written" reading. A zero time is what
// the real one returns when there is nothing to judge on.
func setLoginAt(t *testing.T, at time.Time) {
	t.Helper()
	orig := authResumeNow
	authResumeNow = func() time.Time { return at }
	t.Cleanup(func() { authResumeNow = orig })
}

func arState(t *testing.T, name string) authResumeState {
	t.Helper()
	st, _ := authResumeStates.Read(name)
	return st
}

func arMeta() session.Meta {
	return session.Meta{Name: "ar1", Dir: "/tmp/ar1", Kind: session.KindClaude}
}

// TestAuthResumeWaitsForRenewal is the heart of it: a login that has NOT been renewed must
// never trigger a resume, however long the wait — re-sending onto the same credentials
// reproduces the same 401 and burns an attempt. The credentials being valid *right now* is
// not the test either: after a server-side revocation they read valid from the first tick.
func TestAuthResumeWaitsForRenewal(t *testing.T) {
	f := newAbortFixture(t)
	m := arMeta()
	cut := time.Now()
	a := authAbort(cut)

	// The login on disk is OLDER than the turn that died: it is the one that failed.
	setLoginAt(t, cut.Add(-30*time.Minute))
	for _, d := range []time.Duration{time.Second, time.Hour, 24 * time.Hour} {
		authResumeAttempt(m, arState(t, m.Name), a, cut.Add(d))
		if len(f.sent) != 0 {
			t.Fatalf("resumed %v after the failure without a renewed login: %v", d, f.sent)
		}
	}
	if st := arState(t, m.Name); st.At == "" {
		t.Fatal("the episode is not persisted the moment it opens (it would be reopened every tick)")
	}
}

// TestAuthResumeSendsOnceRenewed: signing in again is what releases it, after the new
// credential has settled — and only one prompt goes out, with the auto-resume wording the
// mirror badges.
func TestAuthResumeSendsOnceRenewed(t *testing.T) {
	f := newAbortFixture(t)
	m := arMeta()
	cut := time.Now()
	a := authAbort(cut)

	authResumeAttempt(m, arState(t, m.Name), a, cut.Add(time.Minute)) // opens the episode
	renewed := cut.Add(10 * time.Minute)
	setLoginAt(t, renewed)

	authResumeAttempt(m, arState(t, m.Name), a, renewed.Add(time.Second))
	if len(f.sent) != 0 {
		t.Fatalf("sent before the new credential settled: %v", f.sent)
	}

	authResumeAttempt(m, arState(t, m.Name), a, renewed.Add(authResumeSettle+time.Second))
	if len(f.sent) != 1 {
		t.Fatalf("resume prompts = %d, want 1: %v", len(f.sent), f.sent)
	}
	if f.sent[0] != abortResumePrompt() {
		t.Errorf("prompt = %q, want the auto-resume wording %q", f.sent[0], abortResumePrompt())
	}
	if st := arState(t, m.Name); st.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", st.Attempts)
	}
	// The next sweep must not fire again — the episode is still open (the tail is still the
	// old failure until the resumed turn lands), so only the backoff holds it back.
	authResumeAttempt(m, arState(t, m.Name), a, renewed.Add(authResumeSettle+time.Minute))
	if len(f.sent) != 1 {
		t.Errorf("ignored the backoff and sent %d times", len(f.sent))
	}
}

// TestAuthResumeCapsThenHandsOver: a renewed login that fails the same way is not the fix.
// Step back and line the report side's counter up, so what reaches the user carries the "cap
// reached" wording instead of the resume quietly retrying forever.
func TestAuthResumeCapsThenHandsOver(t *testing.T) {
	f := newAbortFixture(t)
	m := arMeta()
	cut := time.Now()
	a := authAbort(cut)
	renewed := cut.Add(time.Minute)
	setLoginAt(t, renewed)

	now := renewed.Add(authResumeSettle + time.Second)
	for i := 0; i < chatx.MaxAutoResumeAttempts; i++ {
		authResumeAttempt(m, arState(t, m.Name), a, now)
		now = now.Add(authResumeBackoff + time.Second)
	}
	if len(f.sent) != chatx.MaxAutoResumeAttempts {
		t.Fatalf("resumes = %d, want %d", len(f.sent), chatx.MaxAutoResumeAttempts)
	}
	if arState(t, m.Name).GaveUp != "" {
		t.Fatal("gave up before the cap was reached")
	}

	authResumeAttempt(m, arState(t, m.Name), a, now)
	st := arState(t, m.Name)
	if st.GaveUp != authGaveUpCapped {
		t.Fatalf("gaveUp = %q, want %q", st.GaveUp, authGaveUpCapped)
	}
	if len(f.sent) != chatx.MaxAutoResumeAttempts {
		t.Errorf("gave up and still sent: %v", f.sent)
	}
	if got := chatx.AutoResumeAttempts(m.Name); got != chatx.MaxAutoResumeAttempts {
		t.Errorf("the report side's counter = %d, want %d (the capped wording will not be used)", got, chatx.MaxAutoResumeAttempts)
	}
}

// TestAuthResumeUndeliverableGivesUp: signing in and then working in the pane by hand is
// exactly what a user does here, and every such sweep counts as undeliverable. It must step
// back rather than keep typing at a session somebody is using.
func TestAuthResumeUndeliverableGivesUp(t *testing.T) {
	f := newAbortFixture(t)
	f.pane.Idle = false // a turn is running in the pane
	m := arMeta()
	cut := time.Now()
	a := authAbort(cut)
	renewed := cut.Add(time.Minute)
	setLoginAt(t, renewed)

	now := renewed.Add(authResumeSettle + time.Second)
	for i := 0; i < authResumeMaxDeliverTries; i++ {
		authResumeAttempt(m, arState(t, m.Name), a, now)
		now = now.Add(authResumeBackoff + time.Second)
	}
	if len(f.sent) != 0 {
		t.Fatalf("typed into a busy pane: %v", f.sent)
	}
	if st := arState(t, m.Name); st.GaveUp != authGaveUpUndeliverable {
		t.Errorf("gaveUp = %q, want %q", st.GaveUp, authGaveUpUndeliverable)
	}
}

// TestAuthResumeClosesWhenTailMoves: the episode is closed by the tail no longer being a login
// failure — the resume landed, or the user carried on themselves. Left open, it would keep the
// attempt count of a failure that is over.
func TestAuthResumeClosesWhenTailMoves(t *testing.T) {
	newAbortFixture(t)
	m := arMeta()
	cut := time.Now()
	if err := authResumeStates.Write(m.Name, authResumeState{At: cut.Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	authResumeStep(m, claude.Abort{}, false, cut.Add(time.Minute))
	if _, has := authResumeStates.Read(m.Name); has {
		t.Error("the episode survived the tail moving on")
	}
}

// TestAuthResumeIgnoresRetryable: a cut-off a re-send fixes belongs to abort_resume.go. Both
// sweeps acting on one failure would send two "continue" prompts for one interruption.
func TestAuthResumeIgnoresRetryable(t *testing.T) {
	f := newAbortFixture(t)
	m := arMeta()
	cut := time.Now()
	setLoginAt(t, cut.Add(time.Hour))
	authResumeStep(m, retryableAbort(cut), true, cut.Add(2*time.Hour))
	if len(f.sent) != 0 {
		t.Fatalf("the auth sweep acted on a retryable cut-off: %v", f.sent)
	}
	if _, has := authResumeStates.Read(m.Name); has {
		t.Error("opened a login episode for a cut-off that has nothing to do with the login")
	}
}

const refreshStuckMsg = "Could not refresh your login because another Claude Code process is refreshing it (or exited mid-refresh) · Try again in a minute; if it keeps happening, close other Claude Code windows or sign in again with /login"

func refreshAbort(at time.Time) claude.Abort {
	return claude.Abort{Msg: refreshStuckMsg, Auth: true, At: at}
}

// noFastWatch keeps the real poller out of the state-machine tests, and records the episodes it
// would have been started for.
func noFastWatch(t *testing.T) *[]string {
	t.Helper()
	var started []string
	orig := authResumeFastWatch
	authResumeFastWatch = func(m session.Meta, st authResumeState) { started = append(started, m.Name) }
	t.Cleanup(func() { authResumeFastWatch = orig })
	return &started
}

// TestAuthResumeRefreshLockResumesQuickly: a refresh the lock holder finished is not a person's
// sign-in, so it does not wait the 15s settle — and the quick watch is started for it, once.
func TestAuthResumeRefreshLockResumesQuickly(t *testing.T) {
	f := newAbortFixture(t)
	started := noFastWatch(t)
	m := arMeta()
	cut := time.Now()
	a := refreshAbort(cut)

	authResumeAttempt(m, arState(t, m.Name), a, cut.Add(time.Second)) // opens the episode
	authResumeAttempt(m, arState(t, m.Name), a, cut.Add(2*time.Second))
	if len(*started) != 1 {
		t.Fatalf("fast watch started %d times, want once per episode", len(*started))
	}

	renewed := cut.Add(19 * time.Second)
	setLoginAt(t, renewed)
	authResumeAttempt(m, arState(t, m.Name), a, renewed.Add(authResumeRefreshSettle-time.Second))
	if len(f.sent) != 0 {
		t.Fatalf("sent before the refreshed credential settled: %v", f.sent)
	}
	authResumeAttempt(m, arState(t, m.Name), a, renewed.Add(authResumeRefreshSettle+time.Second))
	if len(f.sent) != 1 {
		t.Fatalf("resume prompts = %d, want 1 (a refresh needs far less settling than %v)", len(f.sent), authResumeSettle)
	}
}

// TestAuthResumeRefreshLockForcedWhenNothingRefreshes: a holder that died leaves a lock claude
// only abandons after 60s, and claude does not retry by itself. Past that point the resume goes
// out without a renewed credential — and never earlier, and never for a revoked login.
func TestAuthResumeRefreshLockForcedWhenNothingRefreshes(t *testing.T) {
	f := newAbortFixture(t)
	noFastWatch(t)
	m := arMeta()
	cut := time.Now()
	setLoginAt(t, cut.Add(-time.Hour)) // never renewed

	a := refreshAbort(cut)
	authResumeAttempt(m, arState(t, m.Name), a, cut)
	authResumeAttempt(m, arState(t, m.Name), a, cut.Add(authResumeRefreshForceAfter-time.Second))
	if len(f.sent) != 0 {
		t.Fatalf("forced before claude's 60s lock staleness could have passed: %v", f.sent)
	}
	authResumeAttempt(m, arState(t, m.Name), a, cut.Add(authResumeRefreshForceAfter))
	if len(f.sent) != 1 {
		t.Fatalf("resume prompts = %d, want 1 once the force point passed", len(f.sent))
	}

	// A revoked login is not that case: no amount of waiting makes a resume useful.
	f2 := newAbortFixture(t)
	m2 := session.Meta{Name: "ar2", Dir: "/tmp/ar2", Kind: session.KindClaude}
	authResumeAttempt(m2, arState(t, m2.Name), authAbort(cut), cut)
	authResumeAttempt(m2, arState(t, m2.Name), authAbort(cut), cut.Add(time.Hour))
	if len(f2.sent) != 0 {
		t.Fatalf("a non-refresh login failure was forced: %v", f2.sent)
	}
}

// TestAuthResumeForcedStillSettles: the force point replaces "no credential written since the
// failure", not the settle. A credential written one second ago may be a sign-in still being
// wired up, so even past 90s the resume waits for it to settle.
func TestAuthResumeForcedStillSettles(t *testing.T) {
	f := newAbortFixture(t)
	noFastWatch(t)
	m := arMeta()
	cut := time.Now()
	a := refreshAbort(cut)
	authResumeAttempt(m, arState(t, m.Name), a, cut)

	now := cut.Add(authResumeRefreshForceAfter + 10*time.Second)
	setLoginAt(t, now.Add(-time.Second))
	authResumeAttempt(m, arState(t, m.Name), a, now)
	if len(f.sent) != 0 {
		t.Fatalf("resumed on a credential written one second ago: %v", f.sent)
	}
	authResumeAttempt(m, arState(t, m.Name), a, now.Add(authResumeRefreshSettle))
	if len(f.sent) != 1 {
		t.Fatalf("resume prompts = %d, want 1 once that credential settled", len(f.sent))
	}
}

// TestAuthResumeForcedNeedsAJudgeableLogin: no readable credential (logged out, a half-written
// file, an environment token, a dead login) gives no moment, and forcing a resume onto it would
// only spend an attempt.
func TestAuthResumeForcedNeedsAJudgeableLogin(t *testing.T) {
	f := newAbortFixture(t)
	noFastWatch(t)
	m := arMeta()
	cut := time.Now()
	a := refreshAbort(cut)
	setLoginAt(t, time.Time{})
	authResumeAttempt(m, arState(t, m.Name), a, cut)
	authResumeAttempt(m, arState(t, m.Name), a, cut.Add(time.Hour))
	if len(f.sent) != 0 {
		t.Fatalf("forced a resume with no credential to judge: %v", f.sent)
	}
}

// TestAuthResumeForcedDoesNotCarryOverToARevokedLogin: refresh-lock error, forced resume, and the
// resumed turn then dies on a revoked login. Nothing may be forced from then on — only a credential
// newer than that failure releases it — and the spent attempt still counts.
func TestAuthResumeForcedDoesNotCarryOverToARevokedLogin(t *testing.T) {
	f := newAbortFixture(t)
	noFastWatch(t)
	m := arMeta()
	cut := time.Now()
	setLoginAt(t, cut.Add(-time.Hour))
	authResumeAttempt(m, arState(t, m.Name), refreshAbort(cut), cut)
	forcedAt := cut.Add(authResumeRefreshForceAfter)
	authResumeAttempt(m, arState(t, m.Name), refreshAbort(cut), forcedAt)
	if len(f.sent) != 1 {
		t.Fatalf("the first forced resume did not go out: %v", f.sent)
	}

	revoked := forcedAt.Add(time.Minute)
	for _, d := range []time.Duration{0, authResumeBackoff + time.Minute, time.Hour} {
		authResumeAttempt(m, arState(t, m.Name), authAbort(revoked), revoked.Add(d))
	}
	if len(f.sent) != 1 {
		t.Fatalf("forced a resume onto a revoked login: %v", f.sent)
	}
	if st := arState(t, m.Name); st.Attempts != 1 {
		t.Errorf("attempts = %d, want the spent one kept", st.Attempts)
	}

	renewed := revoked.Add(2 * time.Hour)
	setLoginAt(t, renewed)
	authResumeAttempt(m, arState(t, m.Name), authAbort(revoked), renewed.Add(authResumeSettle+time.Second))
	if len(f.sent) != 2 {
		t.Fatalf("a credential newer than the revoked failure did not release it: %v", f.sent)
	}
}

// TestAuthResumeForcedClockFollowsTheLatestRefreshFailure: a second refresh-lock failure inside the
// same episode (a manual retry the sweep never saw between) is a new lock to wait out. The 90s
// does not run from the episode's first failure, or it would resume into a lock that is seconds old.
func TestAuthResumeForcedClockFollowsTheLatestRefreshFailure(t *testing.T) {
	f := newAbortFixture(t)
	noFastWatch(t)
	m := arMeta()
	cut := time.Now()
	setLoginAt(t, cut.Add(-time.Hour))
	authResumeAttempt(m, arState(t, m.Name), refreshAbort(cut), cut)
	authResumeAttempt(m, arState(t, m.Name), refreshAbort(cut), cut.Add(authResumeRefreshForceAfter))
	if len(f.sent) != 1 {
		t.Fatalf("the first forced resume did not go out: %v", f.sent)
	}

	again := cut.Add(10 * time.Minute) // past the backoff
	a2 := refreshAbort(again)
	authResumeAttempt(m, arState(t, m.Name), a2, again.Add(time.Second))
	if len(f.sent) != 1 {
		t.Fatalf("forced a second resume one second after a new lock failure: %v", f.sent)
	}
	if st := arState(t, m.Name); st.Attempts != 1 {
		t.Errorf("attempts = %d, want the spent one kept", st.Attempts)
	}
	authResumeAttempt(m, arState(t, m.Name), a2, again.Add(authResumeRefreshForceAfter))
	if len(f.sent) != 2 {
		t.Fatalf("resume prompts = %d, want the second once its own 90s passed", len(f.sent))
	}
}

// TestRefreshWatchFollowsARestartedEpisode: the poller reads the current episode every time, so a
// refresh-lock episode that went through a revoked-login failure and back is watched against its
// latest failure — and the poller steps aside while the episode is not a refresh-lock one.
func TestRefreshWatchFollowsARestartedEpisode(t *testing.T) {
	newAbortFixture(t)
	noFastWatch(t)
	m := arMeta()
	cut := time.Now()
	setLoginAt(t, cut.Add(-time.Hour))

	if !refreshWatchStep(m, cut) {
		t.Fatal("watching an episode that does not exist")
	}
	authResumeAttempt(m, arState(t, m.Name), refreshAbort(cut), cut)
	if refreshWatchStep(m, cut.Add(time.Second)) {
		t.Fatal("the watch ended while the refresh was still pending")
	}

	revoked := cut.Add(time.Minute)
	authResumeAttempt(m, arState(t, m.Name), authAbort(revoked), revoked)
	if !refreshWatchStep(m, revoked.Add(time.Second)) {
		t.Fatal("kept polling an episode that is now a revoked login")
	}

	again := cut.Add(2 * time.Minute)
	latest := refreshAbort(again)
	authResumeAttempt(m, arState(t, m.Name), latest, again)
	origTail, origHand := refreshWatchTail, refreshWatchHandOver
	refreshWatchTail = func(session.Meta) (claude.Abort, bool) { return latest, true }
	handed := 0
	refreshWatchHandOver = func(session.Meta, claude.Abort, bool, time.Time) { handed++ }
	t.Cleanup(func() { refreshWatchTail, refreshWatchHandOver = origTail, origHand })
	renewed := again.Add(19 * time.Second)
	setLoginAt(t, renewed)
	// The reviewed case: judged against the ORIGINAL failure this credential is long "renewed";
	// against the latest one it is exactly the refresh that landed after it.
	if refreshWatchStep(m, renewed.Add(time.Second)) {
		t.Fatal("resumed or ended before the new credential settled")
	}
	if !refreshWatchStep(m, renewed.Add(authResumeRefreshSettle)) {
		t.Fatal("the watch did not finish after handing over")
	}
	if handed != 1 {
		t.Fatalf("handed the session back %d times, want once on the latest failure's refresh", handed)
	}
}

// TestRefreshWatchContinuesWhenTheHandOverRestartsTheEpisode: the poll judged the old failure
// (credential rewritten after it), but the tail read inside the hand over is a newer refresh-lock
// failure that restarts the episode. The watcher holds the key, so the restart's own request to
// start one is refused; it must keep watching rather than end.
func TestRefreshWatchContinuesWhenTheHandOverRestartsTheEpisode(t *testing.T) {
	newAbortFixture(t)
	noFastWatch(t)
	m := arMeta()
	cut := time.Now()
	authResumeAttempt(m, arState(t, m.Name), refreshAbort(cut), cut)
	renewed := cut.Add(19 * time.Second)
	setLoginAt(t, renewed)

	newer := refreshAbort(cut.Add(21 * time.Second)) // a manual retry failed again
	origTail, origHand := refreshWatchTail, refreshWatchHandOver
	refreshWatchTail = func(session.Meta) (claude.Abort, bool) { return newer, true }
	// authResumeAttempt is authResumeStep minus the live-session and preference gates (no tmux here).
	refreshWatchHandOver = func(m session.Meta, a claude.Abort, _ bool, now time.Time) {
		st, _ := authResumeStates.Read(m.Name)
		authResumeAttempt(m, st, a, now)
	}
	t.Cleanup(func() { refreshWatchTail, refreshWatchHandOver = origTail, origHand })

	if done := refreshWatchStep(m, renewed.Add(authResumeRefreshSettle)); done {
		t.Fatal("the watch ended although the hand over restarted the episode on a newer failure")
	}
	if st := arState(t, m.Name); st.At != newer.At.Format(time.RFC3339) {
		t.Fatalf("episode At = %q, want the newer failure %q", st.At, newer.At.Format(time.RFC3339))
	}
}

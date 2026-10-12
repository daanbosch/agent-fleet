package limitepoch

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestCurrentChangesOnlyOnAnotherAccount(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	st1, ok := Current("claude", "acct-a", t0)
	e1, since1 := st1.Epoch, st1.Since
	if !ok || e1 == "" {
		t.Fatalf("no epoch: %q ok=%v", e1, ok)
	}
	if !since1.IsZero() {
		t.Errorf("first account since = %v, want zero (nothing says older readings are someone else's)", since1)
	}
	if st, _ := Current("claude", "acct-a", t0.Add(time.Hour)); st.Epoch != e1 || !st.Since.Equal(since1) || !st.Seen.Equal(t0) {
		t.Errorf("same account moved the epoch: %+v", st)
	}
	t1 := t0.Add(2 * time.Hour)
	st2, _ := Current("claude", "acct-b", t1)
	e2, since2 := st2.Epoch, st2.Since
	if !st2.Seen.After(st1.Seen) {
		t.Errorf("re-login Seen %v does not outrank %v", st2.Seen, st1.Seen)
	}
	if e2 == e1 || !since2.Equal(t1) {
		t.Fatalf("re-login: epoch %q→%q since %v, want a new epoch since %v", e1, e2, since2, t1)
	}
	if Admit(since2, t1.Add(-time.Minute)) {
		t.Error("a reading from before the re-login was admitted under the new account")
	}
	if !Admit(since2, t1.Add(time.Minute)) || Admit(since2, time.Time{}) {
		t.Error("Admit: a later reading must pass and a reading without a time must not")
	}
	// The epoch is not the identity, and the ledger never stores it in the clear.
	b, err := os.ReadFile(ledgerPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "acct-b") || strings.Contains(e2, "acct") {
		t.Errorf("identity leaked: ledger=%s epoch=%s", b, e2)
	}
	// Survives a restart: a reload from disk keeps the same epoch and since.
	mu.Lock()
	loadedAt = ""
	mu.Unlock()
	if st, _ := Current("claude", "acct-b", t1.Add(time.Hour)); st.Epoch != e2 || !st.Since.Equal(since2) {
		t.Errorf("after reload %+v, want %q/%v", st, e2, since2)
	}
	// A→B→A: the return to A is a new login, later than B.
	st3, _ := Current("claude", "acct-a", t1.Add(2*time.Hour))
	if st3.Epoch != e1 || !st3.Seen.After(st2.Seen) || !st3.Since.Equal(t1.Add(2*time.Hour)) {
		t.Errorf("A again = %+v", st3)
	}
}

// Review finding (#2017, round 2): two logins recognised in the same instant (or after the
// clock went back) must still be ordered, at full precision, or the CP can never switch.
func TestSeenIsStrictlyIncreasingEvenWithoutClockProgress(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	at := time.Date(2026, 10, 12, 11, 58, 0, 0, time.UTC)
	a, _ := Current("claude", "a", at)
	b, _ := Current("claude", "b", at)
	c, _ := Current("codex", "x", at.Add(-time.Hour)) // the clock went back
	d, _ := Current("claude", "a", at.Add(-time.Hour))
	order := []Stamp{a, b, c, d}
	for i := 1; i < len(order); i++ {
		if order[i].SeenText() <= order[i-1].SeenText() {
			t.Fatalf("Seen not increasing at %d: %q then %q", i, order[i-1].SeenText(), order[i].SeenText())
		}
	}
	// The switch to b happened "at" 11:58:00; a reading stamped 11:58:00 is a's, not b's.
	if Admit(b.Since, at) {
		t.Fatal("a reading from the switch's own second was admitted under the new account")
	}
}

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
	e1, since1, ok := Current("claude", "acct-a", t0)
	if !ok || e1 == "" {
		t.Fatalf("no epoch: %q ok=%v", e1, ok)
	}
	if !since1.IsZero() {
		t.Errorf("first account since = %v, want zero (nothing says older readings are someone else's)", since1)
	}
	if e, s, _ := Current("claude", "acct-a", t0.Add(time.Hour)); e != e1 || !s.Equal(since1) {
		t.Errorf("same account moved the epoch: %q/%v", e, s)
	}
	t1 := t0.Add(2 * time.Hour)
	e2, since2, _ := Current("claude", "acct-b", t1)
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
	if e, s, _ := Current("claude", "acct-b", t1.Add(time.Hour)); e != e2 || !s.Equal(since2) {
		t.Errorf("after reload %q/%v, want %q/%v", e, s, e2, since2)
	}
}

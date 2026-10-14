package opencode

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func putSid(t *testing.T, path, val string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(val), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestSlotSidsLegacyFallback(t *testing.T) {
	now := time.Now()
	old, newer := now.Add(-time.Hour), now
	cases := []struct {
		name         string
		legacy       string
		legacyTime   time.Time
		state        string
		stateTime    time.Time
		want         string
		legacyStays  bool
		stateAfterwd string
	}{
		{name: "state missing adopts legacy", legacy: "ses_old", legacyTime: old, want: "ses_old", stateAfterwd: "ses_old"},
		{name: "newer legacy wins", legacy: "ses_new", legacyTime: newer, state: "ses_stale", stateTime: old, want: "ses_new", stateAfterwd: "ses_new"},
		{name: "state wins when newer", legacy: "ses_stale", legacyTime: old, state: "ses_new", stateTime: newer, want: "ses_new", legacyStays: true, stateAfterwd: "ses_new"},
		{name: "state wins on equal mtime", legacy: "ses_a", legacyTime: newer, state: "ses_b", stateTime: newer, want: "ses_b", legacyStays: true, stateAfterwd: "ses_b"},
		{name: "empty legacy ignored", legacy: "", legacyTime: newer, state: "ses_b", stateTime: old, want: "ses_b", legacyStays: true, stateAfterwd: "ses_b"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			lp := legacySidPath("slot1")
			putSid(t, lp, c.legacy, c.legacyTime)
			if c.state != "" {
				putSid(t, sids.Path("slot1"), c.state, c.stateTime)
			}
			if got := sids.Read("slot1"); got != c.want {
				t.Fatalf("Read = %q, want %q", got, c.want)
			}
			if _, err := os.Stat(lp); (err == nil) != c.legacyStays {
				t.Fatalf("legacy file present = %v, want %v", err == nil, c.legacyStays)
			}
			if got := sids.SidStore.Read("slot1"); got != c.stateAfterwd {
				t.Fatalf("state dir holds %q, want %q", got, c.stateAfterwd)
			}
		})
	}
}

func TestSlotSidsRemoveDropsLegacyToo(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	putSid(t, legacySidPath("slot1"), "ses_x", time.Now())
	sids.Remove("slot1")
	if got := sids.Read("slot1"); got != "" {
		t.Fatalf("Read after Remove = %q, want none", got)
	}
}

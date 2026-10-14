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
			sids.lastScan = time.Time{}
			lp := filepath.Join(legacySidDir(), "slot1")
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
	sids.lastScan = time.Time{}
	putSid(t, filepath.Join(legacySidDir(), "slot1"), "ses_x", time.Now())
	sids.Remove("slot1")
	if got := sids.Read("slot1"); got != "" {
		t.Fatalf("Read after Remove = %q, want none", got)
	}
}

// A legacy file that cannot be deleted must not bring a cleared conversation back.
func TestSlotSidsRemoveSurvivesUndeletableLegacy(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions do not bind root")
	}
	t.Setenv("HOME", t.TempDir())
	sids.lastScan = time.Time{}
	lp := filepath.Join(legacySidDir(), "slot1")
	putSid(t, lp, "ses_cleared", time.Now().Add(-time.Minute))
	sids.Write("slot1", "ses_cleared")
	if err := os.Chmod(filepath.Dir(lp), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(lp), 0o700) })
	sids.Remove("slot1")
	sids.lastScan = time.Time{}
	if got := sids.Read("slot1"); got != "" {
		t.Fatalf("Read after Remove = %q, want none", got)
	}
	// A later write by an old plugin is newer than the tombstone and counts again.
	_ = os.Chmod(filepath.Dir(lp), 0o700)
	putSid(t, lp, "ses_again", time.Now().Add(time.Minute))
	sids.lastScan = time.Time{}
	if got := sids.Read("slot1"); got != "ses_again" {
		t.Fatalf("Read after a fresh legacy write = %q, want ses_again", got)
	}
}

// The old record is the only copy until the state write lands.
func TestSlotSidsKeepsLegacyWhenStateWriteFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sids.lastScan = time.Time{}
	lp := filepath.Join(legacySidDir(), "slot1")
	putSid(t, lp, "ses_only", time.Now())
	staging := filepath.Join(sids.SidStore.Path("x"), "..", ".staging")
	if err := os.MkdirAll(filepath.Dir(staging), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staging, nil, 0o600); err != nil { // a file where the staging dir goes
		t.Fatal(err)
	}
	if got := sids.Read("slot1"); got != "" {
		t.Fatalf("Read = %q with an unwritable state dir", got)
	}
	if _, err := os.Stat(lp); err != nil {
		t.Fatalf("legacy record lost: %v", err)
	}
	os.Remove(staging)
	sids.lastScan = time.Time{}
	if got := sids.Read("slot1"); got != "ses_only" {
		t.Fatalf("Read after recovery = %q", got)
	}
}

// Polled readers must not re-list the old directory every call.
func TestSlotSidsScanIsThrottled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sids.lastScan = time.Time{}
	_ = sids.Read("slot1")
	putSid(t, filepath.Join(legacySidDir(), "slot1"), "ses_late", time.Now())
	if got := sids.Read("slot1"); got != "" {
		t.Fatalf("second Read inside the interval looked at the old directory: %q", got)
	}
}

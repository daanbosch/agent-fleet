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

func TestSlotSidsReadsTheNewerRecord(t *testing.T) {
	now := time.Now()
	old, newer := now.Add(-time.Hour), now
	cases := []struct {
		name       string
		legacy     string
		legacyTime time.Time
		state      string
		stateTime  time.Time
		want       string
	}{
		{name: "state missing uses legacy", legacy: "ses_old", legacyTime: old, want: "ses_old"},
		{name: "newer legacy wins", legacy: "ses_new", legacyTime: newer, state: "ses_stale", stateTime: old, want: "ses_new"},
		{name: "state wins when newer", legacy: "ses_stale", legacyTime: old, state: "ses_new", stateTime: newer, want: "ses_new"},
		{name: "state wins on equal mtime", legacy: "ses_a", legacyTime: newer, state: "ses_b", stateTime: newer, want: "ses_b"},
		{name: "empty legacy ignored", legacy: "", legacyTime: newer, state: "ses_b", stateTime: old, want: "ses_b"},
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
			// Read never touches either file: nothing to race a plugin's write.
			if b, err := os.ReadFile(lp); err != nil || string(b) != c.legacy {
				t.Fatalf("legacy file changed by Read: %q, %v", b, err)
			}
			if c.state != "" {
				if got := sids.SidStore.Read("slot1"); got != c.state {
					t.Fatalf("state file changed by Read: %q", got)
				}
			} else if _, err := os.Stat(sids.Path("slot1")); err == nil {
				t.Fatal("Read created a state file")
			}
		})
	}
}

// A Write after the legacy record was captured makes the stale legacy file lose by itself.
func TestSlotSidsWriteOutranksStaleLegacy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	putSid(t, legacySidPath("slot1"), "ses_legacy", time.Now().Add(-time.Minute))
	sids.Write("slot1", "ses_current")
	if got := sids.Read("slot1"); got != "ses_current" {
		t.Fatalf("Read = %q", got)
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

// A legacy file that cannot be deleted must not bring a cleared conversation back.
func TestSlotSidsRemoveSurvivesUndeletableLegacy(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions do not bind root")
	}
	t.Setenv("HOME", t.TempDir())
	lp := legacySidPath("slot1")
	putSid(t, lp, "ses_cleared", time.Now().Add(-time.Minute))
	sids.Write("slot1", "ses_cleared")
	if err := os.Chmod(filepath.Dir(lp), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(lp), 0o700) })
	sids.Remove("slot1")
	if got := sids.Read("slot1"); got != "" {
		t.Fatalf("Read after Remove = %q, want none", got)
	}
	// A later write by an old plugin is newer than the tombstone and counts again.
	_ = os.Chmod(filepath.Dir(lp), 0o700)
	putSid(t, lp, "ses_again", time.Now().Add(time.Minute))
	if got := sids.Read("slot1"); got != "ses_again" {
		t.Fatalf("Read after a fresh legacy write = %q, want ses_again", got)
	}
}

// When the old directory cannot even be examined at Remove time, the clear must still stick
// once it becomes readable again.
func TestSlotSidsRemoveSurvivesUnreadableLegacyDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions do not bind root")
	}
	t.Setenv("HOME", t.TempDir())
	lp := legacySidPath("slot1")
	putSid(t, lp, "ses_cleared", time.Now().Add(-time.Minute))
	sids.Write("slot1", "ses_cleared")
	if err := os.Chmod(filepath.Dir(lp), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(lp), 0o700) })
	sids.Remove("slot1")
	if err := os.Chmod(filepath.Dir(lp), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := sids.Read("slot1"); got != "" {
		t.Fatalf("Read after Remove = %q, want none", got)
	}
}

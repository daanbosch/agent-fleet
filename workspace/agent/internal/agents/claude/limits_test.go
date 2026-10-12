package claude

import (
	"os"
	"testing"
	"time"
)

// claude re-renders its statusLine from the rate_limits it already holds. A render that
// restates the same numbers is a cache read and must not move the observation time (ADR 0111
// decision 6) — only a changed reading does.
func TestStatusLineRerenderDoesNotRefreshTheObservation(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	payload := func(pct string) []byte {
		return []byte(`{"rate_limits":{"five_hour":{"used_percentage":` + pct + `,"resets_at":9999999999}}}`)
	}
	captureFromStatusLine(payload("30"))
	c, _ := readCapturedUsage()
	old := time.Now().Add(-2 * time.Hour).Unix()
	// A capture from before the per-window fields: only the capture-wide time.
	c.ObservedAt, c.CapturedAt, c.FiveHourObservedAt, c.SevenDayObservedAt = old, old, 0, 0
	writeCapturedUsage(*c)

	captureFromStatusLine(payload("30"))
	c, at := readCapturedUsage()
	if c.ObservedAt != old {
		t.Fatalf("an unchanged re-render moved observedAt to %d (was %d)", c.ObservedAt, old)
	}
	if at.Unix() == old {
		t.Error("capturedAt should still record the render")
	}
	if r := LimitReading(); len(r.Windows) != 1 || r.Windows[0].ObservedAt.Unix() != old {
		t.Fatalf("LimitReading = %+v, want the original observation time", r)
	}

	captureFromStatusLine(payload("31"))
	if c, _ := readCapturedUsage(); time.Since(time.Unix(c.ObservedAt, 0)) > time.Minute {
		t.Fatalf("a changed reading kept the old observedAt %d", c.ObservedAt)
	}
}

func TestAccountIDKeepsTheLastGoodAnswerOnATornWrite(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	write := func(s string) {
		if err := os.WriteFile(claudeJSONPath(), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"oauthAccount":{"accountUuid":"u-1","emailAddress":"a@example.com"}}`)
	if got := accountID(); got != "u-1" {
		t.Fatalf("accountID = %q", got)
	}
	write(`{"oauthAccount":{"accountUu`) // claude mid-write
	if got := accountID(); got != "u-1" {
		t.Fatalf("a torn read changed the identity to %q", got)
	}
	write(`{"oauthAccount":{"accountUuid":"u-2"},"padding":"x"}`)
	if got := accountID(); got != "u-2" {
		t.Fatalf("a real re-login was not seen: %q", got)
	}
	os.Remove(claudeJSONPath())
	if got := accountID(); got != "" {
		t.Fatalf("no file should be no identity, got %q", got)
	}
}

// Review finding (#2017): a render where only the 5-hour percentage moved must not refresh the
// weekly window's observation time, and a reader that takes one time for both (the chip's
// observedAt) gets the older one.
func TestStatusLineObservationTimeIsPerWindow(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	payload := func(fh string) []byte {
		return []byte(`{"rate_limits":{"five_hour":{"used_percentage":` + fh +
			`,"resets_at":9999999999},"seven_day":{"used_percentage":10,"resets_at":9999999998}}}`)
	}
	captureFromStatusLine(payload("30"))
	c, _ := readCapturedUsage()
	old := time.Now().Add(-2 * time.Hour).Unix()
	c.ObservedAt, c.FiveHourObservedAt, c.SevenDayObservedAt = old, old, old
	writeCapturedUsage(*c)

	captureFromStatusLine(payload("31"))
	c, _ = readCapturedUsage()
	if c.SevenDayObservedAt != old {
		t.Fatalf("unchanged weekly window moved to %d (was %d)", c.SevenDayObservedAt, old)
	}
	if time.Since(time.Unix(c.FiveHourObservedAt, 0)) > time.Minute {
		t.Fatalf("changed 5-hour window kept %d", c.FiveHourObservedAt)
	}
	if c.ObservedAt != old {
		t.Fatalf("capture-wide observedAt = %d, want the older window's %d", c.ObservedAt, old)
	}
	r := LimitReading()
	got := map[string]int64{}
	for _, w := range r.Windows {
		got[w.Unit] = w.ObservedAt.Unix()
	}
	if got["7d"] != old || got["5h"] == old {
		t.Fatalf("LimitReading times = %v", got)
	}
}

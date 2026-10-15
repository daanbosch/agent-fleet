package usagewin

import (
	"testing"
	"time"
)

func TestAdjust(t *testing.T) {
	now := time.Date(2026, 10, 15, 14, 0, 0, 0, time.UTC)
	p, r, s := Adjust(40, 300, now.Add(time.Hour), now)
	if p != 40 || !r.Equal(now.Add(time.Hour)) || s {
		t.Fatalf("future window changed: %v %v %v", p, r, s)
	}
	past := now.Add(-time.Hour) // 13:00
	p, r, s = Adjust(98, 300, past, now)
	if p != 0 || !s || !r.Equal(past.Add(5*time.Hour)) {
		t.Fatalf("past window: %v %v %v", p, r, s)
	}
	// exactly at the reset counts as past
	if _, r, s = Adjust(98, 300, now, now); !s || !r.After(now) {
		t.Fatalf("reset == now: %v %v", r, s)
	}
	// unknown length: flagged, not rolled
	p, r, s = Adjust(98, 0, past, now)
	if p != 0 || !s || !r.Equal(past) {
		t.Fatalf("no length: %v %v %v", p, r, s)
	}
}

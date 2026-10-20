package fastvm

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestProfBuilderStages(t *testing.T) {
	p := newProf("warm_launch", "60000")
	p.addDetail("nat", "1")
	time.Sleep(5 * time.Millisecond)
	p.stage("alpha")
	time.Sleep(5 * time.Millisecond)
	p.stage("bravo")
	time.Sleep(5 * time.Millisecond)
	got := p.finish("done")

	if got.Event != "warm_launch" || got.VMID != "60000" {
		t.Fatalf("bad envelope: %+v", got)
	}
	if len(got.Stages) != 3 {
		t.Fatalf("want 3 stages, got %d: %+v", len(got.Stages), got.Stages)
	}
	names := []string{"alpha", "bravo", "done"}
	for i, name := range names {
		if got.Stages[i].Name != name {
			t.Fatalf("stage %d = %s, want %s", i, got.Stages[i].Name, name)
		}
	}
	var sum float64
	for _, s := range got.Stages {
		sum += s.MS
	}
	if sum > got.TotalMS+1 {
		t.Fatalf("stage sum %.3f exceeds total %.3f", sum, got.TotalMS)
	}
	if !strings.Contains(got.Summary(), "warm_launch") || !strings.Contains(got.Summary(), "nat=1") {
		t.Fatalf("summary missing fields: %s", got.Summary())
	}
}

func TestRecentProfilesRing(t *testing.T) {
	for i := 0; i < profileRingN+5; i++ {
		p := newProf("t", itoaTest(i))
		p.finish("x")
	}
	got := RecentProfiles()
	if len(got) > profileRingN {
		t.Fatalf("ring leak: %d records", len(got))
	}
	if got[0].VMID != itoaTest(profileRingN+4) {
		t.Fatalf("newest first violated: %s", got[0].VMID)
	}
}

func itoaTest(i int) string {
	return fmtInt(i)
}

func fmtInt(i int) string { return strconv.Itoa(i) }

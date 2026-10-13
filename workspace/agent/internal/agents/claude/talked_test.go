package claude

import (
	"errors"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/status"
	"os"
	"strings"
	"testing"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
)

func launchOf(t *testing.T, m session.Meta) (agents.LaunchPlan, error) {
	t.Helper()
	return agentImpl{}.BuildLaunch(m, agents.LaunchOpts{})
}

// A slot that never talked has no jsonl and must start new, as before.
func TestBuildLaunchNeverTalkedStartsFresh(t *testing.T) {
	isolateSlot(t)
	m := session.Meta{Kind: session.KindClaude, Name: "never-talked", Dir: t.TempDir()}
	plan, err := launchOf(t, m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Program, "--session-id") || strings.Contains(plan.Program, "--resume") {
		t.Fatalf("program = %q, want a fresh --session-id start", plan.Program)
	}
}

// A slot that talked and then lost its jsonl must refuse, not start a blank conversation under
// the old session; StartFresh (the user's explicit choice) lets it start fresh.
func TestBuildLaunchTalkedWithoutTranscriptIsRefused(t *testing.T) {
	isolateSlot(t)
	m := session.Meta{Kind: session.KindClaude, Name: "lost-log", Dir: t.TempDir()}
	sid := session.UUID(m.Dir, m.Name)
	NoteTalked(sid)
	if _, err := launchOf(t, m); !errors.Is(err, ErrTranscriptLost) {
		t.Fatalf("err = %v, want ErrTranscriptLost", err)
	}
	StartFresh(sid)
	plan, err := launchOf(t, m)
	if err != nil {
		t.Fatalf("after StartFresh: %v", err)
	}
	if !strings.Contains(plan.Program, "--session-id") {
		t.Fatalf("program = %q, want a fresh start", plan.Program)
	}
}

// A resumable jsonl still resumes, and marks the slot so a later deletion is noticed even for a
// session that talked before the marker existed.
func TestBuildLaunchResumeMarksSlot(t *testing.T) {
	cfg := isolateSlot(t)
	m := session.Meta{Kind: session.KindClaude, Name: "has-log", Dir: t.TempDir()}
	sid := session.UUID(m.Dir, m.Name)
	writeSlotJSONL(t, cfg, "p", sid)
	plan, err := launchOf(t, m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Program, "--resume") {
		t.Fatalf("program = %q, want --resume", plan.Program)
	}
	if !HadConversation(sid) {
		t.Fatal("resume did not mark the slot as talked")
	}
}

// A slot that finished a turn before the marker existed (status TurnEnd) is still recognised.
func TestBuildLaunchLegacyTurnEndEvidenceIsRefused(t *testing.T) {
	isolateSlot(t)
	m := session.Meta{Kind: session.KindClaude, Name: "legacy-lost", Dir: t.TempDir()}
	sid := session.UUID(m.Dir, m.Name)
	status.PersistTurnEndFor(sid, "idle", "", "real-prompt")
	if _, err := launchOf(t, m); !errors.Is(err, ErrTranscriptLost) {
		t.Fatalf("err = %v, want ErrTranscriptLost", err)
	}
	StartFresh(sid)
	if _, err := launchOf(t, m); err != nil {
		t.Fatalf("after StartFresh: %v", err)
	}
}

// Both the slot log and the drifted live log exist; deleting only the live one must not fall
// back to the older slot log (LiveSID's fall-back) and silently resume stale context.
func TestBuildLaunchDeletedLiveLogDoesNotFallBackToSlot(t *testing.T) {
	cfg := isolateSlot(t)
	m := session.Meta{Kind: session.KindClaude, Name: "drifted", Dir: t.TempDir()}
	sid := session.UUID(m.Dir, m.Name)
	live := "47000000-0000-4000-8000-0000000live2"
	writeSlotJSONL(t, cfg, "p", sid)
	livePath := writeSlotJSONL(t, cfg, "p", live)
	sids.Write(sid, live)
	NoteTalked(sid)
	plan, err := launchOf(t, m)
	if err != nil || !strings.Contains(plan.Program, "--resume '"+live+"'") && !strings.Contains(plan.Program, "--resume "+live) {
		t.Fatalf("positive control: plan %q err %v, want resume of the live log", plan.Program, err)
	}
	if err := os.Remove(livePath); err != nil {
		t.Fatal(err)
	}
	if _, err := launchOf(t, m); !errors.Is(err, ErrTranscriptLost) {
		t.Fatalf("err = %v, want ErrTranscriptLost", err)
	}
}

// Pre-marker evidence plus a drifted ledger whose live log is gone: the older slot log must not
// stand in for it.
func TestBuildLaunchLegacyDriftDoesNotFallBackToSlot(t *testing.T) {
	cfg := isolateSlot(t)
	m := session.Meta{Kind: session.KindClaude, Name: "legacy-drift", Dir: t.TempDir()}
	sid := session.UUID(m.Dir, m.Name)
	writeSlotJSONL(t, cfg, "p", sid)
	sids.Write(sid, "47000000-0000-4000-8000-0000000gone1")
	status.PersistTurnEndFor(sid, "idle", "", "real-prompt")
	if _, err := launchOf(t, m); !errors.Is(err, ErrTranscriptLost) {
		t.Fatalf("err = %v, want ErrTranscriptLost", err)
	}
}

// After the user chooses a new conversation, the stale slot log left behind by the lost live
// one is set aside (kept) and the launch starts new instead of resuming it.
func TestStartFreshSetsAsideStaleSlotLog(t *testing.T) {
	cfg := isolateSlot(t)
	m := session.Meta{Kind: session.KindClaude, Name: "fresh-drift", Dir: t.TempDir()}
	sid := session.UUID(m.Dir, m.Name)
	live := "47000000-0000-4000-8000-0000000live3"
	slotLog := writeSlotJSONL(t, cfg, "p", sid)
	liveLog := writeSlotJSONL(t, cfg, "p", live)
	sids.Write(sid, live)
	NoteTalked(sid)
	if err := os.Remove(liveLog); err != nil {
		t.Fatal(err)
	}
	if _, err := launchOf(t, m); !errors.Is(err, ErrTranscriptLost) {
		t.Fatalf("positive control: err = %v", err)
	}
	StartFresh(sid)
	plan, err := launchOf(t, m)
	if err != nil || strings.Contains(plan.Program, "--resume") || !strings.Contains(plan.Program, "--session-id") {
		t.Fatalf("plan %q err %v, want a fresh --session-id start", plan.Program, err)
	}
	if _, err := os.Stat(slotLog + ".superseded"); err != nil {
		t.Fatalf("old slot log was not kept: %v", err)
	}
}

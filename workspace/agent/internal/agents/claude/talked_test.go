package claude

import (
	"errors"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/status"
	"os"
	"path/filepath"
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
	if err := StartFresh(sid); err != nil {
		t.Fatal(err)
	}
	plan, err := launchOf(t, m)
	if err != nil || strings.Contains(plan.Program, "--resume") || !strings.Contains(plan.Program, "--session-id") {
		t.Fatalf("plan %q err %v, want a fresh --session-id start", plan.Program, err)
	}
	if kept, _ := filepath.Glob(slotLog + ".superseded.*"); len(kept) != 1 {
		t.Fatalf("old slot log was not kept: %v", kept)
	}
}

// lostDrift builds a drifted conversation whose live log is gone and returns the slot log path.
func lostDrift(t *testing.T, cfg string, m session.Meta) (sid, slotLog string) {
	t.Helper()
	sid = session.UUID(m.Dir, m.Name)
	live := "47000000-0000-4000-8000-0000000live4"
	slotLog = writeSlotJSONL(t, cfg, "p", sid)
	liveLog := writeSlotJSONL(t, cfg, "p", live)
	sids.Write(sid, live)
	NoteTalked(sid)
	if err := os.Remove(liveLog); err != nil {
		t.Fatal(err)
	}
	if !lostTranscript(sid) {
		t.Fatal("positive control: not lost")
	}
	return sid, slotLog
}

// A second fresh start on the same slot must not overwrite what the first one kept.
func TestStartFreshNeverClobbersEarlierSetAside(t *testing.T) {
	cfg := isolateSlot(t)
	m := session.Meta{Kind: session.KindClaude, Name: "twice", Dir: t.TempDir()}
	sid, slotLog := lostDrift(t, cfg, m)
	if err := os.WriteFile(slotLog+".superseded", []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := StartFresh(sid); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(slotLog + ".superseded"); string(b) != "first" {
		t.Fatalf("earlier set-aside log was overwritten: %q", b)
	}
	if kept, _ := filepath.Glob(slotLog + ".superseded.*"); len(kept) != 1 {
		t.Fatalf("new set-aside missing: %v", kept)
	}
}

// If the log cannot be moved, nothing is forgotten and the next launch still refuses; once the
// obstacle is gone the same call succeeds.
func TestStartFreshFailureKeepsConversationLost(t *testing.T) {
	cfg := isolateSlot(t)
	m := session.Meta{Kind: session.KindClaude, Name: "stuck", Dir: t.TempDir()}
	sid, slotLog := lostDrift(t, cfg, m)
	dir := filepath.Dir(slotLog)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if err := StartFresh(sid); err == nil {
		t.Skip("directory permissions not enforced here (running as root?)")
	}
	if _, err := launchOf(t, m); !errors.Is(err, ErrTranscriptLost) {
		t.Fatalf("after failed StartFresh: err = %v, want ErrTranscriptLost", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := StartFresh(sid); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if _, err := launchOf(t, m); err != nil {
		t.Fatalf("after retry: %v", err)
	}
}

// The same slot id under two project directories (the cwd changed): both are set aside.
func TestStartFreshSetsAsideEveryProjectCopy(t *testing.T) {
	cfg := isolateSlot(t)
	m := session.Meta{Kind: session.KindClaude, Name: "two-projects", Dir: t.TempDir()}
	sid, _ := lostDrift(t, cfg, m)
	writeSlotJSONL(t, cfg, "old-project", sid)
	if err := StartFresh(sid); err != nil {
		t.Fatal(err)
	}
	if rest, _ := filepath.Glob(filepath.Join(cfg, "projects", "*", sid+".jsonl")); len(rest) != 0 {
		t.Fatalf("copies left: %v", rest)
	}
	plan, err := launchOf(t, m)
	if err != nil || strings.Contains(plan.Program, "--resume") {
		t.Fatalf("plan %q err %v, want a fresh start", plan.Program, err)
	}
}

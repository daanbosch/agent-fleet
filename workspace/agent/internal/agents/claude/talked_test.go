package claude

import (
	"errors"
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
// the old session; ForgetTalked (the user's explicit choice) lets it start fresh.
func TestBuildLaunchTalkedWithoutTranscriptIsRefused(t *testing.T) {
	isolateSlot(t)
	m := session.Meta{Kind: session.KindClaude, Name: "lost-log", Dir: t.TempDir()}
	sid := session.UUID(m.Dir, m.Name)
	NoteTalked(sid)
	if _, err := launchOf(t, m); !errors.Is(err, ErrTranscriptLost) {
		t.Fatalf("err = %v, want ErrTranscriptLost", err)
	}
	ForgetTalked(sid)
	plan, err := launchOf(t, m)
	if err != nil {
		t.Fatalf("after ForgetTalked: %v", err)
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

package claude

import (
	"errors"
	"os"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents"
)

// ErrTranscriptLost is BuildLaunch's refusal to resume a slot that held a conversation whose
// jsonl is gone (claude's own retention sweep, a hand delete). Launching anyway would run
// `claude --session-id <sid>` — a blank conversation under the old session, which reads as the
// old one losing its memory with no word said. The user starts fresh explicitly
// (ForgetTalked, then relaunch) instead.
var ErrTranscriptLost = errors.New("この会話のログ（jsonl）が削除されているため再開できません。新しい会話として開始できます")

// talked remembers that a slot has held a real conversation, so a missing jsonl can be told
// apart from "never talked". Without it both look the same on disk and the safe reading
// (start fresh, as a never-used session does) silently discards a deleted conversation.
// Lives in the agent state dir, so it survives a stop and goes with the session ledger.
var talked = agents.NewSidStore("claude-talked")

// NoteTalked records that sid's conversation has at least one finished turn. Called from the
// Stop hook, so the cost is a Stat per turn once the marker exists.
func NoteTalked(sid string) {
	if sid == "" {
		return
	}
	if _, err := os.Stat(talked.Path(sid)); err == nil {
		return
	}
	talked.Write(sid, "1")
}

// HadConversation reports whether sid was ever recorded as talked.
func HadConversation(sid string) bool {
	_, err := os.Stat(talked.Path(sid))
	return err == nil
}

// ForgetTalked drops the marker: the user chose to start a new conversation in this slot.
func ForgetTalked(sid string) { talked.Remove(sid) }

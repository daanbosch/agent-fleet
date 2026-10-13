package claude

import (
	"errors"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/status"
)

// ErrTranscriptLost is BuildLaunch's refusal to resume a slot that held a conversation whose
// jsonl is gone (claude's own retention sweep, a hand delete). Launching anyway would run
// `claude --session-id <sid>` — a blank conversation under the old session, which reads as the
// old one losing its memory with no word said. The user starts fresh explicitly
// (StartFresh, then relaunch) instead.
var ErrTranscriptLost = errors.New("この会話のログ（jsonl）が削除されているため再開できません。新しい会話として開始できます")

// talked remembers, per slot, the id of the log claude last wrote the slot's conversation
// under. Without it a missing jsonl cannot be told from "never talked" (the safe reading,
// start fresh, then silently discards a deleted conversation), and recording the id rather
// than a flag stops LiveSID's fall-back to the slot id from hiding a deleted live log behind
// an older slot log. Lives in the agent state dir, so it survives a stop.
var talked = agents.NewSidStore("claude-talked")

// NoteTalked records the log id of slot's conversation after a finished turn (Stop hook).
// Rewritten only when the id changes, so the steady-state cost is one small read per turn.
func NoteTalked(slot string) {
	if slot == "" {
		return
	}
	if id := LiveSID(slot); talked.Read(slot) != id {
		talked.Write(slot, id)
	}
}

// talkedID is the log id a slot's conversation should be resumable under. ok=false: the slot
// never talked as far as we can tell. A slot with a finished turn on record (the status
// file's TurnEnd or a completion key — both written long before the marker existed) counts
// as talked under its slot id; a slot with neither is indistinguishable from one that never
// talked and starts new.
func talkedID(slot string) (string, bool) {
	if id := talked.Read(slot); id != "" {
		return id, true
	}
	if st, _ := status.Read(slot); st.TurnEnd {
		return LiveSID(slot), true
	}
	if _, ok := status.ReadCompletionKey(slot); ok {
		return LiveSID(slot), true
	}
	return "", false
}

// HadConversation reports whether slot is known to have held a conversation.
func HadConversation(slot string) bool { _, ok := talkedID(slot); return ok }

// StartFresh is the user's explicit choice to begin a new conversation in this slot: it drops
// every trace that the old one existed, the marker and the pre-marker evidence alike.
func StartFresh(slot string) {
	talked.Remove(slot)
	status.Remove(slot)
	status.RemoveCompletionKey(slot)
	sids.Remove(slot)
}

// lostTranscript reports whether slot's conversation is known to have existed but its log
// (under the id it was last written under) is gone or holds nothing resumable.
func lostTranscript(slot string) bool {
	id, ok := talkedID(slot)
	return ok && !resumableAt(id, session.CWDForUUID(slot))
}

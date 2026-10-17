package main

// Wiring of the Agent memory switch's reconciler (#1972): the hooks memoryx cannot fill itself
// because they need packages it does not import.

import (
	"log"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents/codex"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/memoryx"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/notice"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/sessionx"
)

func init() {
	memoryx.AutoSyncLiveClaudeSessions = liveClaudeSessions
	memoryx.AutoSyncCodexFlag = codex.MemoriesFlag
	memoryx.AutoSyncCodexSet = codex.SetMemoriesFlag
	memoryx.AutoSyncNotify = notifyMemorySync
}

// liveClaudeSessions names the running claude sessions, any execution method.
func liveClaudeSessions() []string {
	var out []string
	for _, m := range session.ListMetas() {
		if sessionx.NormalizeKind(m.Kind) == session.KindClaude && sessionx.SessionAlive(m) {
			out = append(out, m.Name)
		}
	}
	return out
}

// notifyMemorySync raises the one Console notification for a run that left conflicts or skipped
// files. It is about the workspace, not a session; the counts are all it carries.
func notifyMemorySync(run memoryx.AutoSyncRun) {
	ev := notice.New("memory-sync", "", "", "")
	ev.TargetType = "workspace"
	ev.Payload["run"] = run.Kind
	ev.Payload["leftovers"] = len(run.Rows) + run.RowsCut
	conflicts, secrets := 0, 0
	for _, p := range run.Projects {
		conflicts += p.Counts["conflict"]
		secrets += p.Counts["secret"]
	}
	ev.Payload["conflicts"], ev.Payload["secrets"] = conflicts, secrets
	if err := notice.PutOnce("memory-sync:"+run.Kind+":"+run.Started, ev); err != nil {
		log.Printf("memory-sync notification: %v", err)
	}
}

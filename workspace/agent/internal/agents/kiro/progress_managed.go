package kiro

import (
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/procx"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
)

var _ agents.ManagedProgressReporter = managedDriver{}

// ManagedProgress: the ACP child sends nothing while a tool runs (measured on 2.31.0: no
// message for 112 s of a 120 s `sleep`), but the tool is a process under that child, in the
// child's own process group (not a session leader, so ToolProcessIn would miss it), and
// procx.GroupToolIn sees it for as long as it lives. The runtime is `kiro-cli` and its
// `kiro-cli-chat` child, which share the group too.
func (managedDriver) ManagedProgress(m session.Meta) (time.Time, bool) {
	h := handleFor(m.Name)
	if h == nil {
		return time.Time{}, false
	}
	return h.progress(time.Now(), procx.Snapshot)
}

func (h *threadHandle) progress(now time.Time, table func() map[int]procx.Info) (time.Time, bool) {
	return h.act.Progress(now, func() bool {
		h.mu.Lock()
		cmd := h.cmd
		h.mu.Unlock()
		if cmd == nil || cmd.Process == nil {
			return false
		}
		return procx.GroupToolIn(cmd.Process.Pid, table(), "kiro-cli")
	})
}

package codex

import (
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
)

var _ agents.ManagedProgressReporter = managedDriver{}

// ManagedProgress: the last notification that names this session's thread. The app-server is one
// daemon shared by every session, so a tool process cannot be attributed to a session from the
// process table; the signal is the notification stream instead. Measured on 0.163.0 through a
// `sleep 120`: the model polls the running command, so an item started/completed pair arrives
// every 30-35 s. The stream is model-driven, so a turn that waits on a
// command without polling would go quiet; the one-hour lapse is what absorbs that.
func (managedDriver) ManagedProgress(m session.Meta) (time.Time, bool) {
	h := handleFor(m.Name)
	if h == nil {
		return time.Time{}, false
	}
	return h.act.Progress(time.Now(), nil)
}

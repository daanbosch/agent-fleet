package agents

import (
	"sync/atomic"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
)

// ManagedProgressReporter is an optional Driver capability: when a Managed session last showed
// a sign of life, for the one-hour bound on a busy row's Workspace hold (sessionx/progress.go).
// A kind implements it only after the signal was MEASURED to stay fresh through a long silent
// tool (ADR 0055, 2026-10-17 addendum); ok=false means "cannot tell", which the caller holds on.
type ManagedProgressReporter interface {
	ManagedProgress(m session.Meta) (at time.Time, ok bool)
}

// activityFresh is how recent the runtime's last message must be for the tool probe to be
// skipped: against an hour-scale lapse "now" and "a minute ago" are the same answer, and the
// probe reads the process table.
const activityFresh = time.Minute

// Activity stamps when a Managed runtime was last heard from. The zero value is ready.
type Activity struct{ ns atomic.Int64 }

// Touch records a message from the runtime. It is one atomic store, safe on a read loop.
func (a *Activity) Touch() { a.ns.Store(time.Now().UnixNano()) }

// At is the last Touch; ok=false when the runtime was never heard from.
func (a *Activity) At() (time.Time, bool) {
	n := a.ns.Load()
	if n == 0 {
		return time.Time{}, false
	}
	return time.Unix(0, n), true
}

// Progress is the newest sign of life: the last message from the runtime, or now while
// toolAlive says a tool is running (a silent tool sends nothing, measured per kind). toolAlive
// may be nil for a kind whose silent tool is covered by its own messages, and is called only
// when the last message is not fresh.
func (a *Activity) Progress(now time.Time, toolAlive func() bool) (time.Time, bool) {
	at, ok := a.At()
	if ok && now.Sub(at) < activityFresh {
		return at, true
	}
	if toolAlive != nil && toolAlive() {
		return now, true
	}
	return at, ok
}

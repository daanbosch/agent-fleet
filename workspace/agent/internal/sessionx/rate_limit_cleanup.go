package sessionx

// Cancelling a usage-limit booking has to be able to fail visibly (ADR 0115 decision 11.5). The
// CP may be unreachable at the moment the Agent cancels, and an id forgotten then is a resume
// that still fires at the reset instant — for a moved source, into a session that must not wake.
// So an id whose delete the CP has not confirmed is kept here, apart from the episode (which
// closes for display regardless), and retried on every sweep. It is a file, not memory, because
// the reset can be days away and the Agent restarts in between.
//
// An id leaves only when the CP confirms the delete or answers that the schedule is gone. There
// is no expiry: a guess that the schedule must have fired by now is exactly what would leave a
// live one behind.

import (
	"log"
	"sync"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/fstore"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/paths"
)

// rateLimitScheduleCleanup is one booking whose cancellation is not yet confirmed.
type rateLimitScheduleCleanup struct {
	Session string `json:"session"`
	Since   string `json:"since"` // first failed attempt (RFC3339)
	Tries   int    `json:"tries"`
	LastErr string `json:"lastErr,omitempty"`
}

// rateLimitCleanups holds every pending cancellation in one file, keyed by schedule id. One file
// rather than one per id: the id is the CP's, and nothing promises it is a safe file name.
var rateLimitCleanups = fstore.JSON[map[string]rateLimitScheduleCleanup](paths.AgentStateDir, "session-rate-limit-cleanup", ".json")

const rateLimitCleanupKey = "pending"

// rateLimitCleanupMu serializes the read-modify-write of the file, and keeps two sweeps from
// deleting the same id at once.
var rateLimitCleanupMu sync.Mutex

// cancelRateLimitSchedule deletes the booking id of session name, and keeps the id for the
// sweep to retry when the CP does not confirm it.
func cancelRateLimitSchedule(name, id string) {
	if id == "" {
		return
	}
	err := dropRateLimitSchedule(id)
	if err == nil {
		return
	}
	log.Printf("rate-limit: could not cancel the schedule %s of %s, keeping it to retry: %v", id, name, err)
	rateLimitCleanupMu.Lock()
	defer rateLimitCleanupMu.Unlock()
	all, _ := rateLimitCleanups.Read(rateLimitCleanupKey)
	if all == nil {
		all = map[string]rateLimitScheduleCleanup{}
	}
	c, ok := all[id]
	if !ok {
		c = rateLimitScheduleCleanup{Session: name, Since: time.Now().Format(time.RFC3339)}
	}
	c.Tries++
	c.LastErr = err.Error()
	all[id] = c
	if werr := rateLimitCleanups.Write(rateLimitCleanupKey, all); werr != nil {
		log.Printf("rate-limit: could not record the pending cancellation of %s: %v", id, werr)
	}
}

// retryRateLimitScheduleCleanups retries every pending cancellation once.
func retryRateLimitScheduleCleanups() {
	rateLimitCleanupMu.Lock()
	defer rateLimitCleanupMu.Unlock()
	all, ok := rateLimitCleanups.Read(rateLimitCleanupKey)
	if !ok || len(all) == 0 {
		return
	}
	changed := false
	for id, c := range all {
		if err := dropRateLimitSchedule(id); err != nil {
			c.Tries++
			c.LastErr = err.Error()
			all[id] = c
			changed = true
			continue
		}
		log.Printf("rate-limit: cancelled the schedule %s of %s after %d failed attempt(s)", id, c.Session, c.Tries)
		delete(all, id)
		changed = true
	}
	if !changed {
		return
	}
	if len(all) == 0 {
		rateLimitCleanups.Remove(rateLimitCleanupKey)
		return
	}
	if err := rateLimitCleanups.Write(rateLimitCleanupKey, all); err != nil {
		log.Printf("rate-limit: could not update the pending cancellations: %v", err)
	}
}

// pendingRateLimitCleanups lists the ids still waiting for a confirmed cancellation.
func pendingRateLimitCleanups() map[string]rateLimitScheduleCleanup {
	rateLimitCleanupMu.Lock()
	defer rateLimitCleanupMu.Unlock()
	all, _ := rateLimitCleanups.Read(rateLimitCleanupKey)
	return all
}

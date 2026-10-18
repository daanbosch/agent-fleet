package memreview

// The review's durable state (ADR 0117 decisions 1, 3 and 8): one record per claimed run, one
// watermark per session, the daily call counter. All of it lives in one directory on the same
// mount as the memory store, outside the memory repo, so nothing a review wrote is ever part of the
// published history and a "Recreate" of the working copies does not touch it.
//
//	runs/<run id>.json        the claim and everything needed to resume it
//	watermark/<key hash>.json how far a session has been reviewed
//	daily.json                the calls made today
//	pending/, rejected/       the proposal queue (queue.go)
//
// Writes go through fstore, which stages and renames, so a reader (or a restart) sees the old
// record or the new one, never half of one.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/fstore"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/paths"
)

// The bounds of ADR 0117 decision 3, confirmed with the P0 caps.
const (
	// MaxAttempts is one call and at most one retry for a window.
	MaxAttempts = 2
	// DailyCallCap is the helper calls per workspace per local day. A bound against a runaway
	// loop sized from one heavy-use sample (P0), not a budget; the budgets UI is P3's.
	DailyCallCap = 250
	// MaxPending bounds the proposal queue: a review that nobody reads must not grow it forever.
	MaxPending = 100
	// DefaultBackoff is the wait after a transient failure before the same run is tried again.
	DefaultBackoff = 5 * time.Minute

	keepTerminalRuns = 200
)

// Run states. A run is created "claimed" with its window fixed, and ends "done" or "failed".
const (
	StateClaimed    = "claimed"    // window fixed; no call in flight
	StateDispatched = "dispatched" // an attempt was recorded and the call may have been made
	StateAnswered   = "answered"   // a validated reply is saved in Ops; queueing is what is left
	StateDone       = "done"
	StateFailed     = "failed"
)

// Attempt outcomes. Failure texts are a closed vocabulary: nothing the model or a process printed is
// stored in a run record.
const (
	OutcomeDispatched = "dispatched"  // written before the call; it is also what a crash leaves
	OutcomeAnswered   = "answered"    // a reply that passed validation
	OutcomeFailed     = "call-failed" // the call returned an error
	OutcomeMalformed  = "malformed"   // the reply was dropped whole (shape, size, evidence...)
	OutcomeSecret     = "secret"      // the reply echoed something the scan refuses
	OutcomeNotSent    = "not-sent"    // no backend could run deny-all; nothing reached a model
	OutcomeLost       = "lost"        // dispatched, and the process ended before the answer was kept
)

// Attempt is one helper call. It is written before the call, so a crash leaves a trace.
type Attempt struct {
	N         int       `json:"n"`
	StartedAt time.Time `json:"startedAt"`
	Outcome   string    `json:"outcome"`
	// Cost says what is known of a lost attempt's cost: "linked" when a ledger row for it was
	// found (Spend is its tokens), "unknown" when none was (the call may or may not have been
	// billed). Empty for an attempt that ended normally: the ledger has its row.
	Cost  string `json:"cost,omitempty"`
	Spend int    `json:"spend,omitempty"`
}

// Run is the durable record of one review of one window.
type Run struct {
	ID      string `json:"id"`
	Key     string `json:"key"`     // the session's identity (the watermark's key)
	Session string `json:"session"` // its name: store lookup and the ledger's ref
	Trigger string `json:"trigger"` // auto | manual
	// Base is the watermark when the run was claimed; First and Last are the turn numbers the
	// window shows. Last is the window end the run is keyed by and the watermark moves to.
	Base      int       `json:"base"`
	First     int       `json:"first"`
	Last      int       `json:"last"`
	State     string    `json:"state"`
	Attempts  []Attempt `json:"attempts,omitempty"`
	Ops       []Op      `json:"ops,omitempty"`
	ProjectID string    `json:"projectId,omitempty"`
	Failure   string    `json:"failure,omitempty"`
	NotBefore time.Time `json:"notBefore,omitempty"`
	Created   time.Time `json:"created"`
	Updated   time.Time `json:"updated"`
}

// Terminal reports whether nothing more happens to the run.
func (r Run) Terminal() bool { return r.State == StateDone || r.State == StateFailed }

// used counts the attempts that spent budget: everything that reached, or may have reached, a
// model. A call that was never sent (no deny-all backend) is free.
func (r Run) used() int {
	n := 0
	for _, a := range r.Attempts {
		if a.Outcome != OutcomeNotSent {
			n++
		}
	}
	return n
}

// runID is keyed by the session identity and the fixed window end, so two triggers cannot claim
// the same window and a restart finds the same run.
func runID(key string, last int) string {
	h := sha256.Sum256([]byte(key + "\x00" + strconv.Itoa(last)))
	return hex.EncodeToString(h[:])[:20]
}

func keyHash(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])[:24]
}

// Errors the caller handles: all of them leave the run, the watermark and the queue as they were.
var (
	ErrBusy      = errors.New("another review is in flight")
	ErrBackoff   = errors.New("the review is backing off after a failure")
	ErrDailyCap  = errors.New("the daily review call cap is spent")
	ErrQueueFull = errors.New("the proposal queue is full")
	ErrNoBackend = errors.New("no backend that can run the review without tools is available")
	ErrDisabled  = errors.New("the memory review is switched off")
)

// stateRoot is the review's directory. It is resolved on every call: tests move HOME and
// CLAUDE_CONFIG_DIR.
func stateRoot() string { return filepath.Join(paths.ClaudeConfigDir(), "af-memory-review") }

var (
	runStore       = fstore.JSON[Run](stateRoot, "runs", ".json")
	watermarkStore = fstore.JSON[watermark](stateRoot, "watermark", ".json")
	dailyStore     = fstore.JSON[daily](stateRoot, "", ".json")

	// stateMu orders every read-modify-write of the files above inside this process. The Agent is
	// their only writer.
	stateMu sync.Mutex
)

type watermark struct {
	Key  string    `json:"key"`
	Turn int       `json:"turn"` // the last turn number that has been reviewed
	At   time.Time `json:"at"`
}

type daily struct {
	Day   string `json:"day"` // the local date
	Calls int    `json:"calls"`
}

// Watermark is the last reviewed turn number of a session (0: none).
func Watermark(key string) int {
	stateMu.Lock()
	defer stateMu.Unlock()
	w, _ := watermarkStore.Read(keyHash(key))
	return w.Turn
}

// moveWatermark only ever moves it forward.
func moveWatermark(key string, turn int, now time.Time) error {
	stateMu.Lock()
	defer stateMu.Unlock()
	w, _ := watermarkStore.Read(keyHash(key))
	if turn <= w.Turn {
		return nil
	}
	return watermarkStore.Write(keyHash(key), watermark{Key: key, Turn: turn, At: now})
}

// saveRun persists a run record.
func saveRun(r *Run, now time.Time) error {
	stateMu.Lock()
	defer stateMu.Unlock()
	r.Updated = now
	return runStore.Write(r.ID, *r)
}

// loadRun reads one run.
func loadRun(id string) (Run, bool) {
	stateMu.Lock()
	defer stateMu.Unlock()
	return runStore.Read(id)
}

// runs lists every run record, newest first.
func runs() []Run {
	stateMu.Lock()
	defer stateMu.Unlock()
	return listRunsLocked()
}

func listRunsLocked() []Run {
	var out []Run
	for _, id := range listKeys(runStore.Dir(), ".json") {
		if r, ok := runStore.Read(id); ok {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out
}

// unfinishedRun is the session's run that has not ended, if any. There is at most one: a window
// is claimed only when none is open.
func unfinishedRun(key string) (Run, bool) {
	for _, r := range runs() {
		if r.Key == key && !r.Terminal() {
			return r, true
		}
	}
	return Run{}, false
}

// pruneRuns keeps the newest terminal records; an open run is never removed.
func pruneRuns() {
	stateMu.Lock()
	defer stateMu.Unlock()
	kept := 0
	for _, r := range listRunsLocked() {
		if !r.Terminal() {
			continue
		}
		if kept++; kept > keepTerminalRuns {
			runStore.Remove(r.ID)
		}
	}
}

// consumeCall counts one helper call against today's cap and persists the count before the call
// is made: a crash then over-counts by one instead of letting a loop under-count forever.
func consumeCall(now time.Time) error {
	stateMu.Lock()
	defer stateMu.Unlock()
	d, _ := dailyStore.Read("daily")
	day := now.Format("2006-01-02")
	if d.Day != day {
		d = daily{Day: day}
	}
	if d.Calls >= DailyCallCap {
		return ErrDailyCap
	}
	d.Calls++
	return dailyStore.Write("daily", d)
}

// refundCall takes back a call that never reached a model.
func refundCall(now time.Time) {
	stateMu.Lock()
	defer stateMu.Unlock()
	d, _ := dailyStore.Read("daily")
	if d.Day == now.Format("2006-01-02") && d.Calls > 0 {
		d.Calls--
		_ = dailyStore.Write("daily", d)
	}
}

// CallsToday is today's count, for display and tests.
func CallsToday(now time.Time) int {
	stateMu.Lock()
	defer stateMu.Unlock()
	d, _ := dailyStore.Read("daily")
	if d.Day != now.Format("2006-01-02") {
		return 0
	}
	return d.Calls
}

// listKeys lists the keys (file names without ext) of a store directory, skipping the staging area.
func listKeys(dir, ext string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if name, ok := strings.CutSuffix(e.Name(), ext); ok && e.Type().IsRegular() {
			out = append(out, name)
		}
	}
	return out
}

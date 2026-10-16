package sessionx

// The source side of moving a session to another agent kind (ADR 0115 decision 11): the
// lifecycle gate every input path holds, and the state transitions a move makes on its source.
// The gate itself is internal/lifegate; this file is where sessionx meets it.
//
// The transitions are begin (check the state under the exclusive gate, write moving), rollback
// (clear moving, repair a booking the move spent) and commit (moving becomes movedTo, the
// usage-limit watch is settled). Stopping the source and taking its spooled peer messages
// belong to the move itself, which owns the order of those steps.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/httpx"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/lifegate"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/uiprefs"
)

// errCodeSessionMoved refuses input to a session that is moving or was moved. The body names
// the target, so a peer sender learns where to send instead.
const errCodeSessionMoved = "session_moved"

// sessionMovedWire is the session_moved refusal: httpx.WriteErr's shape plus the target.
type sessionMovedWire struct {
	Error sessionMovedErr `json:"error"`
}

type sessionMovedErr struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Target  string `json:"target"`
}

// enterSessionGate takes name's lease for an HTTP input path and answers a refusal itself. A
// lease the request's context carries (an in-process delivery that holds one) is borrowed.
func enterSessionGate(w http.ResponseWriter, r *http.Request, name string) (*lifegate.Lease, bool) {
	l, err := lifegate.Enter(r.Context(), name)
	if err != nil {
		writeGateErr(w, err)
		return nil, false
	}
	return l, true
}

// writeGateErr answers a gate refusal, or a step called without the lease it requires.
func writeGateErr(w http.ResponseWriter, err error) {
	var moved *lifegate.MovedError
	switch {
	case errors.As(err, &moved):
		httpx.WriteJSON(w, http.StatusConflict, sessionMovedWire{Error: sessionMovedErr{
			Code: errCodeSessionMoved, Message: moved.Error(), Target: moved.Target,
		}})
	case errors.Is(err, lifegate.ErrNotHeld):
		httpx.WriteErr(w, http.StatusInternalServerError, "session_gate", err.Error())
	default: // the caller went away while a move held the gate
		httpx.WriteErr(w, http.StatusServiceUnavailable, "session_gate", err.Error())
	}
}

// noteBookingRefusedDuringMove records, when the gate refused a scheduled send, that it was the
// session's own usage-limit booking. That once-schedule is spent, and only a rollback can know
// to book it again (repairRateLimitBookingAfterMove).
func noteBookingRefusedDuringMove(name, source, scheduleID string) {
	if scheduleID == "" || injectionSource(source) != TurnSourceSchedule {
		return
	}
	moveEpisodeMu.Lock()
	defer moveEpisodeMu.Unlock()
	st, ok := RateLimitStates.Read(name)
	if !ok || st.ScheduleID != scheduleID || st.FiredDuringMove == scheduleID {
		return
	}
	st.FiredDuringMove = scheduleID
	_ = RateLimitStates.Write(name, st)
	log.Printf("move: %s: its usage-limit resume %s fired while its input was closed", name, scheduleID)
}

// moveEpisodeMu orders the move's edits of the usage-limit episode. The refusal is recorded by
// an input path that holds no lease, so without it a refusal read before a commit closed the
// episode could write the episode back after it.
var moveEpisodeMu sync.Mutex

// moveSourceState is the source's state and its pending interaction ("" for none), as the move
// judges it. A variable so tests need neither tmux nor a runtime.
var moveSourceState = func(m session.Meta) (state, blocker string) {
	return DriveState(m, SessionAlive(m), false), promptBlocker(m.Name)
}

// movableSourceState lists the states a move may start from. idle, limited and spendLimit are
// ADR 0115's; stopped is added because a source parked at its limit for days is usually stopped
// by the idle reaper by then, and a stopped session can start nothing either. Everything else
// is refused: a working source is mid-turn, and a pending question, plan or permission would be
// silently abandoned.
func movableSourceState(st string) bool {
	switch st {
	case "idle", "stopped", agents.StateLimited, agents.StateSpendLimit:
		return true
	}
	return false
}

// moveStateError refuses a move from a state it may not start in.
type moveStateError struct {
	Session string
	State   string
}

func (e *moveStateError) Error() string {
	return fmt.Sprintf("session %s is %s; a move needs it idle, stopped or at its usage limit", e.Session, e.State)
}

var errMoveSourceGone = errors.New("the session to move does not exist")

// sourceMove is one move of a source, from begin until commit or rollback.
type sourceMove struct {
	source string
	target string
}

// beginSessionMove closes source's input for a move to target (ADR 0115 decision 11.2). It
// waits under ctx for the inputs already admitted to commit, checks the state, and writes
// moving; from then on every gated path refuses with session_moved. It must not be called while
// holding a lease on source.
func beginSessionMove(ctx context.Context, source, target string) (*sourceMove, error) {
	if !session.ValidName(target) || target == source {
		return nil, fmt.Errorf("invalid move target %q", target)
	}
	x, err := lifegate.Lock(ctx, source)
	if err != nil {
		return nil, err
	}
	defer x.Unlock()
	m, ok := session.ReadMeta(source)
	if !ok {
		return nil, errMoveSourceGone
	}
	if m.MovedTo != "" {
		return nil, &lifegate.MovedError{Session: source, Target: m.MovedTo, Done: true}
	}
	if m.Moving != "" {
		return nil, &lifegate.MovedError{Session: source, Target: m.Moving}
	}
	state, blocker := moveSourceState(m)
	if blocker != "" {
		return nil, &moveStateError{Session: source, State: blocker}
	}
	if !movableSourceState(state) {
		return nil, &moveStateError{Session: source, State: state}
	}
	if _, ok := UpdateSessionMeta(source, func(cur *session.Meta) bool {
		if cur.Moving != "" || cur.MovedTo != "" {
			return false
		}
		cur.Moving = target
		return true
	}); !ok {
		return nil, errMoveSourceGone
	}
	x.SetClosed(target, false)
	log.Printf("move: %s: input closed for the move to %s", source, target)
	return &sourceMove{source: source, target: target}, nil
}

// rollback opens the source's input again after a move that created no target (ADR 0115
// decision 11.4). The usage-limit booking is repaired first, while input is still closed, so a
// sweep that runs as soon as it opens finds the episode already settled and books nothing twice.
func (mv *sourceMove) rollback(now time.Time) error {
	repairRateLimitBookingAfterMove(mv.source, now)
	if _, ok := UpdateSessionMeta(mv.source, func(cur *session.Meta) bool {
		if cur.Moving != mv.target {
			return false
		}
		cur.Moving = ""
		return true
	}); !ok {
		if m, exists := session.ReadMeta(mv.source); exists && m.Moving != "" {
			return fmt.Errorf("session %s is moving to %s, not %s", mv.source, m.Moving, mv.target)
		}
	}
	lifegate.Reopen(mv.source)
	log.Printf("move: %s: rolled back the move to %s; input is open again", mv.source, mv.target)
	return nil
}

// commit records that the target exists (ADR 0115 decision 11.5): moving becomes movedTo, which
// keeps the source closed until its user reopens it, and the usage-limit watch is settled.
func (mv *sourceMove) commit() error {
	if _, ok := UpdateSessionMeta(mv.source, func(cur *session.Meta) bool {
		if cur.Moving != mv.target {
			return false
		}
		cur.Moving, cur.MovedTo = "", mv.target
		return true
	}); !ok {
		return fmt.Errorf("session %s is not moving to %s", mv.source, mv.target)
	}
	lifegate.SetClosed(mv.source, mv.target, true)
	lifegate.DropReopen(mv.source)
	settleRateLimitAfterMove(mv.source)
	log.Printf("move: %s: moved to %s", mv.source, mv.target)
	return nil
}

// reopenMovedSession is the user's explicit "resume here anyway" (ADR 0115 decision 11.6): the
// only way a moved source takes input again.
func reopenMovedSession(name string) error {
	if _, ok := UpdateSessionMeta(name, func(cur *session.Meta) bool {
		if cur.MovedTo == "" {
			return false
		}
		cur.MovedTo = ""
		return true
	}); !ok {
		return fmt.Errorf("session %s was not moved", name)
	}
	lifegate.Reopen(name)
	return nil
}

// settleRateLimitAfterMove closes the moved source's usage-limit episode and cancels its
// booking. The episode goes at once, so nothing shows the source as waiting for a reset; a
// cancellation the CP does not confirm is kept and retried by the sweep (rate_limit_cleanup.go).
// A booking that fires before that is refused by the gate.
func settleRateLimitAfterMove(name string) {
	moveEpisodeMu.Lock()
	defer moveEpisodeMu.Unlock()
	st, ok := RateLimitStates.Read(name)
	if !ok {
		return
	}
	RateLimitStates.Remove(name)
	cancelRateLimitSchedule(name, st.ScheduleID)
}

// repairRateLimitBookingAfterMove books the resume again when the source's booking fired during
// the move and the gate refused it. That once-schedule is spent, and the watch would never
// replace it: scheduleRateLimitResume declines while the episode holds an id. The new booking
// is at the original instant, or as soon as the scheduler allows when that has passed.
func repairRateLimitBookingAfterMove(name string, now time.Time) {
	moveEpisodeMu.Lock()
	defer moveEpisodeMu.Unlock()
	st, ok := RateLimitStates.Read(name)
	if !ok || st.FiredDuringMove == "" || st.FiredDuringMove != st.ScheduleID {
		return
	}
	cancelRateLimitSchedule(name, st.ScheduleID) // the spent row, or it stays in the Console's list
	st.ScheduleID, st.FiredDuringMove = "", ""
	m, ok := session.ReadMeta(name)
	if !ok || !uiprefs.RateLimitAutoResume() {
		_ = RateLimitStates.Write(name, st)
		return
	}
	at := now.Add(rateLimitResumeLead)
	if t, err := time.Parse(time.RFC3339, st.ResumeAt); err == nil && t.After(at) {
		at = t
	}
	st.ResumeAt = at.Format(time.RFC3339)
	st.ScheduleTries = 1 // this attempt; a failure leaves the sweep its usual retries
	id, err := putRateLimitSchedule(m, at)
	if err != nil {
		// Left without an id, so the sweep's follow-up books it as it does any failed booking.
		log.Printf("move: %s: could not book the usage-limit resume again: %v", name, err)
	} else {
		st.ScheduleID = id
		log.Printf("move: %s: booked the usage-limit resume again at %s (schedule=%s)", name, st.ResumeAt, id)
	}
	_ = RateLimitStates.Write(name, st)
}

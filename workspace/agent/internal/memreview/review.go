package memreview

// reviewWindow end to end (ADR 0117 decisions 1, 3 and 8): claim one window durably, build the
// redacted input, make the helper call under the budget, validate the reply, queue the proposals,
// move the watermark. No trigger lives here (the loop and "Review now" are phase P1 part C's) and
// nothing is published: the output is the pending queue.
//
// Every step that cannot be repeated blindly is written down BEFORE it happens, so a stop at any
// point leaves a record that says what to do next:
//
//	claimed     the window is fixed          -> resume: make (or retry) the call
//	dispatched  an attempt is recorded       -> the call may have been billed; count it, then retry
//	                                            only within the budget
//	answered    the validated reply is kept  -> queue it; the helper is never asked again
//	done/failed the run has ended            -> nothing; the watermark follows it (and is repaired
//	                                            from it if a stop came in between)
//
// At most one review is in flight in the whole Agent, and at most one run is open per session.

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/chatx"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/memoryx"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/transcript"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/uiprefs"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/usagex"
)

// Triggers, the ledger's own values.
const (
	TriggerAuto   = "auto"
	TriggerManual = "manual"
)

// inflight is the one-review-at-a-time limit across sessions.
var inflight atomic.Bool

// Request is one review of one session.
type Request struct {
	// Key is the session's identity (the watermark's key: stable for the life of the session and
	// never reused); Session is its name (store lookup, ledger ref).
	Key, Session string
	Trigger      string
	// Turns are the session's turns from FirstNumber on, as the transcript reader returns them.
	// Turns at or below the watermark are ignored. After a restart the caller passes them again:
	// an open run rebuilds its fixed window from them.
	Turns       []transcript.Turn
	FirstNumber int
}

// Result is what a Review did.
type Result struct {
	RunID string
	State string // StateDone, StateFailed, or "empty" when there was nothing to review
	// Ops are the validated operations of the reply; Queued of them are new in the queue,
	// Skipped were already pending or rejected.
	Ops, Queued, Skipped int
	Failure              string
	Watermark            int
	// More is true when later windows of the same turns are still unreviewed.
	More bool
}

// Reviewer carries the seams. New wires the real ones; tests replace them.
type Reviewer struct {
	Now      func() time.Time
	Enabled  func() bool
	Snapshot func(session string) (memoryx.ReviewSnapshot, error)
	// Call makes one deny-all helper call under ctx's usage tag.
	Call func(ctx context.Context, persona, prompt string) (string, error)
	// Ledger finds the spend of a lost attempt's usage row, if one was written.
	Ledger  func(session string, since time.Time) (spend int, found bool)
	Backoff time.Duration

	// hook is called when a stage has been persisted; tests use it to stop the process there.
	hook func(stage string)
}

// New is the Reviewer the Agent runs.
func New() *Reviewer {
	return &Reviewer{
		Now:      time.Now,
		Enabled:  uiprefs.MemoryReviewEnabled,
		Snapshot: memoryx.ReviewSnapshotFor,
		Call: func(ctx context.Context, persona, prompt string) (string, error) {
			return chatx.OneShotDenyAll(ctx, usagex.FeatureMemoryReview, chatx.OneShotShort, persona, prompt)
		},
		Ledger:  ledgerSpend,
		Backoff: DefaultBackoff,
	}
}

func (r *Reviewer) stage(s string) {
	if r.hook != nil {
		r.hook(s)
	}
}

func (r *Reviewer) backoff() time.Duration {
	if r.Backoff > 0 {
		return r.Backoff
	}
	return DefaultBackoff
}

// Review reviews the oldest unreviewed window of a session, or resumes the run that is open for
// it. Errors (ErrBusy, ErrBackoff, ErrDailyCap, ErrQueueFull, ErrNoBackend, ErrDisabled, or a
// store error) leave the open run as it was; a run that ends, successfully or not, is a Result.
func (r *Reviewer) Review(ctx context.Context, req Request) (Result, error) {
	if req.Key == "" || req.Session == "" || (req.Trigger != TriggerAuto && req.Trigger != TriggerManual) {
		return Result{}, errors.New("a review needs a session key, a session name and an auto or manual trigger")
	}
	if !r.Enabled() {
		return Result{}, ErrDisabled
	}
	if !inflight.CompareAndSwap(false, true) {
		return Result{}, ErrBusy
	}
	defer inflight.Store(false)

	now := r.Now()
	run, open := unfinishedRun(req.Key)
	more := false
	if !open {
		if err := repairWatermark(req.Key, now); err != nil {
			return Result{}, err
		}
		wm := Watermark(req.Key)
		turns, first := trimTurns(req, wm)
		wins := BuildWindows(turns, first)
		if len(wins) == 0 {
			if last := req.FirstNumber + len(req.Turns) - 1; last > wm {
				if err := moveWatermark(req.Key, last, now); err != nil {
					return Result{}, err
				}
			}
			return Result{State: "empty", Watermark: Watermark(req.Key)}, nil
		}
		w := wins[0]
		more = len(wins) > 1
		if pendingCount() >= MaxPending {
			return Result{}, ErrQueueFull
		}
		run = Run{ID: runID(req.Key, w.Last), Key: req.Key, Session: req.Session, Trigger: req.Trigger,
			Base: wm, First: w.First, Last: w.Last, State: StateClaimed, Created: now}
		if err := saveRun(&run, now); err != nil {
			return Result{}, err
		}
		r.stage("claimed")
		pruneRuns(now)
	}
	res, err := r.advance(ctx, &run, req)
	if err == nil && res.Terminal() {
		res.More = more || Watermark(req.Key) < req.FirstNumber+len(req.Turns)-1
	}
	return res, err
}

// Terminal reports whether the run the result describes has ended.
func (r Result) Terminal() bool { return r.State == StateDone || r.State == StateFailed }

// trimTurns drops the turns at or below the watermark and returns the number of the first kept.
func trimTurns(req Request, wm int) ([]transcript.Turn, int) {
	skip := wm + 1 - req.FirstNumber
	if skip <= 0 {
		return req.Turns, req.FirstNumber
	}
	if skip >= len(req.Turns) {
		return nil, req.FirstNumber + len(req.Turns)
	}
	return req.Turns[skip:], req.FirstNumber + skip
}

// windowOf rebuilds the fixed window of an open run from the caller's turns. It fails when the
// turns no longer contain it (the transcript was truncated or replaced) or build another window.
func windowOf(run Run, req Request) (Window, bool) {
	from, to := run.First-req.FirstNumber, run.Last-req.FirstNumber
	if from < 0 || to >= len(req.Turns) || from > to {
		return Window{}, false
	}
	ws := BuildWindows(req.Turns[from:to+1], run.First)
	if len(ws) == 0 || ws[0].First != run.First || ws[0].Last != run.Last {
		return Window{}, false
	}
	return ws[0], true
}

// advance drives an open run to its next resting point.
func (r *Reviewer) advance(ctx context.Context, run *Run, req Request) (Result, error) {
	now := r.Now()
	if run.State == StateAnswered {
		return r.finish(run) // the helper is never asked again for this window
	}
	if run.State == StateDispatched {
		r.settleLost(run)
		run.State = StateClaimed
		if err := saveRun(run, now); err != nil {
			return Result{}, err
		}
	}
	if run.used() >= MaxAttempts {
		return r.fail(run, "attempt budget spent")
	}
	if !r.Enabled() {
		return Result{}, ErrDisabled
	}
	if now.Before(run.NotBefore) && req.Trigger != TriggerManual {
		return Result{}, ErrBackoff
	}
	if pendingCount() >= MaxPending {
		return Result{}, ErrQueueFull
	}
	win, ok := windowOf(*run, req)
	if !ok {
		return r.fail(run, "window no longer available")
	}
	snap, err := r.Snapshot(run.Session)
	if err != nil {
		return Result{}, errors.New("the session's memory could not be read for the review")
	}
	in := BuildInput(win, snap)

	if err := consumeCall(now); err != nil {
		return Result{}, err
	}
	run.Attempts = append(run.Attempts, Attempt{N: len(run.Attempts) + 1, StartedAt: now, Outcome: OutcomeDispatched})
	run.State = StateDispatched
	if err := saveRun(run, now); err != nil {
		refundCall(now)
		return Result{}, err
	}
	r.stage("dispatched")

	ctx = usagex.WithTag(ctx, usagex.Tag{Feature: usagex.FeatureMemoryReview, Trigger: req.Trigger, Ref: run.Session})
	reply, callErr := r.Call(ctx, in.Persona, in.Prompt)
	at := &run.Attempts[len(run.Attempts)-1]
	switch {
	case callErr != nil && ctx.Err() != nil:
		return Result{}, ctx.Err() // stopped mid-call: the record stays "dispatched" for recovery
	case errors.Is(callErr, chatx.ErrDenyAllUnavailable):
		// Nothing reached a model: no attempt spent, no call counted. The attempt written before
		// the call is taken back and only a counter remains, so an unusable pin cannot grow the
		// record by one entry every backoff.
		run.Attempts = run.Attempts[:len(run.Attempts)-1]
		run.Unsent++
		refundCall(now)
		run.State, run.NotBefore = StateClaimed, now.Add(r.backoff())
		if err := saveRun(run, now); err != nil {
			return Result{}, err
		}
		return Result{}, ErrNoBackend
	case callErr != nil:
		return r.retryOrFail(run, at, OutcomeFailed, "the helper call failed", now)
	}

	ops, perr := ParseReply(reply, in)
	if perr != nil {
		var re *ReplyError
		if errors.As(perr, &re) && re.Kind == ReplySecret {
			at.Outcome, at.EndedAt = OutcomeSecret, now // not retried: the helper echoed something it was shown
			return r.fail(run, "reply refused by the secret scan")
		}
		return r.retryOrFail(run, at, OutcomeMalformed, "the reply was dropped", now)
	}
	at.Outcome, at.EndedAt = OutcomeAnswered, now
	run.Ops, run.ProjectID, run.State = ops, snap.ProjectID, StateAnswered
	if err := saveRun(run, now); err != nil {
		return Result{}, err
	}
	r.stage("answered")
	return r.finish(run)
}

// retryOrFail ends an attempt that produced nothing usable: the run goes back to "claimed" with a
// backoff while budget remains, and is parked as failed (watermark moved) when it does not.
func (r *Reviewer) retryOrFail(run *Run, at *Attempt, outcome, failure string, now time.Time) (Result, error) {
	at.Outcome, at.EndedAt = outcome, now
	if run.used() >= MaxAttempts {
		return r.fail(run, failure)
	}
	run.State, run.NotBefore = StateClaimed, now.Add(r.backoff())
	if err := saveRun(run, now); err != nil {
		return Result{}, err
	}
	return Result{RunID: run.ID, State: StateClaimed, Failure: failure, Watermark: Watermark(run.Key)}, nil
}

// settleLost closes an attempt that was dispatched and never answered in this process: it is
// linked to its usage row when the ledger has one, and marked cost-unknown when it does not.
//
// The ledger stamps a row with the second the call finished, the attempt with the instant it
// started, so the search starts at the start's second. When the attempt before it ended in that
// same second its row (stamped no later than its end) could be mistaken for this one's, so the
// search then starts one second later: a call that finished within a second of starting is not
// a case worth risking the wrong row for.
func (r *Reviewer) settleLost(run *Run) {
	n := len(run.Attempts) - 1
	at := &run.Attempts[n]
	at.Outcome = OutcomeLost
	since := at.StartedAt.Truncate(time.Second)
	if n > 0 && !run.Attempts[n-1].EndedAt.IsZero() && run.Attempts[n-1].EndedAt.Truncate(time.Second).Equal(since) {
		since = since.Add(time.Second)
	}
	if r.Ledger != nil {
		if spend, found := r.Ledger(run.Session, since); found {
			at.Cost, at.Spend = "linked", spend
			return
		}
	}
	at.Cost = "unknown"
}

// finish queues the validated operations and ends the run. It is safe to repeat after a stop at
// any of its steps: proposals have derived ids, one that already got a decision is skipped, and
// the run is marked done only once everything is queued. When the queue has no room it stops with
// ErrQueueFull and the run stays `answered`, so nothing of the reply is lost and the helper is not
// asked again; the rest is queued when room appears.
//
// The run ends before the watermark moves (repairWatermark closes the gap after a stop), and the
// proposal text leaves the run record in the same write that ends it: from then on the queue is
// the only place it lives, and a decision deletes it there.
func (r *Reviewer) finish(run *Run) (Result, error) {
	now := r.Now()
	res := Result{RunID: run.ID, State: StateDone, Ops: len(run.Ops)}
	for i, op := range run.Ops {
		p := Proposal{ID: proposalID(run.ID, i), RunID: run.ID, Session: run.Session, Op: op, Created: run.Created}
		p.Hash = OpHash(scopeProject(run, op), op)
		p.ProjectID = scopeProject(run, op)
		how, err := enqueue(p)
		if err != nil {
			return Result{}, err
		}
		if how == EnqueuedNew || how == EnqueuedExisting {
			res.Queued++
		} else {
			res.Skipped++
		}
	}
	r.stage("enqueued")
	run.State, run.Queued, run.Skipped, run.Ops = StateDone, res.Queued, res.Skipped, nil
	if err := saveRun(run, now); err != nil {
		return Result{}, err
	}
	r.stage("ended")
	if err := moveWatermark(run.Key, run.Last, now); err != nil {
		return Result{}, err
	}
	stateMu.Lock()
	gcDecidedLocked()
	stateMu.Unlock()
	res.Watermark = Watermark(run.Key)
	return res, nil
}

// scopeProject is the project a proposal belongs to: the reviewed session's for a project-scoped
// operation, none for the user scope.
func scopeProject(run *Run, op Op) string {
	if op.Scope == "project" {
		return run.ProjectID
	}
	return ""
}

// fail parks the run. The ending is saved FIRST, in one write with the attempt's outcome, so a stop
// or a failed write after it can never make a later start take the attempt for a lost one and call
// again (a refused reply is not retried); the watermark then moves past the window, and
// repairWatermark does it on the next start if this process stopped before.
func (r *Reviewer) fail(run *Run, reason string) (Result, error) {
	now := r.Now()
	run.State, run.Failure, run.Ops = StateFailed, reason, nil
	if err := saveRun(run, now); err != nil {
		return Result{}, err
	}
	r.stage("ended")
	if err := moveWatermark(run.Key, run.Last, now); err != nil {
		return Result{}, err
	}
	return Result{RunID: run.ID, State: StateFailed, Failure: reason, Watermark: Watermark(run.Key)}, nil
}

// ledgerSpend looks for the usage row of an attempt: the first memory.review row of the session
// stamped at or after since, and every other row of the same call (a call that used several models
// is split over several rows), summed.
func ledgerSpend(session string, since time.Time) (int, bool) {
	var first *usagex.Record
	spend := 0
	for _, day := range []string{since.UTC().Format("2006-01-02"), since.UTC().Add(24 * time.Hour).Format("2006-01-02")} {
		for _, row := range usagex.ReadDay(day) {
			if row.Feature != usagex.FeatureMemoryReview || row.Ref != session {
				continue
			}
			if first == nil {
				ts, err := time.Parse(time.RFC3339, row.TS)
				if err != nil || ts.Before(since) {
					continue
				}
				first = &row
				spend += row.Spend
				continue
			}
			if first.Call != "" && row.Call == first.Call {
				spend += row.Spend
			}
		}
	}
	return spend, first != nil
}

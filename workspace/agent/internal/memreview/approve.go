package memreview

// What the member does with the queue (ADR 0117 decision 6): approve, reject, reject all, and undo
// a whole run. Nothing in this file is reachable except through the member-origin routes of
// http_proposals.go: the review loop never calls it, and no af MCP tool has a route to it, so
// neither the helper model nor a session can publish its own proposal.

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/memoryx"
)

// decideMu serialises the member's decisions. Approving is read queue -> publish -> record the
// decision; two of them on one proposal (a double click, two Consoles) must not both reach the
// store. It is taken before stateMu and never inside it.
var decideMu sync.Mutex

func storeProposal(p Proposal) memoryx.ReviewProposal {
	return memoryx.ReviewProposal{
		ID: p.ID, RunID: p.RunID, Session: p.Session, Op: p.Op.Op, Scope: p.Op.Scope, ProjectID: p.ProjectID,
		Name: p.Op.Name, Type: p.Op.Type, Description: p.Op.Description, Body: p.Op.Body,
		BaseRevision: p.Op.BaseRevision,
	}
}

func pendingByID(id string) (Proposal, bool) {
	stateMu.Lock()
	defer stateMu.Unlock()
	return pendingStore.Read(id)
}

// Approve publishes one pending proposal and removes it from the queue.
//
// The store is asked with the revision the review saw, so a memory edited since is not
// overwritten: the proposal is marked stale and stays for the member to reject. A create over a
// forgotten name is refused the same way. A refusal is a *memoryx.ReviewRefusal.
//
// A stop between the publish and the removal is harmless: the commit carries the proposal id, the
// next Approve finds it, writes nothing and finishes the removal. An error from the removal after a
// successful publish is returned with the published result, so the caller can say both.
func Approve(id string, now time.Time) (memoryx.ReviewPublished, error) {
	decideMu.Lock()
	defer decideMu.Unlock()
	p, ok := pendingByID(id)
	if !ok {
		return memoryx.ReviewPublished{}, ErrNoProposal
	}
	pub, err := memoryx.PublishReview(storeProposal(p), now)
	if err != nil {
		var rf *memoryx.ReviewRefusal
		if errors.As(err, &rf) {
			if merr := markStale(id, rf.Kind); merr != nil {
				return memoryx.ReviewPublished{}, merr
			}
		}
		return memoryx.ReviewPublished{}, err
	}
	return pub, Decide(id, DecisionApprove, now)
}

// Reject drops one pending proposal; only its content hash is kept.
func Reject(id string, now time.Time) error {
	decideMu.Lock()
	defer decideMu.Unlock()
	return Decide(id, DecisionReject, now)
}

// RejectAllResult counts what RejectAll did.
type RejectAllResult struct {
	Rejected int `json:"rejected"`
	// Failed are proposals whose rejection could not be completed (the store could not write the
	// hash or delete the text); they are still pending, so running it again retries them.
	Failed int `json:"failed"`
}

// RejectAll rejects every pending proposal of the run, or of every run when runID is "".
func RejectAll(runID string, now time.Time) RejectAllResult {
	decideMu.Lock()
	defer decideMu.Unlock()
	var out RejectAllResult
	for _, p := range Pending() {
		if runID != "" && p.RunID != runID {
			continue
		}
		if err := Decide(p.ID, DecisionReject, now); err != nil {
			if errors.Is(err, ErrNoProposal) {
				continue
			}
			out.Failed++
			continue
		}
		out.Rejected++
	}
	return out
}

// RevertRun undoes what an approved run published (see memoryx.RevertReviewRun). The run's still
// pending proposals are left alone.
func RevertRun(runID string, now time.Time) (memoryx.ReviewRevertReport, error) {
	decideMu.Lock()
	defer decideMu.Unlock()
	return memoryx.RevertReviewRun(runID, now)
}

// ReleaseRejections lets go of the rejected hashes of a memory that has been forgotten. It is the
// forgotten hook memoryx calls (package main installs it): a rejected idea about a memory is
// remembered only as long as the memory is.
func ReleaseRejections(scope, projectID, name string) {
	_, _ = ForgetMemory(scope, name, projectID)
}

// The states of a listed proposal.
const (
	StatePending = "pending"
	StateStale   = "stale"
)

// Current is the live memory a proposal is about.
type Current struct {
	Revision    int    `json:"revision"`
	Type        string `json:"type,omitempty"`
	Description string `json:"description,omitempty"`
	Body        string `json:"body,omitempty"`
	// Withheld: the live file exists but its text is not shown (it fails the store's scan).
	Withheld bool `json:"withheld,omitempty"`
}

// ProposalView is a queued proposal as the approval screen lists it.
type ProposalView struct {
	ID           string    `json:"id"`
	RunID        string    `json:"runId"`
	Session      string    `json:"session"`
	Op           string    `json:"op"`
	Scope        string    `json:"scope"`
	ProjectID    string    `json:"projectId,omitempty"`
	Project      string    `json:"project,omitempty"` // the project's display name
	Name         string    `json:"name"`
	Type         string    `json:"type,omitempty"`
	Description  string    `json:"description,omitempty"`
	Body         string    `json:"body,omitempty"`
	Reason       string    `json:"reason"`
	Evidence     []int     `json:"evidence"`
	BaseRevision int       `json:"baseRevision,omitempty"`
	Created      time.Time `json:"created"`
	// State is pending, or stale when approving would be refused: StaleReason says why (a
	// memoryx.Review* reason, or "changed" for a memory that is not at the base revision).
	State       string `json:"state"`
	StaleReason string `json:"staleReason,omitempty"`
	// Current is the live memory (update and forget, and a create whose name is taken); nil when
	// there is none or the store could not be read.
	Current *Current `json:"current,omitempty"`
	// Diff is the unified diff from the live text to the proposed one (empty text for the absent
	// side), in the form the change list's diff view reads.
	Diff string `json:"diff"`
}

// ReasonChanged is the stale reason for a memory that is not as the review saw it.
const ReasonChanged = "changed"

// Proposals lists the queue oldest first, each with its live state and diff. The live state is read
// from the store now, so a proposal made stale by an edit since shows as stale before anyone
// presses approve; a reason an earlier approval attempt recorded and the store cannot show (a
// refused secret, an unknown project) is kept.
func Proposals() []ProposalView {
	var out []ProposalView
	for _, p := range Pending() {
		out = append(out, viewOf(p))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

func viewOf(p Proposal) ProposalView {
	v := ProposalView{
		ID: p.ID, RunID: p.RunID, Session: p.Session, Op: p.Op.Op, Scope: p.Op.Scope, ProjectID: p.ProjectID,
		Name: p.Op.Name, Type: p.Op.Type, Description: p.Op.Description, Body: p.Op.Body, Reason: p.Op.Reason,
		Evidence: p.Op.Evidence, BaseRevision: p.Op.BaseRevision, Created: p.Created, State: StatePending,
	}
	if v.Evidence == nil {
		v.Evidence = []int{}
	}
	if p.ProjectID != "" {
		v.Project = memoryx.ReviewProjectDisplay(p.ProjectID)
	}
	t, err := memoryx.ReviewTarget(p.Op.Scope, p.ProjectID, p.Op.Name)
	if err == nil {
		if t.Exists {
			v.Current = &Current{Revision: t.Revision, Type: t.Type, Description: t.Description, Body: t.Body, Withheld: t.Withheld}
		}
		switch {
		case p.Op.Op == OpCreate && t.Forgotten:
			v.State, v.StaleReason = StateStale, memoryx.ReviewForgotten
		case p.Op.Op == OpCreate && t.Exists:
			v.State, v.StaleReason = StateStale, ReasonChanged
		case p.Op.Op != OpCreate && (!t.Exists || t.Withheld || t.Revision != p.Op.BaseRevision):
			v.State, v.StaleReason = StateStale, ReasonChanged
		}
	}
	if v.State == StatePending && (p.Stale == memoryx.ReviewSecret || p.Stale == memoryx.ReviewInvalid) {
		v.State, v.StaleReason = StateStale, p.Stale
	}
	v.Diff = proposalDiff(p, v.Current)
	return v
}

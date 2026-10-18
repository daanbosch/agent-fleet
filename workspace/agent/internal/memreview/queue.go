package memreview

// The pending queue (ADR 0117 decisions 6 and 8): proposals are files in a directory beside the
// store, outside the memory repo, so unapproved text is never in the published history. In P1 the
// queue is only written; deciding (approve, reject) is P2's, but the retention rule is fixed here
// and tested with the store it belongs to:
//
//   - pending text is deleted when a decision is made;
//   - a rejected proposal keeps only its content hash, until the memory it concerned is forgotten;
//   - a proposal whose hash was rejected is never queued again, and an identical pending one is not
//     queued twice.
//
// Every file is a scanned artefact: the proposal's strings pass memoryx.ReviewTextProblem again on
// the way in, so the queue is clean even if a caller skipped ParseReply.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/fstore"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/memoryx"
)

// Proposal is one queued operation with what applying it later needs.
type Proposal struct {
	ID      string `json:"id"`
	RunID   string `json:"runId"`
	Session string `json:"session"`
	// ProjectID is the project of the reviewed session when the operation is project-scoped.
	ProjectID string    `json:"projectId,omitempty"`
	Op        Op        `json:"op"`
	Hash      string    `json:"hash"`
	Created   time.Time `json:"created"`
}

// rejection is what survives a rejected proposal: no text, only the hash and which memory it was
// about, so forgetting that memory can release it.
type rejection struct {
	Hash      string    `json:"hash"`
	Scope     string    `json:"scope"`
	Name      string    `json:"name"`
	ProjectID string    `json:"projectId,omitempty"`
	At        time.Time `json:"at"`
}

var (
	pendingStore  = fstore.JSON[Proposal](stateRoot, "pending", ".json")
	rejectedStore = fstore.JSON[rejection](stateRoot, "rejected", ".json")
)

// OpHash identifies the content of an operation: what it would do to which memory, with which
// text. Reason and evidence are the model's justification and do not change the idea.
func OpHash(projectID string, op Op) string {
	h := sha256.New()
	for _, p := range []string{projectID, op.Op, op.Scope, op.Name, op.Type, op.Description, op.Body} {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

func proposalID(runID string, i int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", runID, i)))
	return hex.EncodeToString(h[:])[:20]
}

// Outcome of Enqueue for one proposal.
const (
	EnqueuedNew      = "queued"
	EnqueuedExisting = "existing" // the same proposal is already there (a resumed run)
	SkippedRejected  = "rejected" // the member already rejected this exact idea
	SkippedDuplicate = "duplicate"
)

// enqueue stores one proposal. It is idempotent: the id is derived from the run and the position,
// so a run resumed after a crash that had queued some of its operations queues the rest and
// nothing twice.
func enqueue(p Proposal) (string, error) {
	for _, s := range []string{p.Session, p.ProjectID, p.Op.Op, p.Op.Scope, p.Op.Name, p.Op.Type, p.Op.Description, p.Op.Body, p.Op.Reason} {
		if memoryx.ReviewTextProblem(s) != "" {
			return "", errors.New("a proposal failed the secret scan and was not queued")
		}
	}
	stateMu.Lock()
	defer stateMu.Unlock()
	if _, ok := pendingStore.Read(p.ID); ok {
		return EnqueuedExisting, nil
	}
	if _, ok := rejectedStore.Read(p.Hash); ok {
		return SkippedRejected, nil
	}
	for _, id := range listKeys(pendingStore.Dir(), ".json") {
		if q, ok := pendingStore.Read(id); ok && q.Hash == p.Hash {
			return SkippedDuplicate, nil
		}
	}
	if err := pendingStore.Write(p.ID, p); err != nil {
		return "", err
	}
	return EnqueuedNew, nil
}

// Pending lists the queue, oldest first.
func Pending() []Proposal {
	stateMu.Lock()
	defer stateMu.Unlock()
	return listPendingLocked()
}

func listPendingLocked() []Proposal {
	var out []Proposal
	for _, id := range listKeys(pendingStore.Dir(), ".json") {
		if p, ok := pendingStore.Read(id); ok {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.Before(out[j].Created)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// pendingCount is the queue length.
func pendingCount() int {
	stateMu.Lock()
	defer stateMu.Unlock()
	return len(listKeys(pendingStore.Dir(), ".json"))
}

// Decision values for Decide.
const (
	DecisionApprove = "approve"
	DecisionReject  = "reject"
)

// Decide removes a proposal from the queue. Its text is gone either way; a rejection leaves the
// hash behind (see the file comment). Approving here does not publish anything: P2 owns that and
// calls this after it has applied the proposal.
func Decide(id, decision string, now time.Time) error {
	if decision != DecisionApprove && decision != DecisionReject {
		return errors.New("decision must be approve or reject")
	}
	stateMu.Lock()
	defer stateMu.Unlock()
	p, ok := pendingStore.Read(id)
	if !ok {
		return errors.New("no such proposal")
	}
	if decision == DecisionReject {
		r := rejection{Hash: p.Hash, Scope: p.Op.Scope, Name: p.Op.Name, ProjectID: p.ProjectID, At: now}
		if err := rejectedStore.Write(p.Hash, r); err != nil {
			return err // the text stays until the hash is safe
		}
	}
	pendingStore.Remove(id)
	return nil
}

// ForgetMemory releases the rejected hashes of a memory that has been forgotten, so a later review
// may propose it again. projectID is "" for the user scope.
func ForgetMemory(scope, name, projectID string) int {
	stateMu.Lock()
	defer stateMu.Unlock()
	n := 0
	for _, h := range listKeys(rejectedStore.Dir(), ".json") {
		if r, ok := rejectedStore.Read(h); ok && r.Scope == scope && r.Name == name && r.ProjectID == projectID {
			rejectedStore.Remove(h)
			n++
		}
	}
	return n
}

// Rejected reports whether the member rejected exactly this idea.
func Rejected(hash string) bool {
	stateMu.Lock()
	defer stateMu.Unlock()
	_, ok := rejectedStore.Read(hash)
	return ok
}

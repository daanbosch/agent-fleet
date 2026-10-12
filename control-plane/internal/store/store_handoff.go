package store

// Store for member-to-member handoff (docs/log/77 / ADR 0057).
//
// A handoff is derived from the share ACL, so revocation is done by
// `invalidateUnauthorizedShareDerivatives` (store_share.go), in the same transaction as the
// share change. All that lives here is creation, reads, state transitions and expiry: this
// feature has no side effect on the Agent — the recipient starts the session against their
// own Workspace — so the owner lease and idempotency ledger an RW share proposal needed are
// absent (ADR 0057 decision 3).

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

const handoffOfferCols = `id,tenant_id,catalog_id,owner_membership_id,recipient_membership_id,title,
	ciphertext,key_ref,repo_remote,branch,head_sha,source_session_name,source_session_kind,
	status,created_at,expires_at,decided_at,accepted_session_name,taken_at`

func scanHandoffOffer(row interface{ Scan(...any) error }) (SessionHandoffOffer, bool, error) {
	var r SessionHandoffOffer
	err := row.Scan(&r.ID, &r.TenantID, &r.CatalogID, &r.OwnerMembershipID, &r.RecipientMembershipID,
		&r.Title, &r.Ciphertext, &r.KeyRef, &r.RepoRemote, &r.Branch, &r.HeadSha,
		&r.SourceSessionName, &r.SourceSessionKind, &r.Status, &r.CreatedAt, &r.ExpiresAt,
		&r.DecidedAt, &r.AcceptedSessionName, &r.TakenAt)
	if err == sql.ErrNoRows {
		return SessionHandoffOffer{}, false, nil
	}
	return r, err == nil, err
}

// CreateSessionHandoffOffer creates one offer. created=false means "this session already
// has an outstanding handoff" (ADR 0057 decision 10).
//
// Deliberately not shaped as count-then-INSERT: two concurrent requests would both see zero
// and both go through. Let the partial unique index refuse it, and translate only that
// violation into created=false.
func (s *SQL) CreateSessionHandoffOffer(ctx context.Context, r SessionHandoffOffer) (bool, error) {
	_, err := s.db.ExecContext(ctx, `INSERT INTO session_handoff_offer
		(id,tenant_id,catalog_id,owner_membership_id,recipient_membership_id,title,ciphertext,key_ref,
		 repo_remote,branch,head_sha,source_session_name,source_session_kind,status,created_at,expires_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.TenantID, r.CatalogID, r.OwnerMembershipID, r.RecipientMembershipID, r.Title,
		r.Ciphertext, r.KeyRef, r.RepoRemote, r.Branch, r.HeadSha, r.SourceSessionName,
		r.SourceSessionKind, r.Status, r.CreatedAt, r.ExpiresAt)
	if err != nil && isUniqueViolation(err) {
		return false, nil
	}
	return err == nil, err
}

// isUniqueViolation decides "hit a unique constraint" in both dialects. Depending on the
// driver's error type would 500 on one series only (the same break as
// [[schema-dialect-parity]]), so both messages are matched.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "unique constraint") || // sqlite (modernc) / some postgres
		strings.Contains(m, "duplicate key value") || // postgres
		strings.Contains(m, "constraint failed: unique") // sqlite's other wording
}

func (s *SQL) GetSessionHandoffOffer(ctx context.Context, id string) (SessionHandoffOffer, bool, error) {
	return scanHandoffOffer(s.db.QueryRowContext(ctx,
		`SELECT `+handoffOfferCols+` FROM session_handoff_offer WHERE id=?`, id))
}

func (s *SQL) listHandoffOffers(ctx context.Context, where string, arg string) ([]SessionHandoffOffer, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+handoffOfferCols+` FROM session_handoff_offer WHERE `+where+` ORDER BY created_at DESC`, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SessionHandoffOffer{}
	for rows.Next() {
		r, _, err := scanHandoffOffer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListSessionHandoffOffersByOwner is the sender's ledger: the history of handoffs they
// offered. Notifications are ephemeral by decision, so this is the only way to trace one
// afterwards — decided offers are returned too (docs/log/77 §77.10).
func (s *SQL) ListSessionHandoffOffersByOwner(ctx context.Context, membershipID string) ([]SessionHandoffOffer, error) {
	return s.listHandoffOffers(ctx, `owner_membership_id=?`, membershipID)
}

// ListSessionHandoffOffersByRecipient is the recipient's inbox. It returns only outstanding
// offers and leaves out those on sessions the owner archived — the same discipline as the
// share list (docs/log/59 §1: a folded conversation is not shown to recipients). Decided
// ones are left out because an accepted offer is evidenced by the new session, and a
// declined one may simply disappear.
func (s *SQL) ListSessionHandoffOffersByRecipient(ctx context.Context, membershipID string) ([]SessionHandoffOffer, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+handoffOfferCols+` FROM session_handoff_offer
		WHERE recipient_membership_id=? AND status='pending'
		  AND EXISTS (SELECT 1 FROM shared_session_catalog c
		              WHERE c.id=session_handoff_offer.catalog_id AND c.archived=0)
		ORDER BY created_at DESC`, membershipID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SessionHandoffOffer{}
	for rows.Next() {
		r, _, err := scanHandoffOffer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// HandoffTakeWindow is how long a recipient's take holds a pending offer against the
// sender's withdraw, a decline and expiry. It covers a launch that has to start a cold
// workspace first, the same allowance as the inbox's launch window; past it, a take whose
// Console closed mid-launch no longer pins the offer.
const HandoffTakeWindow = 10 * time.Minute

// handoffUntakenGuard is what makes a decision on a pending offer wait for an outstanding
// take to settle. It is a variable only so the race test's positive control can drop it and
// show the race is real; nothing else assigns it.
var handoffUntakenGuard = ` AND (take_token='' OR taken_at<=?)`

// handoffTakeStale is the taken_at at or before which a take no longer holds the offer.
func handoffTakeStale() string {
	return time.Now().UTC().Add(-HandoffTakeWindow).Format(time.RFC3339)
}

// TransitionSessionHandoffOffer is the conditional from → to update. changed=false means
// somebody decided it first, or, from pending, that the recipient has taken it and is
// launching (TakeSessionHandoffOffer). Anything but accepted clears the body: there is no
// reason to keep a decided handoff's body in the CP (accepted keeps it so the recipient can
// start the session again).
func (s *SQL) TransitionSessionHandoffOffer(ctx context.Context, id, from, to, decidedAt, acceptedSessionName string) (bool, error) {
	body := `''`
	if to == "accepted" {
		body = `ciphertext`
	}
	q := `UPDATE session_handoff_offer
		SET status=?,decided_at=?,accepted_session_name=?,ciphertext=` + body + `,take_token='',taken_at=''
		WHERE id=? AND status=?`
	args := []any{to, decidedAt, acceptedSessionName, id, from}
	if from == "pending" && handoffUntakenGuard != "" {
		q += handoffUntakenGuard
		args = append(args, handoffTakeStale())
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// TakeSessionHandoffOffer reserves a pending offer for its recipient before the launch
// (issue #2020). taken=false means it is no longer pending, has run past its deadline, or
// is already held by a take that has not gone stale. Together with the guard on
// TransitionSessionHandoffOffer this makes the sender's withdraw and the recipient's take
// exclusive: whichever lands first wins, the other changes nothing (ADR 0057 decision 6).
func (s *SQL) TakeSessionHandoffOffer(ctx context.Context, id, recipientMembershipID, token, now string) (bool, error) {
	if token == "" {
		return false, nil
	}
	res, err := s.db.ExecContext(ctx, `UPDATE session_handoff_offer SET take_token=?,taken_at=?
		WHERE id=? AND recipient_membership_id=? AND status='pending' AND expires_at>?
		  AND (take_token='' OR taken_at<=?)`,
		token, now, id, recipientMembershipID, now, handoffTakeStale())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ReleaseSessionHandoffOffer gives a take back after a launch that failed or was abandoned,
// so the offer is pending and decidable again. Only the take's own token releases it.
func (s *SQL) ReleaseSessionHandoffOffer(ctx context.Context, id, recipientMembershipID, token string) (bool, error) {
	if token == "" {
		return false, nil
	}
	res, err := s.db.ExecContext(ctx, `UPDATE session_handoff_offer SET take_token='',taken_at=''
		WHERE id=? AND recipient_membership_id=? AND status='pending' AND take_token=?`,
		id, recipientMembershipID, token)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// AcceptSessionHandoffOffer settles a take as accepted once the session exists. It needs the
// take's token: a launch that never took the offer, or whose take was overtaken, cannot
// accept it. The body is kept, as for every acceptance.
func (s *SQL) AcceptSessionHandoffOffer(ctx context.Context, id, recipientMembershipID, token, decidedAt, acceptedSessionName string) (bool, error) {
	if token == "" {
		return false, nil
	}
	res, err := s.db.ExecContext(ctx, `UPDATE session_handoff_offer
		SET status='accepted',decided_at=?,accepted_session_name=?,take_token='',taken_at=''
		WHERE id=? AND recipient_membership_id=? AND status='pending' AND take_token=?`,
		decidedAt, acceptedSessionName, id, recipientMembershipID, token)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ExpireSessionHandoffOffers expires the offers past their deadline and returns the rows it
// expired. They come back so the caller can assemble "tell the owner exactly once as it
// lapses" (docs/log/77 §77.9): asking afterwards which offers were affected queries a set
// that is already empty.
func (s *SQL) ExpireSessionHandoffOffers(ctx context.Context, now string) ([]SessionHandoffOffer, error) {
	due, err := s.listHandoffOffers(ctx, `status='pending' AND expires_at<=?`, now)
	if err != nil || len(due) == 0 {
		return nil, err
	}
	out := make([]SessionHandoffOffer, 0, len(due))
	for _, o := range due {
		changed, err := s.TransitionSessionHandoffOffer(ctx, o.ID, "pending", "expired", now, "")
		if err != nil {
			return out, err
		}
		if changed {
			out = append(out, o)
		}
	}
	return out, nil
}

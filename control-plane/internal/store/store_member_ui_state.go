package store

import (
	"context"
	"database/sql"
)

// MemberUIState is a membership's Console-only UI state document (ADR 0114). State is the JSON
// the CP validated; the store does not parse it. Version is the optimistic-concurrency counter
// a write must match.
type MemberUIState struct {
	MembershipID string
	State        string
	Version      int64
	UpdatedAt    string
}

// MemberUIStateStore holds the per-membership UI state. Every call carries membership_id, so a
// member can only ever read or write their own document.
type MemberUIStateStore interface {
	// GetMemberUIState returns ok=false when the member never saved any.
	GetMemberUIState(ctx context.Context, membershipID string) (MemberUIState, bool, error)
	// PutMemberUIState writes row.State as version expect+1 if the stored version is still
	// expect (0 = no row yet). ok=false means another write got there first; the caller reads
	// again and retries.
	PutMemberUIState(ctx context.Context, row MemberUIState, expect int64) (bool, error)
}

func (s *SQL) GetMemberUIState(ctx context.Context, membershipID string) (MemberUIState, bool, error) {
	r := MemberUIState{MembershipID: membershipID}
	err := s.db.QueryRowContext(ctx,
		`SELECT state, version, updated_at FROM member_ui_state WHERE membership_id=?`, membershipID,
	).Scan(&r.State, &r.Version, &r.UpdatedAt)
	if err == sql.ErrNoRows {
		return MemberUIState{MembershipID: membershipID}, false, nil
	}
	return r, err == nil, err
}

func (s *SQL) PutMemberUIState(ctx context.Context, r MemberUIState, expect int64) (bool, error) {
	var res sql.Result
	var err error
	if expect == 0 {
		// DO NOTHING rather than an upsert: a row that appeared since the read is a lost race,
		// and overwriting it would drop the other writer's keys.
		res, err = s.db.ExecContext(ctx,
			`INSERT INTO member_ui_state(membership_id, state, version, updated_at) VALUES(?, ?, 1, ?)
			 ON CONFLICT(membership_id) DO NOTHING`,
			r.MembershipID, r.State, r.UpdatedAt)
	} else {
		res, err = s.db.ExecContext(ctx,
			`UPDATE member_ui_state SET state=?, version=version+1, updated_at=?
			 WHERE membership_id=? AND version=?`,
			r.State, r.UpdatedAt, r.MembershipID, expect)
	}
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

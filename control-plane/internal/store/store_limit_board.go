package store

// Store for the opt-in limit board (ADR 0111 decisions 6 and 7, issue #1962).
//
// Every row is keyed to a tenant membership. Nothing here reduces or projects: what a viewer
// may see is decided in the CP (limit_board.go) before anything leaves, and these reads return
// the member's own data.

import (
	"context"
	"database/sql"
	"strings"
)

// LimitObservation is one quota unit's last real observation. Times are RFC3339 UTC with
// second precision, which is what makes the string comparison in the upsert an ordering.
type LimitObservation struct {
	MembershipID, Kind, Unit, Epoch string
	UsedPct                         float64
	ResetsAt                        string
	WindowMins                      int
	ObservedAt, ReceivedAt, Source  string
}

// LimitSessionStates is the reaper's last read of a member's session limit states. States is
// the JSON the CP wrote; the store does not interpret it.
type LimitSessionStates struct {
	MembershipID, States, ObservedAt string
}

// LimitPublication is what a member publishes. Viewers are membership ids, used only when
// Audience is "members".
type LimitPublication struct {
	MembershipID, Audience, Level, UpdatedAt string
	Viewers                                  []string
}

type LimitBoardStore interface {
	// RecordLimitObservations stores one kind's observations under epoch. A different epoch
	// replaces the stored one only when its epochSince is strictly later, and then deletes the
	// kind's other-epoch rows; otherwise the call is refused (accepted=false) and nothing is
	// written. Per unit, a row is replaced only by a strictly newer observation, so an older or
	// identical one is a no-op whichever source sends it. obs may be empty: an epoch on its own
	// is what clears the previous account's values.
	RecordLimitObservations(ctx context.Context, membershipID, kind, epoch, epochSince, at string, obs []LimitObservation) (accepted bool, err error)
	// ListLimitObservations returns the current-epoch observations of the given memberships.
	ListLimitObservations(ctx context.Context, membershipIDs []string) ([]LimitObservation, error)
	PutLimitSessionStates(ctx context.Context, row LimitSessionStates) error
	ListLimitSessionStates(ctx context.Context, membershipIDs []string) (map[string]LimitSessionStates, error)
	// GetLimitPublication: found=false means hidden, the default.
	GetLimitPublication(ctx context.Context, membershipID string) (LimitPublication, bool, error)
	PutLimitPublication(ctx context.Context, p LimitPublication) error
	ListLimitPublications(ctx context.Context, membershipIDs []string) (map[string]LimitPublication, error)
	// LimitBoardEnabled is the tenant switch. No row means on.
	LimitBoardEnabled(ctx context.Context, tenantID string) (bool, error)
	SetLimitBoardEnabled(ctx context.Context, tenantID string, enabled bool, at string) error
	// ListMembersByTenantPage is the board's roster: active members ordered by user key,
	// starting after the given key, at most limit rows.
	ListMembersByTenantPage(ctx context.Context, tenantID, afterUserKey string, limit int) ([]MemberInfo, error)
}

func (s *SQL) RecordLimitObservations(ctx context.Context, membershipID, kind, epoch, epochSince, at string, obs []LimitObservation) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO limit_epoch(membership_id, kind, epoch, epoch_since, changed_at) VALUES(?,?,?,?,?)
		ON CONFLICT(membership_id, kind) DO NOTHING`, membershipID, kind, epoch, epochSince, at); err != nil {
		return false, err
	}
	// The switch is fenced on epoch_since, not on arrival order: a browser post read before a
	// re-login and delivered after the reaper stored the new account would otherwise delete the
	// new rows and bring the old account back. The UPDATE also takes the row lock, so two racing
	// switches serialise here and the SELECT below sees the winner.
	if _, err := tx.ExecContext(ctx, `UPDATE limit_epoch SET epoch=?, epoch_since=?, changed_at=?
		WHERE membership_id=? AND kind=? AND epoch<>? AND epoch_since < ?`,
		epoch, epochSince, at, membershipID, kind, epoch, epochSince); err != nil {
		return false, err
	}
	// The same account seen again after a login elsewhere raises its own bound, so a late report
	// from the login in between cannot outrank it.
	if _, err := tx.ExecContext(ctx, `UPDATE limit_epoch SET epoch_since=?
		WHERE membership_id=? AND kind=? AND epoch=? AND epoch_since < ?`,
		epochSince, membershipID, kind, epoch, epochSince); err != nil {
		return false, err
	}
	var current string
	if err := tx.QueryRowContext(ctx, `SELECT epoch FROM limit_epoch WHERE membership_id=? AND kind=?`,
		membershipID, kind).Scan(&current); err != nil {
		return false, err
	}
	if current != epoch {
		return false, nil
	}
	// Unconditional rather than only on a change: it is what guarantees no row of another
	// epoch survives, even one written by a transaction that raced this one.
	if _, err := tx.ExecContext(ctx, `DELETE FROM limit_observation WHERE membership_id=? AND kind=? AND epoch<>?`,
		membershipID, kind, epoch); err != nil {
		return false, err
	}
	for _, o := range obs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO limit_observation
			(membership_id, kind, unit, epoch, used_pct, resets_at, window_mins, observed_at, received_at, source)
			VALUES(?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(membership_id, kind, unit) DO UPDATE SET epoch=excluded.epoch, used_pct=excluded.used_pct,
				resets_at=excluded.resets_at, window_mins=excluded.window_mins, observed_at=excluded.observed_at,
				received_at=excluded.received_at, source=excluded.source
			WHERE limit_observation.observed_at < excluded.observed_at`,
			membershipID, kind, o.Unit, epoch, o.UsedPct, o.ResetsAt, o.WindowMins, o.ObservedAt, o.ReceivedAt, o.Source); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// inList renders "?,?,…" for n arguments.
func inList(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

func idArgs(ids []string) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

func (s *SQL) ListLimitObservations(ctx context.Context, membershipIDs []string) ([]LimitObservation, error) {
	if len(membershipIDs) == 0 {
		return nil, nil
	}
	// The join on the current epoch is the read-side half of "an older epoch is never shown".
	rows, err := s.db.QueryContext(ctx, `SELECT o.membership_id, o.kind, o.unit, o.epoch, o.used_pct, o.resets_at,
			o.window_mins, o.observed_at, o.received_at, o.source
		FROM limit_observation o JOIN limit_epoch e ON e.membership_id=o.membership_id AND e.kind=o.kind AND e.epoch=o.epoch
		WHERE o.membership_id IN (`+inList(len(membershipIDs))+`) ORDER BY o.membership_id, o.kind, o.unit`, idArgs(membershipIDs)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LimitObservation
	for rows.Next() {
		var o LimitObservation
		if err := rows.Scan(&o.MembershipID, &o.Kind, &o.Unit, &o.Epoch, &o.UsedPct, &o.ResetsAt, &o.WindowMins,
			&o.ObservedAt, &o.ReceivedAt, &o.Source); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *SQL) PutLimitSessionStates(ctx context.Context, row LimitSessionStates) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO limit_session_state(membership_id, states, observed_at) VALUES(?,?,?)
		ON CONFLICT(membership_id) DO UPDATE SET states=excluded.states, observed_at=excluded.observed_at
		WHERE limit_session_state.observed_at <= excluded.observed_at`, row.MembershipID, row.States, row.ObservedAt)
	return err
}

func (s *SQL) ListLimitSessionStates(ctx context.Context, membershipIDs []string) (map[string]LimitSessionStates, error) {
	out := map[string]LimitSessionStates{}
	if len(membershipIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT membership_id, states, observed_at FROM limit_session_state
		WHERE membership_id IN (`+inList(len(membershipIDs))+`)`, idArgs(membershipIDs)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r LimitSessionStates
		if err := rows.Scan(&r.MembershipID, &r.States, &r.ObservedAt); err != nil {
			return nil, err
		}
		out[r.MembershipID] = r
	}
	return out, rows.Err()
}

func (s *SQL) GetLimitPublication(ctx context.Context, membershipID string) (LimitPublication, bool, error) {
	m, err := s.ListLimitPublications(ctx, []string{membershipID})
	if err != nil {
		return LimitPublication{}, false, err
	}
	p, ok := m[membershipID]
	return p, ok, nil
}

func (s *SQL) PutLimitPublication(ctx context.Context, p LimitPublication) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO limit_publication(membership_id, audience, level, updated_at) VALUES(?,?,?,?)
		ON CONFLICT(membership_id) DO UPDATE SET audience=excluded.audience, level=excluded.level, updated_at=excluded.updated_at`,
		p.MembershipID, p.Audience, p.Level, p.UpdatedAt); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM limit_publication_member WHERE membership_id=?`, p.MembershipID); err != nil {
		return err
	}
	for _, v := range p.Viewers {
		if _, err := tx.ExecContext(ctx, `INSERT INTO limit_publication_member(membership_id, viewer_membership_id) VALUES(?,?)
			ON CONFLICT(membership_id, viewer_membership_id) DO NOTHING`, p.MembershipID, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *SQL) ListLimitPublications(ctx context.Context, membershipIDs []string) (map[string]LimitPublication, error) {
	out := map[string]LimitPublication{}
	if len(membershipIDs) == 0 {
		return out, nil
	}
	args := idArgs(membershipIDs)
	rows, err := s.db.QueryContext(ctx, `SELECT membership_id, audience, level, updated_at FROM limit_publication
		WHERE membership_id IN (`+inList(len(membershipIDs))+`)`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p LimitPublication
		if err := rows.Scan(&p.MembershipID, &p.Audience, &p.Level, &p.UpdatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		out[p.MembershipID] = p
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT membership_id, viewer_membership_id FROM limit_publication_member
		WHERE membership_id IN (`+inList(len(membershipIDs))+`) ORDER BY membership_id, viewer_membership_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, viewer string
		if err := rows.Scan(&id, &viewer); err != nil {
			return nil, err
		}
		if p, ok := out[id]; ok {
			p.Viewers = append(p.Viewers, viewer)
			out[id] = p
		}
	}
	return out, rows.Err()
}

func (s *SQL) LimitBoardEnabled(ctx context.Context, tenantID string) (bool, error) {
	var enabled int
	err := s.db.QueryRowContext(ctx, `SELECT enabled FROM limit_board_tenant WHERE tenant_id=?`, tenantID).Scan(&enabled)
	if err == sql.ErrNoRows {
		return true, nil
	}
	return enabled != 0, err
}

func (s *SQL) SetLimitBoardEnabled(ctx context.Context, tenantID string, enabled bool, at string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO limit_board_tenant(tenant_id, enabled, updated_at) VALUES(?,?,?)
		ON CONFLICT(tenant_id) DO UPDATE SET enabled=excluded.enabled, updated_at=excluded.updated_at`,
		tenantID, b2i(enabled), at)
	return err
}

func (s *SQL) ListMembersByTenantPage(ctx context.Context, tenantID, afterUserKey string, limit int) ([]MemberInfo, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT m.id, i.user_key, i.email, i.role, m.role
		 FROM membership m JOIN identity i ON i.id = m.identity_id
		 WHERE m.tenant_id=? AND m.status='active' AND i.user_key > ?
		 ORDER BY i.user_key LIMIT ?`, tenantID, afterUserKey, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemberInfo
	for rows.Next() {
		var mi MemberInfo
		if err := rows.Scan(&mi.MembershipID, &mi.UserKey, &mi.Email, &mi.IdentityRole, &mi.MemberRole); err != nil {
			return nil, err
		}
		out = append(out, mi)
	}
	return out, rows.Err()
}

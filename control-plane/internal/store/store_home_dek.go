package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Home DEK schemes (migrations/0090_home_dek.sql).
const (
	HomeDEKMigrating = "migrating"
	HomeDEKRandom    = "random"
)

// HomeDEK is the random credential-store key of one member's home, sealed by the key
// custodian. Ciphertext is never logged.
type HomeDEK struct {
	MembershipID, Ciphertext, KeyRef, Scheme, CreatedAt, MigratedAt string
	// ConfirmEpoch is bumped by every RemigrateHomeDEK; a confirm must name the one it read.
	ConfirmEpoch int64
}

// HomeDEKCounts tallies home_dek against the workspaces: Migrating and Random are rows by
// scheme, WithoutKey the workspaces whose home has no row yet (never started since the
// deployment turned the random key on, or it is off).
type HomeDEKCounts struct {
	Migrating, Random, WithoutKey int
}

func (s *SQL) GetHomeDEK(ctx context.Context, membershipID string) (HomeDEK, bool, error) {
	d := HomeDEK{MembershipID: membershipID}
	err := s.db.QueryRowContext(ctx,
		`SELECT ciphertext, key_ref, scheme, created_at, migrated_at, confirm_epoch FROM home_dek WHERE membership_id=?`,
		membershipID).Scan(&d.Ciphertext, &d.KeyRef, &d.Scheme, &d.CreatedAt, &d.MigratedAt, &d.ConfirmEpoch)
	if errors.Is(err, sql.ErrNoRows) {
		return HomeDEK{}, false, nil
	}
	if err != nil {
		return HomeDEK{}, false, err
	}
	return d, true, nil
}

// InsertHomeDEK never replaces a stored key: the home's store may already be sealed under it,
// and overwriting it would make that store unreadable.
func (s *SQL) InsertHomeDEK(ctx context.Context, d HomeDEK) (HomeDEK, error) {
	if d.Scheme == "" {
		d.Scheme = HomeDEKMigrating
	}
	if d.CreatedAt == "" {
		d.CreatedAt = NowTS()
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO home_dek(membership_id, ciphertext, key_ref, scheme, created_at)
		 VALUES(?, ?, ?, ?, ?) ON CONFLICT(membership_id) DO NOTHING`,
		d.MembershipID, d.Ciphertext, d.KeyRef, d.Scheme, d.CreatedAt); err != nil {
		return HomeDEK{}, err
	}
	got, ok, err := s.GetHomeDEK(ctx, d.MembershipID)
	if err != nil {
		return HomeDEK{}, err
	}
	if !ok {
		return HomeDEK{}, errors.New("store: home_dek row vanished right after its insert")
	}
	return got, nil
}

func (s *SQL) CountHomeDEKs(ctx context.Context) (HomeDEKCounts, error) {
	var c HomeDEKCounts
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM home_dek WHERE scheme='migrating'),
		(SELECT COUNT(*) FROM home_dek WHERE scheme='random'),
		(SELECT COUNT(*) FROM workspace w WHERE NOT EXISTS
			(SELECT 1 FROM home_dek h WHERE h.membership_id = w.membership_id))`).
		Scan(&c.Migrating, &c.Random, &c.WithoutKey)
	return c, err
}

// ConfirmHomeDEK is conditioned on the row the start read: the sealed key, the scheme and the
// confirm epoch. A report about a key that is not the stored one, or one taken before a
// remigrate, can never confirm it.
func (s *SQL) ConfirmHomeDEK(ctx context.Context, read HomeDEK) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE home_dek SET scheme='random', migrated_at=?
		 WHERE membership_id=? AND ciphertext=? AND scheme='migrating' AND confirm_epoch=?`,
		NowTS(), read.MembershipID, read.Ciphertext, read.ConfirmEpoch)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *SQL) RemigrateHomeDEK(ctx context.Context, membershipID string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE home_dek SET scheme='migrating', migrated_at='', confirm_epoch=confirm_epoch+1
		 WHERE membership_id=? AND scheme='random'`, membershipID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// DeleteHomeDEK drops the key of a home that is proven gone, on behalf of the holder of the
// membership's lifecycle lease (leaseToken). In one transaction it locks the membership row,
// the lock every lease acquire, renew and release also takes, and (FOR UPDATE on Postgres) the
// one a workspace insert's foreign key needs, checks that the lease is still the holder's and unexpired, and then
// deletes the row, conditioned on the row that was read (sealed key, confirm epoch), on the
// scheme being 'random' and on no workspace row naming the membership. The statement starts
// after the lock, so under Postgres' READ COMMITTED it sees every workspace committed before
// it. A lease that was lost, a key rotated or re-minted since the read, a migrating key and a
// home that has a workspace again all answer false. A migrating key may already seal a store
// (its confirm may only have been lost), so only the operator path may drop one.
func (s *SQL) DeleteHomeDEK(ctx context.Context, read HomeDEK, leaseToken string) (bool, error) {
	if leaseToken == "" {
		return false, nil
	}
	return s.deleteHomeDEKLocked(ctx, read.MembershipID, leaseToken,
		`DELETE FROM home_dek
		 WHERE membership_id=? AND ciphertext=? AND scheme='random' AND confirm_epoch=?
		   AND NOT EXISTS (SELECT 1 FROM workspace w WHERE w.membership_id = home_dek.membership_id)`,
		read.MembershipID, read.Ciphertext, read.ConfirmEpoch)
}

// deleteHomeDEKLocked runs del under the membership lock. leaseToken "" means the caller holds
// no lease, so any live lease on the membership (an operation in progress) refuses it.
func (s *SQL) deleteHomeDEKLocked(ctx context.Context, membershipID, leaseToken, del string, args ...any) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err = lockSessionShareOwner(ctx, tx, membershipID); err != nil {
		return false, err
	}
	if s.dialect == "postgres" {
		// The UPDATE above leaves the key column alone, so Postgres takes only a lock a
		// workspace insert's foreign key does not conflict with. FOR UPDATE does.
		var one int
		if err = tx.QueryRowContext(ctx, `SELECT 1 FROM membership WHERE id=? FOR UPDATE`, membershipID).Scan(&one); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
	}
	// The lease timestamps use this one fixed-width format (workspace_handlers.go leaseTS).
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
	var held, live int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_share_owner_lease
		WHERE owner_membership_id=? AND operation_id=? AND expires_at>?`, membershipID, leaseToken, now).Scan(&held); err != nil {
		return false, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_share_owner_lease
		WHERE owner_membership_id=? AND expires_at>?`, membershipID, now).Scan(&live); err != nil {
		return false, err
	}
	if (leaseToken != "" && held != 1) || (leaseToken == "" && live != 0) {
		return false, nil
	}
	res, err := tx.ExecContext(ctx, del, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, tx.Commit()
}

// OrphanHomeDEK is a home_dek row with no workspace row. That is a candidate, not a proof:
// a home can outlive its workspace row (DeleteWorkspace keeps it). MembershipExists says
// the membership is still there, so the member may get a workspace, and the home, back.
type OrphanHomeDEK struct {
	MembershipID, KeyRef, Scheme, CreatedAt string
	MembershipExists                        bool
}

// ListOrphanHomeDEKs lists the rows no workspace row names, oldest first. The sealed key is
// not read.
func (s *SQL) ListOrphanHomeDEKs(ctx context.Context) ([]OrphanHomeDEK, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT h.membership_id, h.key_ref, h.scheme, h.created_at,
		        EXISTS (SELECT 1 FROM membership m WHERE m.id = h.membership_id)
		 FROM home_dek h
		 WHERE NOT EXISTS (SELECT 1 FROM workspace w WHERE w.membership_id = h.membership_id)
		 ORDER BY h.created_at, h.membership_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrphanHomeDEK
	for rows.Next() {
		var o OrphanHomeDEK
		if err := rows.Scan(&o.MembershipID, &o.KeyRef, &o.Scheme, &o.CreatedAt, &o.MembershipExists); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// DeleteOrphanHomeDEK is the operator's removal: any scheme, still only while no workspace
// row names the membership and no operation holds the membership's lease. The caller has to
// hold the operator's word that the home is gone.
func (s *SQL) DeleteOrphanHomeDEK(ctx context.Context, membershipID string) (bool, error) {
	return s.deleteHomeDEKLocked(ctx, membershipID, "",
		`DELETE FROM home_dek
		 WHERE membership_id=?
		   AND NOT EXISTS (SELECT 1 FROM workspace w WHERE w.membership_id = home_dek.membership_id)`,
		membershipID)
}

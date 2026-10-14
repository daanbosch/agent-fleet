package store

import (
	"context"
	"database/sql"
	"errors"
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

// DeleteHomeDEK drops the key of a home that is proven gone. It is conditioned on the row
// that was read (sealed key, confirm epoch), on the scheme being 'random', and on no
// workspace row naming the membership, all in the one statement: a key that was rotated or
// re-minted since the read, a migrating key, and a home that has a workspace again are all
// left alone, and the answer is false. A migrating key may already seal a store (its confirm
// may only have been lost), so only the operator path may drop one (DeleteOrphanHomeDEK).
func (s *SQL) DeleteHomeDEK(ctx context.Context, read HomeDEK) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM home_dek
		 WHERE membership_id=? AND ciphertext=? AND scheme='random' AND confirm_epoch=?
		   AND NOT EXISTS (SELECT 1 FROM workspace w WHERE w.membership_id = home_dek.membership_id)`,
		read.MembershipID, read.Ciphertext, read.ConfirmEpoch)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
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
// row names the membership. The caller has to hold the operator's word that the home is gone.
func (s *SQL) DeleteOrphanHomeDEK(ctx context.Context, membershipID string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM home_dek
		 WHERE membership_id=?
		   AND NOT EXISTS (SELECT 1 FROM workspace w WHERE w.membership_id = home_dek.membership_id)`,
		membershipID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

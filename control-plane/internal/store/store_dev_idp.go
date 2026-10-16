package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// DevIdPKey is one RS256 signing key of the development OIDC IdP (ADR 0116). PrivateEnc is the
// PKCS#8 DER, base64, sealed by the key custodian under KeyRef; PublicKey is the PKIX DER,
// base64, so publishing the JWKS never unseals anything.
type DevIdPKey struct {
	KID, PrivateEnc, KeyRef, PublicKey string
	State                              string // "active" | "retired"
	CreatedAt, RetiredAt               int64  // unix seconds; RetiredAt is 0 while active
}

const (
	DevIdPKeyActive  = "active"
	DevIdPKeyRetired = "retired"
)

// DevIdPClient is a workspace's registration. SecretEnc is custodian-sealed under KeyRef (the
// workspace's tenant), because the secret is injected into the container again at every start.
type DevIdPClient struct {
	WorkspaceID, MembershipID, ClientID, SecretEnc, KeyRef string
	CreatedAt                                              int64
}

const (
	DevIdPGrantCode   = "code"
	DevIdPGrantAccess = "access"
)

// DevIdPGrant is an authorization code or an access token. Hash is the SHA-256 of the value.
type DevIdPGrant struct {
	Hash, Kind, Parent, ClientID, WorkspaceID, MembershipID, IdentityID string
	RedirectURI, Challenge, Nonce, Scope, Claims                        string
	ExpiresAt, UsedAt                                                   int64
}

// DevIdPGrantConsent is a pending consent: the authorize request the member has been asked to
// decide on. It is single-use and short-lived, and its id is the CSRF token of the form.
const DevIdPGrantConsent = "consent"

// DevIdPPersona is a test identity defined for one workspace (ADR 0116, #2223).
type DevIdPPersona struct {
	ID, WorkspaceID, Handle, Name string
	CreatedAt                     int64
}

// ErrDevIdPPersonaExists: the workspace already has a persona with that handle.
var ErrDevIdPPersonaExists = errors.New("dev idp: persona handle already exists")

// ErrDevIdPWorkspaceGone: the workspace row does not exist (never did, or was deleted first).
var ErrDevIdPWorkspaceGone = errors.New("dev idp: workspace not found")

// ErrDevIdPPersonaLimit: the workspace already holds the maximum number of personas.
var ErrDevIdPPersonaLimit = errors.New("dev idp: persona limit reached")

// DevIdPCodeState is what ConsumeDevIdPCode found.
type DevIdPCodeState int

const (
	// DevIdPCodeOK: the code was live and is now spent.
	DevIdPCodeOK DevIdPCodeState = iota
	// DevIdPCodeUnknown: no such code, expired, or not a code.
	DevIdPCodeUnknown
	// DevIdPCodeReplayed: the code had been spent already.
	DevIdPCodeReplayed
)

// DevIdPStore holds the IdP's keys, registrations and grants.
type DevIdPStore interface {
	ListDevIdPKeys(ctx context.Context) ([]DevIdPKey, error)
	InsertDevIdPKey(ctx context.Context, k DevIdPKey) error
	// RetireDevIdPKeys marks every active key other than keepKID as retired at `at`.
	RetireDevIdPKeys(ctx context.Context, keepKID string, at int64) error
	// DeleteRetiredDevIdPKeys drops the keys retired at or before `before`.
	DeleteRetiredDevIdPKeys(ctx context.Context, before int64) error

	GetDevIdPClient(ctx context.Context, workspaceID string) (DevIdPClient, bool, error)
	GetDevIdPClientByID(ctx context.Context, clientID string) (DevIdPClient, bool, error)
	// CountDevIdPClients is the number of workspaces registered, i.e. opted in.
	CountDevIdPClients(ctx context.Context) (int, error)
	// CreateDevIdPClient inserts the registration and reports false when the workspace
	// already has one (the existing row stands).
	CreateDevIdPClient(ctx context.Context, c DevIdPClient) (bool, error)
	DeleteDevIdPClient(ctx context.Context, workspaceID string) error

	// InsertDevIdPGrant stores a grant and drops every grant that has expired by `now`.
	InsertDevIdPGrant(ctx context.Context, g DevIdPGrant, now int64) error
	// ConsumeDevIdPCode spends a code exactly once, however many callers race for it.
	ConsumeDevIdPCode(ctx context.Context, hash string, now int64) (DevIdPGrant, DevIdPCodeState, error)
	GetDevIdPAccess(ctx context.Context, hash string, now int64) (DevIdPGrant, bool, error)
	// RevokeDevIdPCode records that a spent code was presented again: the code stays (until
	// `until`, so the mark outlives every token it could have produced), is marked revoked, and
	// the tokens already issued from it are deleted.
	RevokeDevIdPCode(ctx context.Context, hash string, until int64) error

	ListDevIdPPersonas(ctx context.Context, workspaceID string) ([]DevIdPPersona, error)
	// GetDevIdPPersona is scoped to the workspace: another workspace's id is "not found".
	GetDevIdPPersona(ctx context.Context, workspaceID, id string) (DevIdPPersona, bool, error)
	// CreateDevIdPPersona returns ErrDevIdPPersonaLimit when the workspace already holds max
	// personas (counted in the same transaction as the insert, so concurrent creates cannot
	// overshoot) and ErrDevIdPPersonaExists on a duplicate handle.
	CreateDevIdPPersona(ctx context.Context, p DevIdPPersona, max int) error
	DeleteDevIdPPersona(ctx context.Context, workspaceID, id string) (bool, error)
	// DevIdPEmailTaken reports whether an address belongs to a real person (an identity or a
	// linked sign-in), compared case-insensitively.
	DevIdPEmailTaken(ctx context.Context, email string) (bool, error)
	// ConsumeDevIdPConsent spends a pending consent exactly once.
	ConsumeDevIdPConsent(ctx context.Context, hash string, now int64) (DevIdPGrant, bool, error)
}

func (s *SQL) ListDevIdPKeys(ctx context.Context) ([]DevIdPKey, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT kid, private_enc, key_ref, public_key, state, created_at, retired_at
		 FROM dev_idp_key ORDER BY created_at DESC, kid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DevIdPKey
	for rows.Next() {
		var k DevIdPKey
		if err := rows.Scan(&k.KID, &k.PrivateEnc, &k.KeyRef, &k.PublicKey, &k.State, &k.CreatedAt, &k.RetiredAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *SQL) InsertDevIdPKey(ctx context.Context, k DevIdPKey) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO dev_idp_key(kid, private_enc, key_ref, public_key, state, created_at, retired_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?)`,
		k.KID, k.PrivateEnc, k.KeyRef, k.PublicKey, k.State, k.CreatedAt, k.RetiredAt)
	return err
}

func (s *SQL) RetireDevIdPKeys(ctx context.Context, keepKID string, at int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE dev_idp_key SET state=?, retired_at=? WHERE state=? AND kid<>?`,
		DevIdPKeyRetired, at, DevIdPKeyActive, keepKID)
	return err
}

func (s *SQL) DeleteRetiredDevIdPKeys(ctx context.Context, before int64) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM dev_idp_key WHERE state=? AND retired_at<=?`, DevIdPKeyRetired, before)
	return err
}

func (s *SQL) GetDevIdPClient(ctx context.Context, workspaceID string) (DevIdPClient, bool, error) {
	return s.devIdPClient(ctx, `workspace_id=?`, workspaceID)
}

func (s *SQL) GetDevIdPClientByID(ctx context.Context, clientID string) (DevIdPClient, bool, error) {
	return s.devIdPClient(ctx, `client_id=?`, clientID)
}

func (s *SQL) devIdPClient(ctx context.Context, where, arg string) (DevIdPClient, bool, error) {
	var c DevIdPClient
	err := s.db.QueryRowContext(ctx,
		`SELECT workspace_id, membership_id, client_id, secret_enc, key_ref, created_at
		 FROM dev_idp_client WHERE `+where, arg).
		Scan(&c.WorkspaceID, &c.MembershipID, &c.ClientID, &c.SecretEnc, &c.KeyRef, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return DevIdPClient{}, false, nil
	}
	return c, err == nil, err
}

func (s *SQL) CountDevIdPClients(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dev_idp_client`).Scan(&n)
	return n, err
}

func (s *SQL) CreateDevIdPClient(ctx context.Context, c DevIdPClient) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO dev_idp_client(workspace_id, membership_id, client_id, secret_enc, key_ref, created_at)
		 VALUES(?, ?, ?, ?, ?, ?) ON CONFLICT(workspace_id) DO NOTHING`,
		c.WorkspaceID, c.MembershipID, c.ClientID, c.SecretEnc, c.KeyRef, c.CreatedAt)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *SQL) DeleteDevIdPClient(ctx context.Context, workspaceID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM dev_idp_client WHERE workspace_id=?`, workspaceID)
	return err
}

func (s *SQL) InsertDevIdPGrant(ctx context.Context, g DevIdPGrant, now int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM dev_idp_grant WHERE expires_at<=?`, now); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO dev_idp_grant(hash, kind, parent, client_id, workspace_id, membership_id,
		   identity_id, redirect_uri, challenge, nonce, scope, claims, expires_at, used_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`,
		g.Hash, g.Kind, g.Parent, g.ClientID, g.WorkspaceID, g.MembershipID,
		g.IdentityID, g.RedirectURI, g.Challenge, g.Nonce, g.Scope, g.Claims, g.ExpiresAt); err != nil {
		return err
	}
	if g.Parent == "" {
		return nil
	}
	// Insert first, look second: a replay that revoked the parent before this point is seen
	// here, and one that revokes after it deletes this row. Neither order leaves a live token.
	var revoked int
	if err := s.db.QueryRowContext(ctx,
		`SELECT revoked FROM dev_idp_grant WHERE hash=?`, g.Parent).Scan(&revoked); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if revoked != 0 {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM dev_idp_grant WHERE hash=?`, g.Hash)
		return ErrDevIdPCodeRevoked
	}
	return nil
}

// ErrDevIdPCodeRevoked is returned when a token is issued from a code that was replayed.
var ErrDevIdPCodeRevoked = errors.New("dev idp: the code was replayed")

const devIdPGrantCols = `hash, kind, parent, client_id, workspace_id, membership_id, identity_id,
	redirect_uri, challenge, nonce, scope, claims, expires_at, used_at`

func scanDevIdPGrant(row interface{ Scan(...any) error }) (DevIdPGrant, error) {
	var g DevIdPGrant
	err := row.Scan(&g.Hash, &g.Kind, &g.Parent, &g.ClientID, &g.WorkspaceID, &g.MembershipID,
		&g.IdentityID, &g.RedirectURI, &g.Challenge, &g.Nonce, &g.Scope, &g.Claims, &g.ExpiresAt, &g.UsedAt)
	return g, err
}

// ConsumeDevIdPCode flips used_at in one conditional UPDATE, so of any number of callers
// presenting the same code exactly one sees RowsAffected = 1.
func (s *SQL) ConsumeDevIdPCode(ctx context.Context, hash string, now int64) (DevIdPGrant, DevIdPCodeState, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE dev_idp_grant SET used_at=? WHERE hash=? AND kind=? AND used_at=0 AND expires_at>?`,
		now, hash, DevIdPGrantCode, now)
	if err != nil {
		return DevIdPGrant{}, DevIdPCodeUnknown, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return DevIdPGrant{}, DevIdPCodeUnknown, err
	}
	g, err := scanDevIdPGrant(s.db.QueryRowContext(ctx,
		`SELECT `+devIdPGrantCols+` FROM dev_idp_grant WHERE hash=? AND kind=? AND expires_at>?`,
		hash, DevIdPGrantCode, now))
	if errors.Is(err, sql.ErrNoRows) {
		return DevIdPGrant{}, DevIdPCodeUnknown, nil
	}
	if err != nil {
		return DevIdPGrant{}, DevIdPCodeUnknown, err
	}
	if n == 1 {
		return g, DevIdPCodeOK, nil
	}
	return g, DevIdPCodeReplayed, nil
}

func (s *SQL) GetDevIdPAccess(ctx context.Context, hash string, now int64) (DevIdPGrant, bool, error) {
	g, err := scanDevIdPGrant(s.db.QueryRowContext(ctx,
		`SELECT `+devIdPGrantCols+` FROM dev_idp_grant WHERE hash=? AND kind=? AND expires_at>?
		 AND NOT EXISTS (SELECT 1 FROM dev_idp_grant p WHERE p.hash=dev_idp_grant.parent AND p.revoked<>0)`,
		hash, DevIdPGrantAccess, now))
	if errors.Is(err, sql.ErrNoRows) {
		return DevIdPGrant{}, false, nil
	}
	return g, err == nil, err
}

func (s *SQL) RevokeDevIdPCode(ctx context.Context, hash string, until int64) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE dev_idp_grant SET revoked=1, expires_at=CASE WHEN expires_at>? THEN expires_at ELSE ? END
		 WHERE hash=? AND kind=?`, until, until, hash, DevIdPGrantCode); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM dev_idp_grant WHERE parent=?`, hash)
	return err
}

func (s *SQL) ListDevIdPPersonas(ctx context.Context, workspaceID string) ([]DevIdPPersona, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, workspace_id, handle, name, created_at FROM dev_idp_persona
		 WHERE workspace_id=? ORDER BY handle`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DevIdPPersona
	for rows.Next() {
		var p DevIdPPersona
		if err := rows.Scan(&p.ID, &p.WorkspaceID, &p.Handle, &p.Name, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *SQL) GetDevIdPPersona(ctx context.Context, workspaceID, id string) (DevIdPPersona, bool, error) {
	var p DevIdPPersona
	err := s.db.QueryRowContext(ctx,
		`SELECT id, workspace_id, handle, name, created_at FROM dev_idp_persona
		 WHERE workspace_id=? AND id=?`, workspaceID, id).
		Scan(&p.ID, &p.WorkspaceID, &p.Handle, &p.Name, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return DevIdPPersona{}, false, nil
	}
	return p, err == nil, err
}

func (s *SQL) CreateDevIdPPersona(ctx context.Context, p DevIdPPersona, max int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Serialises creates per workspace: a count taken without it is stale by the time the
	// insert runs (Postgres READ COMMITTED does not block a concurrent insert of a new row).
	lock, err := tx.ExecContext(ctx, `UPDATE workspace SET id=id WHERE id=?`, p.WorkspaceID)
	if err != nil {
		return err
	}
	// No row means no lock was taken: refuse rather than count unserialised (and rather than
	// recreate a persona for a workspace that was just deleted).
	if c, err := lock.RowsAffected(); err != nil {
		return err
	} else if c == 0 {
		return ErrDevIdPWorkspaceGone
	}
	var n int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM dev_idp_persona WHERE workspace_id=?`, p.WorkspaceID).Scan(&n); err != nil {
		return err
	}
	if n >= max {
		return ErrDevIdPPersonaLimit
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO dev_idp_persona(id, workspace_id, handle, name, created_at)
		 VALUES(?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		p.ID, p.WorkspaceID, p.Handle, p.Name, p.CreatedAt)
	if err != nil {
		return err
	}
	if c, err := res.RowsAffected(); err != nil || c == 0 {
		if err != nil {
			return err
		}
		return ErrDevIdPPersonaExists
	}
	return tx.Commit()
}

func (s *SQL) DeleteDevIdPPersona(ctx context.Context, workspaceID, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM dev_idp_persona WHERE workspace_id=? AND id=?`, workspaceID, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *SQL) DevIdPEmailTaken(ctx context.Context, email string) (bool, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return false, nil
	}
	var one int
	for _, q := range []string{
		`SELECT 1 FROM identity WHERE LOWER(email)=? LIMIT 1`,
		`SELECT 1 FROM identity_provider WHERE LOWER(email)=? LIMIT 1`,
	} {
		err := s.db.QueryRowContext(ctx, q, email).Scan(&one)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
	}
	return false, nil
}

func (s *SQL) ConsumeDevIdPConsent(ctx context.Context, hash string, now int64) (DevIdPGrant, bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE dev_idp_grant SET used_at=? WHERE hash=? AND kind=? AND used_at=0 AND expires_at>?`,
		now, hash, DevIdPGrantConsent, now)
	if err != nil {
		return DevIdPGrant{}, false, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return DevIdPGrant{}, false, err
	}
	g, err := scanDevIdPGrant(s.db.QueryRowContext(ctx,
		`SELECT `+devIdPGrantCols+` FROM dev_idp_grant WHERE hash=?`, hash))
	return g, err == nil, err
}

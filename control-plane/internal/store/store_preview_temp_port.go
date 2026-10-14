package store

import (
	"context"
	"database/sql"
	"errors"
)

// PreviewTempPort is one temporary preview grant (see migrations/0098): the owner's open of an
// unlisted port, bound to the listener sockets seen at that moment.
type PreviewTempPort struct {
	WorkspaceID string
	Port        int
	Slug        string
	Inodes      string // comma-separated socket inodes
	CreatedAt   string
}

// PreviewTempPortStore holds the grants. Every call carries the workspace id, so one member's
// grants are never reachable through another's workspace.
type PreviewTempPortStore interface {
	// UpsertPreviewTempPort creates the grant, or replaces the one for the same port.
	UpsertPreviewTempPort(ctx context.Context, g PreviewTempPort) error
	GetPreviewTempPort(ctx context.Context, workspaceID string, port int) (PreviewTempPort, bool, error)
	ListPreviewTempPorts(ctx context.Context, workspaceID string) ([]PreviewTempPort, error)
	// SavePreviewTempPort records a grant under the workspace lock, deciding in one transaction
	// whether the grant is still for the current running slug, whether the cap allows it, and
	// whether it is new (audit row written then, in the same transaction).
	SavePreviewTempPort(ctx context.Context, g PreviewTempPort, maxGrants int, audit AuditLog) (PreviewTempSave, error)
	// DeletePreviewTempPortIf deletes the grant only while it still holds these inodes, so a
	// slow "the listener is gone" cannot remove a grant the owner has just renewed.
	DeletePreviewTempPortIf(ctx context.Context, workspaceID string, port int, inodes string) error
	// ClearPreviewTempPorts removes every grant of the workspace (slug rotation, stop).
	ClearPreviewTempPorts(ctx context.Context, workspaceID string) error
}

func (s *SQL) UpsertPreviewTempPort(ctx context.Context, g PreviewTempPort) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO preview_temp_port(workspace_id, port, slug, inodes, created_at) VALUES(?, ?, ?, ?, ?)
		 ON CONFLICT(workspace_id, port) DO UPDATE SET
		   slug=excluded.slug, inodes=excluded.inodes, created_at=excluded.created_at`,
		g.WorkspaceID, g.Port, g.Slug, g.Inodes, g.CreatedAt)
	return err
}

func (s *SQL) GetPreviewTempPort(ctx context.Context, workspaceID string, port int) (PreviewTempPort, bool, error) {
	g := PreviewTempPort{WorkspaceID: workspaceID, Port: port}
	err := s.db.QueryRowContext(ctx,
		`SELECT slug, inodes, created_at FROM preview_temp_port WHERE workspace_id=? AND port=?`,
		workspaceID, port).Scan(&g.Slug, &g.Inodes, &g.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PreviewTempPort{}, false, nil
	}
	return g, err == nil, err
}

func (s *SQL) ListPreviewTempPorts(ctx context.Context, workspaceID string) ([]PreviewTempPort, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT port, slug, inodes, created_at FROM preview_temp_port WHERE workspace_id=? ORDER BY port`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PreviewTempPort
	for rows.Next() {
		g := PreviewTempPort{WorkspaceID: workspaceID}
		if err := rows.Scan(&g.Port, &g.Slug, &g.Inodes, &g.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *SQL) DeletePreviewTempPortIf(ctx context.Context, workspaceID string, port int, inodes string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM preview_temp_port WHERE workspace_id=? AND port=? AND inodes=?`, workspaceID, port, inodes)
	return err
}

func (s *SQL) ClearPreviewTempPorts(ctx context.Context, workspaceID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM preview_temp_port WHERE workspace_id=?`, workspaceID)
	return err
}

// PreviewTempSave is the outcome of SavePreviewTempPort.
type PreviewTempSave int

const (
	// PreviewTempCreated: a new grant (or the same port bound to a different listener); audited.
	PreviewTempCreated PreviewTempSave = iota
	// PreviewTempRenewed: the same port and listener again; nothing new was exposed.
	PreviewTempRenewed
	// PreviewTempCap: the workspace already holds maxGrants other grants.
	PreviewTempCap
	// PreviewTempStale: the workspace is not running under g.Slug any more (rotated or stopped).
	PreviewTempStale
)

func (s *SQL) SavePreviewTempPort(ctx context.Context, g PreviewTempPort, maxGrants int, audit AuditLog) (PreviewTempSave, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err = lockWorkspace(ctx, tx, g.WorkspaceID); err != nil {
		return 0, err
	}
	var state, slug string
	if err = tx.QueryRowContext(ctx, `SELECT state, preview_slug FROM workspace WHERE id=?`, g.WorkspaceID).Scan(&state, &slug); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PreviewTempStale, nil
		}
		return 0, err
	}
	if state != "running" || slug == "" || slug != g.Slug {
		return PreviewTempStale, nil
	}
	var prevInodes string
	existed := true
	if err = tx.QueryRowContext(ctx, `SELECT inodes FROM preview_temp_port WHERE workspace_id=? AND port=?`, g.WorkspaceID, g.Port).Scan(&prevInodes); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		existed = false
	}
	if !existed {
		var n int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM preview_temp_port WHERE workspace_id=? AND slug=?`, g.WorkspaceID, slug).Scan(&n); err != nil {
			return 0, err
		}
		if n >= maxGrants {
			return PreviewTempCap, nil
		}
	}
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO preview_temp_port(workspace_id, port, slug, inodes, created_at) VALUES(?, ?, ?, ?, ?)
		 ON CONFLICT(workspace_id, port) DO UPDATE SET
		   slug=excluded.slug, inodes=excluded.inodes, created_at=excluded.created_at`,
		g.WorkspaceID, g.Port, g.Slug, g.Inodes, g.CreatedAt); err != nil {
		return 0, err
	}
	out := PreviewTempRenewed
	if !existed || prevInodes != g.Inodes {
		out = PreviewTempCreated
		if _, err = tx.ExecContext(ctx,
			`INSERT INTO audit_log(id, tenant_id, actor_kind, actor_id, action, target, detail, at, http_status)
			 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			audit.ID, audit.TenantID, audit.ActorKind, audit.ActorID, audit.Action, audit.Target, audit.Detail, audit.At, audit.HTTPStatus); err != nil {
			return 0, err
		}
	}
	return out, tx.Commit()
}

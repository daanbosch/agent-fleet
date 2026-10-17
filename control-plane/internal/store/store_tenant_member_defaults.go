package store

import (
	"context"
	"database/sql"
)

// TenantMemberDefaults is a tenant's defaults for its members' Console (ADR 0114 decision 8).
// Defaults is the JSON the admin API validated; the store does not parse it.
type TenantMemberDefaults struct {
	TenantID  string
	Defaults  string
	UpdatedBy string
	UpdatedAt string
}

// TenantMemberDefaultsStore holds them. Every call carries tenant_id, so one tenant's admin can
// never read or write another's.
type TenantMemberDefaultsStore interface {
	// GetTenantMemberDefaults returns ok=false when the tenant never saved any.
	GetTenantMemberDefaults(ctx context.Context, tenantID string) (TenantMemberDefaults, bool, error)
	// PutTenantMemberDefaults replaces the tenant's whole document.
	PutTenantMemberDefaults(ctx context.Context, row TenantMemberDefaults) error
}

func (s *SQL) GetTenantMemberDefaults(ctx context.Context, tenantID string) (TenantMemberDefaults, bool, error) {
	r := TenantMemberDefaults{TenantID: tenantID}
	err := s.db.QueryRowContext(ctx,
		`SELECT defaults, updated_by, updated_at FROM tenant_member_defaults WHERE tenant_id=?`, tenantID,
	).Scan(&r.Defaults, &r.UpdatedBy, &r.UpdatedAt)
	if err == sql.ErrNoRows {
		return TenantMemberDefaults{TenantID: tenantID}, false, nil
	}
	return r, err == nil, err
}

func (s *SQL) PutTenantMemberDefaults(ctx context.Context, r TenantMemberDefaults) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tenant_member_defaults(tenant_id, defaults, updated_by, updated_at) VALUES(?, ?, ?, ?)
		 ON CONFLICT(tenant_id) DO UPDATE SET
		   defaults=excluded.defaults, updated_by=excluded.updated_by, updated_at=excluded.updated_at`,
		r.TenantID, r.Defaults, r.UpdatedBy, r.UpdatedAt)
	return err
}

package main

// The deployment switch of the development OIDC IdP (ADR 0116, decision 3 as amended by the
// 2026-10-16 note): on unless a super admin turned it off in the Admin modal or the operator
// locked it with AF_DEV_IDP=0. It lives in deployment_setting beside the egress mode, so a
// toggle reaches every handler without a restart.
//
// Turning it off only stops the IdP being served. Workspace opt-ins and their client
// registrations stay, so turning it back on does not rotate anyone's secret.

import (
	"context"
	"errors"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/k-k1/agent-fleet/control-plane/internal/store"
)

const (
	// devIdPSettingKey holds "on" or "off"; no row means on.
	devIdPSettingKey = "dev_idp"
	// devIdPSwitchTTL bounds how long another CP process keeps serving (or refusing) after a
	// toggle. This process invalidates on PUT, so only replicas wait.
	devIdPSwitchTTL = 3 * time.Second
)

// devIdPSwitch caches the stored switch. gen stops a read that began before an invalidate
// from storing its stale answer after it.
type devIdPSwitch struct {
	mu  sync.Mutex
	on  bool
	at  time.Time
	ok  bool
	gen uint64
	now func() time.Time // test clock; nil = time.Now
}

func (c *devIdPSwitch) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *devIdPSwitch) invalidate() {
	c.mu.Lock()
	c.ok = false
	c.gen++
	c.mu.Unlock()
}

// errDevIdPSwitchStale: the read finished after a flip, or after the cache window had already
// run out, so its answer cannot be trusted and is neither returned nor cached.
var errDevIdPSwitchStale = errors.New("dev idp: the deployment switch read was stale")

// devIdPStored is the admin's choice, true when none was made. The window is measured from
// when the read STARTED: a slow read must not extend how long an old "on" is believed. A
// read that crossed an invalidate or outlived the window is retried once, then fails; an
// unreadable store answers an error. None of those is cached.
func (m *manager) devIdPStored(ctx context.Context) (bool, error) {
	c := &m.devIdPSw
	for try := 0; try < 2; try++ {
		c.mu.Lock()
		start := c.clock()
		if c.ok && start.Sub(c.at) < devIdPSwitchTTL {
			on := c.on
			c.mu.Unlock()
			return on, nil
		}
		gen := c.gen
		c.mu.Unlock()
		rctx, cancel := context.WithTimeout(ctx, devIdPSwitchTTL)
		v, err := m.store.GetSetting(rctx, devIdPSettingKey)
		cancel()
		if err != nil {
			return false, err
		}
		on := strings.TrimSpace(v) != "off"
		c.mu.Lock()
		fresh := c.gen == gen && c.clock().Sub(start) < devIdPSwitchTTL
		if fresh {
			c.on, c.at, c.ok = on, start, true
		}
		c.mu.Unlock()
		if fresh {
			return on, nil
		}
	}
	return false, errDevIdPSwitchStale
}

// devIdPEnabled is the deployment switch in force. When in doubt — no store, a failed
// read — the answer is false: an identity provider must not stay up on a guess.
func (m *manager) devIdPEnabled(ctx context.Context) bool {
	if m.devIdPEnvOff || m.store == nil {
		return false
	}
	on, err := m.devIdPStored(ctx)
	return err == nil && on
}

type devIdPAdminAPI struct{ memberAuth }

func registerDevIdPAdminRoutes(mux *http.ServeMux, cfg config) {
	a := devIdPAdminAPI{memberAuth{cfg.mgr}}
	mux.HandleFunc("GET /api/admin/dev-idp", a.withSuperAdmin(a.get))
	mux.HandleFunc("PUT /api/admin/dev-idp", a.withSuperAdmin(a.put))
}

// devIdPAdminStatus is what the modal draws.
type devIdPAdminStatus struct {
	Enabled bool `json:"enabled"` // served right now
	Setting bool `json:"setting"` // the stored choice; true when none was made
	// Locked: AF_DEV_IDP=0 forces it off and the setting cannot change that.
	Locked bool `json:"locked"`
	// MissingBaseURL: PUBLIC_BASE_URL is unset, so there is no origin for the issuer.
	MissingBaseURL bool   `json:"missing_base_url"`
	Issuer         string `json:"issuer"`
	// OptedIn counts the workspaces registered with the IdP; they survive the switch.
	OptedIn int `json:"opted_in"`
}

func (a devIdPAdminAPI) devIdpNow(ctx context.Context) (devIdPAdminStatus, error) {
	setting, err := a.mgr.devIdPStored(ctx)
	if err != nil {
		return devIdPAdminStatus{}, err
	}
	n, err := a.mgr.store.CountDevIdPClients(ctx)
	if err != nil {
		return devIdPAdminStatus{}, err
	}
	base := strings.TrimRight(a.mgr.publicBaseURL, "/")
	s := devIdPAdminStatus{
		Setting: setting, Locked: a.mgr.devIdPEnvOff, MissingBaseURL: base == "", OptedIn: n,
		Enabled: a.mgr.devIdPIssuer(ctx) != "",
	}
	if base != "" {
		s.Issuer = base + devIdPPath
	}
	return s, nil
}

func (a devIdPAdminAPI) get(w http.ResponseWriter, r *http.Request, _ store.Identity) {
	s, err := a.devIdpNow(r.Context())
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (a devIdPAdminAPI) put(w http.ResponseWriter, r *http.Request, ident store.Identity) {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil || body.Enabled == nil {
		writeAPIErr(w, &apiError{http.StatusBadRequest, "bad_body", `want {"enabled": true|false}`})
		return
	}
	if a.mgr.devIdPEnvOff {
		writeAPIErr(w, &apiError{http.StatusConflict, "locked", "AF_DEV_IDP=0 locks the development OIDC IdP off on this deployment"})
		return
	}
	ctx := r.Context()
	state := "off"
	if *body.Enabled {
		state = "on"
	}
	// Audit first and refuse when it cannot be written: switching an identity provider that
	// signs in as any member on must not go unrecorded.
	if err := a.mgr.store.InsertAudit(ctx, store.AuditLog{
		ID: store.NewID(), TenantID: "", ActorKind: "admin", ActorID: ident.ID,
		Action: "dev_idp.set", Target: state, HTTPStatus: http.StatusOK, At: store.NowTS(),
	}); err != nil {
		writeAPIErr(w, &apiError{http.StatusServiceUnavailable, errCodeAuditUnavailable, "nothing was changed: " + err.Error()})
		return
	}
	if err := a.mgr.store.SetSetting(ctx, devIdPSettingKey, state); err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	a.mgr.devIdPSw.invalidate()
	s, err := a.devIdpNow(ctx)
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	writeJSON(w, http.StatusOK, s)
}

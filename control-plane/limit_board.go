// limit_board.go — the opt-in limit board (ADR 0111 decisions 6 and 7, issue #1962).
//
// Two sources feed it under one observation contract: the limit snapshot the reaper reads
// from GET /sessions, and the Console's usage-observation post (notification.go). Both land in
// recordLimitKind, which keeps the provider's observedAt apart from receivedAt, never lets an
// older observation overwrite a newer one, and drops what a previous login epoch left.
//
// What leaves the CP is decided here and nowhere else: projectLimits reduces a member's data
// to the level they chose before it is serialised, and boardEntry gives a viewer outside the
// audience exactly what it gives for a member nobody has ever observed. Any future path (the
// handover picker, an SSE nudge, an export) must go through boardEntry too.
package main

import (
	"context"
	"encoding/json"
	"log"
	"math"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/k-k1/agent-fleet/control-plane/internal/store"
)

// limitStaleAfter is ADR 0111 addendum item 6: within one window use only grows, so an older
// observation from before the reset is still a correct lower bound, but past an hour (about a
// fifth of a five-hour window) it is marked stale.
const limitStaleAfter = 60 * time.Minute

// limitSnapshotVersion is the only snapshot version this CP reads. Any other value makes every
// kind unknown and leaves the rest of the envelope alone.
const limitSnapshotVersion = 1

// limitKindUnits is decision 6's P1 coverage: the kinds whose quota the board collects, and
// for each the units it accepts. copilot's pools are named by the provider, so any plain id
// is taken. agy is deliberately absent (its refresh scrapes the TUI), as is every other kind.
var limitKindUnits = map[string]func(unit string) bool{
	"claude":  func(u string) bool { return u == "5h" || u == "7d" },
	"codex":   func(u string) bool { return u == "5h" || u == "7d" },
	"muse":    func(u string) bool { return u == "window" || u == "weekly" },
	"copilot": copilotPoolID.MatchString,
}

var copilotPoolID = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

// limitSessionStates are the session limit states the board shows, the second of decision
// 6's two axes. A blocked session says nothing about other models or kinds.
var limitSessionStates = map[string]bool{stateLimited: true, stateSpendLimit: true, stateBlocked: true, stateAuth: true}

// limitSnapshotWire is the "limits" member of the Agent's GET /sessions envelope.
type limitSnapshotWire struct {
	V int `json:"v"`
	// Kinds stays raw per kind, so one malformed kind costs only that kind.
	Kinds map[string]json.RawMessage `json:"kinds"`
}

type limitKindWire struct {
	Epoch string `json:"epoch"`
	// EpochSince is the Agent's time for this login; it orders epochs (store: epoch_since).
	EpochSince string          `json:"epochSince"`
	Units      []limitUnitWire `json:"units"`
}

// limitUnitWire is one quota unit as both sources send it.
type limitUnitWire struct {
	Unit       string  `json:"unit"`
	UsedPct    float64 `json:"usedPct"`
	ResetsAt   string  `json:"resetsAt,omitempty"`
	WindowMins int     `json:"windowMins,omitempty"`
	ObservedAt string  `json:"observedAt"`
	Derived    bool    `json:"derived,omitempty"`
}

// limitKindRecord is one kind's validated observations, ready to store. Obs may be empty: the
// epoch alone still clears what a previous login left.
type limitKindRecord struct {
	Kind, Epoch, EpochSince string
	Obs                     []store.LimitObservation
}

// parseLimitSnapshot turns the raw snapshot into per-kind records. It never fails: a snapshot
// that does not parse, or carries another version, yields nothing (every kind unknown), and a
// kind that does not parse is skipped alone. The envelope it rode in on was decoded before
// this runs and is not affected either way.
func parseLimitSnapshot(raw json.RawMessage, now time.Time) []limitKindRecord {
	if len(raw) == 0 {
		return nil
	}
	var snap limitSnapshotWire
	if json.Unmarshal(raw, &snap) != nil || snap.V != limitSnapshotVersion {
		return nil
	}
	var out []limitKindRecord
	for kind, kraw := range snap.Kinds {
		if limitKindUnits[kind] == nil {
			continue
		}
		var k limitKindWire
		if json.Unmarshal(kraw, &k) != nil || len(k.Epoch) > 64 {
			continue
		}
		since, ok := limitEpochSince(k.EpochSince, now)
		if !ok {
			continue
		}
		out = append(out, limitKindRecord{Kind: kind, Epoch: k.Epoch, EpochSince: since,
			Obs: validLimitUnits(kind, k.Units, now, "agent")})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// limitTS normalises an RFC3339 instant to UTC seconds. The store orders observations by
// string comparison, which is only an ordering in this one format.
func limitTS(s string) (string, time.Time, bool) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return "", time.Time{}, false
	}
	t = t.UTC().Truncate(time.Second)
	return t.Format(time.RFC3339), t, true
}

// limitEpochSince validates an epoch's login time. Without one the epoch cannot be ordered
// against the stored one, so the kind is skipped rather than allowed to replace anything.
func limitEpochSince(s string, now time.Time) (string, bool) {
	ts, t, ok := limitTS(s)
	if !ok || t.After(now.Add(limitClockSkew)) {
		return "", false
	}
	return ts, true
}

// limitClockSkew bounds how far in the future an observation may claim to be. A workspace
// clock ahead of the CP's would otherwise pin a value as "newest" until the clock caught up.
const limitClockSkew = 5 * time.Minute

// validLimitUnits keeps the units that honour the contract: a known unit, a real observation
// time, a sane percentage, and not derived. A derived value never clears "reset not yet
// observed" and never moves the stored observation, so it is dropped here rather than stored.
func validLimitUnits(kind string, units []limitUnitWire, now time.Time, source string) []store.LimitObservation {
	ok := limitKindUnits[kind]
	recv := now.UTC().Format(time.RFC3339)
	seen := map[string]bool{}
	var out []store.LimitObservation
	for _, u := range units {
		if u.Derived || !ok(u.Unit) || seen[u.Unit] || math.IsNaN(u.UsedPct) || u.UsedPct < 0 || u.UsedPct > 1000 {
			continue
		}
		obsAt, obsT, good := limitTS(u.ObservedAt)
		if !good || obsT.After(now.Add(limitClockSkew)) {
			continue
		}
		reset := ""
		if u.ResetsAt != "" {
			if reset, _, good = limitTS(u.ResetsAt); !good {
				continue
			}
		}
		mins := u.WindowMins
		if mins < 0 || mins > 366*24*60 {
			mins = 0
		}
		seen[u.Unit] = true
		out = append(out, store.LimitObservation{Kind: kind, Unit: u.Unit, UsedPct: u.UsedPct, ResetsAt: reset,
			WindowMins: mins, ObservedAt: obsAt, ReceivedAt: recv, Source: source})
	}
	return out
}

// limitSessionWire is one entry of the stored session-state list.
type limitSessionWire struct {
	Kind     string `json:"kind"`
	State    string `json:"state"`
	ResumeAt string `json:"resumeAt,omitempty"`
}

// limitSessionsOf reduces the reaper's session list to the limit states, without session
// names: the board says what a member's sessions ran into, not which sessions they run.
func limitSessionsOf(sessions []sessionWire) []limitSessionWire {
	seen := map[limitSessionWire]bool{}
	out := []limitSessionWire{}
	for _, s := range sessions {
		if !limitSessionStates[s.State] {
			continue
		}
		e := limitSessionWire{Kind: s.Kind, State: s.State}
		// Only a limited session can carry a resume time. spend_limit never does: waiting
		// does not clear it, and a time next to it would say it does.
		if s.State == stateLimited {
			if at, _, ok := limitTS(s.RateLimitResumeAt); ok {
				e.ResumeAt = at
			}
		}
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].State != out[j].State {
			return out[i].State < out[j].State
		}
		return out[i].ResumeAt < out[j].ResumeAt
	})
	return out
}

// recordLimitsFromEnvelope stores what one reaper read says about the member's limits. It runs
// after every idle and busy decision of the sweep has been taken from the same envelope, and
// nothing it does (a failed parse, a failed write) reaches back into them.
func (m *manager) recordLimitsFromEnvelope(ctx context.Context, ws store.Workspace, env agentSessionsEnvelope, now time.Time) {
	if on, err := m.store.LimitBoardEnabled(ctx, ws.TenantID); err != nil || !on {
		return
	}
	for _, k := range parseLimitSnapshot(env.Limits, now) {
		if _, err := m.store.RecordLimitObservations(ctx, ws.MembershipID, k.Kind, k.Epoch, k.EpochSince, now.UTC().Format(time.RFC3339), k.Obs); err != nil {
			log.Printf("limit board: record %s/%s: %v", ws.MembershipID, k.Kind, err)
		}
	}
	states, _ := json.Marshal(limitSessionsOf(env.Sessions))
	if err := m.store.PutLimitSessionStates(ctx, store.LimitSessionStates{MembershipID: ws.MembershipID,
		States: string(states), ObservedAt: now.UTC().Format(time.RFC3339)}); err != nil {
		log.Printf("limit board: session states %s: %v", ws.MembershipID, err)
	}
}

// --- projection ---------------------------------------------------------------------------

// limitUnitView is one quota unit as a viewer receives it. Which fields are filled depends on
// the publisher's level; the zero values are omitted, so a reduced unit does not even carry the
// field names of what was withheld.
type limitUnitView struct {
	Unit       string `json:"unit"`
	WindowMins int    `json:"windowMins,omitempty"`
	// Status: "current", "stale" (older than limitStaleAfter: still a lower bound), or
	// "reset_not_observed" (the reset time has passed and nothing newer arrived — never 0%).
	Status     string   `json:"status"`
	Band       string   `json:"band,omitempty"`
	UsedPct    *float64 `json:"usedPct,omitempty"`
	ResetsAt   string   `json:"resetsAt,omitempty"`
	ObservedAt string   `json:"observedAt,omitempty"`
}

type limitSessionView struct {
	State    string `json:"state"`
	ResumeAt string `json:"resumeAt,omitempty"`
}

type limitKindView struct {
	Kind     string             `json:"kind"`
	Units    []limitUnitView    `json:"units,omitempty"`
	Sessions []limitSessionView `json:"sessions,omitempty"`
}

// limitView is what one member's limits look like to one viewer. An empty limitView is what a
// hidden member, a member outside the viewer's audience and a never-observed member all look
// like, and it must stay that way.
type limitView struct {
	Kinds              []limitKindView `json:"kinds"`
	SessionsObservedAt string          `json:"sessionsObservedAt,omitempty"`
	SessionsStale      bool            `json:"sessionsStale,omitempty"`
}

// limitBand is decision 6's band: per unit, never aggregated. ≥95 is "near limit"; a real
// limit hit is the session state, never this band.
func limitBand(pct float64) string {
	switch {
	case pct < 50:
		return "lt50"
	case pct < 80:
		return "lt80"
	case pct < 95:
		return "lt95"
	}
	return "ge95"
}

// projectLimits reduces one member's stored data to level. level is "state", "band" or
// "exact"; anything else projects nothing.
func projectLimits(level string, obs []store.LimitObservation, ss *store.LimitSessionStates, now time.Time) limitView {
	v := limitView{Kinds: []limitKindView{}}
	if level != "state" && level != "band" && level != "exact" {
		return v
	}
	kinds := map[string]*limitKindView{}
	kindOf := func(k string) *limitKindView {
		if kinds[k] == nil {
			kinds[k] = &limitKindView{Kind: k}
		}
		return kinds[k]
	}
	if level != "state" {
		for _, o := range obs {
			u := limitUnitView{Unit: o.Unit, WindowMins: o.WindowMins, Status: "current", ObservedAt: o.ObservedAt}
			obsT, _ := time.Parse(time.RFC3339, o.ObservedAt)
			reset, _ := time.Parse(time.RFC3339, o.ResetsAt)
			switch {
			case o.ResetsAt != "" && !now.Before(reset):
				// Past the reset the old value loses even its lower-bound meaning.
				u.Status = "reset_not_observed"
			case now.Sub(obsT) > limitStaleAfter:
				u.Status = "stale"
			}
			if u.Status != "reset_not_observed" {
				u.Band = limitBand(o.UsedPct)
				if level == "exact" {
					pct := o.UsedPct
					u.UsedPct, u.ResetsAt = &pct, o.ResetsAt
				}
			}
			k := kindOf(o.Kind)
			k.Units = append(k.Units, u)
		}
	}
	if ss != nil && ss.ObservedAt != "" {
		v.SessionsObservedAt = ss.ObservedAt
		if t, err := time.Parse(time.RFC3339, ss.ObservedAt); err == nil && now.Sub(t) > limitStaleAfter {
			v.SessionsStale = true
		}
		var list []limitSessionWire
		_ = json.Unmarshal([]byte(ss.States), &list)
		for _, s := range list {
			if !limitSessionStates[s.State] {
				continue
			}
			sv := limitSessionView{State: s.State}
			if s.State == stateLimited {
				sv.ResumeAt = s.ResumeAt
			}
			k := kindOf(s.Kind)
			k.Sessions = append(k.Sessions, sv)
		}
	}
	for _, k := range kinds {
		v.Kinds = append(v.Kinds, *k)
	}
	sort.Slice(v.Kinds, func(i, j int) bool { return v.Kinds[i].Kind < v.Kinds[j].Kind })
	return v
}

// limitVisibleTo reports whether pub lets viewer see the publisher's data, and at what level.
// The caller guarantees both memberships are active members of one tenant.
func limitVisibleTo(pub store.LimitPublication, found bool, viewerMembershipID string) (string, bool) {
	if !found {
		return "", false
	}
	switch pub.Audience {
	case "tenant":
		return pub.Level, true
	case "members":
		if slices.Contains(pub.Viewers, viewerMembershipID) {
			return pub.Level, true
		}
	}
	return "", false
}

// boardEntry is the ONE place a member's limits are reduced for another member. Outside the
// audience it returns the empty view without reading anything else, which is what makes a
// hidden member and a never-observed member indistinguishable.
func boardEntry(pub store.LimitPublication, found bool, viewerMembershipID string, obs []store.LimitObservation,
	ss *store.LimitSessionStates, now time.Time) limitView {
	level, ok := limitVisibleTo(pub, found, viewerMembershipID)
	if !ok {
		return projectLimits("", nil, nil, now)
	}
	return projectLimits(level, obs, ss, now)
}

// --- REST ---------------------------------------------------------------------------------

type limitBoardAPI struct {
	memberAuth
	store store.Store
	now   func() time.Time
}

func newLimitBoardAPI(m *manager) limitBoardAPI {
	return limitBoardAPI{memberAuth: memberAuth{m}, store: m.store, now: time.Now}
}

func registerLimitBoardRoutes(mux *http.ServeMux, cfg config) {
	a := newLimitBoardAPI(cfg.mgr)
	mux.HandleFunc("GET /api/limit-board", a.withMembership(a.board))
	mux.HandleFunc("GET /api/limit-board/me", a.withMembership(a.me))
	mux.HandleFunc("PUT /api/limit-board/me", a.withMembership(a.setMe))
	mux.HandleFunc("GET /api/admin/tenants/{slug}/limit-board", a.tenantSwitch)
	mux.HandleFunc("PUT /api/admin/tenants/{slug}/limit-board", a.setTenantSwitch)
}

const (
	limitRosterDefault = 50
	limitRosterMax     = 100
)

type limitBoardMember struct {
	UserKey string `json:"userKey"`
	Email   string `json:"email"`
	limitView
}

// limitBoardPage is GET /api/limit-board. Next is the user key to pass as after= for the
// following page, "" on the last one.
type limitBoardPage struct {
	Enabled bool               `json:"enabled"`
	Members []limitBoardMember `json:"members"`
	Next    string             `json:"next"`
}

// limitBoardMe is GET/PUT /api/limit-board/me: the caller's own setting and values.
type limitBoardMe struct {
	Enabled   bool                   `json:"enabled"`
	Audience  string                 `json:"audience"`
	Level     string                 `json:"level"`
	Members   []limitAudienceMember  `json:"members"`
	Own       limitView              `json:"own"`
	Publishes []limitPublishedTenant `json:"publishes"`
}

// limitBoardSwitch is the tenant switch, GET/PUT /api/admin/tenants/{slug}/limit-board.
type limitBoardSwitch struct {
	Tenant  string `json:"tenant"`
	Enabled bool   `json:"enabled"`
}

// board (GET /api/limit-board?after=&limit=) is the roster: the tenant's active members other
// than the caller, a page at a time, each reduced to what the caller may see. It is its own
// listing rather than the 20-row recipient search (decision 6).
func (a limitBoardAPI) board(w http.ResponseWriter, r *http.Request, _ store.Identity, mv store.MembershipView) {
	ctx := r.Context()
	on, err := a.store.LimitBoardEnabled(ctx, mv.TenantID)
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	if !on {
		writeJSON(w, http.StatusOK, limitBoardPage{Members: []limitBoardMember{}})
		return
	}
	limit := limitRosterDefault
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = min(n, limitRosterMax)
	}
	// One extra row says whether there is a next page. The caller is excluded after the fetch,
	// so the page can come back one short; next is still exact.
	rows, err := a.store.ListMembersByTenantPage(ctx, mv.TenantID, r.URL.Query().Get("after"), limit+1)
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		next = rows[len(rows)-1].UserKey
	}
	ids := make([]string, 0, len(rows))
	for _, m := range rows {
		if m.MembershipID != mv.MembershipID {
			ids = append(ids, m.MembershipID)
		}
	}
	pubs, err := a.store.ListLimitPublications(ctx, ids)
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	// Only the data of members who publish to the caller is read at all.
	var visible []string
	for _, id := range ids {
		p, found := pubs[id]
		if _, ok := limitVisibleTo(p, found, mv.MembershipID); ok {
			visible = append(visible, id)
		}
	}
	obs, err := a.store.ListLimitObservations(ctx, visible)
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	states, err := a.store.ListLimitSessionStates(ctx, visible)
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	byMember := map[string][]store.LimitObservation{}
	for _, o := range obs {
		byMember[o.MembershipID] = append(byMember[o.MembershipID], o)
	}
	now := a.now()
	out := make([]limitBoardMember, 0, len(ids))
	for _, m := range rows {
		if m.MembershipID == mv.MembershipID {
			continue
		}
		p, found := pubs[m.MembershipID]
		var ss *store.LimitSessionStates
		if s, ok := states[m.MembershipID]; ok {
			ss = &s
		}
		out = append(out, limitBoardMember{UserKey: m.UserKey, Email: m.Email,
			limitView: boardEntry(p, found, mv.MembershipID, byMember[m.MembershipID], ss, now)})
	}
	writeJSON(w, http.StatusOK, limitBoardPage{Enabled: true, Members: out, Next: next})
}

type limitAudienceMember struct {
	UserKey string `json:"userKey"`
	Email   string `json:"email"`
}

type limitPublishedTenant struct {
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Audience string `json:"audience"`
	Level    string `json:"level"`
}

// me (GET /api/limit-board/me) is the member's own view: their data unreduced, their setting
// for this tenant, and every tenant they publish to — one provider account can show up in more
// than one tenant, and the member has to be able to see that (ADR 0111 addendum item 3).
func (a limitBoardAPI) me(w http.ResponseWriter, r *http.Request, ident store.Identity, mv store.MembershipView) {
	ctx := r.Context()
	on, err := a.store.LimitBoardEnabled(ctx, mv.TenantID)
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	pub, found, err := a.store.GetLimitPublication(ctx, mv.MembershipID)
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	if !found {
		pub = store.LimitPublication{Audience: "none", Level: "state"}
	}
	members, err := a.store.ListMembersByTenant(ctx, mv.TenantID)
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	audience := []limitAudienceMember{}
	for _, m := range members {
		if slices.Contains(pub.Viewers, m.MembershipID) {
			audience = append(audience, limitAudienceMember{UserKey: m.UserKey, Email: m.Email})
		}
	}
	obs, err := a.store.ListLimitObservations(ctx, []string{mv.MembershipID})
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	states, err := a.store.ListLimitSessionStates(ctx, []string{mv.MembershipID})
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	var ss *store.LimitSessionStates
	if s, ok := states[mv.MembershipID]; ok {
		ss = &s
	}
	tenants, aerr := a.publishedTenants(ctx, ident)
	if aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	// With the tenant switch off nothing is shown, the member's own values included: the
	// screen says nobody's values are collected or shown, and that must hold for this route too.
	// The setting stays readable and editable.
	own := projectLimits("", nil, nil, a.now())
	if on {
		own = projectLimits("exact", obs, ss, a.now())
	}
	writeJSON(w, http.StatusOK, limitBoardMe{Enabled: on, Audience: pub.Audience, Level: pub.Level, Members: audience,
		Own: own, Publishes: tenants})
}

// publishedTenants lists the caller's active memberships whose publication is not hidden.
func (a limitBoardAPI) publishedTenants(ctx context.Context, ident store.Identity) ([]limitPublishedTenant, *apiError) {
	ms, err := a.store.ListMemberships(ctx, ident.ID)
	if err != nil {
		return nil, internalErr(err)
	}
	ids := make([]string, 0, len(ms))
	for _, m := range ms {
		ids = append(ids, m.MembershipID)
	}
	pubs, err := a.store.ListLimitPublications(ctx, ids)
	if err != nil {
		return nil, internalErr(err)
	}
	out := []limitPublishedTenant{}
	for _, m := range ms {
		if p, ok := pubs[m.MembershipID]; ok && p.Audience != "none" {
			out = append(out, limitPublishedTenant{Slug: m.TenantSlug, Name: m.TenantName, Audience: p.Audience, Level: p.Level})
		}
	}
	return out, nil
}

// limitAudienceMax bounds a named audience. ADR 0111 sizes tenants at 2–30 members with limits
// that hold at about 100.
const limitAudienceMax = 100

// setMe (PUT /api/limit-board/me {audience, level, members}) sets the caller's publication for
// THIS tenant membership only. A named member must be an active member of the same tenant:
// the check is what keeps a setting from ever naming someone in another tenant.
func (a limitBoardAPI) setMe(w http.ResponseWriter, r *http.Request, ident store.Identity, mv store.MembershipView) {
	var in struct {
		Audience string   `json:"audience"`
		Level    string   `json:"level"`
		Members  []string `json:"members"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		writeAPIErr(w, &apiError{http.StatusBadRequest, "bad_request", "invalid JSON body"})
		return
	}
	switch in.Audience {
	case "none", "tenant", "members":
	default:
		writeAPIErr(w, &apiError{http.StatusBadRequest, "bad_audience", "audience must be none, tenant or members"})
		return
	}
	switch in.Level {
	case "state", "band", "exact":
	default:
		writeAPIErr(w, &apiError{http.StatusBadRequest, "bad_level", "level must be state, band or exact"})
		return
	}
	if len(in.Members) > limitAudienceMax {
		writeAPIErr(w, &apiError{http.StatusBadRequest, "too_many_members", "too many members"})
		return
	}
	ctx := r.Context()
	var viewers []string
	if in.Audience == "members" {
		members, err := a.store.ListMembersByTenant(ctx, mv.TenantID)
		if err != nil {
			writeAPIErr(w, internalErr(err))
			return
		}
		byKey := map[string]string{}
		for _, m := range members {
			byKey[m.UserKey] = m.MembershipID
		}
		for _, k := range in.Members {
			id, ok := byKey[strings.TrimSpace(k)]
			if !ok || id == mv.MembershipID {
				writeAPIErr(w, &apiError{http.StatusBadRequest, "unknown_member", "not a member of this tenant: " + k})
				return
			}
			if !slices.Contains(viewers, id) {
				viewers = append(viewers, id)
			}
		}
	}
	if err := a.store.PutLimitPublication(ctx, store.LimitPublication{MembershipID: mv.MembershipID,
		Audience: in.Audience, Level: in.Level, UpdatedAt: store.NowTS(), Viewers: viewers}); err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	a.me(w, r, ident, mv)
}

// tenantSwitch (GET /api/admin/tenants/{slug}/limit-board) reads the tenant switch. On by
// default (ADR 0111 addendum item 4): members are still hidden until each one opts in.
func (a limitBoardAPI) tenantSwitch(w http.ResponseWriter, r *http.Request) {
	_, t, ok := a.tenantAdminFor(w, r, r.PathValue("slug"))
	if !ok {
		return
	}
	on, err := a.store.LimitBoardEnabled(r.Context(), t.ID)
	if err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	writeJSON(w, http.StatusOK, limitBoardSwitch{Tenant: t.Slug, Enabled: on})
}

// setTenantSwitch (PUT /api/admin/tenants/{slug}/limit-board {enabled}). Turning it off stops
// collection and empties the board for every member of the tenant; nothing else is widened for
// the admin, who sees no member's limits through this route.
func (a limitBoardAPI) setTenantSwitch(w http.ResponseWriter, r *http.Request) {
	ident, t, ok := a.tenantAdminFor(w, r, r.PathValue("slug"))
	if !ok {
		return
	}
	var in struct {
		Enabled *bool `json:"enabled"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || in.Enabled == nil {
		writeAPIErr(w, &apiError{http.StatusBadRequest, "bad_request", "enabled is required"})
		return
	}
	if err := a.store.SetLimitBoardEnabled(r.Context(), t.ID, *in.Enabled, store.NowTS()); err != nil {
		writeAPIErr(w, internalErr(err))
		return
	}
	_ = a.store.InsertAudit(r.Context(), store.AuditLog{
		ID: store.NewID(), TenantID: t.ID, ActorKind: "admin", ActorID: ident.ID,
		Action: "limit_board.set", Target: t.Slug, Detail: strconv.FormatBool(*in.Enabled), At: store.NowTS(),
	})
	writeJSON(w, http.StatusOK, limitBoardSwitch{Tenant: t.Slug, Enabled: *in.Enabled})
}

//go:build e2e

// The limit board (ADR 0111 decisions 6–7, addendum items 3–6; guide/ref/limits) as other
// members of the tenant see it, on the multi-member harness.
//
// CI has no provider credentials, so no provider is ever asked. Two sources feed the board,
// and each scenario names the one it drives:
//
//   - browser: POST /api/notifications/usage-observations, the Console usage chip's post,
//     with the observedAt and login epoch the Agent stamps on a chip reading. It needs no
//     workspace, so TestLimitBoardVisibility runs without docker.
//   - reaper: the CP's sweep reads GET /sessions?limits=1 from a running workspace, whose
//     Agent builds the snapshot from claude's statusLine capture in CLAUDE_CONFIG_DIR. The
//     capture is written inside the container, by `workspace-agent statusline` (the command
//     claude itself pipes its statusLine JSON to) or as a file when its age has to be in the
//     past. TestLimitBoardReaperPull.
//
// Nothing sleeps for the 60-minute staleness: an observation is stamped in the past instead,
// which is the same comparison the CP makes at read time.
package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestLimitBoardVisibility drives the board's visibility rules through the browser source.
func TestLimitBoardVisibility(t *testing.T) {
	f := bootMemberFleet(t, envOr("WS_IMAGE", "agent-fleet/workspace:dev"))
	alice, bob, carol, dan := f.addMember("alice"), f.addMember("bob"), f.addMember("carol"), f.addMember("dan")
	now := time.Now().UTC()
	epoch := boardEpoch{epoch: "e2e0000000000001", since: now.Add(-24 * time.Hour)}

	// --- hidden by default: observed, but outside every audience (decision 7) ---
	postUsage(t, bob, "claude", epoch,
		usageWindow("5h", 85, now.Add(2*time.Hour), now.Add(-2*time.Minute)),
		usageWindow("7d", 42, now.Add(72*time.Hour), now.Add(-2*time.Minute)))
	if me := boardMe(t, bob); me.Audience != "none" || len(me.Publishes) != 0 {
		t.Fatalf("bob's default publication = %s/%s publishes %v, want none and no tenant", me.Audience, me.Level, me.Publishes)
	}
	requireOwnUnit(t, bob, "claude", "5h", 85) // the post was stored: hidden is not "never observed"
	requireLikeNeverObserved(t, alice, bob, dan, "default (hidden)")

	// --- audience tenant, level exact ---
	setPublication(t, bob, "tenant", "exact")
	e := boardEntryOf(t, alice, bob)
	u5 := e.unit(t, "claude", "5h")
	if u5.Status != "current" || u5.UsedPct == nil || *u5.UsedPct != 85 || u5.Band != "lt95" || u5.ResetsAt == "" || u5.ObservedAt == "" {
		t.Fatalf("exact 5h = %+v, want current 85%% lt95 with resetsAt and observedAt", u5)
	}
	if u7 := e.unit(t, "claude", "7d"); u7.UsedPct == nil || *u7.UsedPct != 42 || u7.Band != "lt50" {
		t.Fatalf("exact 7d = %+v, want 42%% lt50", u7)
	}

	// --- level band: the band per unit, and not even the field names of the exact values ---
	setPublication(t, bob, "tenant", "band")
	raw := boardRaw(t, alice, bob)
	for _, leak := range []string{`"usedPct"`, `"resetsAt"`} {
		if strings.Contains(raw, leak) {
			t.Fatalf("band level carries %s: %s", leak, raw)
		}
	}
	if u := boardEntryOf(t, alice, bob).unit(t, "claude", "5h"); u.Band != "lt95" || u.Status != "current" {
		t.Fatalf("band 5h = %+v, want lt95 current", u)
	}
	if u := boardEntryOf(t, alice, bob).unit(t, "claude", "7d"); u.Band != "lt50" {
		t.Fatalf("band 7d = %+v, want lt50", u)
	}

	// --- level state: no unit at all (the browser source carries no session states) ---
	setPublication(t, bob, "tenant", "state")
	if raw := boardRaw(t, alice, bob); strings.Contains(raw, `"units"`) || strings.Contains(raw, `"band"`) {
		t.Fatalf("state level carries units: %s", raw)
	}

	// --- audience named members: carol sees, alice is outside and sees "never observed" ---
	setPublication(t, bob, "members", "exact", carol.key)
	if u := boardEntryOf(t, carol, bob).unit(t, "claude", "5h"); u.UsedPct == nil || *u.UsedPct != 85 {
		t.Fatalf("named viewer carol sees 5h %+v, want 85%%", u)
	}
	requireLikeNeverObserved(t, alice, bob, dan, "outside a named audience")
	if me := boardMe(t, bob); len(me.Members) != 1 || me.Members[0].UserKey != carol.key {
		t.Fatalf("bob's named audience = %+v, want [%s]", me.Members, carol.key)
	}

	// --- stale after 60 minutes, and past the reset: stamped in the past, no sleep ---
	setPublication(t, bob, "tenant", "exact")
	postUsage(t, bob, "codex", epoch,
		usageWindow("5h", 60, now.Add(3*time.Hour), now.Add(-90*time.Minute)),
		usageWindow("7d", 30, now.Add(-10*time.Minute), now.Add(-2*time.Hour)))
	e = boardEntryOf(t, alice, bob)
	if u := e.unit(t, "codex", "5h"); u.Status != "stale" || u.UsedPct == nil || *u.UsedPct != 60 || u.Band != "lt80" {
		t.Fatalf("codex 5h observed 90 min ago = %+v, want stale 60%% lt80 (still a lower bound)", u)
	}
	if u := e.unit(t, "codex", "7d"); u.Status != "reset_not_observed" || u.UsedPct != nil || u.Band != "" {
		t.Fatalf("codex 7d past its reset = %+v, want reset_not_observed without value or band", u)
	}
	if u := e.unit(t, "claude", "5h"); u.Status != "current" {
		t.Fatalf("claude 5h observed 2 min ago reads %s, want current", u.Status)
	}

	// --- the tenant switch off: nothing shown, nothing collected ---
	setBoardSwitch(t, f, f.slug, false)
	var page boardPage
	decode(t, alice, http.MethodGet, "/api/limit-board", nil, 200, &page)
	if page.Enabled || len(page.Members) != 0 {
		t.Fatalf("board with the tenant switch off = %+v, want disabled and empty", page)
	}
	if me := boardMe(t, bob); me.Enabled || len(me.Own.Kinds) != 0 {
		t.Fatalf("bob's own values with the switch off = %+v, want disabled and empty", me)
	}
	postUsage(t, bob, "claude", epoch, usageWindow("5h", 99, now.Add(2*time.Hour), time.Now().UTC()))
	setBoardSwitch(t, f, f.slug, true)
	if u := boardEntryOf(t, alice, bob).unit(t, "claude", "5h"); u.UsedPct == nil || *u.UsedPct != 85 {
		t.Fatalf("after the switch is back on 5h = %+v, want 85%%: the post made while it was off was stored", u)
	}

	// --- a member of two tenants publishes in each separately ---
	other := f.addTenant()
	bob2, erin := bob.joinTenant(other), f.addMemberIn(other, "erin")
	if me := boardMe(t, bob2); me.Audience != "none" {
		t.Fatalf("bob's publication in %s = %s, want none: the setting is per membership", other, me.Audience)
	}
	postUsage(t, bob2, "claude", epoch, usageWindow("5h", 30, now.Add(2*time.Hour), now.Add(-time.Minute)))
	requireLikeNeverObserved(t, erin, bob2, nil, "hidden in the second tenant")
	setPublication(t, bob2, "tenant", "band")
	if raw := boardRaw(t, erin, bob2); strings.Contains(raw, `"usedPct"`) || !strings.Contains(raw, `"band":"lt50"`) {
		t.Fatalf("bob in %s (band, 30%%) reads %s", other, raw)
	}
	if u := boardEntryOf(t, alice, bob).unit(t, "claude", "5h"); u.UsedPct == nil || *u.UsedPct != 85 {
		t.Fatalf("bob in %s after publishing in %s: 5h = %+v, want 85%%", f.slug, other, u)
	}
	code, b := bob2.do(http.MethodPut, "/api/limit-board/me", map[string]any{"audience": "members", "level": "exact", "members": []string{alice.key}})
	if code != http.StatusBadRequest || !strings.Contains(string(b), "unknown_member") {
		t.Fatalf("naming a member of another tenant: %d %s, want 400 unknown_member", code, truncate(b))
	}
	got := map[string]string{}
	for _, p := range boardMe(t, bob).Publishes {
		got[p.Slug] = p.Audience + "/" + p.Level
	}
	if want := map[string]string{f.slug: "tenant/exact", other: "tenant/band"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("bob publishes to %v, want %v", got, want)
	}
	// erin is in the second tenant only, and never shows up in the first one's roster.
	if keys := rosterKeys(t, alice, 100); slices.Contains(keys, erin.key) {
		t.Fatalf("%s's roster lists erin from %s: %v", f.slug, other, keys)
	}

	// --- the paginated roster: every other member exactly once, in user-key order ---
	want := []string{}
	for _, m := range []*member{bob, carol, dan} {
		want = append(want, m.key)
	}
	slices.Sort(want)
	for _, limit := range []int{1, 2, 100} {
		if keys := rosterKeys(t, alice, limit); !slices.Equal(keys, want) {
			t.Fatalf("roster in pages of %d = %v, want %v", limit, keys, want)
		}
	}

	// --- band boundaries, each side of every edge. A member of its own, added after the
	// roster check; each post is observed a minute after the previous one, because the store
	// keeps the newer observation and would ignore an older or equal one ---
	gus := f.addMember("gus")
	setPublication(t, gus, "tenant", "band")
	for i, c := range []struct {
		pct  float64
		band string
	}{
		{0, "lt50"}, {49.9, "lt50"}, {50, "lt80"}, {79.9, "lt80"},
		{80, "lt95"}, {94.9, "lt95"}, {95, "ge95"}, {100, "ge95"},
	} {
		at := now.Add(time.Duration(i-20) * time.Minute)
		postUsage(t, gus, "claude", epoch, usageWindow("5h", c.pct, now.Add(2*time.Hour), at))
		raw := boardRaw(t, alice, gus)
		if strings.Contains(raw, `"usedPct"`) || strings.Contains(raw, `"resetsAt"`) {
			t.Fatalf("band level at %v%% carries an exact value: %s", c.pct, raw)
		}
		u := boardEntryOf(t, alice, gus).unit(t, "claude", "5h")
		if u.Band != c.band || u.ObservedAt != at.Format(time.RFC3339) {
			t.Fatalf("%v%% reads band %q observed %s, want %q observed %s", c.pct, u.Band, u.ObservedAt, c.band, at.Format(time.RFC3339))
		}
	}
}

// TestLimitBoardReaperPull feeds the board from a running workspace through the reaper.
func TestLimitBoardReaperPull(t *testing.T) {
	// A sweep every 2 s instead of every minute. The tier clocks keep their defaults (an hour
	// and more), which are on, so the sweep runs and stops nothing during the test.
	f := startMemberFleet(t, "AF_IDLE_SWEEP_INTERVAL=2s")
	alice, dave, frank := f.addMember("alice"), f.addMember("dave"), f.addMember("frank")
	setPublication(t, dave, "tenant", "exact")
	// frank publishes but has no workspace and no browser: nothing has ever reached the CP.
	setPublication(t, frank, "tenant", "exact")

	dave.startWorkspace()
	sh := dave.createSession(map[string]any{"kind": "shell", "title": "limits"})
	now := time.Now().UTC()
	reset5h, reset7d := now.Add(2*time.Hour).Unix(), now.Add(72*time.Hour).Unix()

	// --- a capture observed two hours ago reaches the board as stale ---
	old := now.Add(-2 * time.Hour).Unix()
	fixture, _ := json.Marshal(map[string]any{
		"fiveHour":   map[string]any{"usedPercent": 64, "resetsAt": reset5h},
		"sevenDay":   map[string]any{"usedPercent": 20, "resetsAt": reset7d},
		"capturedAt": old, "observedAt": old, "fiveHourObservedAt": old, "sevenDayObservedAt": old,
	})
	dave.shell(sh, fmt.Sprintf(`printf '%%s' '%s' > "${CLAUDE_CONFIG_DIR:-$HOME/.claude}/af-usage.json"`, fixture))
	u := waitBoardUnit(t, alice, dave, "claude", "5h", func(u boardUnit) bool { return u.UsedPct != nil && *u.UsedPct == 64 })
	if u.Status != "stale" || u.ObservedAt != time.Unix(old, 0).UTC().Format(time.RFC3339) {
		t.Fatalf("reaper-fed 5h observed 2h ago = %+v, want stale with observedAt %s", u, time.Unix(old, 0).UTC().Format(time.RFC3339))
	}

	// --- a new statusLine render: the real capture command, observed now ---
	payload, _ := json.Marshal(map[string]any{"rate_limits": map[string]any{
		"five_hour": map[string]any{"used_percentage": 71, "resets_at": reset5h},
		"seven_day": map[string]any{"used_percentage": 20, "resets_at": reset7d},
	}})
	dave.shell(sh, fmt.Sprintf(`printf '%%s' '%s' | workspace-agent statusline`, payload))
	u = waitBoardUnit(t, alice, dave, "claude", "5h", func(u boardUnit) bool { return u.UsedPct != nil && *u.UsedPct == 71 })
	if obs, err := time.Parse(time.RFC3339, u.ObservedAt); u.Status != "current" || err != nil || time.Since(obs) > 5*time.Minute {
		t.Fatalf("reaper-fed 5h after a new render = %+v, want current and observed just now", u)
	}
	// The 7d value did not change, so its observation time stayed two hours back.
	if u7 := boardEntryOf(t, alice, dave).unit(t, "claude", "7d"); u7.Status != "stale" {
		t.Fatalf("unchanged 7d after the render = %+v, want stale (a re-render is not an observation)", u7)
	}

	// --- level state from the reaper: the session-state axis, no units ---
	setPublication(t, dave, "tenant", "state")
	e := boardEntryOf(t, alice, dave)
	if e.SessionsObservedAt == "" {
		t.Fatalf("state level of a reaper-fed member carries no sessionsObservedAt: %+v", e)
	}
	if raw := boardRaw(t, alice, dave); strings.Contains(raw, `"units"`) || strings.Contains(raw, `"usedPct"`) {
		t.Fatalf("state level carries units: %s", raw)
	}

	// --- a member nothing has reached reads unknown, never "available" ---
	if raw := boardRaw(t, alice, frank); raw != `{"kinds":[]}` {
		t.Fatalf("frank (published, never reached) reads %s, want {\"kinds\":[]}", raw)
	}

	// --- once the workspace is stopped the reaper no longer reaches it: nothing moves ---
	dave.stopWorkspace()
	time.Sleep(3 * time.Second) // let a sweep that read the Agent before the stop finish writing
	before := boardEntryOf(t, alice, dave).SessionsObservedAt
	time.Sleep(5 * time.Second) // at least two sweeps of 2 s
	if after := boardEntryOf(t, alice, dave).SessionsObservedAt; after != before {
		t.Fatalf("sessionsObservedAt moved from %s to %s with the workspace stopped", before, after)
	}
	setPublication(t, dave, "tenant", "exact")
	if u := boardEntryOf(t, alice, dave).unit(t, "claude", "7d"); u.Status != "stale" {
		t.Fatalf("stopped workspace's 7d = %+v, want stale: nothing refreshed it", u)
	}
}

// --- limit board API helpers ----------------------------------------------

// boardEpoch is the login epoch the Agent stamps on a chip reading (limitepoch.Annotate).
type boardEpoch struct {
	epoch string
	since time.Time
}

// usageWindow is one window of the usage-observation post, as the chip sends it.
func usageWindow(key string, pct float64, resetsAt, observedAt time.Time) map[string]any {
	return map[string]any{"windowKey": key, "percent": pct,
		"resetsAt": resetsAt.UTC().Format(time.RFC3339), "observedAt": observedAt.UTC().Format(time.RFC3339)}
}

// postUsage is the browser source: the Console's usage-observation post.
func postUsage(t *testing.T, m *member, source string, e boardEpoch, windows ...map[string]any) {
	t.Helper()
	m.call(http.MethodPost, "/api/notifications/usage-observations", map[string]any{
		"source": source, "windows": windows,
		"limitEpoch": e.epoch, "limitEpochSince": e.since.UTC().Format(time.RFC3339Nano),
	}, http.StatusNoContent)
}

type boardUnit struct {
	Unit       string   `json:"unit"`
	Status     string   `json:"status"`
	Band       string   `json:"band"`
	UsedPct    *float64 `json:"usedPct"`
	ResetsAt   string   `json:"resetsAt"`
	ObservedAt string   `json:"observedAt"`
}

type boardKind struct {
	Kind  string      `json:"kind"`
	Units []boardUnit `json:"units"`
}

type boardMember struct {
	UserKey            string      `json:"userKey"`
	Kinds              []boardKind `json:"kinds"`
	SessionsObservedAt string      `json:"sessionsObservedAt"`
	raw                json.RawMessage
}

type boardPage struct {
	Enabled bool              `json:"enabled"`
	Members []json.RawMessage `json:"members"`
	Next    string            `json:"next"`
}

type boardMeView struct {
	Enabled  bool   `json:"enabled"`
	Audience string `json:"audience"`
	Level    string `json:"level"`
	Members  []struct {
		UserKey string `json:"userKey"`
	} `json:"members"`
	Own struct {
		Kinds []boardKind `json:"kinds"`
	} `json:"own"`
	Publishes []struct {
		Slug     string `json:"slug"`
		Audience string `json:"audience"`
		Level    string `json:"level"`
	} `json:"publishes"`
}

// unit finds one unit, failing when the entry has none.
func (e boardMember) unit(t *testing.T, kind, unit string) boardUnit {
	t.Helper()
	for _, k := range e.Kinds {
		for _, u := range k.Units {
			if k.Kind == kind && u.Unit == unit {
				return u
			}
		}
	}
	t.Fatalf("board entry %s has no %s/%s: %s", e.UserKey, kind, unit, e.raw)
	return boardUnit{}
}

func boardMe(t *testing.T, m *member) boardMeView {
	t.Helper()
	var me boardMeView
	decode(t, m, http.MethodGet, "/api/limit-board/me", nil, 200, &me)
	return me
}

func setPublication(t *testing.T, m *member, audience, level string, members ...string) {
	t.Helper()
	if members == nil {
		members = []string{}
	}
	me := m.putJSON("/api/limit-board/me", map[string]any{"audience": audience, "level": level, "members": members}, 200)
	if me["audience"] != audience || me["level"] != level {
		t.Fatalf("%s: publication set to %v/%v, want %s/%s", m.name, me["audience"], me["level"], audience, level)
	}
}

func setBoardSwitch(t *testing.T, f *memberFleet, slug string, on bool) {
	t.Helper()
	path := "/api/admin/tenants/" + slug + "/limit-board"
	if got := f.admin.putJSON(path, map[string]any{"enabled": on}, 200); got["enabled"] != on {
		t.Fatalf("tenant switch set to %v, want %v", got["enabled"], on)
	}
	if got := f.admin.getJSON(path); got["enabled"] != on {
		t.Fatalf("tenant switch reads %v, want %v", got["enabled"], on)
	}
}

// rosterPage reads one page of viewer's roster.
func rosterPage(t *testing.T, viewer *member, after string, limit int) boardPage {
	t.Helper()
	var p boardPage
	decode(t, viewer, http.MethodGet, fmt.Sprintf("/api/limit-board?limit=%d&after=%s", limit, url.QueryEscape(after)), nil, 200, &p)
	if !p.Enabled {
		t.Fatalf("%s: the board reads disabled", viewer.name)
	}
	if len(p.Members) > limit {
		t.Fatalf("%s: a page of %d holds %d members", viewer.name, limit, len(p.Members))
	}
	return p
}

// rosterKeys walks the whole roster in pages of limit and returns the user keys in order.
func rosterKeys(t *testing.T, viewer *member, limit int) []string {
	t.Helper()
	var keys []string
	after := ""
	for range 100 {
		p := rosterPage(t, viewer, after, limit)
		for _, raw := range p.Members {
			var e boardMember
			if err := json.Unmarshal(raw, &e); err != nil {
				t.Fatalf("roster entry: %v: %s", err, raw)
			}
			keys = append(keys, e.UserKey)
		}
		if p.Next == "" {
			return keys
		}
		after = p.Next
	}
	t.Fatalf("%s: the roster did not end after 100 pages: %v", viewer.name, keys)
	return nil
}

// boardEntryOf returns subject's entry on viewer's board.
func boardEntryOf(t *testing.T, viewer, subject *member) boardMember {
	t.Helper()
	e, ok := findBoardEntry(t, viewer, subject)
	if !ok {
		t.Fatalf("%s's board has no entry for %s", viewer.name, subject.name)
	}
	return e
}

func findBoardEntry(t *testing.T, viewer, subject *member) (boardMember, bool) {
	t.Helper()
	after := ""
	for range 100 {
		p := rosterPage(t, viewer, after, 100)
		for _, raw := range p.Members {
			var e boardMember
			if err := json.Unmarshal(raw, &e); err != nil {
				t.Fatalf("roster entry: %v: %s", err, raw)
			}
			if e.UserKey == subject.key {
				e.raw = raw
				return e, true
			}
		}
		if p.Next == "" {
			break
		}
		after = p.Next
	}
	return boardMember{}, false
}

// boardRaw is subject's entry on viewer's board as JSON, without the identifying fields: the
// part that carries what subject published.
func boardRaw(t *testing.T, viewer, subject *member) string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(boardEntryOf(t, viewer, subject).raw, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "userKey")
	delete(m, "email")
	b, _ := json.Marshal(m)
	return string(b)
}

// requireLikeNeverObserved fails unless subject reads on viewer's board exactly as a member
// nothing was ever observed for (never, or the literal empty view when never is nil).
func requireLikeNeverObserved(t *testing.T, viewer, subject, never *member, why string) {
	t.Helper()
	want := `{"kinds":[]}`
	if never != nil {
		want = boardRaw(t, viewer, never)
	}
	if got := boardRaw(t, viewer, subject); got != want {
		t.Fatalf("%s: %s sees %s as %s, want what a never-observed member reads: %s", why, viewer.name, subject.name, got, want)
	}
}

// requireOwnUnit checks m's own view (GET /me, unreduced) holds kind/unit at pct.
func requireOwnUnit(t *testing.T, m *member, kind, unit string, pct float64) {
	t.Helper()
	me := boardMe(t, m)
	own := boardMember{UserKey: m.key, Kinds: me.Own.Kinds}
	if u := own.unit(t, kind, unit); u.UsedPct == nil || *u.UsedPct != pct {
		t.Fatalf("%s's own %s/%s = %+v, want %v%%", m.name, kind, unit, u, pct)
	}
}

// waitBoardUnit polls viewer's board until subject's kind/unit satisfies ok. The reaper sweeps
// on its own clock and the Agent caches its snapshot for 20 s, so this can take a while.
func waitBoardUnit(t *testing.T, viewer, subject *member, kind, unit string, ok func(boardUnit) bool) boardUnit {
	t.Helper()
	var got boardUnit
	waitFor(t, 90*time.Second, fmt.Sprintf("%s's %s/%s on %s's board", subject.name, kind, unit, viewer.name), func() (bool, string) {
		code, b := viewer.do(http.MethodGet, "/api/limit-board?limit=100", nil)
		var p boardPage
		if code != 200 || json.Unmarshal(b, &p) != nil {
			return false, fmt.Sprintf("%d %s", code, truncate(b))
		}
		for _, raw := range p.Members {
			var e boardMember
			if json.Unmarshal(raw, &e) != nil || e.UserKey != subject.key {
				continue
			}
			for _, k := range e.Kinds {
				for _, u := range k.Units {
					if k.Kind == kind && u.Unit == unit && ok(u) {
						got = u
						return true, ""
					}
				}
			}
			return false, string(raw)
		}
		return false, "no entry: " + string(truncate(b))
	})
	return got
}

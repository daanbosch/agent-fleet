//go:build e2e

// The member inbox's fleet acceptance (ADR 0111 P1-A, docs/log/130) on the multi-member
// harness: a real CP binary under AUTH=proxy, one client per identity and tenant, the
// checklist's tables read straight from the CP's database.
//
// The scenarios in this file use the inbox routes only, which never resolve a workspace, so
// they need no docker and run wherever the e2e module runs. What needs a member's real
// workspace (launch, take-in into list_memos, flush) is inbox_workspace_test.go. Each
// scenario names the docs/log/130 IDs it checks; what is decided at the handler or store
// level (50-goroutine races, Postgres) stays in control-plane's own tests.
package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- inbox client --------------------------------------------------------------

type inboxPerson struct {
	UserKey string `json:"userKey"`
	Removed bool   `json:"removed"`
}

// inboxItem is the subset of inboxItemWire the scenarios read.
type inboxItem struct {
	ID           string       `json:"id"`
	Kind         string       `json:"kind"`
	State        string       `json:"state"`
	Delivery     string       `json:"delivery"`
	Body         string       `json:"body"`
	Sender       inboxPerson  `json:"sender"`
	Assignee     *inboxPerson `json:"assignee"`
	AssignedToMe bool         `json:"assignedToMe"`
	CanClaim     bool         `json:"canClaim"`
	Unseen       bool         `json:"unseen"`
	Session      *struct {
		Name       string      `json:"name"`
		Dir        string      `json:"dir"`
		LaunchedAt string      `json:"launchedAt"`
		By         inboxPerson `json:"by"`
	} `json:"session"`
}

type inboxList struct {
	Items   []inboxItem `json:"items"`
	Mailbox struct {
		LastSeq int64 `json:"lastSeq"`
		Unseen  int   `json:"unseen"`
		DND     bool  `json:"dnd"`
	} `json:"mailbox"`
}

func (m *member) inbox() inboxList {
	m.t.Helper()
	var l inboxList
	decode(m.t, m, http.MethodGet, "/api/inbox", nil, 200, &l)
	return l
}

// inboxFind returns the item as m's list shows it.
func (m *member) inboxFind(id string) (inboxItem, bool) {
	m.t.Helper()
	for _, it := range m.inbox().Items {
		if it.ID == id {
			return it, true
		}
	}
	return inboxItem{}, false
}

// inboxGet is GET /api/inbox/items/{id}, which must answer 200.
func (m *member) inboxGet(id string) inboxItem {
	m.t.Helper()
	var e struct {
		Item inboxItem `json:"item"`
	}
	decode(m.t, m, http.MethodGet, "/api/inbox/items/"+id, nil, 200, &e)
	return e.Item
}

func keys(to []*member) []string {
	out := make([]string, len(to))
	for i, r := range to {
		out[i] = r.key
	}
	return out
}

// send sends a DM or a request and fails unless it is created.
func (m *member) send(kind, body string, to ...*member) inboxItem {
	m.t.Helper()
	return m.sendWith(map[string]any{"kind": kind, "body": body, "recipients": keys(to)})
}

func (m *member) sendWith(body map[string]any) inboxItem {
	m.t.Helper()
	var e struct {
		Item inboxItem `json:"item"`
	}
	decode(m.t, m, http.MethodPost, "/api/inbox/items", body, http.StatusCreated, &e)
	if e.Item.ID == "" {
		m.t.Fatalf("%s: send answered no item id", m.name)
	}
	return e.Item
}

// act runs one operation of the state table (claim, decline, release, done, withdraw) and
// returns the status and the body.
func (m *member) act(id, op string) (int, map[string]any) {
	code, b := m.do(http.MethodPost, "/api/inbox/items/"+id+"/"+op, map[string]any{})
	out := map[string]any{}
	_ = json.Unmarshal(b, &out)
	return code, out
}

// mustAct runs op and fails unless it answers want.
func (m *member) mustAct(id, op string, want int) map[string]any {
	m.t.Helper()
	code, out := m.act(id, op)
	if code != want {
		m.t.Fatalf("%s: %s %s: %d (want %d) %v", m.name, op, id, code, want, out)
	}
	return out
}

// conflictOf reads an inbox 409: the error code, the state and the assignee's user key.
func conflictOf(out map[string]any) (code, state, assignee string) {
	e, _ := out["error"].(map[string]any)
	code, _ = e["code"].(string)
	state, _ = e["state"].(string)
	if a, ok := e["assignee"].(map[string]any); ok {
		assignee, _ = a["userKey"].(string)
	}
	return code, state, assignee
}

// itemState is the item's state and assignee membership id as the item table holds them.
func (f *memberFleet) itemState(id string) string {
	f.t.Helper()
	rows := f.sql(`SELECT state, assignee_membership_id FROM inbox_item WHERE id=?`, id)
	if len(rows) != 1 {
		f.t.Fatalf("item %s: rows %q", id, rows)
	}
	return rows[0]
}

// race runs every fn at the same instant and returns their results in order.
func race(fns ...func() (int, map[string]any)) ([]int, []map[string]any) {
	codes, outs := make([]int, len(fns)), make([]map[string]any, len(fns))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, fn := range fns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			codes[i], outs[i] = fn()
		}()
	}
	close(start)
	wg.Wait()
	return codes, outs
}

// inboxRowCounts is the row count of every inbox table, for "nothing was created".
func (f *memberFleet) inboxRowCounts() string {
	f.t.Helper()
	return strings.Join(f.sql(`SELECT
		(SELECT count(*) FROM inbox_item), (SELECT count(*) FROM inbox_participant),
		(SELECT count(*) FROM inbox_attachment), (SELECT count(*) FROM inbox_sender_rule)`), "")
}

func noInboxFleet(t *testing.T) *memberFleet {
	t.Helper()
	return bootMemberFleet(t, envOr("WS_IMAGE", "agent-fleet/workspace:dev"))
}

// --- scenarios -------------------------------------------------------------------

// TestInboxAccess: A-1 (signed out), A-2 (invalid recipients look alike), A-4 (another
// tenant's member), A-5 (a member who is not a participant), A-10 (the audit of a send).
func TestInboxAccess(t *testing.T) {
	t.Parallel()
	f := noInboxFleet(t)
	t2 := f.addTenant("t2")
	a, b, c, gone := f.addMember("alice"), f.addMember("bob"), f.addMember("carol"), f.addMember("erin")
	dave := f.addMemberTo(t2, "dave")
	gone.remove()
	const secret = "A-10 body text that must never reach the audit log"
	it := a.send("request", secret, b)

	// A-1: no identity at all.
	anon := &member{f: f, t: t, name: "signed-out", tenant: f.slug}
	before := f.inboxRowCounts()
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, "/api/inbox"},
		{http.MethodPost, "/api/inbox/items"},
		{http.MethodGet, "/api/inbox/items/" + it.ID},
		{http.MethodPost, "/api/inbox/items/" + it.ID + "/claim"},
		{http.MethodPost, "/api/inbox/items/" + it.ID + "/withdraw"},
	} {
		code, body := anon.do(r.method, r.path, map[string]any{"kind": "dm", "body": "x", "recipients": []string{b.key}})
		if code != http.StatusUnauthorized {
			t.Fatalf("A-1: signed out %s %s: %d %s, want 401", r.method, r.path, code, truncate(body))
		}
	}
	if got := f.inboxRowCounts(); got != before {
		t.Fatalf("A-1: inbox rows %s -> %s after signed-out requests", before, got)
	}
	if s := f.itemState(it.ID); s != "open|" {
		t.Fatalf("A-1: item is %q after a signed-out claim, want open", s)
	}

	// A-2: an unknown key, a member of T2 only, a removed member of T1, and a valid recipient
	// beside an invalid one. Same status, same body, nothing created.
	var bodies []string
	for _, to := range [][]string{{"nobody-" + f.slug}, {dave.key}, {gone.key}, {b.key, dave.key}} {
		code, body := a.do(http.MethodPost, "/api/inbox/items", map[string]any{"kind": "request", "body": "A-2 refused body", "recipients": to})
		var e map[string]any
		_ = json.Unmarshal(body, &e)
		if c, _ := apiErr(e); code != http.StatusBadRequest || c != "inbox_invalid_recipient" {
			t.Fatalf("A-2: send to %v: %d %s, want 400 inbox_invalid_recipient", to, code, truncate(body))
		}
		bodies = append(bodies, string(body))
	}
	for _, body := range bodies[1:] {
		if body != bodies[0] {
			t.Fatalf("A-2: refusals differ, so they tell the recipients apart:\n%s\n%s", bodies[0], body)
		}
	}
	if got := f.inboxRowCounts(); got != before {
		t.Fatalf("A-2: inbox rows %s -> %s after refused sends", before, got)
	}
	if n := len(b.inbox().Items); n != 1 {
		t.Fatalf("A-2: bob holds %d items, want only the first one (no partial delivery)", n)
	}

	// A-4: dave, in T2 only, knows the id.
	for _, op := range []string{"", "/read", "/claim", "/decline", "/withdraw"} {
		method := http.MethodPost
		if op == "" {
			method = http.MethodGet
		}
		if code, body := dave.do(method, "/api/inbox/items/"+it.ID+op, map[string]any{}); code != http.StatusNotFound {
			t.Fatalf("A-4: T2's dave %s item%s: %d %s, want 404", method, op, code, truncate(body))
		}
	}
	// A-5: carol is in T1 but not a participant.
	for _, op := range []string{"", "/read", "/claim", "/decline"} {
		method := http.MethodPost
		if op == "" {
			method = http.MethodGet
		}
		if code, body := c.do(method, "/api/inbox/items/"+it.ID+op, map[string]any{}); code != http.StatusNotFound {
			t.Fatalf("A-5: carol %s item%s: %d %s, want 404", method, op, code, truncate(body))
		}
	}
	if n := len(c.inbox().Items); n != 0 {
		t.Fatalf("A-5: carol's list shows %d items", n)
	}
	if s := f.itemState(it.ID); s != "open|" {
		t.Fatalf("A-4/A-5: item is %q after the outsiders' requests, want open", s)
	}
	if got := b.inboxGet(it.ID); got.Delivery != "unread" {
		t.Fatalf("A-4/A-5: bob's delivery is %q, want unread", got.Delivery)
	}

	// A-10: the send is audited with who, what kind, to whom, when; refusals are not sends;
	// the body is nowhere in the ledger.
	code, raw := f.admin.do(http.MethodGet, "/api/admin/audit?tenant="+f.slug+"&limit=1000", nil)
	if code != 200 {
		t.Fatalf("A-10: audit: %d %s", code, truncate(raw))
	}
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "A-2 refused body") {
		t.Fatalf("A-10: an inbox body is in the audit log")
	}
	var audit struct {
		Audit []map[string]any `json:"audit"`
	}
	_ = json.Unmarshal(raw, &audit)
	var sends []map[string]any
	for _, e := range audit.Audit {
		if e["action"] == "inbox.send" {
			sends = append(sends, e)
		}
	}
	if len(sends) != 1 {
		t.Fatalf("A-10: %d inbox.send entries, want 1 (refused sends are not sends): %v", len(sends), sends)
	}
	e := sends[0]
	if e["actor_email"] != a.email || e["target"] != it.ID || e["detail"] != "kind=request recipients="+b.key || e["at"] == "" {
		t.Fatalf("A-10: inbox.send entry = %v, want alice -> %s, kind=request recipients=%s", e, it.ID, b.key)
	}
}

// TestInboxTenants: A-3 (one person in two tenants), W-1, W-2 and W-4 (a recipient with no
// workspace record uses the whole inbox and still has none).
func TestInboxTenants(t *testing.T) {
	t.Parallel()
	f := noInboxFleet(t)
	t2 := f.addTenant("t2")
	a, b := f.addMember("alice"), f.addMember("bob")
	a2, b2 := f.addMemberTo(t2, "alice"), f.addMemberTo(t2, "bob")
	noWorkspace := func(m *member, when string) {
		t.Helper()
		if n := f.count(`SELECT count(*) FROM workspace WHERE membership_id=?`, m.membershipID()); n != 0 {
			t.Fatalf("%s has %d workspace record(s) %s", m.name, n, when)
		}
	}
	// W-1: the starting point. No member has touched a workspace route.
	noWorkspace(b2, "right after the invite (W-1)")

	// A-3: alice (T1) sends to bob (T1); bob's T2 view stays empty, its stream silent.
	s1, s2 := b.openStream("/api/inbox/stream"), b2.openStream("/api/inbox/stream")
	for _, s := range []*eventStream{s1, s2} {
		if _, ok := s.next(10 * time.Second); !ok {
			t.Fatalf("A-3: a stream sent no first frame")
		}
	}
	it := a.send("dm", "A-3 for bob in T1", b)
	if _, ok := s1.next(10 * time.Second); !ok {
		t.Fatalf("A-3: bob's T1 stream did not move on a T1 send")
	}
	if frame, ok := s2.next(4 * time.Second); ok {
		t.Fatalf("A-3: bob's T2 stream sent %s on a T1 send", frame)
	}
	if l := b2.inbox(); len(l.Items) != 0 || l.Mailbox.Unseen != 0 {
		t.Fatalf("A-3: bob's T2 list holds %d items, unseen %d; want 0, 0", len(l.Items), l.Mailbox.Unseen)
	}
	if l := b.inbox(); len(l.Items) != 1 || l.Items[0].ID != it.ID || l.Mailbox.Unseen != 1 {
		t.Fatalf("A-3: bob's T1 list = %d items, unseen %d; want the one item, unseen 1", len(l.Items), l.Mailbox.Unseen)
	}
	if code, _ := b2.do(http.MethodGet, "/api/inbox/items/"+it.ID, nil); code != http.StatusNotFound {
		t.Fatalf("A-3: bob reads the T1 item under T2: %d, want 404", code)
	}

	// W-2: in T2, where bob has no workspace record, alice sends a DM and two requests and
	// bob works through all of them. AUTH=proxy has no login page; every request carries
	// X-AF-Tenant (§130.8 settled points).
	dm := a2.sendWith(map[string]any{"kind": "dm", "body": "W-2 dm", "recipients": []string{b2.key},
		"attachments": []map[string]any{{"type": "recipe", "recipe": map[string]any{"prompt": "W-4 recipe prompt"}}}})
	r1, r2 := a2.send("request", "W-2 request 1", b2), a2.send("request", "W-2 request 2", b2)
	l := b2.inbox()
	if len(l.Items) != 3 {
		t.Fatalf("W-2: bob's T2 list holds %d items, want 3", len(l.Items))
	}
	b2.postJSON("/api/inbox/items/"+dm.ID+"/read", map[string]any{}, 200)
	b2.postJSON("/api/inbox/cursor", map[string]any{"seq": l.Mailbox.LastSeq}, 200)
	for _, op := range []string{"claim", "release", "claim", "done"} {
		b2.mustAct(r1.ID, op, 200)
	}
	b2.mustAct(r2.ID, "decline", 200)
	if s := f.itemState(r1.ID); !strings.HasPrefix(s, "done|") {
		t.Fatalf("W-2: request 1 is %q, want done", s)
	}
	if s := f.itemState(r2.ID); s != "declined|" {
		t.Fatalf("W-2: request 2 is %q, want declined", s)
	}
	b2.getJSON("/api/inbox/settings")
	// W-4: take-in is an inbox route too: one memo, still no workspace.
	memos := func() int { return f.count(`SELECT count(*) FROM memo WHERE membership_id=?`, b2.membershipID()) }
	n := memos()
	got := b2.postJSON("/api/inbox/items/"+dm.ID+"/take-in", map[string]any{}, http.StatusCreated)
	if got["created"] != true || memos() != n+1 {
		t.Fatalf("W-4: take-in answered %v, memos %d -> %d", got, n, memos())
	}
	noWorkspace(b2, "after using the inbox (W-2, W-4)")
	noWorkspace(a2, "after sending (W-2)")
	noWorkspace(b, "after using the T1 inbox (A-3)")
}

// TestInboxReceiveControlsAndLimits: A-6 (chosen members, block), A-7 (do-not-disturb), A-8
// (recipients, body size, pending per pair, sends per hour, each on both sides of the edge).
func TestInboxReceiveControlsAndLimits(t *testing.T) {
	t.Parallel()
	f := noInboxFleet(t)
	a, b, c := f.addMember("alice"), f.addMember("bob"), f.addMember("carol")

	// A-6: bob accepts from carol only, then blocks alice instead.
	refused := func(why string) {
		t.Helper()
		before := f.inboxRowCounts()
		code, body := a.do(http.MethodPost, "/api/inbox/items", map[string]any{"kind": "request", "body": "A-6", "recipients": []string{b.key, c.key}})
		var e struct {
			Error struct {
				Code       string   `json:"code"`
				Recipients []string `json:"recipients"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		if code != http.StatusForbidden || e.Error.Code != "inbox_recipient_not_accepting" ||
			len(e.Error.Recipients) != 1 || e.Error.Recipients[0] != b.key {
			t.Fatalf("A-6 (%s): send answered %d %s, want 403 inbox_recipient_not_accepting naming bob", why, code, truncate(body))
		}
		if got := f.inboxRowCounts(); got != before {
			t.Fatalf("A-6 (%s): inbox rows %s -> %s", why, before, got)
		}
		if n := len(c.inbox().Items); n != 0 {
			t.Fatalf("A-6 (%s): carol got %d items from a refused send", why, n)
		}
	}
	b.call(http.MethodPut, "/api/inbox/settings", map[string]any{"acceptFrom": "chosen", "allowed": []string{c.key}}, 200)
	refused("chosen members")
	b.call(http.MethodPut, "/api/inbox/settings", map[string]any{"acceptFrom": "tenant"}, 200)
	b.call(http.MethodPut, "/api/inbox/blocks/"+a.key, nil, 200)
	refused("block")
	b.call(http.MethodDelete, "/api/inbox/blocks/"+a.key, nil, 200)
	a.send("request", "A-6 after unblock", b, c) // the positive control: the same send goes through

	// A-7: carol turns on do-not-disturb. Delivery is unchanged, nothing is declined.
	c.call(http.MethodPut, "/api/inbox/settings", map[string]any{"acceptFrom": "tenant", "dnd": true}, 200)
	dm, rq := a.send("dm", "A-7 dm", c), a.send("request", "A-7 request", c)
	l := c.inbox()
	if !l.Mailbox.DND {
		t.Fatalf("A-7: carol's mailbox does not read dnd")
	}
	for _, id := range []string{dm.ID, rq.ID} {
		got, ok := c.inboxFind(id)
		if !ok || got.Delivery != "unread" || !got.Unseen {
			t.Fatalf("A-7: item %s under do-not-disturb = %+v (found %v), want delivered, unread", id, got, ok)
		}
	}
	if s := f.itemState(rq.ID); s != "open|" {
		t.Fatalf("A-7: request under do-not-disturb is %q, want open", s)
	}

	// A-8, recipients: 20 pass, 21 do not.
	var many []*member
	for i := 1; i <= 21; i++ {
		many = append(many, f.addMember(fmt.Sprintf("r%02d", i)))
	}
	x := f.addMember("xavier")
	x.send("dm", "A-8 to twenty", many[:20]...)
	limited := func(sender *member, body map[string]any, status int, code string) {
		t.Helper()
		before := f.inboxRowCounts()
		got := sender.call(http.MethodPost, "/api/inbox/items", body, status)
		if c, _ := apiErr(got); c != code {
			t.Fatalf("A-8: refusal code %q, want %q (%v)", c, code, got)
		}
		if after := f.inboxRowCounts(); after != before {
			t.Fatalf("A-8 (%s): inbox rows %s -> %s", code, before, after)
		}
	}
	limited(x, map[string]any{"kind": "dm", "body": "A-8 to 21", "recipients": keys(many)}, http.StatusBadRequest, "inbox_too_many_recipients")
	// Body: the cap exactly, and one byte over.
	x.send("dm", strings.Repeat("b", 16<<10), b)
	limited(x, map[string]any{"kind": "dm", "body": strings.Repeat("b", 16<<10+1), "recipients": []string{b.key}},
		http.StatusRequestEntityTooLarge, "inbox_body_too_large")
	// Pending per pair: ten unread DMs from yuri to bob, the eleventh is refused.
	y := f.addMember("yuri")
	for i := 0; i < 10; i++ {
		y.send("dm", fmt.Sprintf("A-8 pending %d", i), b)
	}
	limited(y, map[string]any{"kind": "dm", "body": "A-8 pending 11", "recipients": []string{b.key}},
		http.StatusTooManyRequests, "inbox_pending_limit")
	// Per hour: thirty sends from zoe, spread so no pair reaches its own limit, then one more.
	z := f.addMember("zoe")
	for i := 0; i < 30; i++ {
		z.send("dm", fmt.Sprintf("A-8 hourly %d", i), many[i%len(many)])
	}
	limited(z, map[string]any{"kind": "dm", "body": "A-8 hourly 31", "recipients": []string{c.key}},
		http.StatusTooManyRequests, "inbox_rate_limited")
}

// TestInboxClaimRaces: R-1 (two recipients claim at once, 20 rounds), R-2 (claim against
// withdraw), R-3 (done or release against withdraw), R-4 (a double claim), R-5 (every
// operation on every final state). The 50-goroutine race on both dialects is
// TestInboxClaimRaceHasOneWinner; this is the same contest through the real HTTP stack.
func TestInboxClaimRaces(t *testing.T) {
	t.Parallel()
	f := noInboxFleet(t)
	a, b, c := f.addMember("alice"), f.addMember("bob"), f.addMember("carol")
	s2, s3 := f.addMember("sam"), f.addMember("tess")
	claim := func(m *member, id string) func() (int, map[string]any) {
		return func() (int, map[string]any) { return m.act(id, "claim") }
	}

	// R-1
	for i := 0; i < 20; i++ {
		it := a.send("request", fmt.Sprintf("R-1 round %d", i), b, c)
		codes, outs := race(claim(b, it.ID), claim(c, it.ID))
		var winner, loser *member
		var lost map[string]any
		switch {
		case codes[0] == 200 && codes[1] == 409:
			winner, loser, lost = b, c, outs[1]
		case codes[1] == 200 && codes[0] == 409:
			winner, loser, lost = c, b, outs[0]
		default:
			t.Fatalf("R-1 round %d: claims answered %v (%v), want one 200 and one 409", i, codes, outs)
		}
		if code, state, who := conflictOf(lost); code != "inbox_state_conflict" || state != "claimed" || who != winner.key {
			t.Fatalf("R-1 round %d: the loser's 409 = %v, want claimed by %s", i, lost, winner.key)
		}
		if s := f.itemState(it.ID); s != "claimed|"+winner.membershipID() {
			t.Fatalf("R-1 round %d: item table %q, want claimed by %s", i, s, winner.name)
		}
		if got := loser.inboxGet(it.ID); got.CanClaim || got.Assignee == nil || got.Assignee.UserKey != winner.key {
			t.Fatalf("R-1 round %d: the loser sees %+v, want claimed by %s and nothing to claim", i, got, winner.key)
		}
		winner.mustAct(it.ID, "done", 200) // final, so the pair's pending limit is never reached
	}

	// R-2
	for i := 0; i < 10; i++ {
		it := s2.send("request", fmt.Sprintf("R-2 round %d", i), b)
		codes, outs := race(claim(b, it.ID), func() (int, map[string]any) { return s2.act(it.ID, "withdraw") })
		if codes[1] != 200 {
			t.Fatalf("R-2 round %d: withdraw answered %d %v", i, codes[1], outs[1])
		}
		if s := f.itemState(it.ID); !strings.HasPrefix(s, "withdrawn|") {
			t.Fatalf("R-2 round %d: final state %q, want withdrawn", i, s)
		}
		got := b.inboxGet(it.ID)
		switch codes[0] {
		case 200: // claimed first: bob is told it was withdrawn
			if got.State != "withdrawn" || !got.Unseen {
				t.Fatalf("R-2 round %d: bob sees %+v after the withdraw, want withdrawn and unseen", i, got)
			}
		case 409:
			if _, state, _ := conflictOf(outs[0]); state != "withdrawn" {
				t.Fatalf("R-2 round %d: late claim's 409 = %v, want withdrawn", i, outs[0])
			}
		default:
			t.Fatalf("R-2 round %d: claim answered %d %v", i, codes[0], outs[0])
		}
		if got.State != "withdrawn" || got.CanClaim {
			t.Fatalf("R-2 round %d: bob holds %+v, want withdrawn with nothing to claim", i, got)
		}
	}

	// R-3: done against withdraw, then release against withdraw.
	for _, other := range []string{"done", "release"} {
		for i := 0; i < 10; i++ {
			it := s3.send("request", fmt.Sprintf("R-3 %s round %d", other, i), b)
			b.mustAct(it.ID, "claim", 200)
			codes, outs := race(func() (int, map[string]any) { return b.act(it.ID, other) },
				func() (int, map[string]any) { return s3.act(it.ID, "withdraw") })
			final := strings.SplitN(f.itemState(it.ID), "|", 2)[0]
			switch other {
			case "done":
				ok := (codes[0] == 200 && codes[1] == 409 && final == "done") ||
					(codes[1] == 200 && codes[0] == 409 && final == "withdrawn")
				if !ok {
					t.Fatalf("R-3 done round %d: answers %v (%v), final %q; want one winner and its state", i, codes, outs, final)
				}
			case "release":
				if codes[1] != 200 || final != "withdrawn" || (codes[0] != 200 && codes[0] != 409) {
					t.Fatalf("R-3 release round %d: answers %v (%v), final %q; want withdraw 200, withdrawn", i, codes, outs, final)
				}
			}
			if sb, sa := b.inboxGet(it.ID).State, s3.inboxGet(it.ID).State; sb != final || sa != final {
				t.Fatalf("R-3 %s round %d: bob sees %s, tess sees %s, table %s", other, i, sb, sa, final)
			}
		}
	}

	// R-4: bob claims twice at once, as two tabs would.
	r4 := a.send("request", "R-4", b)
	codes, outs := race(claim(b, r4.ID), claim(b, r4.ID))
	if codes[0] != 200 || codes[1] != 200 {
		t.Fatalf("R-4: double claim answered %v (%v), want 200 twice", codes, outs)
	}
	if s := f.itemState(r4.ID); s != "claimed|"+b.membershipID() {
		t.Fatalf("R-4: item table %q, want claimed by bob", s)
	}

	// R-5: done (r4), declined, withdrawn; every operation is refused and changes nothing.
	b.mustAct(r4.ID, "done", 200)
	declined := a.send("request", "R-5 declined", b)
	b.mustAct(declined.ID, "decline", 200)
	withdrawn := a.send("request", "R-5 withdrawn", b)
	a.mustAct(withdrawn.ID, "withdraw", 200)
	for _, it := range []inboxItem{r4, declined, withdrawn} {
		want := f.itemState(it.ID)
		for _, op := range []struct {
			m  *member
			op string
		}{{b, "claim"}, {b, "decline"}, {b, "release"}, {b, "done"}, {a, "withdraw"}} {
			code, out := op.m.act(it.ID, op.op)
			if c, _, _ := conflictOf(out); code != http.StatusConflict || c != "inbox_state_conflict" {
				t.Fatalf("R-5: %s on a %s item: %d %v, want 409", op.op, want, code, out)
			}
		}
		if got := f.itemState(it.ID); got != want {
			t.Fatalf("R-5: item %s moved from %q to %q", it.ID, want, got)
		}
	}
}

// TestInboxMemberRemoval: M-1..M-6 (logical removal, block, re-invite) and M-9, M-7, M-8
// (the end of a body: removal, workspace destroy and hard delete, purge twice). The purge
// runs at CP start-up, so restarting the CP is how the scenario runs it. Destroying a
// workspace goes through the runtime, which is why this one needs docker.
func TestInboxMemberRemoval(t *testing.T) {
	t.Parallel()
	f := startMemberFleet(t)
	a, b, c := f.addMember("alice"), f.addMember("bob"), f.addMember("carol")
	aID, bID := a.membershipID(), b.membershipID()
	// A workspace record for alice and bob, so M-7 meets the hard delete's real precondition.
	// GET /api/workspace is a withResolved route: it creates the record, it starts nothing.
	for _, m := range []*member{a, b} {
		m.getJSON("/api/workspace")
		if n := f.count(`SELECT count(*) FROM workspace WHERE membership_id=?`, m.membershipID()); n != 1 {
			t.Fatalf("%s has %d workspace records, want 1", m.name, n)
		}
	}
	state := func(id string) string { t.Helper(); return strings.SplitN(f.itemState(id), "|", 2)[0] }
	requireState := func(id, want, what string) {
		t.Helper()
		if got := state(id); got != want {
			t.Fatalf("%s: item is %s, want %s", what, got, want)
		}
		if got := a.inboxGet(id).State; got != want {
			t.Fatalf("%s: alice's list reads %s, want %s", what, got, want)
		}
	}

	// --- bob, the assignee, is removed (M-1, M-2, M-5) and re-invited (M-6) ---
	m1a := a.send("request", "M-1 (a)", b, c)
	b.mustAct(m1a.ID, "claim", 200)
	m1b := a.send("request", "M-1 (b)", b)
	b.mustAct(m1b.ID, "claim", 200)
	m2 := a.send("request", "M-2", b, c)
	c.mustAct(m2.ID, "decline", 200)
	m6 := a.send("request", "M-6 carol takes it while bob is away", b, c)
	b.mustAct(m6.ID, "claim", 200)
	m9 := a.send("dm", "M-9 alice and bob only", b)
	stream := b.openStream("/api/inbox/stream")
	if _, ok := stream.next(10 * time.Second); !ok {
		t.Fatalf("M-5: bob's stream sent no first frame")
	}
	b.remove()
	if !stream.endsWithin(10 * time.Second) {
		t.Fatalf("M-5: bob's stream is still open after the removal")
	}
	if code, body := b.do(http.MethodGet, "/api/inbox", nil); code != http.StatusUnauthorized && code != http.StatusForbidden {
		t.Fatalf("M-5: removed bob lists the inbox: %d %s, want 401 or 403", code, truncate(body))
	}
	requireState(m1a.ID, "open", "M-1 (a)")
	if got := c.inboxGet(m1a.ID); !got.CanClaim || got.Assignee != nil {
		t.Fatalf("M-1 (a): carol sees %+v, want it open for her", got)
	}
	requireState(m1b.ID, "declined", "M-1 (b)")
	requireState(m2.ID, "declined", "M-2")
	requireState(m6.ID, "open", "M-6 setup")
	c.mustAct(m6.ID, "claim", 200)

	b.reinvite()
	if got := b.inboxGet(m1a.ID); got.State != "open" || !got.CanClaim {
		t.Fatalf("M-6 (a): re-invited bob sees %+v, want it open for him again", got)
	}
	if got := b.inboxGet(m6.ID); got.State != "claimed" || got.Assignee == nil || got.Assignee.UserKey != c.key {
		t.Fatalf("M-6 (a): the request carol took reads %+v, want carol's", got)
	}
	if got := b.inboxGet(m1b.ID); got.State != "declined" || got.CanClaim {
		t.Fatalf("M-6 (b): re-invited bob sees %+v, want declined", got)
	}

	// --- bob blocks the sender of requests he holds (M-4) ---
	m4a := a.send("request", "M-4 (a)", b, c)
	b.mustAct(m4a.ID, "claim", 200)
	m4b := a.send("request", "M-4 (b)", b)
	b.mustAct(m4b.ID, "claim", 200)
	b.call(http.MethodPut, "/api/inbox/blocks/"+a.key, nil, 200)
	requireState(m4a.ID, "open", "M-4 (a)")
	if got, ok := b.inboxFind(m4a.ID); ok && (got.CanClaim || got.AssignedToMe) {
		t.Fatalf("M-4 (a): bob still has it waiting: %+v", got)
	}
	if got := c.inboxGet(m4a.ID); !got.CanClaim {
		t.Fatalf("M-4 (a): carol cannot claim it: %+v", got)
	}
	requireState(m4b.ID, "declined", "M-4 (b)")
	b.call(http.MethodDelete, "/api/inbox/blocks/"+a.key, nil, 200)

	// --- alice, the sender, is removed (M-3) ---
	m3open := a.send("request", "M-3 open", c)
	m3claimed := a.send("request", "M-3 claimed", c)
	c.mustAct(m3claimed.ID, "claim", 200)
	a.remove()
	for _, id := range []string{m3open.ID, m3claimed.ID} {
		if got := state(id); got != "withdrawn" {
			t.Fatalf("M-3: alice's request is %s after her removal, want withdrawn", got)
		}
		if got := c.inboxGet(id); got.State != "withdrawn" || got.CanClaim || !got.Unseen {
			t.Fatalf("M-3: carol sees %+v, want withdrawn, unseen, nothing to do", got)
		}
	}

	// --- M-9: the last participant of the DM leaves; nobody reads anything ---
	if n := f.count(`SELECT count(*) FROM inbox_item WHERE id=?`, m9.ID); n != 1 {
		t.Fatalf("M-9: the DM is gone while bob is still a member")
	}
	b.remove()
	if n := f.count(`SELECT count(*) FROM inbox_item WHERE id=?`, m9.ID); n != 0 {
		t.Fatalf("M-9: the DM's body outlives its last participant's membership")
	}

	// --- M-7: destroy the workspaces, then the hard delete ---
	sendsBefore := f.count(`SELECT count(*) FROM audit_log WHERE action='inbox.send'`)
	for _, m := range []*member{a, b} {
		if code, out := m.deleteMembership(); code != http.StatusConflict {
			t.Fatalf("M-7: hard delete with a workspace record answered %d %v, want 409 workspace_present", code, out)
		}
		m.destroyWorkspace()
		waitFor(t, 30*time.Second, m.name+"'s workspace record gone", func() (bool, string) {
			n := f.count(`SELECT count(*) FROM workspace WHERE membership_id=?`, m.membershipID())
			return n == 0, fmt.Sprint(n)
		})
		if code, out := m.deleteMembership(); code != 200 {
			t.Fatalf("M-7: hard delete of %s answered %d %v", m.name, code, out)
		}
	}
	for _, id := range []string{aID, bID} {
		if n := f.count(`SELECT (SELECT count(*) FROM inbox_participant WHERE membership_id=?1)
			+ (SELECT count(*) FROM inbox_mailbox WHERE membership_id=?1)
			+ (SELECT count(*) FROM inbox_sender_rule WHERE membership_id=?1 OR sender_membership_id=?1)`, id); n != 0 {
			t.Fatalf("M-7: %d delivery/mailbox/rule rows of a deleted membership remain", n)
		}
	}
	if got := c.inboxGet(m3open.ID); got.Body != "M-3 open" {
		t.Fatalf("M-7: carol cannot read what she took part in: %+v", got)
	}
	if n := f.count(`SELECT count(*) FROM audit_log WHERE action='inbox.send'`); n != sendsBefore {
		t.Fatalf("M-7: inbox.send audit entries %d -> %d", sendsBefore, n)
	}

	// --- M-8: the purge, twice ---
	orphans := `SELECT
		(SELECT count(*) FROM inbox_participant p WHERE NOT EXISTS (SELECT 1 FROM membership m WHERE m.id=p.membership_id))
		+ (SELECT count(*) FROM inbox_mailbox x WHERE NOT EXISTS (SELECT 1 FROM membership m WHERE m.id=x.membership_id))
		+ (SELECT count(*) FROM inbox_participant p WHERE NOT EXISTS (SELECT 1 FROM inbox_item i WHERE i.id=p.item_id))
		+ (SELECT count(*) FROM inbox_attachment x WHERE NOT EXISTS (SELECT 1 FROM inbox_item i WHERE i.id=x.item_id))
		+ (SELECT count(*) FROM inbox_item i WHERE NOT EXISTS (SELECT 1 FROM inbox_participant p
			JOIN membership m ON m.id=p.membership_id WHERE p.item_id=i.id AND m.status='active'))`
	f.cp.restart()
	if n := f.count(orphans); n != 0 {
		t.Fatalf("M-8: %d orphan inbox rows after the first purge", n)
	}
	first := f.inboxRowCounts()
	f.cp.restart()
	if n := f.count(orphans); n != 0 {
		t.Fatalf("M-8: %d orphan inbox rows after the second purge", n)
	}
	if again := f.inboxRowCounts(); again != first {
		t.Fatalf("M-8: the second purge changed the inbox tables: %s -> %s", first, again)
	}
	if got := c.inboxGet(m3open.ID); got.Body != "M-3 open" {
		t.Fatalf("M-8: the purge took carol's item: %+v", got)
	}
}

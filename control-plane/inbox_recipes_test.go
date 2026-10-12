package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/k-k1/agent-fleet/control-plane/internal/store"
)

func TestCanonicalRepoIdentity(t *testing.T) {
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{"", "", true},
		{"github.com/k-k1/agent-fleet", "github.com/k-k1/agent-fleet", true},
		{"https://github.com/K-K1/Agent-Fleet.git", "github.com/k-k1/agent-fleet", true},
		{"https://user:secret@github.com/k-k1/agent-fleet?tab=x#readme", "github.com/k-k1/agent-fleet", true},
		{"git@github.com:k-k1/agent-fleet.git", "github.com/k-k1/agent-fleet", true},
		{"ssh://git@bitbucket.org/team/repo", "bitbucket.org/team/repo", true},
		{"https://gitlab.com/a/b", "", false},           // not a provider a member can connect
		{"https://github.com/a/b/tree/main", "", false}, // not owner/repo
		{"https://github.com/a", "", false},             // no repo
		{"file:///etc/passwd", "", false},               // not a remote
		{"github.com/a/b c", "", false},                 // not a name
		{"https://github.com/../etc", "", false},        // not a name
	} {
		got, ok := canonicalRepoIdentity(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("canonicalRepoIdentity(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func (e *inboxEnv) sendWith(who, kind string, attachments []map[string]any, to ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	var keys []string
	for _, n := range to {
		keys = append(keys, key(n))
	}
	return e.do(who, "POST", "/api/inbox/items", map[string]any{"kind": kind, "recipients": keys,
		"body": "please look at the flaky test", "attachments": attachments})
}

var testRecipe = map[string]any{"type": "recipe", "recipe": map[string]any{
	"prompt": "Find why TestLogin is flaky", "kind": "claude", "model": "opus", "effort": "high",
	"repo": "https://x:tok@github.com/k-k1/agent-fleet.git?ref=1"}}

type takeInResp struct {
	Memo    memoDTO `json:"memo"`
	Created bool    `json:"created"`
}

// TestInboxRecipeTakeIn is docs/log/130 T-1..T-3 through the routes: the recipe is invisible
// to the memo queue until taken in, a request's recipe only by its assignee, once, with the
// server's origin, and a DM's by any recipient.
func TestInboxRecipeTakeIn(t *testing.T) {
	e := newInboxEnv(t)
	rec := e.sendWith("a", "request", []map[string]any{testRecipe}, "b", "c")
	if rec.Code != http.StatusCreated {
		t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
	}
	id := decode[itemResp](t, rec).Item.ID
	got := decode[itemResp](t, e.do("b", "GET", "/api/inbox/items/"+id, nil)).Item
	if len(got.Attachments) != 1 || got.Attachments[0].Recipe == nil {
		t.Fatalf("attachments: %+v", got.Attachments)
	}
	r := got.Attachments[0].Recipe
	if r.Repo != "github.com/k-k1/agent-fleet" || r.Prompt != "Find why TestLogin is flaky" || r.CanTakeIn {
		t.Fatalf("recipe as b sees it before claiming: %+v", r)
	}
	if strings.Contains(rec.Body.String(), "tok@") {
		t.Fatal("the repository's userinfo was kept")
	}
	if n := e.count("memo"); n != 0 {
		t.Fatalf("T-1: %d memo row(s) before take-in", n)
	}
	if r := e.do("b", "POST", "/api/inbox/items/"+id+"/take-in", nil); r.Code != http.StatusConflict {
		t.Fatalf("take-in before claiming: %d %s", r.Code, r.Body.String())
	}
	_ = e.do("b", "POST", "/api/inbox/items/"+id+"/claim", nil)
	first := e.do("b", "POST", "/api/inbox/items/"+id+"/take-in", nil)
	if first.Code != http.StatusCreated {
		t.Fatalf("T-2 take-in: %d %s", first.Code, first.Body.String())
	}
	m := decode[takeInResp](t, first).Memo
	if m.Origin == nil || m.Origin.ItemID != id || m.Origin.Sender != key("a") || m.Body != "Find why TestLogin is flaky" {
		t.Fatalf("T-2 memo: %+v", m)
	}
	second := e.do("b", "POST", "/api/inbox/items/"+id+"/take-in", nil)
	if second.Code != http.StatusOK || decode[takeInResp](t, second).Memo.ID != m.ID {
		t.Fatalf("T-3 second take-in: %d %s", second.Code, second.Body.String())
	}
	for _, who := range []string{"c", "a"} {
		if r := e.do(who, "POST", "/api/inbox/items/"+id+"/take-in", nil); r.Code != http.StatusConflict {
			t.Fatalf("T-3 take-in by %s: %d %s", who, r.Code, r.Body.String())
		}
	}
	if n := e.count("memo"); n != 1 {
		t.Fatalf("%d memos after the take-ins", n)
	}
	if got := decode[itemResp](t, e.do("b", "GET", "/api/inbox/items/"+id, nil)).Item.Attachments[0].Recipe; got.MemoID != m.ID || !got.CanTakeIn {
		t.Fatalf("the card does not show the memo taken in: %+v", got)
	}
	// What list_memos reads, through the memo token: the one memo with its origin.
	list := e.memoTokenDo("b", "GET", "/internal/memos", nil)
	var memos []memoDTO
	_ = json.Unmarshal(list.Body.Bytes(), &memos)
	if len(memos) != 1 || memos[0].Origin == nil || memos[0].Origin.ItemID != id {
		t.Fatalf("T-2 list_memos: %s", list.Body.String())
	}
	// A DM: any recipient may take it in.
	dm := decode[itemResp](t, e.sendWith("a", "dm", []map[string]any{testRecipe}, "b", "c")).Item.ID
	if r := e.do("c", "POST", "/api/inbox/items/"+dm+"/take-in", nil); r.Code != http.StatusCreated {
		t.Fatalf("DM take-in: %d %s", r.Code, r.Body.String())
	}
}

func TestInboxRecipeRefusesBadRepo(t *testing.T) {
	e := newInboxEnv(t)
	bad := map[string]any{"type": "recipe", "recipe": map[string]any{"prompt": "x", "repo": "https://gitlab.example/a/b"}}
	rec := e.sendWith("a", "dm", []map[string]any{bad}, "b")
	if rec.Code != http.StatusBadRequest || decode[errResp](t, rec).Error.Code != errCodeInboxRepoInvalid {
		t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
	}
	if e.count("inbox_item") != 0 {
		t.Fatal("a refused recipe left an item")
	}
}

// memoTokenDo calls the /internal/memos face as the member's agent would, with its memo token.
func (e *inboxEnv) memoTokenDo(who, method, path string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Authorization", "Bearer "+mintMemoToken(memoSignKey(e.mgr.tokenSignMaster()), e.members[who]))
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

// TestInboxOriginCannotBeForged is docs/log/130 V-2 and V-3: add_memo and update_memo reach
// the CP through /internal/memos with their arguments as they are, and neither can set,
// change or remove an origin. The Console's own memo routes cannot either.
func TestInboxOriginCannotBeForged(t *testing.T) {
	e := newInboxEnv(t)
	forged := map[string]any{"kind": "text", "body": "mine", "origin": map[string]any{"itemId": "x", "sender": "boss"}}
	for _, r := range []*httptest.ResponseRecorder{
		e.memoTokenDo("b", "POST", "/internal/memos", forged),
		e.do("b", "POST", "/api/memos", forged),
	} {
		if r.Code != http.StatusCreated || strings.Contains(r.Body.String(), `"origin"`) {
			t.Fatalf("V-3 create with an origin: %d %s", r.Code, r.Body.String())
		}
	}
	id := decode[itemResp](t, e.sendWith("a", "dm", []map[string]any{testRecipe}, "b")).Item.ID
	m := decode[takeInResp](t, e.do("b", "POST", "/api/inbox/items/"+id+"/take-in", nil)).Memo
	for _, r := range []*httptest.ResponseRecorder{
		e.memoTokenDo("b", "PATCH", "/internal/memos/"+m.ID, map[string]any{"body": "edited by the agent", "origin": nil}),
		e.do("b", "PATCH", "/api/memos/"+m.ID, map[string]any{"body": "edited", "origin": map[string]any{"itemId": "y", "sender": "z"}}),
	} {
		if r.Code != http.StatusOK {
			t.Fatalf("patch: %d %s", r.Code, r.Body.String())
		}
		got := decode[memoDTO](t, r)
		if got.Origin == nil || got.Origin.ItemID != id || got.Origin.Sender != key("a") {
			t.Fatalf("V-2/V-3: the origin after an edit: %+v", got.Origin)
		}
	}
}

// capturingAgent records the prompts a flush sends and the session creates it is asked for.
type capturingAgent struct {
	mu      sync.Mutex
	prompts []string
	creates []map[string]any
	srv     *httptest.Server
}

func newCapturingAgent(t *testing.T) *capturingAgent {
	c := &capturingAgent{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/turn"):
			p, _ := body["prompt"].(string)
			c.prompts = append(c.prompts, p)
			_, _ = io.WriteString(w, `{"ok":true}`)
		case r.Method == "POST" && r.URL.Path == "/sessions":
			c.creates = append(c.creates, body)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"name":"s-new","dir":"/home/dev/repos/app"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (e *inboxEnv) workspaceFor(who string, agent *capturingAgent) store.Workspace {
	e.t.Helper()
	ws := store.Workspace{ID: "ws-" + who, TenantID: e.t1.ID, MembershipID: e.members[who], ContainerName: who,
		Network: "n", DataDir: "/d", AgentPort: "1", AgentToken: "tok", State: "running", CreatedAt: store.NowTS()}
	if err := e.st.CreateWorkspace(context.Background(), ws); err != nil {
		e.t.Fatal(err)
	}
	e.mgr.rts[e.members[who]] = cachedRT{rt: stubRuntime{endpoint: agent.srv.URL, token: "tok"}, ws: ws}
	return ws
}

// TestMemoFlushKeepsOriginAlone is docs/log/130 T-4: a taken-in memo is never sent in one
// message with the member's own memos, an edited text included, and it goes with the
// server's origin line.
func TestMemoFlushKeepsOriginAlone(t *testing.T) {
	e := newInboxEnv(t)
	agent := newCapturingAgent(t)
	e.workspaceFor("b", agent)
	id := decode[itemResp](t, e.sendWith("a", "dm", []map[string]any{testRecipe}, "b")).Item.ID
	origin := decode[takeInResp](t, e.do("b", "POST", "/api/inbox/items/"+id+"/take-in", nil)).Memo
	own := decode[memoDTO](t, e.do("b", "POST", "/api/memos", map[string]any{"kind": "text", "body": "my own note"}))
	for _, text := range []string{"", "edited together"} {
		r := e.do("b", "POST", "/api/memos/flush", map[string]any{"sessionName": "s1", "ids": []string{own.ID, origin.ID}, "text": text})
		if r.Code != http.StatusConflict || decode[errResp](t, r).Error.Code != errCodeMemoOriginAlone {
			t.Fatalf("flush of mixed memos (text=%q): %d %s", text, r.Code, r.Body.String())
		}
	}
	if len(agent.prompts) != 0 {
		t.Fatalf("a refused flush reached the session: %q", agent.prompts)
	}
	if r := e.do("b", "POST", "/api/memos/flush", map[string]any{"sessionName": "s1", "ids": []string{origin.ID}, "text": "my edit of it"}); r.Code != 200 {
		t.Fatalf("flush of the origin memo alone: %d %s", r.Code, r.Body.String())
	}
	if r := e.do("b", "POST", "/api/memos/flush", map[string]any{"sessionName": "s1", "ids": []string{own.ID}}); r.Code != 200 {
		t.Fatalf("flush of the own memo: %d %s", r.Code, r.Body.String())
	}
	if len(agent.prompts) != 2 {
		t.Fatalf("prompts: %q", agent.prompts)
	}
	if !strings.HasPrefix(agent.prompts[0], memoOriginLine(store.Memo{OriginItemID: id, OriginSenderKey: key("a")})) ||
		!strings.HasSuffix(agent.prompts[0], "my edit of it") {
		t.Fatalf("the origin memo's message: %q", agent.prompts[0])
	}
	if agent.prompts[1] != "my own note\n" && agent.prompts[1] != "my own note" {
		t.Fatalf("the own memo's message changed: %q", agent.prompts[1])
	}
	m := decode[[]memoDTO](t, e.do("b", "GET", "/api/memos", nil))
	for _, x := range m {
		if x.ID == origin.ID && (x.Origin == nil || x.SentAt == "") {
			t.Fatalf("after its flush the memo lost its origin or was not marked sent: %+v", x)
		}
	}
}

// TestInboxProvenanceStamp: the session create carries the provenance the CP decided — from
// the memo the launch was seeded from, or from the request — and never one the client wrote.
func TestInboxProvenanceStamp(t *testing.T) {
	e := newInboxEnv(t)
	agent := newCapturingAgent(t)
	e.workspaceFor("b", agent)
	id := decode[itemResp](t, e.sendWith("a", "request", []map[string]any{testRecipe}, "b")).Item.ID
	_ = e.do("b", "POST", "/api/inbox/items/"+id+"/claim", nil)
	memo := decode[takeInResp](t, e.do("b", "POST", "/api/inbox/items/"+id+"/take-in", nil)).Memo
	forged := map[string]any{"sender": "boss", "item": "made-up"}
	for _, c := range []struct {
		body map[string]any
		want *inboxOriginRef
	}{
		{map[string]any{"dir": "/d", "kind": "claude", "inbox_origin": forged}, nil},
		{map[string]any{"dir": "/d", "kind": "claude", "inbox_memo_id": memo.ID, "inbox_origin": forged}, &inboxOriginRef{Sender: key("a"), Item: id}},
		{map[string]any{"dir": "/d", "kind": "claude", "inbox_memo_id": "not-mine"}, nil},
	} {
		if r := e.do("b", "POST", "/api/sessions", c.body); r.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", r.Code, r.Body.String())
		}
		got := agent.creates[len(agent.creates)-1]
		if _, leaked := got["inbox_memo_id"]; leaked {
			t.Fatal("inbox_memo_id reached the Agent")
		}
		o, has := got["inbox_origin"].(map[string]any)
		if (c.want == nil) == has || (c.want != nil && (o["sender"] != c.want.Sender || o["item"] != c.want.Item)) {
			t.Fatalf("body %v reached the Agent with inbox_origin %v", c.body, got["inbox_origin"])
		}
	}
	// The request launch stamps the request itself.
	if r := e.do("b", "POST", "/api/inbox/items/"+id+"/launch", map[string]any{"dir": "/d", "kind": "claude", "inbox_origin": forged}); r.Code != http.StatusCreated {
		t.Fatalf("launch: %d %s", r.Code, r.Body.String())
	}
	o, _ := agent.creates[len(agent.creates)-1]["inbox_origin"].(map[string]any)
	if o["item"] != id || o["sender"] != key("a") {
		t.Fatalf("the request launch's provenance: %v", o)
	}
}

// TestInboxProvenanceVerifiedOnRelay: a badge the Agent reports is kept only when it names an
// item the session's owner takes part in, with that item's sender.
func TestInboxProvenanceVerifiedOnRelay(t *testing.T) {
	e := newInboxEnv(t)
	id := e.send("a", "dm", "b")
	other := e.send("a", "dm", "c")
	ws := workspaceAPI{memberAuth: memberAuth{e.mgr}}
	mv := store.MembershipView{MembershipID: e.members["b"], TenantID: e.t1.ID}
	raw := func(sender, item string) json.RawMessage {
		b, _ := json.Marshal(inboxOriginRef{Sender: sender, Item: item})
		return b
	}
	list := ws.verifyInboxOrigins(context.Background(), mv, []sessionWire{
		{Name: "ok", InboxOrigin: raw(key("a"), id)},
		{Name: "wrong-sender", InboxOrigin: raw(key("c"), id)},
		{Name: "not-mine", InboxOrigin: raw(key("a"), other)},
		{Name: "made-up", InboxOrigin: raw(key("a"), "nope")},
	})
	for _, s := range list {
		if (s.Name == "ok") != (len(s.InboxOrigin) > 0) {
			t.Errorf("%s: badge %s", s.Name, s.InboxOrigin)
		}
	}
}

// sessionFixture puts one session of a's in the share catalogue.
func (e *inboxEnv) sessionFixture() store.SharedSessionCatalog {
	e.t.Helper()
	ctx := context.Background()
	ws := store.Workspace{ID: "ws-a", TenantID: e.t1.ID, MembershipID: e.members["a"], ContainerName: "a",
		Network: "n", DataDir: "/d", AgentPort: "1", AgentToken: "tok", State: "stopped", CreatedAt: store.NowTS()}
	if err := e.st.CreateWorkspace(ctx, ws); err != nil {
		e.t.Fatal(err)
	}
	if err := e.st.ReplaceSharedSessionCatalog(ctx, ws.ID, e.members["a"], []store.SharedSessionCatalog{{ID: store.NewID(),
		WorkspaceID: ws.ID, OwnerMembershipID: e.members["a"], Name: "s1", Kind: "claude", Dir: "/repos/app", Repo: "app",
		Title: "Fix the login race", WorkingCopyID: "wc-1", CreatedAt: store.NowTS(), State: "running", LastSeen: store.NowTS()}}); err != nil {
		e.t.Fatal(err)
	}
	rows, err := e.st.ListSharedSessionCatalogByOwner(ctx, e.members["a"])
	if err != nil || len(rows) != 1 {
		e.t.Fatalf("catalog: %v %v", rows, err)
	}
	return rows[0]
}

// TestInboxSessionAttachment is decision 5: only the owner shares by attaching, with every
// recipient and the scope confirmed; a reader without access sees an opaque placeholder,
// decided on every read; a revoked share turns the card into the placeholder; withdrawing a
// message revokes nothing; a non-owner can attach a session shared with them, granting nothing.
func TestInboxSessionAttachment(t *testing.T) {
	e := newInboxEnv(t)
	cat := e.sessionFixture()
	confirm := map[string]any{"recipients": []string{key("b"), key("c")}, "scope": "session"}
	// The share confirmation has to name every recipient.
	partial := map[string]any{"type": "session", "sessionName": "s1", "share": map[string]any{"recipients": []string{key("b")}, "scope": "session"}}
	if r := e.sendWith("a", "dm", []map[string]any{partial}, "b", "c"); r.Code != http.StatusBadRequest ||
		decode[errResp](t, r).Error.Code != errCodeInboxShareUnconfirmed {
		t.Fatalf("partial confirmation: %d %s", r.Code, r.Body.String())
	}
	// Attached without a share: neither recipient has access, so both see the placeholder.
	plain := decode[itemResp](t, e.sendWith("a", "dm", []map[string]any{{"type": "session", "sessionName": "s1"}}, "b", "c")).Item.ID
	for _, who := range []string{"b", "c"} {
		body := e.do(who, "GET", "/api/inbox/items/"+plain, nil).Body.String()
		got := decode[itemResp](t, e.do(who, "GET", "/api/inbox/items/"+plain, nil)).Item.Attachments
		if len(got) != 1 || got[0].Available || got[0].Session != nil {
			t.Fatalf("%s sees an unshared session: %+v", who, got)
		}
		for _, leak := range []string{cat.ID, "Fix the login race", "wc-1", `"s1"`, "/repos/app"} {
			if strings.Contains(body, leak) {
				t.Fatalf("the placeholder for %s leaks %q: %s", who, leak, body)
			}
		}
	}
	// With the confirmed share: both see it, and a share rule exists for each.
	shared := decode[itemResp](t, e.sendWith("a", "dm", []map[string]any{{"type": "session", "sessionName": "s1", "share": confirm}}, "b", "c")).Item.ID
	for _, who := range []string{"b", "c"} {
		got := decode[itemResp](t, e.do(who, "GET", "/api/inbox/items/"+shared, nil)).Item.Attachments
		if len(got) != 1 || !got[0].Available || got[0].Session.CatalogID != cat.ID || got[0].Session.Mine {
			t.Fatalf("%s after the share: %+v", who, got)
		}
		if rows, _ := e.st.ListSessionSharesByRecipient(context.Background(), e.members[who]); len(rows) != 1 {
			t.Fatalf("%s's shares: %+v", who, rows)
		}
	}
	// Decided on every read: the earlier message now shows the session too.
	if got := decode[itemResp](t, e.do("b", "GET", "/api/inbox/items/"+plain, nil)).Item.Attachments; !got[0].Available {
		t.Fatal("the earlier card did not follow the share")
	}
	// A non-owner may attach a session shared with them, granting nothing; one not shared
	// with them is refused, and so is a share they cannot make.
	if r := e.sendWith("b", "dm", []map[string]any{{"type": "session", "catalogId": cat.ID}}, "c"); r.Code != http.StatusCreated {
		t.Fatalf("b attaches a session shared with b: %d %s", r.Code, r.Body.String())
	}
	if r := e.sendWith("b", "dm", []map[string]any{{"type": "session", "catalogId": cat.ID,
		"share": map[string]any{"recipients": []string{key("c")}, "scope": "session"}}}, "c"); r.Code != http.StatusForbidden {
		t.Fatalf("b shares a's session: %d %s", r.Code, r.Body.String())
	}
	// Withdrawing a message revokes no share.
	_ = e.do("a", "POST", "/api/inbox/items/"+shared+"/withdraw", nil)
	if rows, _ := e.st.ListSessionSharesByRecipient(context.Background(), e.members["b"]); len(rows) != 1 {
		t.Fatal("deleting the message took the share with it")
	}
	// Revoking the share turns every card into the placeholder.
	rows, _ := e.st.ListSessionSharesByRecipient(context.Background(), e.members["c"])
	if err := e.st.DeleteSessionShare(context.Background(), rows[0].ID, e.members["a"]); err != nil {
		t.Fatal(err)
	}
	if got := decode[itemResp](t, e.do("c", "GET", "/api/inbox/items/"+shared, nil)).Item.Attachments; got[0].Available {
		t.Fatal("a revoked share still shows the session")
	}
	if r := e.sendWith("c", "dm", []map[string]any{{"type": "session", "catalogId": cat.ID}}, "b"); r.Code != http.StatusNotFound {
		t.Fatalf("c attaches a session no longer shared with c: %d %s", r.Code, r.Body.String())
	}
	// The owner sees their own.
	if got := decode[itemResp](t, e.do("a", "GET", "/api/inbox/items/"+plain, nil)).Item.Attachments; !got[0].Available || !got[0].Session.Mine {
		t.Fatalf("the owner's view: %+v", got)
	}
}

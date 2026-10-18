package mcpsrv

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k-k1/agent-fleet/control-plane/internal/runtime"
	"github.com/k-k1/agent-fleet/control-plane/internal/store"
)

// memRT is a runtime that only knows its state.
type memRT struct {
	runtime.Runtime
	state string
}

func (r memRT) State(context.Context) string { return r.state }

// memCP wires the three seams a memory tool call crosses: resolution, the Agent and the scope ladder.
type memCP struct {
	testCP
	state     string
	agentErr  error
	gotMethod string
	gotPath   string
	gotBody   string
	calls     int
}

func (c *memCP) ResolveByMembership(context.Context, string, string) (*Resolved, *APIError) {
	return &Resolved{RT: memRT{state: c.state}, MV: store.MembershipView{MembershipID: "m1", TenantID: "t1"}}, nil
}

func (c *memCP) AgentText(_ context.Context, _ runtime.Runtime, method, path string, body []byte) (string, error) {
	c.calls++
	c.gotMethod, c.gotPath, c.gotBody = method, path, string(body)
	return `{"ok":true}`, c.agentErr
}

func memCall(t *testing.T, cp *memCP, scope, tool string, args map[string]any) (string, bool) {
	t.Helper()
	a := New(cp)
	params, _ := json.Marshal(map[string]any{"name": tool, "arguments": args})
	ctx := withMCPCall(context.Background(), mcpCallInfo{patID: "pat1", client: "claude-code/9.9"})
	resp := a.mcpToolCall(ctx, &mcpPrincipal{patID: "pat1", identityID: "i1", membershipID: "m1", scope: scope},
		rpcRequest{ID: json.RawMessage(`1`), Params: params})
	if resp.Error != nil {
		return resp.Error.Message, true
	}
	r := resp.Result.(map[string]any)
	text := r["content"].([]map[string]any)[0]["text"].(string)
	isErr, _ := r["isError"].(bool)
	return text, isErr
}

func memStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "cp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return st
}

// Reads are listed for a read PAT, writes only from the write tier, and a read PAT cannot call a
// write tool by name either.
func TestMemoryToolsFollowTheScopeTier(t *testing.T) {
	cp := &memCP{testCP: testCP{store: memStore(t)}, state: "running"}
	list := func(scope string) map[string]bool {
		out := map[string]bool{}
		for _, tl := range New(cp).toolsFor(context.Background(), &mcpPrincipal{identityID: "nobody", scope: scope}) {
			out[tl.name] = true
		}
		return out
	}
	r, w := list("read"), list("write")
	for _, n := range []string{"memory_projects", "memory_index", "memory_search", "memory_read"} {
		if !r[n] || !w[n] {
			t.Errorf("%s must be listed at read and write", n)
		}
	}
	for _, n := range []string{"memory_save", "memory_forget"} {
		if r[n] || !w[n] {
			t.Errorf("%s: read=%v write=%v", n, r[n], w[n])
		}
	}
	text, isErr := memCall(t, cp, "read", "memory_save", map[string]any{"name": "x"})
	if !strings.Contains(text, "unknown or unauthorized tool") || cp.calls != 0 {
		t.Fatalf("read PAT reached memory_save: %q isErr=%v calls=%d", text, isErr, cp.calls)
	}
}

// Arguments reach the Agent's external routes unchanged, with the client name and an explicit project.
func TestMemoryToolsProxyToTheAgent(t *testing.T) {
	cp := &memCP{testCP: testCP{store: memStore(t)}, state: "running"}
	if _, isErr := memCall(t, cp, "read", "memory_read", map[string]any{"name": "n1", "scope": "project", "project": "demo-0123456789ab"}); isErr {
		t.Fatal("read failed")
	}
	if cp.gotMethod != "GET" || cp.gotPath != "/agents/memory/external/read?name=n1&project=demo-0123456789ab&scope=project" {
		t.Fatalf("read proxied as %s %s", cp.gotMethod, cp.gotPath)
	}
	if _, isErr := memCall(t, cp, "write", "memory_save", map[string]any{
		"name": "n1", "description": "d", "type": "project", "body": "b", "project": "demo-0123456789ab", "scope": "project"}); isErr {
		t.Fatal("save failed")
	}
	var got map[string]any
	if cp.gotMethod != "POST" || cp.gotPath != "/agents/memory/external/entries" || json.Unmarshal([]byte(cp.gotBody), &got) != nil ||
		got["project"] != "demo-0123456789ab" || got["client"] != "claude-code/9.9" || got["name"] != "n1" {
		t.Fatalf("save proxied as %s %s %s", cp.gotMethod, cp.gotPath, cp.gotBody)
	}
}

// A stopped workspace is answered by the CP, with no call to the Agent.
func TestMemoryToolsSayWhenTheWorkspaceIsStopped(t *testing.T) {
	cp := &memCP{testCP: testCP{store: memStore(t)}, state: "stopped"}
	text, isErr := memCall(t, cp, "read", "memory_index", nil)
	if !isErr || !strings.Contains(text, "not running") || !strings.Contains(text, "start the workspace") || cp.calls != 0 {
		t.Fatalf("stopped: %q isErr=%v calls=%d", text, isErr, cp.calls)
	}
}

// The Agent's refusal is relayed as its own message, not as "agent 403: {json}".
func TestMemoryToolsRelayTheAgentsRefusal(t *testing.T) {
	cp := &memCP{testCP: testCP{store: memStore(t)}, state: "running",
		agentErr: errors.New(`agent 403: {"error":{"code":"memory_external_disabled","message":"turn it on in Settings > Agent memory"}}`)}
	text, isErr := memCall(t, cp, "read", "memory_projects", nil)
	if !isErr || text != "memory_external_disabled: turn it on in Settings > Agent memory" {
		t.Fatalf("refusal: %q", text)
	}
}

// External writes are audited with an id-shaped target only; the memory's name never reaches the
// ledger, and a failed write is not recorded.
func TestMemoryWritesAreAudited(t *testing.T) {
	st := memStore(t)
	cp := &memCP{testCP: testCP{store: st}, state: "running"}
	secretName := "ghp-looking-name"
	memCall(t, cp, "write", "memory_save", map[string]any{"name": secretName, "description": "d", "type": "user", "body": "b", "scope": "user"})
	memCall(t, cp, "write", "memory_forget", map[string]any{"name": secretName, "revision": 1, "scope": "project", "project": "demo-0123456789ab"})
	memCall(t, cp, "write", "memory_save", map[string]any{"name": "y", "description": "d", "type": "user", "body": "b", "project": "Not An ID/../x"})
	cp.agentErr = errors.New("agent 409: {}")
	memCall(t, cp, "write", "memory_save", map[string]any{"name": "z", "description": "d", "type": "user", "body": "b"})

	rows, err := st.ListAuditByTenant(context.Background(), "t1", 10)
	if err != nil || len(rows) != 3 {
		t.Fatalf("audit rows: %d %v", len(rows), err)
	}
	byAction := map[string][]string{}
	for _, r := range rows {
		if r.ActorKind != "mcp" || r.ActorID != "pat1" || strings.Contains(r.Target+r.Detail, secretName) {
			t.Errorf("row %+v", r)
		}
		byAction[r.Action] = append(byAction[r.Action], r.Target)
	}
	if len(byAction["memory.entry.save"]) != 2 || len(byAction["memory.entry.forget"]) != 1 || byAction["memory.entry.forget"][0] != "demo-0123456789ab" {
		t.Errorf("actions %v", byAction)
	}
	for _, tgt := range byAction["memory.entry.save"] {
		if tgt != "" {
			t.Errorf("a non-id project reached the ledger: %q", tgt)
		}
	}
}

// The client's name comes from the stateless `_meta`, else the User-Agent.
func TestMCPClientName(t *testing.T) {
	r := httptest.NewRequest("POST", "/mcp", nil)
	r.Header.Set("User-Agent", "ua/1")
	if got := mcpClientName(r, paramsMeta{}); got != "ua/1" {
		t.Errorf("fallback: %q", got)
	}
	p := paramsMeta{Meta: map[string]json.RawMessage{metaClientInfo: json.RawMessage(`{"name":"claude-code","version":"2.0"}`)}}
	if got := mcpClientName(r, p); got != "claude-code/2.0" {
		t.Errorf("meta: %q", got)
	}
}

// With scope omitted the Agent works in the project when one is named, so the audit says project;
// an explicit user scope stays user.
func TestMemoryAuditUsesTheAgentsDefaultScope(t *testing.T) {
	st := memStore(t)
	cp := &memCP{testCP: testCP{store: st}, state: "running"}
	base := map[string]any{"name": "n", "description": "d", "type": "user", "body": "b"}
	with := func(extra map[string]any) map[string]any {
		m := map[string]any{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	memCall(t, cp, "write", "memory_save", with(map[string]any{"project": "demo-0123456789ab"}))
	memCall(t, cp, "write", "memory_save", with(map[string]any{"project": "demo-0123456789ab", "scope": "user"}))
	memCall(t, cp, "write", "memory_save", with(nil))
	memCall(t, cp, "write", "memory_forget", map[string]any{"name": "n", "revision": 1, "project": "demo-0123456789ab"})
	rows, _ := st.ListAuditByTenant(context.Background(), "t1", 10)
	n := map[string]int{}
	for _, r := range rows {
		n[r.Action+" "+r.Detail]++
	}
	if n["memory.entry.save external scope=project"] != 1 || n["memory.entry.save external scope=user"] != 2 ||
		n["memory.entry.forget external scope=project"] != 1 {
		t.Fatalf("audit details: %v", n)
	}
}

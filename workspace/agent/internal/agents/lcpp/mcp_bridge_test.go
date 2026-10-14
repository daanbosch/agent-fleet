package lcpp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/harness"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/mcpc"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/transcript"
)

// bridgeRun drives one prompt through a session whose model plays calls (one harness.Turn per
// engine round trip, plus a closing text turn) and returns the tool results in order, the first
// request's tool list, and the session's records.
func bridgeRun(t *testing.T, name string, skipPermissions bool, calls ...harness.ToolCall) (results []string, firstTools []harness.ToolDef, h agents.ThreadHandle, m session.Meta) {
	t.Helper()
	var turns []harness.Turn
	for _, c := range calls {
		turns = append(turns, harness.Turn{ToolCalls: []harness.ToolCall{c}, Finish: harness.FinishToolCalls})
	}
	turns = append(turns, harness.Turn{Content: "done"})
	client := &scriptedClient{
		turns: turns,
		onSend: func(_ []harness.Message, tools []harness.ToolDef) {
			if firstTools == nil {
				firstTools = tools
			}
		},
	}
	wireEngine(t, client)
	m = testMeta(t, name)
	m.SkipPermissions = &skipPermissions
	h, err := NewDriver().Resume(m)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if err := h.Send(agents.TurnInput{Prompt: "go"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if skipPermissions {
		waitState(t, h, agents.TurnCompleted, agents.TurnFailed)
		recs, _, err := Open(sidFor(m)).Records()
		if err != nil {
			t.Fatalf("Records: %v", err)
		}
		for _, r := range recs {
			if r.Kind == KindToolResult {
				results = append(results, r.Content)
			}
		}
	}
	return results, firstTools, h, m
}

func call(id, name string, args any) harness.ToolCall {
	b, _ := json.Marshal(args)
	return harness.ToolCall{ID: id, Name: name, Arguments: string(b)}
}

func toolNames(defs []harness.ToolDef) string {
	var n []string
	for _, d := range defs {
		n = append(n, d.Name)
	}
	return strings.Join(n, ",")
}

func TestMCPDeferredHidesSchemasBehindBridge(t *testing.T) {
	mcpTestHome(t)
	t.Setenv(mcpToolsEnv, "deferred")
	registerFakeMCPServer(t, "fx", "")
	_, tools, _, _ := bridgeRun(t, "sess-bridge-hidden", true)
	names := toolNames(tools)
	for _, want := range []string{"tool_search", "tool_describe", "tool_call", "bash", "read"} {
		if !strings.Contains(names, want) {
			t.Fatalf("tools = %s, missing %s (the bridge and the builtins must be visible)", names, want)
		}
	}
	if strings.Contains(names, "mcp__") {
		t.Fatalf("tools = %s: an MCP schema was sent although deferral is on", names)
	}
}

func TestMCPDirectModeKeepsSchemasInRequest(t *testing.T) {
	mcpTestHome(t)
	t.Setenv(mcpToolsEnv, "direct")
	registerFakeMCPServer(t, "fx", "")
	_, tools, _, _ := bridgeRun(t, "sess-bridge-direct", true)
	names := toolNames(tools)
	if !strings.Contains(names, "mcp__fx__echo") || strings.Contains(names, "tool_search") {
		t.Fatalf("tools = %s, want the MCP tools direct and no bridge", names)
	}
}

func TestMCPAutoModeDefersOnlyPastThreshold(t *testing.T) {
	mcpTestHome(t)
	registerFakeMCPServer(t, "fx", "")
	_, tools, _, _ := bridgeRun(t, "sess-bridge-auto-small", true)
	if names := toolNames(tools); !strings.Contains(names, "mcp__fx__echo") || strings.Contains(names, "tool_search") {
		t.Fatalf("three tiny tools: tools = %s, want them direct (below the threshold)", names)
	}

	old := mcpDeferMinBytes
	mcpDeferMinBytes = 50
	t.Cleanup(func() { mcpDeferMinBytes = old })
	_, tools, _, _ = bridgeRun(t, "sess-bridge-auto-big", true)
	if names := toolNames(tools); strings.Contains(names, "mcp__") || !strings.Contains(names, "tool_call") {
		t.Fatalf("past the threshold: tools = %s, want the bridge", names)
	}
}

func TestMCPBridgeSearchDescribeCall(t *testing.T) {
	mcpTestHome(t)
	t.Setenv(mcpToolsEnv, "deferred")
	registerFakeMCPServer(t, "fx", "")
	echo := mcpc.PrefixToolName("fx", "echo")
	results, _, _, m := bridgeRun(t, "sess-bridge-flow", true,
		call("c1", "tool_search", map[string]any{"query": "echo"}),
		call("c2", "tool_describe", map[string]any{"name": echo}),
		call("c3", "tool_call", map[string]any{"name": echo, "arguments": map[string]any{"msg": "via-bridge"}}),
	)
	if len(results) != 3 {
		t.Fatalf("results = %q, want 3", results)
	}
	if !strings.Contains(results[0], echo+" — echoes msg back") || strings.Contains(results[0], "touch") {
		t.Fatalf("search = %q, want only the echo tool with its one-line description", results[0])
	}
	if !strings.Contains(results[1], `"input_schema"`) || !strings.Contains(results[1], `"msg"`) {
		t.Fatalf("describe = %q, want the full schema", results[1])
	}
	if results[2] != "via-bridge" {
		t.Fatalf("call = %q, want the real server's answer", results[2])
	}

	// The transcript shows the tool that was reached, not the bridge.
	turns, err := Open(sidFor(m)).TranscriptFor(m.Model)
	if err != nil {
		t.Fatalf("TranscriptFor: %v", err)
	}
	var parts []transcript.Part
	for _, turn := range turns {
		for _, p := range turn.Parts {
			if p.Kind == "tool" {
				parts = append(parts, p)
			}
		}
	}
	if len(parts) != 3 || parts[2].Tool != echo || !strings.Contains(parts[2].Info, "via-bridge") || parts[2].Output != "via-bridge" {
		t.Fatalf("tool parts = %+v, want the third rendered as %s with its arguments", parts, echo)
	}
}

func TestMCPBridgeCallRefusals(t *testing.T) {
	mcpTestHome(t)
	t.Setenv(mcpToolsEnv, "deferred")
	registerFakeMCPServer(t, "fx", "")
	echo := mcpc.PrefixToolName("fx", "echo")
	touch := mcpc.PrefixToolName("fx", "touch")
	marker := filepath.Join(t.TempDir(), "marker")
	results, _, _, _ := bridgeRun(t, "sess-bridge-refusals", true,
		// not described yet
		call("c1", "tool_call", map[string]any{"name": touch, "arguments": map[string]any{"path": marker}}),
		// unknown tool
		call("c2", "tool_call", map[string]any{"name": "mcp__fx__nope"}),
		call("c3", "tool_describe", map[string]any{"name": "mcp__fx__nope"}),
		// schema violation (msg is required)
		call("c4", "tool_describe", map[string]any{"name": echo}),
		call("c5", "tool_call", map[string]any{"name": echo, "arguments": map[string]any{}}),
		call("c6", "tool_call", map[string]any{"name": echo, "arguments": map[string]any{"msg": 5}}),
		// malformed body
		harness.ToolCall{ID: "c7", Name: "tool_call", Arguments: `{"name":`},
		// arguments must be an object, whatever the schema says
		call("c8", "tool_call", map[string]any{"name": echo, "arguments": []any{}}),
		call("c9", "tool_call", map[string]any{"name": echo, "arguments": "x"}),
	)
	want := []string{"has not been described yet", "unknown tool", "unknown tool", `"input_schema"`, "do not match its schema", "do not match its schema", "invalid arguments", "must be a JSON object", "must be a JSON object"}
	if len(results) != len(want) {
		t.Fatalf("results = %q, want %d", results, len(want))
	}
	for i, w := range want {
		if !strings.Contains(results[i], w) {
			t.Errorf("result %d = %q, want it to contain %q", i, results[i], w)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a call refused for not being described still reached the server")
	}
}

// The approval gate must treat tool_call exactly like a direct call: ask, name the real tool,
// and on a refusal never reach the server.
func TestMCPBridgeCallRequiresApprovalAndNamesTheRealTool(t *testing.T) {
	mcpTestHome(t)
	t.Setenv(mcpToolsEnv, "deferred")
	registerFakeMCPServer(t, "fx", "")
	touch := mcpc.PrefixToolName("fx", "touch")
	marker := filepath.Join(t.TempDir(), "should-not-exist")
	_, _, h, _ := bridgeRun(t, "sess-bridge-approval", false,
		call("c1", "tool_describe", map[string]any{"name": touch}),
		call("c2", "tool_call", map[string]any{"name": touch, "arguments": map[string]any{"path": marker}}),
	)
	snap := waitState(t, h, agents.TurnWaitingInteraction)
	if snap.Interaction == nil || !strings.Contains(snap.Interaction.Prompt, "run "+touch) {
		t.Fatalf("Interaction = %+v, want an approval prompt naming %s", snap.Interaction, touch)
	}
	if err := h.Respond(agents.InteractionReply{ID: snap.Interaction.ID, Decision: agents.DecisionDeny}); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	waitState(t, h, agents.TurnCompleted, agents.TurnFailed)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a denied tool_call still reached the real server")
	}
}

// Measured saving with a server whose tool list is the size of af's: the request carries the
// three bridge schemas instead of every MCP schema.
func TestMCPDeferredShrinksRequest(t *testing.T) {
	var defs []harness.ToolDef
	for i := 0; i < 70; i++ {
		defs = append(defs, harness.ToolDef{
			Name:        mcpc.PrefixToolName("af", "tool_number_"+string(rune('a'+i%26))+string(rune('a'+i/26))),
			Description: strings.Repeat("Does something useful with the session and returns a result. ", 4),
			Parameters:  json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"identifier"},"flag":{"type":"boolean"},"limit":{"type":"integer"}},"required":["id"]}`),
		})
	}
	size := func(ds []harness.ToolDef) int { b, _ := json.Marshal(ds); return len(b) }
	h := &threadHandle{}
	var bridged []harness.ToolDef
	for _, tl := range h.bridgeTools(defs) {
		bridged = append(bridged, tl.Def)
	}
	direct, deferred := size(defs), size(bridged)
	t.Logf("70 MCP tools: %d bytes direct, %d bytes behind the bridge", direct, deferred)
	if !mcpDeferred(defs) {
		t.Fatal("70 tools of this size must pass the auto threshold")
	}
	if deferred*10 > direct {
		t.Fatalf("bridge costs %d bytes against %d direct: expected at least a 10x saving", deferred, direct)
	}
}

// A definition that changed after tool_describe (tools/list_changed, a reconnect) must be
// described again; an unchanged one stays valid.
func TestDescribedSetDetectsChangedDefinition(t *testing.T) {
	var s describedSet
	d := harness.ToolDef{Name: "mcp__fx__echo", Description: "echo", Parameters: json.RawMessage(`{"type":"object"}`)}
	if got := s.state(d); got != "never" {
		t.Fatalf("before describe = %q, want never", got)
	}
	s.add(d)
	if got := s.state(d); got != "" {
		t.Fatalf("after describe = %q, want current", got)
	}
	changed := d
	changed.Parameters = json.RawMessage(`{"type":"object","required":["msg"]}`)
	if got := s.state(changed); got != "stale" {
		t.Fatalf("new schema = %q, want stale", got)
	}
	changed = d
	changed.Description = "echo, now louder"
	if got := s.state(changed); got != "stale" {
		t.Fatalf("new description = %q, want stale", got)
	}
}

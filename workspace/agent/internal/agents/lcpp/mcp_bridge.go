package lcpp

// mcp_bridge.go keeps MCP tool schemas out of the request until the model asks for them
// (ADR 0093, the 2026-10-14 note). A self-hosted model runs with a small window, and the
// builtin af server alone advertises dozens of tools, so sending every schema on every turn
// is a fixed cost paid before the model has done anything. Past a size threshold the model is
// offered three small tools instead — tool_search, tool_describe, tool_call — and reaches the
// real tools through them. lcpp's own builtins (read, bash, ...) are never deferred.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/harness"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/mcpc"
)

const (
	toolSearchName   = "tool_search"
	toolDescribeName = "tool_describe"
	toolCallName     = "tool_call"
)

// mcpToolsEnv selects how MCP tools reach the model: "auto" (default) defers them once their
// schemas pass mcpDeferMinBytes, "deferred" always defers, "direct" never does (the pre-bridge
// behaviour). Read on every turn, like the MCP server set, so a change applies from the next turn.
const mcpToolsEnv = "AF_LCPP_MCP_TOOLS"

// mcpDeferMinBytes is the auto-mode threshold: the summed size of every MCP tool's name,
// description and schema. About 1.5k tokens at ~4 bytes each — a handful of small tools stays
// direct (the bridge's own three schemas and the extra round trip would cost more than they
// save), while one 70-tool server is far past it. A var so tests can move it.
var mcpDeferMinBytes = 6000

// mcpSearchMaxResults caps one tool_search reply.
const mcpSearchMaxResults = 25

// mcpDeferred reports whether defs should be hidden behind the bridge this turn.
func mcpDeferred(defs []harness.ToolDef) bool {
	if len(defs) == 0 {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv(mcpToolsEnv))) {
	case "direct":
		return false
	case "deferred":
		return true
	}
	size := 0
	for _, d := range defs {
		size += len(d.Name) + len(d.Description) + len(d.Parameters)
	}
	return size > mcpDeferMinBytes
}

// describedSet remembers, per MCP tool, a fingerprint of the definition (description and
// schema) the model was shown by tool_describe. tool_call refuses a tool never described, and a
// tool whose definition has changed since (server edit, reconnect, tools/list_changed): a call
// written against a stale schema is a guess. Lives on the session handle, so it spans turns; a
// restarted session starts empty and the model is told to describe again.
type describedSet struct {
	mu sync.Mutex
	m  map[string]string
}

func defFingerprint(d harness.ToolDef) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d:%s\n%d:%s", len(d.Description), d.Description, len(d.Parameters), d.Parameters)
	return hex.EncodeToString(h.Sum(nil))
}

func (s *describedSet) add(d harness.ToolDef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]string{}
	}
	s.m[d.Name] = defFingerprint(d)
}

// state is "" (current), "never" or "stale".
func (s *describedSet) state(d harness.ToolDef) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	fp, ok := s.m[d.Name]
	switch {
	case !ok:
		return "never"
	case fp != defFingerprint(d):
		return "stale"
	}
	return ""
}

// bridgeTools returns the three bridge tools over defs (this turn's MCP tool snapshot).
func (h *threadHandle) bridgeTools(defs []harness.ToolDef) []harness.Tool {
	byName := make(map[string]harness.ToolDef, len(defs))
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		byName[d.Name] = d
		names = append(names, d.Name)
	}
	sort.Strings(names)
	b := &mcpBridge{mgr: h.mcpMgr, byName: byName, names: names, described: &h.mcpDescribed}
	return []harness.Tool{
		{Def: toolSearchToolDef, Run: b.runSearch},
		{Def: toolDescribeToolDef, Run: b.runDescribe},
		{Def: toolCallToolDef, Target: b.target, Run: b.runCall},
	}
}

type mcpBridge struct {
	mgr       *mcpc.Manager
	byName    map[string]harness.ToolDef
	names     []string // sorted
	described *describedSet
}

var toolSearchToolDef = harness.ToolDef{
	Name: toolSearchName,
	Description: "Find external (MCP) tools by keyword. Their schemas are not loaded; this returns tool names with " +
		"a one-line description. Then call tool_describe for the schema and tool_call to run one.",
	Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"Keywords; every word must match the tool name or description. Empty lists tools."}}}`),
}

var toolDescribeToolDef = harness.ToolDef{
	Name:        toolDescribeName,
	Description: "Get the full description and JSON input schema of one external (MCP) tool found with tool_search. Required before tool_call.",
	Parameters:  json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"Exact tool name from tool_search."}},"required":["name"]}`),
}

var toolCallToolDef = harness.ToolDef{
	Name:        toolCallName,
	Description: "Run an external (MCP) tool you have read with tool_describe. Arguments must match its schema.",
	Parameters:  json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"Exact tool name."},"arguments":{"type":"object","description":"Arguments for the tool."}},"required":["name"]}`),
}

type bridgeCallArgs struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// current is the tool's definition as the Manager holds it right now, which can be newer than
// the turn-start snapshot in byName (tools/list_changed arrives mid-turn). ok=false when the
// tool is gone.
func (b *mcpBridge) current(name string) (harness.ToolDef, bool) {
	for _, d := range b.mgr.ToolDefs() {
		if d.Name == name {
			return d, true
		}
	}
	return harness.ToolDef{}, false
}

func (b *mcpBridge) runSearch(_ context.Context, _ *harness.Runtime, argsJSON string) (string, error) {
	var a struct {
		Query string `json:"query"`
	}
	if err := decodeBridgeArgs(argsJSON, &a); err != nil {
		return "", err
	}
	terms := strings.Fields(strings.ToLower(a.Query))
	var sb strings.Builder
	matched := 0
	for _, name := range b.names {
		d := b.byName[name]
		hay := strings.ToLower(name + " " + d.Description)
		ok := true
		for _, t := range terms {
			if !strings.Contains(hay, t) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		matched++
		if matched <= mcpSearchMaxResults {
			fmt.Fprintf(&sb, "%s — %s\n", name, oneLine(d.Description))
		}
	}
	switch {
	case matched == 0:
		return fmt.Sprintf("no tool matches %q (%d tools available; try fewer or different words, or an empty query to list)", a.Query, len(b.names)), nil
	case matched > mcpSearchMaxResults:
		fmt.Fprintf(&sb, "… %d more; narrow the query\n", matched-mcpSearchMaxResults)
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

func (b *mcpBridge) runDescribe(_ context.Context, _ *harness.Runtime, argsJSON string) (string, error) {
	var a struct {
		Name string `json:"name"`
	}
	if err := decodeBridgeArgs(argsJSON, &a); err != nil {
		return "", err
	}
	d, ok := b.current(a.Name)
	if !ok {
		return "", fmt.Errorf("unknown tool %q: use tool_search to find the exact name", a.Name)
	}
	b.described.add(d)
	schema := d.Parameters
	if len(schema) == 0 {
		schema = json.RawMessage(`{"type":"object"}`)
	}
	out, err := json.Marshal(struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	}{d.Name, d.Description, schema})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// target tells the loop which MCP tool a tool_call reaches, so plan mode and the approval gate
// judge that tool — with its real name in the prompt — rather than the bridge. ok=false
// (unknown tool) leaves the bridge as the judged tool; runCall then refuses the same call.
func (b *mcpBridge) target(argsJSON string) (harness.Tool, bool) {
	var a bridgeCallArgs
	if err := decodeBridgeArgs(argsJSON, &a); err != nil {
		return harness.Tool{}, false
	}
	d, ok := b.byName[a.Name]
	if !ok {
		return harness.Tool{}, false
	}
	return harness.Tool{Def: d, Mutates: mcpToolMutates(d)}, true
}

func (b *mcpBridge) runCall(ctx context.Context, _ *harness.Runtime, argsJSON string) (string, error) {
	var a bridgeCallArgs
	if err := decodeBridgeArgs(argsJSON, &a); err != nil {
		return "", err
	}
	def, ok := b.current(a.Name)
	if !ok {
		return "", fmt.Errorf("unknown tool %q: use tool_search to find the exact name", a.Name)
	}
	switch b.described.state(def) {
	case "never":
		return "", fmt.Errorf("%s has not been described yet: call tool_describe with this name first, then tool_call", def.Name)
	case "stale":
		return "", fmt.Errorf("%s changed since you described it: call tool_describe again to read the current schema, then tool_call", def.Name)
	}
	args := bytes.TrimSpace(a.Arguments)
	if len(args) == 0 || bytes.Equal(args, []byte("null")) {
		args = []byte("{}")
	}
	if args[0] != '{' {
		return "", fmt.Errorf("arguments for %s must be a JSON object", def.Name)
	}
	if err := validateMCPArgs(def.Parameters, args); err != nil {
		return "", fmt.Errorf("arguments for %s do not match its schema: %w", def.Name, err)
	}
	return runMCPTool(ctx, b.mgr, def, string(args))
}

// validateMCPArgs checks args against the tool's own advertised schema, so a bad call is
// reported to the model before it reaches the server. A schema this validator cannot compile
// is not the model's fault: it is skipped and the server remains the judge.
func validateMCPArgs(schema json.RawMessage, args []byte) error {
	if len(schema) == 0 {
		return nil
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
	if err != nil {
		return nil
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	const res = "urn:agent-fleet:lcpp-mcp-tool"
	if err := c.AddResource(res, doc); err != nil {
		return nil
	}
	sch, err := c.Compile(res)
	if err != nil {
		return nil
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(args))
	if err != nil {
		return fmt.Errorf("arguments are not valid JSON: %w", err)
	}
	if err := sch.Validate(inst); err != nil {
		return err
	}
	return nil
}

func decodeBridgeArgs(argsJSON string, v any) error {
	s := strings.TrimSpace(argsJSON)
	if s == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(s), v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// oneLine is a description's first line, clipped to a length a search listing can carry.
func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const max = 120
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}

// bridgeCallTarget unpacks a tool_call record into the real tool name and its arguments, for
// transcript rendering. ok=false for any other call, or a tool_call whose body is unreadable
// (it then renders as itself).
func bridgeCallTarget(tc harness.ToolCall) (name, args string, ok bool) {
	if tc.Name != toolCallName {
		return "", "", false
	}
	var a bridgeCallArgs
	if json.Unmarshal([]byte(tc.Arguments), &a) != nil || a.Name == "" {
		return "", "", false
	}
	return a.Name, string(a.Arguments), true
}

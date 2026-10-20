// fastvm-mcp — stdio MCP server exposing the fastvm sandbox lifecycle
// through the official Go SDK. One process serves one session; it is
// transport-only, stateless between tool calls, and adds leak-guard defaults
// (NamePrefix-scoped VM names) so a crashed consumer can never orphan VMs
// outside the agpoc-* sweep scope.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/team-telnyx/fast-vm-spinner/sdk/fastvm"
)

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`
}

func main() {
	api := os.Getenv("FASTVM_API")
	if api == "" {
		api = "http://10.242.128.198:8090"
	}
	user := os.Getenv("FASTVM_DEFAULT_USER")
	if user == "" {
		user = "dav"
	}
	client := fastvm.New(api, user)

	tools := []toolDef{
		{
			Name:        "launch",
			Description: "Launch a fastvm warm clone (Firecracker microVM restored from a warm template; boots in ~0.5 s). Returns the VM record. Untrusted-workload sandbox: guest starts from the template's snapshot state.",
			InputSchema: props(map[string]any{
				"template":    prop("string", "warm template name"),
				"name":        prop("string", "name for the VM; default agpoc-<timestamp> (leak-guard scope)"),
				"ttl_minutes": prop("integer", "forward-compat: server-side TTL sweeper minutes (requires feat/agent-substrate server)"),
				"egress":      prop("array", "forward-compat: per-VM egress allowlist of hosts/CIDRs (requires feat/agent-substrate server)"),
				"ssh_key":     prop("string", "SSH public key to authorize in-guest (async on warm clones)"),
			}),
			Annotations: map[string]any{"readOnlyHint": false, "openWorldHint": false},
		},
		{
			Name:        "exec",
			Description: "Execute a shell command inside a fastvm VM's guest and return combined stdout/stderr plus exit code.",
			InputSchema: props(map[string]any{
				"vm":              prop("string", "VM name or ID"),
				"command":         prop("string", "shell command to run in the guest"),
				"timeout_seconds": prop("integer", "command timeout (default 120 s)"),
			}, "vm", "command"),
			Annotations: map[string]any{"readOnlyHint": false, "openWorldHint": false},
		},
		{
			Name:        "pause",
			Description: "Pause a fastvm VM: guest CPUs stop, memory stays resident, IP and filesystem persist. Freezes whole-VM state.",
			InputSchema: props(map[string]any{"vm": prop("string", "VM name or ID")}, "vm"),
			Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": false},
		},
		{
			Name:        "resume",
			Description: "Resume a paused fastvm VM.",
			InputSchema: props(map[string]any{"vm": prop("string", "VM name or ID")}, "vm"),
			Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": false},
		},
		{
			Name:        "destroy",
			Description: "Destroy a fastvm VM (irreversible: guest disk and RAM are deleted).",
			InputSchema: props(map[string]any{"vm": prop("string", "VM name or ID")}, "vm"),
			Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": true},
		},
		{
			Name:        "list",
			Description: "List the current user's fastvm VMs (name, id, ip, paused, expiry).",
			InputSchema: props(map[string]any{}, nil...),
			Annotations: map[string]any{"readOnlyHint": true, "openWorldHint": false},
		},
		{
			Name:        "get",
			Description: "Resolve a fastvm VM by name or ID and return its record.",
			InputSchema: props(map[string]any{"vm": prop("string", "VM name or ID")}, "vm"),
			Annotations: map[string]any{"readOnlyHint": true, "openWorldHint": false},
		},
		// ---- Wayland GUI tool group (sway headless stack: sway + foot + grim) ----
		{
			Name:        "way_screenshot",
			Description: "Capture a native (grim) screenshot of a Wayland GUI VM's desktop as a PNG image. Requires a VM launched from a way-capable template (e.g. warm-vnc-way).",
			InputSchema: props(map[string]any{"vm": prop("string", "VM name or ID with a running sway desktop")}, "vm"),
			Annotations: map[string]any{"readOnlyHint": true},
		},
		{
			Name:        "way_windows",
			Description: "List windows (app_id, title, focused) of a Wayland GUI VM from the sway tree.",
			InputSchema: props(map[string]any{"vm": prop("string", "VM name or ID")}, "vm"),
			Annotations: map[string]any{"readOnlyHint": true},
		},
		{
			Name:        "way_exec",
			Description: "Launch a GUI application on the desktop of a Wayland GUI VM via swaymsg exec (e.g. 'foot -T notes', 'firefox').",
			InputSchema: props(map[string]any{
				"vm":  prop("string", "VM name or ID"),
				"cmd": prop("string", "command to launch"),
			}, "vm", "cmd"),
			Annotations: map[string]any{"readOnlyHint": false, "openWorldHint": false},
		},
		{
			Name:        "way_focus",
			Description: "Focus a window by app_id in a Wayland GUI VM (swaymsg [app_id=\"x\"] focus).",
			InputSchema: props(map[string]any{
				"vm":     prop("string", "VM name or ID"),
				"app_id": prop("string", "application id to focus"),
			}, "vm", "app_id"),
			Annotations: map[string]any{"readOnlyHint": false},
		},
		{
			Name:        "way_close",
			Description: "Close (kill) windows matching an app_id in a Wayland GUI VM.",
			InputSchema: props(map[string]any{
				"vm":     prop("string", "VM name or ID"),
				"app_id": prop("string", "application id to close"),
			}, "vm", "app_id"),
			Annotations: map[string]any{"destructiveHint": true},
		},
		{
			Name:        "way_layout",
			Description: "Set the container layout of a Wayland GUI VM: tabbed | stacking | splith | splitv.",
			InputSchema: props(map[string]any{
				"vm":   prop("string", "VM name or ID"),
				"mode": prop("string", "layout mode: tabbed | stacking | splith | splitv"),
			}, "vm", "mode"),
			Annotations: map[string]any{"readOnlyHint": false},
		},
		{
			Name:        "way_workspace",
			Description: "Switch workspace in a Wayland GUI VM, or list workspaces when 'name' is omitted.",
			InputSchema: props(map[string]any{
				"vm":   prop("string", "VM name or ID"),
				"name": prop("string", "workspace to switch to (optional; omit to list)"),
			}, "vm"),
			Annotations: map[string]any{"readOnlyHint": false},
		},
		{
			Name:        "way_swaymsg",
			Description: "Raw swaymsg escape hatch for a Wayland GUI VM (e.g. 'move left', 'resize grow width 200 px', 'move workspace 1', 'xwayland on').",
			InputSchema: props(map[string]any{
				"vm":  prop("string", "VM name or ID"),
				"cmd": prop("string", "swaymsg command"),
			}, "vm", "cmd"),
			Annotations: map[string]any{"readOnlyHint": false},
		},
	}

	stdout := bufio.NewWriter(os.Stdout)
	defer stdout.Flush()

	dec := json.NewDecoder(bufio.NewReader(os.Stdin))
	for {
		var req jsonRPCRequest
		if err := dec.Decode(&req); err != nil {
			if err == io.EOF {
				return
			}
			continue
		}
		if strings.HasPrefix(req.Method, "notifications/") {
			continue // no response to notifications
		}
		result, rpcErr := dispatch(client, tools, req)
		if req.ID == nil {
			continue // a request without id is a notification despite its method
		}
		resp := jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: result}
		if rpcErr != nil {
			resp.Result = nil
			resp.Error = rpcErr
		}
		b, err := json.Marshal(resp)
		if err != nil {
			continue
		}
		stdout.Write(append(b, '\n'))
		stdout.Flush()
	}
}

func dispatch(c *fastvm.Client, tools []toolDef, req jsonRPCRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		proto := "2024-11-05"
		if p.ProtocolVersion != "" {
			proto = p.ProtocolVersion // echo client's version to avoid renegotiation
		}
		return map[string]any{
			"protocolVersion": proto,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "fastvm", "version": "0.1.0"},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		return callTool(c, req.Params)
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
}

func callTool(c *fastvm.Client, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid tool call params: " + err.Error()}
	}
	fail := func(err error) (any, *rpcError) {
		return toolText(p.Name + ": error: " + err.Error(), true), nil
	}
	switch p.Name {
	case "launch":
		var a struct {
			Template   string   `json:"template"`
			Name       string   `json:"name"`
			SSHKey     string   `json:"ssh_key"`
			TTLMinutes int      `json:"ttl_minutes"`
			Egress     []string `json:"egress"`
		}
		_ = json.Unmarshal(p.Arguments, &a)
		vm, err := c.WarmLaunch(fastvm.LaunchOptions{Template: a.Template, Name: a.Name, SSHKey: a.SSHKey, TTLMinutes: a.TTLMinutes, Egress: a.Egress})
		if err != nil {
			return fail(err)
		}
		b, _ := json.Marshal(vm)
		return toolText(fmt.Sprintf("launched %s (vmid %s, ip %s)\n%s", vm.Name, vm.VMID, vm.IP, string(b)), false), nil
	case "exec":
		var a struct {
			VM             string `json:"vm"`
			Command        string `json:"command"`
			TimeoutSeconds int    `json:"timeout_seconds"`
		}
		if err := json.Unmarshal(p.Arguments, &a); err != nil || a.VM == "" || a.Command == "" {
			return toolText("exec needs 'vm' and 'command'", true), nil
		}
		res, err := c.Exec(a.VM, a.Command, a.TimeoutSeconds)
		if err != nil {
			return fail(err)
		}
		out := strings.TrimRight(res.Output, "\n")
		return toolText(fmt.Sprintf("exit_code=%d\n%s", res.ExitCode, out), res.ExitCode != 0), nil
	case "pause":
		var a struct {
			VM string `json:"vm"`
		}
		_ = json.Unmarshal(p.Arguments, &a)
		if err := c.Pause(a.VM); err != nil {
			return fail(err)
		}
		return toolText("paused "+a.VM, false), nil
	case "resume":
		var a struct {
			VM string `json:"vm"`
		}
		_ = json.Unmarshal(p.Arguments, &a)
		if err := c.Resume(a.VM); err != nil {
			return fail(err)
		}
		return toolText("resumed "+a.VM, false), nil
	case "destroy":
		var a struct {
			VM string `json:"vm"`
		}
		_ = json.Unmarshal(p.Arguments, &a)
		if a.VM == "" {
			return toolText("destroy needs 'vm'", true), nil
		}
		if strings.HasPrefix(a.VM, "[") || a.VM == "all" || a.VM == "*" {
			return toolText("refusing to destroy all-VMs; destroy names one VM at a time", true), nil
		}
		if err := c.Destroy(a.VM); err != nil {
			// A VM that is already gone counts as destroyed (idempotent UX).
			if strings.Contains(strings.ToLower(err.Error()), "not found") {
				return toolText(a.VM+" already gone", false), nil
			}
			return fail(err)
		}
		return toolText("destroyed "+a.VM, false), nil
	case "list":
		vms, err := c.List()
		if err != nil {
			return fail(err)
		}
		var b strings.Builder
		if len(vms) == 0 {
			b.WriteString("no VMs\n")
		}
		for _, v := range vms {
			paused := ""
			if v.Paused {
				paused = " [paused]"
			}
			fmt.Fprintf(&b, "%-30s vmid=%s ip=%s%s created=%s\n", v.Name, v.VMID, v.IP, paused, v.Created)
		}
		return toolText(b.String(), false), nil
	case "get":
		var a struct {
			VM string `json:"vm"`
		}
		_ = json.Unmarshal(p.Arguments, &a)
		info, err := c.Get(a.VM)
		if err != nil {
			return fail(err)
		}
		b, _ := json.Marshal(info)
		return toolText(string(b), false), nil
	}
	if strings.HasPrefix(p.Name, "way_") {
		var a toolCallParams
		if err := json.Unmarshal(p.Arguments, &a); err != nil {
			return toolText(p.Name+": invalid arguments: "+err.Error(), true), nil
		}
		return handleWayTool(c, p.Name, a)
	}
	return nil, &rpcError{Code: -32602, Message: "unknown tool: " + p.Name}
}

// ---- Wayland GUI tool group -------------------------------------------
//
// These target a VM running the sway-headless stack (template warm-vnc-way:
// sway + foot + grim, desktop user 'ubuntu', XDG_RUNTIME_DIR=/tmp/xdg). Control
// surface = swaymsg i3-IPC; screenshots = grim. Deterministic window control
// for agents — far more reliable than pixel VNC input (which sway 1.9 headless
// gates). Raw VNC remains available through separate mcp-vnc registrations.

// wayShell wraps an inner snippet for the desktop user inside the VM.
func wayShell(inner string) string {
	prefix := "export XDG_RUNTIME_DIR=/tmp/xdg WAYLAND_DISPLAY=wayland-1 SWAYSOCK=$(ls /tmp/xdg/sway-ipc* 2>/dev/null | head -1); "
	return "su - ubuntu -c " + shellQuote(prefix+inner)
}

// shellQuote quotes a string for a POSIX shell using single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''` ) + "'"
}

// wayExec runs an in-guest command, strips the SSH host-key banner, and fails
// on non-zero exit, returning clean output.
func wayExec(c *fastvm.Client, vm, inner string, timeout int) (string, error) {
	res, err := c.Exec(vm, wayShell(inner), timeout)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return res.Output, fmt.Errorf("exit_code=%d: %s", res.ExitCode, tail(strings.TrimSpace(res.Output), 400))
	}
	return stripBanner(res.Output), nil
}

func stripBanner(out string) string {
	lines := strings.Split(out, "\n")
	kept := lines[:0]
	for _, ln := range lines {
		if strings.HasPrefix(ln, "Warning: Permanently added") || strings.TrimSpace(ln) == "" {
			continue
		}
		kept = append(kept, ln)
	}
	return strings.Join(kept, "\n")
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// swayOK formats swaymsg's JSON reply (usually [{"success": true}]).
func swayOK(out string) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return "ok"
	}
	return tail(out, 300)
}

func handleWayTool(c *fastvm.Client, name string, p toolCallParams) (any, *rpcError) {
	fail := func(err error) (any, *rpcError) {
		return toolText(name+": error: "+err.Error(), true), nil
	}
	if p.VM == "" {
		return toolText(name+" needs 'vm'", true), nil
	}
	switch name {
	case "way_screenshot":
		// One guest command: capture, base64, cleanup. The long base64 line
		// is extracted from whatever else the exec channel prepends.
		inner := "grim /tmp/.shot.png 2>&1; base64 -w0 /tmp/.shot.png; rm -f /tmp/.shot.png"
		res, err := c.Exec(p.VM, wayShell(inner), 60)
		if err != nil {
			return fail(err)
		}
		b64 := longestBase64(stripBanner(res.Output))
		if b64 == "" {
			return toolText("way_screenshot: grim produced no image (output: "+tail(res.Output, 200)+")", true), nil
		}
		return map[string]any{
			"content": []map[string]any{{"type": "image", "data": b64, "mimeType": "image/png"}},
		}, nil
	case "way_windows":
		out, err := wayExec(c, p.VM, "swaymsg -t get_tree", 30)
		if err != nil {
			return fail(err)
		}
		var tree any
		if err := json.Unmarshal([]byte(out), &tree); err != nil {
			return fail(fmt.Errorf("sway tree not JSON: %w", err))
		}
		wins := collectWindows(tree)
		b, _ := json.MarshalIndent(wins, "", "  ")
		return toolText(string(b), false), nil
	case "way_exec":
		if p.Cmd == "" {
			return toolText("way_exec needs 'cmd'", true), nil
		}
		out, err := wayExec(c, p.VM, "swaymsg exec "+shellQuote(p.Cmd), 30)
		if err != nil && !strings.Contains(out, "\"success\": true") {
			return fail(err)
		}
		return toolText("launched: "+p.Cmd+"\n"+swayOK(out), false), nil
	case "way_focus":
		if p.AppID == "" {
			return toolText("way_focus needs 'app_id'", true), nil
		}
		out, err := wayExec(c, p.VM, "swaymsg "+shellQuote(`[app_id="`+p.AppID+`"] focus`), 30)
		if err != nil {
			return fail(err)
		}
		return toolText(swayOK(out), false), nil
	case "way_close":
		if p.AppID == "" {
			return toolText("way_close needs 'app_id'", true), nil
		}
		out, err := wayExec(c, p.VM, "swaymsg "+shellQuote(`[app_id="`+p.AppID+`"] kill`), 30)
		if err != nil {
			return fail(err)
		}
		return toolText(swayOK(out), false), nil
	case "way_layout":
		switch p.Mode {
		case "tabbed", "stacking", "splith", "splitv":
		default:
			return toolText("way_layout: mode must be tabbed|stacking|splith|splitv", true), nil
		}
		out, err := wayExec(c, p.VM, "swaymsg layout "+p.Mode, 30)
		if err != nil {
			return fail(err)
		}
		return toolText(swayOK(out), false), nil
	case "way_workspace":
		if p.Name == "" {
			out, err := wayExec(c, p.VM, "swaymsg -t get_workspaces", 30)
			if err != nil {
				return fail(err)
			}
			// Workspaces: keep name + focused.
			simplified := extractWorkspaces(out)
			b, _ := json.MarshalIndent(simplified, "", "  ")
			return toolText(string(b), false), nil
		}
		out, err := wayExec(c, p.VM, "swaymsg workspace "+shellQuote(p.Name), 30)
		if err != nil {
			return fail(err)
		}
		return toolText(swayOK(out), false), nil
	case "way_swaymsg":
		if p.Cmd == "" {
			return toolText("way_swaymsg needs 'cmd'", true), nil
		}
		out, err := wayExec(c, p.VM, "swaymsg "+shellQuote(p.Cmd), 30)
		if err != nil {
			return fail(err)
		}
		return toolText(swayOK(out), false), nil
	}
	return nil, &rpcError{Code: -32602, Message: "unknown way tool: " + name}
}

// collectWindows walks a sway get_tree JSON payload, returning app windows.
func collectWindows(node any) []map[string]any {
	wins := []map[string]any{}
	var walk func(any)
	walk = func(n any) {
		obj, ok := n.(map[string]any)
		if !ok {
			return
		}
		if t, _ := obj["type"].(string); t == "con" {
			appID, _ := obj["app_id"].(string)
			if appID != "" {
				name, _ := obj["name"].(string)
				w := map[string]any{"app_id": appID, "name": name, "focused": obj["focused"] == true}
				if num, ok := obj["num"].(float64); ok && num > 0 {
					w["num"] = int(num)
				}
				wins = append(wins, w)
			}
		}
		for _, key := range []string{"nodes", "floating_nodes"} {
			if arr, ok := obj[key].([]any); ok {
				for _, ch := range arr {
					walk(ch)
				}
			}
		}
	}
	walk(node)
	return wins
}

func extractWorkspaces(out string) []map[string]any {
	var arr []any
	if err := json.Unmarshal([]byte(out), &arr); err != nil {
		return []map[string]any{{"raw": tail(out, 400)}}
	}
	ws := []map[string]any{}
	for _, it := range arr {
		if obj, ok := it.(map[string]any); ok {
			name, _ := obj["name"].(string)
			ws = append(ws, map[string]any{"name": name, "focused": obj["focused"] == true})
		}
	}
	return ws
}

// longestBase64 extracts the longest base64-looking token from output.
func longestBase64(out string) string {
	best := ""
	for _, tok := range strings.Fields(out) {
		if len(tok) < 100 {
			continue
		}
		ok := true
		for _, r := range tok {
			if !((r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '+' || r == '/' || r == '=') {
				ok = false
				break
			}
		}
		if ok && len(tok) > len(best) {
			best = tok
		}
	}
	return best
}

// toolCallParams is the superset of arguments way/lifecycle tools consume.
type toolCallParams struct {
	VM    string `json:"vm"`
	Cmd   string `json:"cmd"`
	Name  string `json:"name"`
	AppID string `json:"app_id"`
	Mode  string `json:"mode"`
}

// toolText builds an MCP tools/call result content array from text.
func toolText(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}
}

func props(defs map[string]any, required ...string) map[string]any {
	schema := map[string]any{
		"type":       "object",
		"properties": defs,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func prop(typ, desc string) map[string]any {
	return map[string]any{"type": typ, "description": desc}
}

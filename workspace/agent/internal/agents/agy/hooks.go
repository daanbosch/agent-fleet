package agy

// agy's lifecycle hooks as the primary signal for a slot's conversation UUID and its
// working / idle state (#2156). agy runs the commands in ~/.gemini/config/hooks.json at
// SessionStart / PreInvocation / PreToolUse / Stop with a camelCase JSON object on stdin and
// the launching pane's environment (measured on 1.3.5: AF_SESSION_NAME arrives, so the slot is
// named by the env af set at launch, never guessed from a cwd or a conversation id).
//
// Invariants:
//   - Hooks fire only once the first prompt is submitted, and an Esc cancel fires NO Stop
//     (measured). A hook "working" is therefore never trusted past the pane and the staleAfter
//     bound (hookLiveState), and every consumer falls back to the DB / brain paths when no
//     usable record exists.
//   - The command's stdout must be a JSON object, and the command must never fail or block agy:
//     every error is swallowed and "{}" is printed.
//   - hooks.json is shared with the user: only the entry named hookName is ours.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/fstore"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/paths"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
)

// HookSubcommand is the workspace-agent subcommand the hooks.json entry runs.
const HookSubcommand = "agy-hook"

// hookName is the key of af's entry in hooks.json. agy merges named hooks across files, so a
// user's own names are untouched by ours.
const hookName = "agent-fleet"

// hookTimeout is the handler timeout agy enforces on us. The command only writes a small file.
const hookTimeout = 10

// hookRecord is the latest lifecycle event of one slot, written by the hook process and read
// by the agent's list / mirror routes.
type hookRecord struct {
	ConvID string `json:"conv,omitempty"`
	// State is "working" or "idle"; "" for SessionStart, which only names the conversation.
	State string `json:"state,omitempty"`
	At    string `json:"at"` // RFC3339
}

var hookRecords = fstore.JSON[hookRecord](paths.AgentStateDir, "agy-hook", ".json")

// hookInput is the subset of the hook's stdin the agent reads.
type hookInput struct {
	ConversationID string `json:"conversationId"`
	// FullyIdle is a pointer so a Stop without the key reads as "unknown" rather than busy.
	FullyIdle *bool `json:"fullyIdle"`
}

// hookStateFor maps an event to the state it proves; "" proves none.
func hookStateFor(event string, in hookInput) string {
	switch event {
	case "PreInvocation", "PreToolUse":
		return "working"
	case "Stop":
		if in.FullyIdle != nil && !*in.FullyIdle {
			return "working"
		}
		return "idle"
	}
	return ""
}

// RunHook is `workspace-agent agy-hook <Event>`. It always prints "{}" and returns normally:
// a failing hook must not disturb the agy turn it observes.
func RunHook(args []string) {
	defer fmt.Fprintln(os.Stdout, "{}")
	if len(args) == 0 {
		return
	}
	var in hookInput
	_ = json.NewDecoder(io.LimitReader(os.Stdin, 1<<20)).Decode(&in)
	recordHook(os.Getenv(agents.SessionNameEnvVar), args[0], in, time.Now())
}

func recordHook(name, event string, in hookInput, now time.Time) {
	if name == "" {
		return // an agy the user runs by hand: not ours
	}
	m, ok := session.ReadMeta(name)
	if !ok || m.Kind != session.KindAgy {
		return
	}
	state := hookStateFor(event, in)
	if state == "" && event != "SessionStart" {
		return
	}
	slotSid := session.UUID(m.Dir, m.Name)
	rec := hookRecord{ConvID: in.ConversationID, State: state, At: now.Format(time.RFC3339)}
	if state == "" {
		// SessionStart names the conversation but says nothing about the turn: keep what a
		// concurrent writer has already recorded for this one.
		if prev, ok := hookRecords.Read(slotSid); ok && prev.ConvID == rec.ConvID {
			rec.State = prev.State
		}
	}
	_ = hookRecords.Write(slotSid, rec)
}

// hookConversation is the conversation UUID the slot's hook reported, "" when none did.
func hookConversation(slotSid string) string {
	rec, ok := hookRecords.Read(slotSid)
	if !ok {
		return ""
	}
	return rec.ConvID
}

// hookLiveState is the hook's verdict on the slot, "" when it has none to give (no record,
// another conversation's, or a "working" too old to believe) — the caller then uses the DB path.
// "idle" is a turn boundary agy itself announced. "working" is only as good as the pane:
// a cancelled turn leaves it behind, so an idle composer footer overrides it.
func hookLiveState(m session.Meta, p liveProbes) string {
	slotSid := session.UUID(m.Dir, m.Name)
	rec, ok := hookRecords.Read(slotSid)
	if !ok || rec.State == "" {
		return ""
	}
	if conv := sids.Read(slotSid); conv != "" && rec.ConvID != "" && conv != rec.ConvID {
		return ""
	}
	if rec.State == "idle" {
		return "idle"
	}
	at, err := time.Parse(time.RFC3339, rec.At)
	if err != nil || p.now().Sub(at) >= staleAfter {
		return ""
	}
	if p.paneIdleSettled(m.Name) {
		return "idle"
	}
	return "working"
}

// clearHook forgets the slot's record: a hook state of an earlier launch says nothing about
// this one.
func clearHook(slotSid string) { hookRecords.Remove(slotSid) }

func hooksPath() string { return filepath.Join(paths.GeminiHome(), "config", "hooks.json") }

var hooksMu sync.Mutex

// hookEntry is af's hooks.json entry. SessionStart / PreInvocation / Stop take handlers
// directly; the tool events take {matcher, hooks} groups (a grouped SessionStart never fired —
// measured on 1.3.5).
func hookEntry(exe string) map[string]any {
	handler := func(event string) map[string]any {
		return map[string]any{
			"type":    "command",
			"command": session.ShellQuote(exe) + " " + HookSubcommand + " " + event,
			"timeout": hookTimeout,
		}
	}
	return map[string]any{
		"SessionStart":  []any{handler("SessionStart")},
		"PreInvocation": []any{handler("PreInvocation")},
		"PreToolUse":    []any{map[string]any{"matcher": "*", "hooks": []any{handler("PreToolUse")}}},
		"Stop":          []any{handler("Stop")},
	}
}

// EnsureHooks installs af's named entry into the global hooks.json, leaving every other name
// as the user wrote it. Idempotent: nothing is written when the entry is already current. Run
// at every launch because the command embeds this binary's path, which a rebuilt or relocated
// agent invalidates. A file that is not plain JSON is left alone (the error says why) and the
// slot runs on the DB path.
func EnsureHooks() error { return ensureHooksAt(hooksPath(), paths.ExePath()) }

func ensureHooksAt(path, exe string) error {
	hooksMu.Lock()
	defer hooksMu.Unlock()
	root := map[string]any{}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &root); err != nil || root == nil {
			return fmt.Errorf("%s is not a plain JSON object, leaving it alone", path)
		}
	case !os.IsNotExist(err):
		return err
	}
	// Round-trip so the comparison is between decoded values on both sides.
	want, err := jsonNorm(hookEntry(exe))
	if err != nil {
		return err
	}
	if reflect.DeepEqual(root[hookName], want) {
		return nil
	}
	root[hookName] = want
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// Write through a symlink rather than replacing it: the dotfiles may be links onto
	// persistent storage that must stay links.
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	tmp := path + ".af-tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func jsonNorm(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	err = json.Unmarshal(b, &out)
	return out, err
}

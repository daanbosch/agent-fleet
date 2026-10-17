package cursor

// The `stop` hook as the source of per-turn token usage for Terminal sessions (#2286). The
// interactive TUI runs the commands in ~/.cursor/hooks.json with a JSON object on stdin, in the
// launching pane's environment (measured on 2026.10.16: AF_SESSION_NAME arrives, so the slot is
// named by the env af set at launch). Its `stop` payload carries `input_tokens`,
// `output_tokens`, `cache_read_tokens` and `cache_write_tokens` for a `completed` turn
// (docs/log/132 §2, §4). Nothing else in the cursor CLI reports tokens on the live path.
//
// Invariants:
//   - The command never blocks or fails the CLI: every error is swallowed, nothing is printed,
//     and it only appends one small row to the ledger.
//   - Only ids and token counts are read from stdin and written down. The payload's paths,
//     e-mail and transcript location are never stored (ADR 0029: no content in the ledger).
//   - ~/.cursor/hooks.json is shared with the user: only the `stop` entry whose command runs
//     HookSubcommand is ours. Anything about the file we do not understand leaves it untouched.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/paths"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/usagex"
)

// HookSubcommand is the workspace-agent subcommand the hooks.json entry runs.
const HookSubcommand = "cursor-hook"

// hookTimeout is the handler timeout (seconds) cursor enforces on us; the command only
// appends a row.
const hookTimeout = 10

// stopInput is the subset of the `stop` payload the hook reads. The token fields are pointers:
// an `aborted` / `error` stop carries none, and an absent count is not a zero count.
type stopInput struct {
	ConversationID  string `json:"conversation_id"`
	GenerationID    string `json:"generation_id"`
	Model           string `json:"model"`
	Status          string `json:"status"`
	InputTokens     *int   `json:"input_tokens"`
	OutputTokens    *int   `json:"output_tokens"`
	CacheReadTokens *int   `json:"cache_read_tokens"`
	CacheWriteToken *int   `json:"cache_write_tokens"`
}

// RunHook is `workspace-agent cursor-hook <event>`. It prints nothing and returns normally:
// a failing hook must not disturb the turn it observes.
func RunHook(args []string) {
	defer func() { _ = recover() }()
	if len(args) == 0 || args[0] != "stop" {
		return
	}
	var in stopInput
	if json.NewDecoder(io.LimitReader(os.Stdin, 1<<20)).Decode(&in) != nil {
		return
	}
	recordStop(os.Getenv(agents.SessionNameEnvVar), in, time.Now())
}

// recordStop appends the ledger row of one completed turn. The generation id is the dedup key
// (usagex.Record.Key): an interrupted turn fires `stop` twice with one id, and a retried
// delivery is possible, so the aggregation counts one row per key.
func recordStop(name string, in stopInput, now time.Time) {
	if name == "" || !session.ValidName(name) || in.GenerationID == "" {
		return // a cursor-agent the user runs by hand, or a payload that cannot be attributed
	}
	// Only a completed turn that carries tokens is a row. aborted / error stops carry none and
	// are never rows; the missing counts must not be read as zeros.
	if in.Status != "completed" || in.InputTokens == nil || in.OutputTokens == nil {
		return
	}
	m, ok := session.ReadMeta(name)
	if !ok || m.Kind != session.KindCursor || m.DriverKind() == session.DriverManaged {
		return
	}
	// A cursor-agent started from inside the pane (a run_command) inherits the env but is not
	// the slot's conversation.
	if want := sids.Read(session.UUID(m.Dir, m.Name)); want != "" && in.ConversationID != "" && want != in.ConversationID {
		return
	}
	for _, p := range []*int{in.CacheReadTokens, in.CacheWriteToken} {
		if p != nil && *p < 0 {
			return
		}
	}
	if *in.InputTokens < 0 || *in.OutputTokens < 0 {
		return
	}
	cread, cwrite := intOr0(in.CacheReadTokens), intOr0(in.CacheWriteToken)
	// input_tokens already includes cache_read_tokens (docs/log/132 §4); the ledger's In is
	// the fresh part, so spend and the price estimate do not count the cached prefix twice.
	// cache_write_tokens was 0 in every measured turn, so it is kept as ccreate and not
	// subtracted from input.
	fresh := max(*in.InputTokens-cread, 0)
	rec := usagex.Record{
		TS: now.UTC().Format(time.RFC3339), Call: in.GenerationID, Feature: usagex.FeatureSession,
		Origin: session.OriginOf(m), OriginConv: m.OriginConv, Kind: m.Kind, Ref: m.Name,
		Key: "cursor-stop:" + in.GenerationID,
		In:  fresh, Out: *in.OutputTokens, CacheRead: cread, CacheCreate: cwrite,
		Spend: usagex.Spend(fresh, cwrite, *in.OutputTokens),
		OK:    true,
		// partial, not exact: the numbers are the CLI's own hook report, the multi-step sum
		// was verified on one version only, and cache_write semantics are unmeasured.
		Measured:   usagex.MeasuredPartial,
		TokensOnly: true,
	}
	// The model is the raw selector: "default" for Auto, which is not the model Auto picked.
	if in.Model != "" && in.Model != "default" {
		rec.Model, rec.ModelRaw, rec.ModelReq, rec.ModelSrc = in.Model, in.Model, in.Model, usagex.ModelRequest
	} else {
		rec.ModelRaw, rec.ModelSrc = in.Model, usagex.ModelUnknown
	}
	_ = usagex.AppendRows([]usagex.Record{rec})
}

func intOr0(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func hooksPath() string { return filepath.Join(Home(), "hooks.json") }

// hookCommand is the hooks.json command string; ours is recognised by running HookSubcommand.
func hookCommand(exe string) string {
	return session.ShellQuote(exe) + " " + HookSubcommand + " stop"
}

func isOurs(entry json.RawMessage) bool {
	var e struct {
		Command string `json:"command"`
	}
	return json.Unmarshal(entry, &e) == nil && strings.Contains(e.Command, " "+HookSubcommand+" ")
}

// EnsureHooks installs af's `stop` entry into the global hooks.json. Idempotent: nothing is
// written when the entry is already current. Run at every launch because the command embeds
// this binary's path, which a rebuilt or relocated agent invalidates. The global file (not the
// project's .cursor/hooks.json) is used because a project file would dirty the user's repo.
//
// The merge fails closed: a file that is not a plain JSON object, whose `version` is not 1,
// whose `hooks` is not an object or whose `hooks.stop` is not an array is left byte-for-byte
// alone (the error says why) and the session just records no usage. The user's other events
// and `stop` entries are kept as written (values preserved; whitespace and key order are
// normalised). The caller logs.
func EnsureHooks() error { return ensureHooksAt(hooksPath(), paths.ExePath()) }

// ensureHooksMaxTries bounds the re-read loop when the user saves the file between our read
// and our rename.
const ensureHooksMaxTries = 3

func ensureHooksAt(path, exe string) error {
	// Resolve the real target first so a rename never replaces a symlink, whether the link is the
	// file or the config directory. A link whose target is missing is not ours to create through.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return err
	}
	target := filepath.Join(dir, filepath.Base(path))
	if fi, err := os.Lstat(target); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if target, err = filepath.EvalSymlinks(target); err != nil {
			return fmt.Errorf("%s is a dangling symlink, leaving it alone: %w", path, err)
		}
	}
	unlock, err := lockHooks()
	if err != nil {
		return err
	}
	defer unlock()
	for range ensureHooksMaxTries {
		done, err := ensureHooksOnce(target, exe)
		if err != nil || done {
			return err
		}
	}
	return fmt.Errorf("%s kept changing while it was being updated, leaving it alone", path)
}

// mergeStop returns the file content with af's stop entry in place, and whether it differs
// from orig. orig == nil means the file does not exist.
func mergeStop(orig []byte, exe string) (out []byte, changed bool, err error) {
	root := map[string]json.RawMessage{}
	if orig != nil {
		if err := json.Unmarshal(orig, &root); err != nil || root == nil {
			return nil, false, fmt.Errorf("not a plain JSON object")
		}
	}
	if v, ok := root["version"]; ok {
		var n float64
		if json.Unmarshal(v, &n) != nil || n != 1 {
			return nil, false, fmt.Errorf("unknown version %s", v)
		}
	} else if orig == nil {
		root["version"] = json.RawMessage("1")
	}
	hooks := map[string]json.RawMessage{}
	if raw, ok := root["hooks"]; ok {
		if json.Unmarshal(raw, &hooks) != nil || hooks == nil {
			return nil, false, fmt.Errorf(`"hooks" is not an object`)
		}
	}
	var stops []json.RawMessage
	if raw, ok := hooks["stop"]; ok {
		if json.Unmarshal(raw, &stops) != nil {
			return nil, false, fmt.Errorf(`"hooks.stop" is not an array`)
		}
	}
	ours, err := json.Marshal(map[string]any{"command": hookCommand(exe), "timeout": hookTimeout})
	if err != nil {
		return nil, false, err
	}
	// Replace our entry where it stands (drop any duplicates), else append after the user's.
	merged := make([]json.RawMessage, 0, len(stops)+1)
	placed := false
	for _, e := range stops {
		if !isOurs(e) {
			merged = append(merged, e)
			continue
		}
		if !placed {
			merged = append(merged, ours)
			placed = true
		}
	}
	if !placed {
		merged = append(merged, ours)
	}
	if reflect.DeepEqual(canon(stops), canon(merged)) {
		return orig, false, nil
	}
	if hooks["stop"], err = json.Marshal(merged); err != nil {
		return nil, false, err
	}
	if root["hooks"], err = json.Marshal(hooks); err != nil {
		return nil, false, err
	}
	out, err = json.MarshalIndent(root, "", "  ")
	return append(out, '\n'), true, err
}

// canon decodes entries so that two spellings of one JSON value compare equal.
func canon(in []json.RawMessage) []any {
	out := make([]any, len(in))
	for i, e := range in {
		_ = json.Unmarshal(e, &out[i])
	}
	return out
}

// ensureHooksOnce merges once. done=false means the file changed under us before the rename and
// the caller should read it again.
func ensureHooksOnce(target, exe string) (done bool, err error) {
	orig, err := os.ReadFile(target)
	var mode os.FileMode = 0o600
	switch {
	case err == nil:
		if fi, err := os.Stat(target); err == nil {
			mode = fi.Mode().Perm()
		}
	case !os.IsNotExist(err):
		return false, err
	default:
		orig = nil
	}
	out, changed, err := mergeStop(orig, exe)
	if err != nil {
		return false, fmt.Errorf("%s: %w, leaving it alone", target, err)
	}
	if !changed {
		return true, nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".hooks.json.af-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Chmod(mode); err != nil { // keep the user's mode; a new file is 0600
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	// An editor may have saved since our read: do not publish a merge of the old content.
	if now, err := os.ReadFile(target); !(err == nil && string(now) == string(orig)) && !(orig == nil && os.IsNotExist(err)) {
		return false, nil
	}
	return true, os.Rename(tmp.Name(), target)
}

// hooksLockPath is the cross-process lock for hooks.json, kept in the agent's own state so
// nothing is added to the user's config directory.
func hooksLockPath() string { return filepath.Join(paths.AgentStateDir(), "cursor-hooks.lock") }

var hooksMu sync.Mutex

// lockHooks serialises writers of hooks.json across processes. The wait is bounded: a launch
// must not hang on a stuck holder, and giving up only costs the usage signal.
func lockHooks() (func(), error) {
	hooksMu.Lock()
	if err := os.MkdirAll(filepath.Dir(hooksLockPath()), 0o700); err != nil {
		hooksMu.Unlock()
		return nil, err
	}
	f, err := os.OpenFile(hooksLockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		hooksMu.Unlock()
		return nil, err
	}
	deadline := time.Now().Add(2 * time.Second)
	for !tryFlock(f) {
		if time.Now().After(deadline) {
			_ = f.Close()
			hooksMu.Unlock()
			return nil, fmt.Errorf("timed out waiting for the hooks.json lock")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return func() { _ = f.Close(); hooksMu.Unlock() }, nil
}

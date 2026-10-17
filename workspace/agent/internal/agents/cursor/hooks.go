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
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
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

// isOurs recognises only the exact entry hookCommand generates: the command is one
// single-quoted absolute path followed by " cursor-hook stop", and the entry has no key besides
// command and timeout. Anything that merely mentions the subcommand (an argument to another
// tool, a wrapper, a script) is the user's and is kept. hooks.json has no field for a name, and
// an extra key's effect on cursor is unmeasured, so the shape itself is the marker.
func isOurs(entry json.RawMessage) bool {
	var e map[string]json.RawMessage
	if json.Unmarshal(entry, &e) != nil {
		return false
	}
	for k := range e {
		if k != "command" && k != "timeout" {
			return false
		}
	}
	var cmd string
	if json.Unmarshal(e["command"], &cmd) != nil {
		return false
	}
	const suffix = "' " + HookSubcommand + " stop"
	if !strings.HasPrefix(cmd, "'/") || !strings.HasSuffix(cmd, suffix) {
		return false
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(cmd, "'"), suffix)
	// ShellQuote writes a quote inside the path as '\'' ; any other quote ends the word early.
	return !strings.Contains(strings.ReplaceAll(inner, `'\''`, ""), "'")
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
	} else {
		return nil, false, fmt.Errorf(`no "version": 1`)
	}
	hooks := map[string]json.RawMessage{}
	if raw, ok := root["hooks"]; ok {
		if json.Unmarshal(raw, &hooks) != nil || hooks == nil {
			return nil, false, fmt.Errorf(`"hooks" is not an object`)
		}
	}
	var stops []json.RawMessage
	if raw, ok := hooks["stop"]; ok {
		// null decodes into a nil slice without error, so the type is checked on the text.
		if t := bytes.TrimSpace(raw); len(t) == 0 || t[0] != '[' || json.Unmarshal(raw, &stops) != nil {
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

// maxHooksSize bounds what is read of the user's file; a larger one is not a hooks.json.
const maxHooksSize = 1 << 20

// readHooks reads path only when it is a regular file. A FIFO or device would block the
// launch forever, so it is opened non-blocking and checked with fstat before any read.
// exists=false means there is no file.
func readHooks(path string) (data []byte, mode os.FileMode, exists bool, err error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, 0, false, err
	}
	if !fi.Mode().IsRegular() {
		return nil, 0, false, fmt.Errorf("%s is not a regular file", path)
	}
	data, err = io.ReadAll(io.LimitReader(f, maxHooksSize+1))
	if err != nil {
		return nil, 0, false, err
	}
	if len(data) > maxHooksSize {
		return nil, 0, false, fmt.Errorf("%s is larger than %d bytes", path, maxHooksSize)
	}
	return data, fi.Mode().Perm(), true, nil
}

// Test seams: hookAfterRead runs between the read and the final check, hookBeforeRename right
// before the atomic replace, hookAfterRename right after it. They are where an editor's save or
// a killed process would land.
var hookAfterRead, hookBeforeRename, hookAfterRename func()

func seam(f func()) {
	if f != nil {
		f()
	}
}

// backupSuffix names the single backup kept beside the user's file.
const backupSuffix = ".af-prev"

// ensureHooksOnce merges once. done=false means the file changed under us and the caller should
// read it again.
//
// What this guarantees, and what it cannot:
//   - The path is never empty: the new content replaces the old with one atomic rename, so a
//     process killed at any point leaves either the old or the new file, and the next ensure
//     merges again from whatever is there.
//   - Before the rename the old content gets a second name (a hard link, a copy where links
//     fail) at <file>.af-prev. That file is the user's previous hooks.json and is kept.
//   - A save made by replacing the file (what most editors do) between the final comparison and
//     the rename is the one window left, a few microseconds wide; its content is not lost from
//     disk but is not merged either, and ends up only if the user saves again.
//   - A write straight into the old file (an editor holding it open) after the rename lands in
//     the inode the backup still names. It is detected by comparing the backup with what was
//     merged from, and merged into the published file at once.
func ensureHooksOnce(target, exe string) (done bool, err error) {
	orig, mode, exists, err := readHooks(target)
	if err != nil {
		return false, fmt.Errorf("%w, leaving it alone", err)
	}
	if !exists {
		orig, mode = nil, 0o600
	}
	out, changed, err := mergeStop(orig, exe)
	if err != nil {
		return false, fmt.Errorf("%s: %w, leaving it alone", target, err)
	}
	if !changed {
		return true, nil
	}
	seam(hookAfterRead)
	tmp, err := writeTemp(target, out, mode)
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp) // no-op once renamed
	// An editor may have saved since our read: do not publish a merge of the old content.
	if now, _, nowExists, err := readHooks(target); err != nil || nowExists != exists || !bytes.Equal(now, orig) {
		return false, nil
	}
	if !exists {
		// Link fails with EEXIST when the user created the file since our check.
		if err := os.Link(tmp, target); err != nil {
			if os.IsExist(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}
	backup := target + backupSuffix
	if err := keepBackup(target, backup, orig, mode); err != nil {
		return false, fmt.Errorf("cannot keep a backup of %s: %w", target, err)
	}
	seam(hookBeforeRename)
	if err := os.Rename(tmp, target); err != nil {
		return false, err
	}
	seam(hookAfterRename)
	// The backup still names the old inode: a write into it after our check shows up here.
	late, _, ok, err := readHooks(backup)
	if err != nil || !ok || bytes.Equal(late, orig) {
		return true, nil
	}
	merged, _, mergeErr := mergeStop(late, exe)
	if mergeErr != nil {
		merged = late // not mergeable any more: put the user's text back as written
	}
	tmp2, err := writeTemp(target, merged, mode)
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp2)
	if err := os.Rename(tmp2, target); err != nil {
		return false, err
	}
	if mergeErr != nil {
		return false, fmt.Errorf("%s was edited while it was being updated and is not mergeable: %w, restored as saved", target, mergeErr)
	}
	return true, nil
}

// writeTemp writes content to a new temp file beside target with the given mode.
func writeTemp(target string, content []byte, mode os.FileMode) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(target), ".hooks.json.af-*")
	if err != nil {
		return "", err
	}
	_, werr := f.Write(content)
	cerr := f.Chmod(mode) // keep the user's mode; a new file is 0600
	if err := f.Close(); werr == nil && cerr == nil {
		werr = err
	}
	if werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(f.Name())
		return "", werr
	}
	return f.Name(), nil
}

// keepBackup gives target's current inode a second name, replacing the previous backup. Where
// hard links are unavailable it falls back to a copy of orig, which loses the late-write
// detection but keeps the content.
func keepBackup(target, backup string, orig []byte, mode os.FileMode) error {
	if err := os.Remove(backup); err != nil && !os.IsNotExist(err) {
		return err
	}
	if os.Link(target, backup) == nil {
		return nil
	}
	return os.WriteFile(backup, orig, mode)
}

// hooksLockPath is the cross-process lock for hooks.json, kept in the agent's own state so
// nothing is added to the user's config directory.
func hooksLockPath() string { return filepath.Join(paths.AgentStateDir(), "cursor-hooks.lock") }

var hooksMu sync.Mutex

// lockHooks serialises writers of hooks.json across processes. The wait is bounded: a launch
// must not hang on a stuck holder, and giving up only costs the usage signal.
func lockHooks() (func(), error) {
	if !lockSupported {
		// Without an advisory lock two agent processes can race each other; giving up only
		// costs the usage numbers.
		return nil, fmt.Errorf("no cross-process lock on this OS")
	}
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

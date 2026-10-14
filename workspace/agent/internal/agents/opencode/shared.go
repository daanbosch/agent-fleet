package opencode

// Terminal (CLI) sessions on opencode 2 attached to af's own supervised `opencode serve`
// (issue #2120). A private `--standalone` server per TUI costs ~415 MB per session against
// ~182 MB when the TUIs share one server (measured on 2.0.28, docs/log/119 and the note this
// change appends), so on v2 the TUI is launched with `--server <addr>` and every af session
// stays apart by two things the server keeps per opencode session:
//
//   - metadata: af creates (or forks) the opencode session itself with {"af.sid", "af.name"},
//     records slot → session id at once, and the plugins resolve sessionID → af session through
//     ctx.session.get. A fork copies metadata, so the copy is re-tagged; PATCH replaces the
//     whole object, so every tag is a read-merge-write.
//   - environment: PUT /api/session/{id}/environment replaces the server environment for that
//     session's shell. It is held in memory only (lost on a server restart, not copied by a
//     fork) and without it the shell sees the SERVER's environment, including provider keys and
//     the server password. So the env goes in before the TUI can submit anything (attach.go),
//     and again, from the pane's own environment, on every new server generation (restore).
//
// Provider keys cannot be moved out of the server environment: the credential API stores them
// in opencode.db in plaintext (measured), which af's rule against plaintext credentials rules out.
//
// AF_OPENCODE_TUI_STANDALONE=1 puts the old `--standalone` launch back without a release.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/paths"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/tmuxx"
)

// standaloneEnv, when "1", keeps Terminal sessions on a private server each (the launch #2105
// shipped). It is read on every launch, so flipping it needs no restart of the Agent.
const standaloneEnv = "AF_OPENCODE_TUI_STANDALONE"

// Metadata keys af stamps on an opencode session. The status, TUI and env-gate plugins read
// the same names (workspace/opencode-plugin).
const (
	metaSID     = "af.sid"
	metaName    = "af.name"
	metaManaged = "af.managed"
)

// sharedTUI reports whether Terminal sessions attach to af's serve: opencode 2 only (v1 has no
// per-session environment to keep sessions apart with), unless switched back.
func sharedTUI() bool {
	return installedMajor() >= 2 && os.Getenv(standaloneEnv) != "1" && !Serve().Disabled()
}

// slotModes records how each Terminal slot's pane was last launched. The workspace-wide switch
// says what the NEXT launch does; the pane that is running keeps the mode it started with (an
// automatic fall back to --standalone, a switch flipped later), and its state, dependents,
// restore, drain and stop must follow that, not the switch.
var slotModes = agents.NewSidStore("opencode-mode")

const modeShared, modeStandalone = "shared", "standalone"

func markSlotMode(sid, mode string) { slotModes.Write(sid, mode) }

// sharedSlot reports whether slot m's pane is attached to af's serve. A slot with no record was
// launched before this existed, i.e. --standalone. The switch (sharedTUI) is deliberately not
// part of it: flipping it changes the NEXT launch, and a pane still attached to the serve keeps
// being a dependent, restored, drained and interrupted.
func sharedSlot(m session.Meta) bool {
	return installedMajor() >= 2 && m.Kind == session.KindOpencode && m.DriverKind() == session.DriverTUI &&
		slotModes.Read(session.UUID(m.Dir, m.Name)) == modeShared
}

// terminalSlots lists the live opencode Terminal slots attached to the shared serve; a var so
// tests need no tmux.
var terminalSlots = func() []session.Meta {
	live := tmuxx.LiveSessionNames()
	var out []session.Meta
	for _, m := range session.ListMetas() {
		if sharedSlot(m) && !m.Archived && live[m.Name] {
			out = append(out, m)
		}
	}
	return out
}

// sharedTerminals is how many Terminal panes depend on the shared daemon right now. Derived from
// the metas and tmux rather than a registry, so an Agent restart does not forget attached TUIs
// and let the idle stop pull the server out from under them.
func sharedTerminals() int { return len(terminalSlots()) }

// sharedDaemonWanted is whether the daemon has to be started the shared way (a stable password,
// the plugins' shared mode): a launch that will attach to it, or a pane that already is.
func sharedDaemonWanted() bool { return sharedTUI() || sharedTerminals() > 0 }

// --- API calls ---------------------------------------------------------------------

// apiJSON sends one JSON request through the authenticated client. A non-2xx answer is a
// statusError; out, when set, is filled from the body.
func apiJSON(method, u string, body, out any) error {
	return apiJSONWith(serveClient, method, u, body, out)
}

// apiJSONWith is apiJSON through client c (the attach wrapper brings its own credentials).
func apiJSONWith(c *http.Client, method, u string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return statusError(res.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

type sharedSession struct {
	ID       string            `json:"id"`
	Metadata map[string]string `json:"metadata"`
}

func sessionPathURL(addr, ses string) string { return addr + "/api/session/" + url.PathEscape(ses) }

func getSharedSession(addr, ses string) (*sharedSession, error) {
	var w struct {
		Data sharedSession `json:"data"`
	}
	if err := apiJSON("GET", sessionPathURL(addr, ses), nil, &w); err != nil {
		return nil, err
	}
	return &w.Data, nil
}

// launchModelRef is the model object of the v2 API for a launch-model string "provider/id[#variant]".
func launchModelRef(model string) (map[string]string, bool) {
	prov, id, ok := splitModel(model)
	if !ok {
		return nil, false
	}
	ref := map[string]string{"providerID": prov, "id": id}
	if base, variant, found := strings.Cut(id, "#"); found && base != "" && variant != "" {
		ref["id"], ref["variant"] = base, variant
	}
	return ref, true
}

// allowAll is the --auto of the TUI for a session af creates through the API.
var allowAll = []map[string]string{{"action": "*", "resource": "*", "effect": "allow"}}

func sharedMetadata(sid, name string) map[string]string {
	return map[string]string{metaSID: sid, metaName: name}
}

// createSharedSession makes the opencode session for a fresh Terminal slot.
func createSharedSession(addr string, m session.Meta, sid string) (string, error) {
	body := map[string]any{
		"location":    map[string]string{"directory": m.CWD()},
		"permissions": allowAll,
		"metadata":    sharedMetadata(sid, m.Name),
	}
	if ref, ok := launchModelRef(m.Model); ok {
		body["model"] = ref
	}
	if ag := agentForMode(m.Mode); ag != "" {
		body["agent"] = ag
	}
	var w struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := apiJSON("POST", addr+"/api/session", body, &w); err != nil {
		return "", fmt.Errorf("create the opencode session: %w", err)
	}
	if w.Data.ID == "" {
		return "", errors.New("create the opencode session: no id in the answer")
	}
	return w.Data.ID, nil
}

// forkSharedSession copies src (up to, not including, the message `before` when set).
func forkSharedSession(addr, src, before string) (string, error) {
	body := map[string]string{}
	if before != "" {
		body["before"] = before
	}
	var w struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := apiJSON("POST", sessionPathURL(addr, src)+"/fork", body, &w); err != nil {
		return "", fmt.Errorf("fork the opencode session: %w", err)
	}
	if w.Data.ID == "" {
		return "", errors.New("fork the opencode session: no id in the answer")
	}
	return w.Data.ID, nil
}

// afterFork is a test seam: it runs between a fork existing and its claim.
var afterFork = func(addr, ses string) {}

// switchSharedSession applies the slot's model and agent to an existing session: a session that
// exists ignores the config a fresh one reads (measured, #2118).
func switchSharedSession(addr, ses string, m session.Meta) error {
	if ref, ok := launchModelRef(m.Model); ok {
		if err := apiJSON("POST", sessionPathURL(addr, ses)+"/model", map[string]any{"model": ref}, nil); err != nil {
			return fmt.Errorf("switch the model: %w", err)
		}
	}
	if ag := agentForMode(m.Mode); ag != "" {
		if err := apiJSON("POST", sessionPathURL(addr, ses)+"/agent", map[string]string{"agent": ag}, nil); err != nil {
			return fmt.Errorf("switch the agent: %w", err)
		}
	}
	return nil
}

// tagSharedSession makes ses belong to (sid, name), keeping every other metadata key. PATCH
// replaces the object, so the current one is read first.
func tagSharedSession(addr, ses, sid, name string) error {
	cur, err := getSharedSession(addr, ses)
	if err != nil {
		return err
	}
	if cur.Metadata[metaSID] == sid && cur.Metadata[metaName] == name {
		return nil
	}
	md := map[string]string{}
	for k, v := range cur.Metadata {
		md[k] = v
	}
	md[metaSID], md[metaName] = sid, name
	return apiJSON("PATCH", sessionPathURL(addr, ses), map[string]any{"metadata": md}, nil)
}

// putEnvironment replaces the session's shell environment with vars.
func putEnvironment(addr, ses string, vars map[string]string) error {
	return apiJSON("PUT", sessionPathURL(addr, ses)+"/environment", map[string]any{"variables": vars}, nil)
}

// --- launch ---------------------------------------------------------------------

// ensureServe starts or adopts the daemon; a var so tests need no process.
var ensureServe = func() (string, int, error) { return Serve().Ensure() }

// restartServe replaces the daemon; a var for the same reason.
var restartServe = func(reason string) bool { return Serve().Restart(reason) }

// sharedPlan is what a shared launch adds to the pane: the program and the environment that
// carries the server password (never the command line).
type sharedPlan struct {
	Program string
	Env     []string
}

// buildSharedLaunch prepares slot m on af's serve: the daemon is ensured, the opencode session is
// created / resumed / forked and recorded, and the returned program PUTs the pane's environment
// before it starts the TUI. ok=false means "use the standalone launch" (the serve cannot be
// had, e.g. opencode is not connected); an error means the launch must stop, because a
// half-prepared session would attach to the wrong conversation.
func buildSharedLaunch(m session.Meta, sid string) (plan sharedPlan, ok bool, err error) {
	addr, _, eerr := ensureServe()
	if eerr != nil {
		log.Printf("opencode: %s: shared serve unavailable, launching --standalone: %v", m.Name, eerr)
		markSlotMode(sid, modeStandalone)
		return sharedPlan{}, false, nil
	}
	// A TUI of one version against a server of another is the protocol skew nobody can debug from
	// a pane, and a daemon outlives an in-place upgrade of the binary. Replace it before this
	// TUI attaches; an adopted daemon (no process handle) cannot be, and is attached to as it is.
	if want, have := installedVersion(), daemonVersion(addr); want != "" && have != "" && want != have {
		log.Printf("opencode: %s: serve is %s, installed opencode is %s: restarting it", m.Name, have, want)
		if restartServe("opencode " + have + " → " + want) {
			if addr, _, eerr = ensureServe(); eerr != nil {
				log.Printf("opencode: %s: shared serve unavailable after the restart, launching --standalone: %v", m.Name, eerr)
				markSlotMode(sid, modeStandalone)
				return sharedPlan{}, false, nil
			}
		}
	}
	ses := sids.Read(sid)
	if ses != "" {
		if _, gerr := getSharedSession(addr, ses); errors.Is(gerr, statusError(http.StatusNotFound)) {
			ses = "" // the conversation is gone; start over rather than attach to nothing
		} else if gerr != nil {
			return sharedPlan{}, false, fmt.Errorf("opencode: read the session: %w", gerr)
		}
	}
	switch {
	case ses != "":
		if err := switchSharedSession(addr, ses, m); err != nil {
			return sharedPlan{}, false, err
		}
		// The slot's own record names it, but a pane of another slot may hold it: refuse, never
		// overwrite what another pane owns.
		if err := claimSessionFor(addr, m, ses, claimOpts{}); err != nil {
			return sharedPlan{}, false, fmt.Errorf("opencode: take the session: %w", err)
		}
	case m.ForkFrom != "":
		// A fork carries its source's tag the moment it exists, so the source's pane can claim it
		// before this slot does. The claim is exclusive and the tag is never overwritten: a copy
		// somebody else got first is left to them and a new one is made.
		srcSID := ""
		if src, gerr := getSharedSession(addr, m.ForkFrom); gerr == nil {
			srcSID = src.Metadata[metaSID]
		}
		for attempt := 0; ; attempt++ {
			ses, err = forkSharedSession(addr, m.ForkFrom, m.ForkAt)
			if err != nil {
				return sharedPlan{}, false, err
			}
			afterFork(addr, ses)
			err = claimSessionFor(addr, m, ses, claimOpts{inheritedFrom: srcSID})
			if err == nil {
				break
			}
			if !errors.Is(err, errClaimTaken) || attempt >= 2 {
				return sharedPlan{}, false, fmt.Errorf("opencode: claim the fork: %w", err)
			}
		}
		if err := switchSharedSession(addr, ses, m); err != nil {
			return sharedPlan{}, false, err
		}
	default:
		ses, err = createSharedSession(addr, m, sid)
		if err != nil {
			return sharedPlan{}, false, err
		}
	}
	sids.Write(sid, ses)
	markSlotMode(sid, modeShared)

	flags := strings.Fields(envOr("AGENT_OPENCODE_FLAGS", "--auto"))
	if bad := connectionFlag(flags); bad != "" {
		return sharedPlan{}, false, fmt.Errorf("opencode: AGENT_OPENCODE_FLAGS may not carry %s: the session and server are af's to choose", bad)
	}
	parts := []string{"workspace-agent", "opencode-attach", "--server", session.ShellQuote(addr), "--session", session.ShellQuote(ses), "--"}
	for _, f := range flags {
		parts = append(parts, session.ShellQuote(f))
	}
	return sharedPlan{
		Program: strings.Join(parts, " "),
		Env: []string{
			"OPENCODE_SERVER_USERNAME=" + serveUser,
			"OPENCODE_SERVER_PASSWORD=" + servePassword(),
			sharedPaneEnv + "=1",
		},
	}, true, nil
}

// sharedPaneEnv marks a pane whose TUI runs on af's serve; the TUI plugin does nothing without it
// (a --standalone pane already has its own server and AF_SESSION_SID in the shell's env).
const sharedPaneEnv = "AF_OPENCODE_SHARED"

// --- stop -----------------------------------------------------------------------

// interruptTerminal stops the turn a Terminal slot has running on the shared serve. Closing the
// pane does not: the server runs the turn, not the TUI.
func interruptTerminal(m session.Meta) {
	if !sharedSlot(m) || !Serve().Running() {
		return
	}
	ses := sids.Read(session.UUID(m.Dir, m.Name))
	if ses == "" {
		return
	}
	if err := apiJSON("POST", sessionPathURL(serveAddr(), ses)+"/interrupt", map[string]string{}, nil); err != nil {
		log.Printf("opencode: interrupt %s: %v", m.Name, err)
	}
}

// GracefulStop interrupts a shared Terminal slot's running turn before its pane is killed; the
// caller still kills the pane (false).
func (agentImpl) GracefulStop(m session.Meta) bool {
	interruptTerminal(m)
	return false
}

// --- restart gap ------------------------------------------------------------------

// restoreTerminalEnvs re-sends the environment of every live shared Terminal slot to a daemon
// that has just started (the environment is lost with the process). It is a warm-up, not the
// guarantee: the server plugin makes every shell command ask the Agent to confirm its session's
// environment first (putEnvironmentFor, once per session and server generation), so a PUT that
// fails here leaves the command refused rather than run with the server's environment.
func restoreTerminalEnvs(addr string) {
	for _, m := range terminalSlots() {
		ses := sids.Read(session.UUID(m.Dir, m.Name))
		if ses == "" {
			continue
		}
		vars, found := paneEnv(m.Name)
		if !found {
			log.Printf("opencode: %s: no pane environment to restore", m.Name)
			continue
		}
		var err error
		for try := 0; try < 3; try++ {
			if err = putEnvironment(addr, ses, vars); err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			log.Printf("opencode: %s: restore environment: %v", m.Name, err)
		}
	}
}

// RunPutEnv is `workspace-agent opencode-put-env <slot name> <opencode session id>`, called by
// the server plugin before a shell command of a session whose environment this server generation
// has not been confirmed to hold (a child of a Terminal session, a fork or an older session
// picked in the TUI, anything after a restart). It PUTs the slot's pane environment on that
// session, but only if the session is the slot's: its af.name must be the slot's name, which a
// subagent session inherits from its parent. Exit status 0 means the environment is in place.
func RunPutEnv(args []string) {
	if len(args) != 2 {
		os.Exit(2)
	}
	if err := putEnvironmentFor(serveAddr(), args[0], args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "opencode-put-env: %v\n", err)
		os.Exit(1)
	}
}

// Exit status of `opencode-claim` for a session that is another slot's.
const claimTaken = 3

// RunClaim is `workspace-agent opencode-claim <slot name> <opencode session id>`, the TUI plugin's
// way to take a session its pane shows. Exit 0: the session is the slot's (environment put, tag
// set); 3: it belongs to another slot; anything else: not decided, try again.
func RunClaim(args []string) {
	if len(args) != 2 {
		os.Exit(2)
	}
	switch err := claimSession(serveAddr(), args[0], args[1], claimOpts{putEnv: true}); {
	case err == nil:
	case errors.Is(err, errClaimTaken):
		os.Exit(claimTaken)
	default:
		fmt.Fprintf(os.Stderr, "opencode-claim: %v\n", err)
		os.Exit(1)
	}
}

var errClaimTaken = errors.New("the session belongs to another slot")

// claimOpts: putEnv puts the slot's pane environment (the TUI plugin's claim; the launch has no
// pane yet, its wrapper does it). inheritedFrom is the af.sid a fork carries over from its source:
// that tag alone does not make the session someone else's, only the claim file does.
type claimOpts struct {
	putEnv        bool
	inheritedFrom string
}

// claimSession makes ses the slot's: one winner per session, however many panes show it. The
// decision is an exclusive file create keyed by the session id, taken BEFORE anything is read or
// written, so two panes that both saw the session untagged cannot both put an environment and a
// tag (the tag has no compare-and-set upstream: PATCH replaces the metadata). The losing pane
// changes nothing. A claim that fails before the tag is set is released for the next try.
func claimSession(addr, name, ses string, o claimOpts) error {
	m, ok := session.ReadMeta(name)
	if !ok || m.Kind != session.KindOpencode || m.DriverKind() != session.DriverTUI {
		return fmt.Errorf("%q is not an opencode Terminal slot", name)
	}
	return claimSessionFor(addr, m, ses, o)
}

func claimSessionFor(addr string, m session.Meta, ses string, o claimOpts) error {
	name := m.Name
	sid := session.UUID(m.Dir, m.Name)
	dir := filepath.Join(paths.AgentStateDir(), "opencode-claim")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, url.PathEscape(ses))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	switch {
	case err == nil:
		_, _ = f.WriteString(sid)
		f.Close()
	case errors.Is(err, os.ErrExist):
		if owner, _ := os.ReadFile(path); strings.TrimSpace(string(owner)) != sid {
			return errClaimTaken
		}
	default:
		return err
	}
	info, gerr := getSharedSession(addr, ses)
	if gerr != nil {
		_ = os.Remove(path)
		return gerr
	}
	if other := info.Metadata[metaSID]; other != "" && other != sid && (o.inheritedFrom == "" || other != o.inheritedFrom) {
		return errClaimTaken // the tag is the authority; the file is left as it is
	}
	if info.Metadata[metaManaged] != "" {
		return errClaimTaken
	}
	if o.putEnv {
		vars, found := paneEnv(name)
		if !found {
			_ = os.Remove(path)
			return fmt.Errorf("no pane environment for %s", name)
		}
		if perr := putEnvironment(addr, ses, vars); perr != nil {
			_ = os.Remove(path)
			return perr
		}
	}
	if terr := tagSharedSession(addr, ses, sid, name); terr != nil {
		_ = os.Remove(path)
		return terr
	}
	return nil
}

func putEnvironmentFor(addr, name, ses string) error {
	m, ok := session.ReadMeta(name)
	if !ok || m.Kind != session.KindOpencode || m.DriverKind() != session.DriverTUI {
		return fmt.Errorf("%q is not an opencode Terminal slot", name)
	}
	info, err := getSharedSession(addr, ses)
	if err != nil {
		return err
	}
	if info.Metadata[metaName] != name || info.Metadata[metaSID] != session.UUID(m.Dir, m.Name) {
		return fmt.Errorf("session %s does not belong to %s", ses, name)
	}
	vars, found := paneEnv(name)
	if !found {
		return fmt.Errorf("no pane environment for %s", name)
	}
	return putEnvironment(addr, ses, vars)
}

type daemonInfo struct {
	Version string `json:"version"`
	PID     int    `json:"pid"`
}

// readDaemonInfo is /api/info; the zero value when it cannot be read.
func readDaemonInfo(addr string) daemonInfo {
	var info daemonInfo
	ctx, cancel := controlCtx()
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", addr+"/api/info", nil)
	if err != nil {
		return info
	}
	res, err := serveClient.Do(req)
	if err != nil {
		return info
	}
	defer res.Body.Close()
	if json.NewDecoder(res.Body).Decode(&info) != nil {
		return daemonInfo{}
	}
	return info
}

// daemonPID is the pid /api/info reports (0 when it cannot be read).
func daemonPID(addr string) int { return readDaemonInfo(addr).PID }

func daemonVersion(addr string) string { return readDaemonInfo(addr).Version }

// connectionFlag names the first flag that would point the TUI at another server or session than
// the one the environment was put on, or "".
func connectionFlag(flags []string) string {
	for _, f := range flags {
		name, _, _ := strings.Cut(f, "=")
		switch name {
		case "--server", "--session", "-s", "--continue", "-c", "--standalone", "--fork", "--url", "--attach":
			return f
		}
	}
	return ""
}

package opencode

// The client layer for the shared `opencode serve` daemon: authentication, server major
// version detection, and the v1/v2 route table. serve.go and driver.go call these helpers
// instead of spelling a path, so a route lives in exactly one place per major version.
//
// Measured (1.19.0 and 2.0.28, both with OPENCODE_SERVER_PASSWORD set; user `opencode`):
//   - Both majors answer 401 to every request without Basic auth, the SSE stream included.
//   - The majors disagree about each other's paths without saying so: v2 answers the v1
//     /global/health with 200 text/html (the web UI), and v1 answers /api/info the same
//     way. A status-code-only health check therefore accepts the wrong server, so a probe
//     passes only on a JSON body that names the server (healthyV1 / healthyV2).
//   - v2 has no question endpoints (they became forms) and its PATCH /api/experimental/config
//     takes `shell` only; a provider block is refused with 400. A change that has to reach
//     a running v2 daemon therefore needs a restart (ErrLiveConfigUnsupported).
//   - v2's session list filters on `directory`; `location[directory]` is accepted and
//     silently ignored (returns every project's sessions).
//
// Until the image pin moves (workspace/Dockerfile OPENCODE_VERSION) the v1 routes stay the
// default: serveMajor() is v1 until a probe has seen a v2 server.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/paths"
)

// serveUser is the Basic auth user both majors expect (OPENCODE_SERVER_USERNAME default).
const serveUser = "opencode"

// ErrLiveConfigUnsupported: the running daemon cannot take this configuration change over
// HTTP, so only a new process applies it.
var ErrLiveConfigUnsupported = errors.New("opencode serve cannot take this change live")

// --- authentication -----------------------------------------------------------

var (
	pwMu     sync.Mutex
	pw       string
	pwLoaded bool
)

// servePasswordFile holds the password of the daemon this Agent started, so an Agent that
// restarts can still adopt it (it lives in the config root: it is a credential).
func servePasswordFile() string {
	return filepath.Join(paths.AgentConfigDir(), "opencode-serve-password")
}

// servePassword returns the password the current daemon expects, or "" when none is known
// (a daemon an older Agent started has none).
func servePassword() string {
	pwMu.Lock()
	defer pwMu.Unlock()
	if !pwLoaded {
		pwLoaded = true
		if b, err := os.ReadFile(servePasswordFile()); err == nil {
			pw = strings.TrimSpace(string(b))
		}
	}
	return pw
}

// genServePassword makes a candidate password: 256 random bits, never logged.
func genServePassword() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// useServePassword switches the password requests carry, without touching the file.
func useServePassword(p string) {
	pwMu.Lock()
	pw, pwLoaded = p, true
	pwMu.Unlock()
}

// persistServePassword writes the file atomically (tmp + rename): a half-written file would
// lock a restarted Agent out of the daemon exactly like a lost one.
func persistServePassword(p string) error {
	f := servePasswordFile()
	if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
		return err
	}
	tmp := f + ".tmp"
	if err := os.WriteFile(tmp, []byte(p+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f)
}

// newServePassword generates, adopts and persists a password in one step. ensure does not
// use it: it must not forget the old password before the new daemon has proved itself.
func newServePassword() (string, error) {
	p, err := genServePassword()
	if err != nil {
		return "", err
	}
	if err := persistServePassword(p); err != nil {
		return "", err
	}
	useServePassword(p)
	return p, nil
}

// serveRejectsAuth reports that something on addr answers 401: a daemon is there and holds
// a password this Agent does not have. Starting another one would only collide on the port.
func serveRejectsAuth(addr string) bool {
	for _, p := range []string{"/global/health", "/api/info"} {
		res, err := serveClient.Get(addr + p)
		if err != nil {
			continue
		}
		res.Body.Close()
		if res.StatusCode == http.StatusUnauthorized {
			return true
		}
	}
	return false
}

// authTransport adds Basic auth to every request that goes to the serve daemon. Wrapping
// the transport (rather than each call site) is what keeps the SSE stream, the blocking
// turn call and the OAuth calls from each needing to remember it.
type authTransport struct{ base http.RoundTripper }

func (t authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if p := servePassword(); p != "" && req.Header.Get("Authorization") == "" && sameServeHost(req.URL) {
		req = req.Clone(req.Context())
		req.SetBasicAuth(serveUser, p)
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// sameServeHost keeps the password from leaving for any host but the daemon's own.
func sameServeHost(u *url.URL) bool {
	want, err := url.Parse(serveAddr())
	return err == nil && want.Host == u.Host
}

// --- version detection ----------------------------------------------------------

type apiMajor int32

const (
	apiV1 apiMajor = 1
	apiV2 apiMajor = 2
)

var detectedMajor atomic.Int32

// serveMajor is the major version of the daemon last probed; v1 until a probe says otherwise.
func serveMajor() apiMajor {
	if apiMajor(detectedMajor.Load()) == apiV2 {
		return apiV2
	}
	return apiV1
}

func isV2() bool { return serveMajor() == apiV2 }

// healthyV2 accepts only a JSON /api/info that carries a version of major 2 or later.
func healthyV2(addr string) bool {
	body, ok := fetchJSON(addr + "/api/info")
	if !ok {
		return false
	}
	var info struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(body, &info) != nil || info.Version == "" {
		return false
	}
	major, _, _ := strings.Cut(info.Version, ".")
	n, err := strconv.Atoi(major)
	return err == nil && n >= 2
}

// healthyV1 accepts only the JSON {"healthy":true} /global/health answers.
func healthyV1(addr string) bool {
	body, ok := fetchJSON(addr + "/global/health")
	if !ok {
		return false
	}
	var h struct {
		Healthy bool `json:"healthy"`
	}
	return json.Unmarshal(body, &h) == nil && h.Healthy
}

// fetchJSON is a 200 whose Content-Type is JSON; anything else (an HTML page, 401) is not.
func fetchJSON(u string) ([]byte, bool) {
	res, err := serveClient.Get(u)
	if err != nil {
		return nil, false
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
		return nil, false
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	return b, err == nil
}

// probeServe reports whether a daemon of either major answers at addr, and records which.
// The major seen last is asked first, so a steady-state probe is one request.
func probeServe(addr string) bool {
	order := []apiMajor{apiV1, apiV2}
	if isV2() {
		order = []apiMajor{apiV2, apiV1}
	}
	for _, m := range order {
		if (m == apiV2 && healthyV2(addr)) || (m == apiV1 && healthyV1(addr)) {
			detectedMajor.Store(int32(m))
			return true
		}
	}
	return false
}

// --- route table ----------------------------------------------------------------

func eventURL(addr string) string {
	if isV2() {
		return addr + "/api/event"
	}
	return addr + "/global/event"
}

func sessionURL(addr, ses, dir string) string {
	if isV2() {
		return addr + "/api/session/" + url.PathEscape(ses)
	}
	return dirQ(addr+"/session/"+url.PathEscape(ses), dir)
}

func abortURL(addr, ses, dir string) string {
	if isV2() {
		return addr + "/api/session/" + url.PathEscape(ses) + "/interrupt"
	}
	return dirQ(addr+"/session/"+url.PathEscape(ses)+"/abort", dir)
}

func forkURL(addr, ses, dir string) string {
	if isV2() {
		return addr + "/api/session/" + url.PathEscape(ses) + "/fork"
	}
	return dirQ(addr+"/session/"+url.PathEscape(ses)+"/fork", dir)
}

// forkBody is the fork request; at is the message the copy stops BEFORE (both majors).
func forkBody(at string) (string, error) {
	if at == "" {
		return "{}", nil
	}
	key := "messageID"
	if isV2() {
		key = "before"
	}
	b, err := json.Marshal(map[string]string{key: at})
	return string(b), err
}

// createSessionRequest returns the URL and body that create a session in dir.
func createSessionRequest(addr, dir, title string) (string, []byte) {
	if isV2() {
		// The allow-all ruleset is the --auto of the TUI: the container is the sandbox and
		// nobody can answer a prompt from chat.
		b, _ := json.Marshal(map[string]any{
			"title": title, "location": map[string]string{"directory": dir},
			"permissions": []map[string]string{{"action": "*", "resource": "*", "effect": "allow"}},
		})
		return addr + "/api/session", b
	}
	b, _ := json.Marshal(map[string]string{"title": title})
	return addr + "/session?directory=" + url.QueryEscape(dir), b
}

// sessionID extracts the id from a create/fork response: v2 wraps it in {"data":{…}}.
func sessionID(body io.Reader) string {
	var s struct {
		ID   string `json:"id"`
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.NewDecoder(body).Decode(&s) != nil {
		return ""
	}
	if isV2() {
		return s.Data.ID
	}
	return s.ID
}

// activeSessions lists the sessions the daemon is running, id → status type. An idle
// session is not listed on either major (measured). v1 scopes the list to a project
// (directory); v2's /api/session/active is global.
func activeSessions(addr, dir string) (map[string]string, error) {
	u := dirQ(addr+"/session/status", dir)
	if isV2() {
		u = addr + "/api/session/active"
	}
	res, err := serveClient.Get(u)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("active sessions: HTTP %d", res.StatusCode)
	}
	type st struct {
		Type string `json:"type"`
	}
	var m map[string]st
	if isV2() {
		var w struct {
			Data map[string]st `json:"data"`
		}
		if err := json.NewDecoder(res.Body).Decode(&w); err != nil {
			return nil, err
		}
		m = w.Data
	} else if err := json.NewDecoder(res.Body).Decode(&m); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(m))
	for id, s := range m {
		out[id] = s.Type
	}
	return out, nil
}

func permissionReplyRequest(addr, ses, id, dir string) (string, string) {
	if isV2() {
		return addr + "/api/session/" + url.PathEscape(ses) + "/permission/" + url.PathEscape(id) + "/reply", `{"decision":"always"}`
	}
	return dirQ(addr+"/permission/"+url.PathEscape(id)+"/reply", dir), `{"reply":"always"}`
}

// --- turns ----------------------------------------------------------------------

// newTurnRequest builds the request that submits one input. v1: the blocking
// POST /session/{id}/message, whose agent/model/variant ride in the body. v2:
// POST /api/session/{id}/prompt, which takes text and files only (agent and model are
// session state there, see sendTurn).
func newTurnRequest(addr, ses, dir string, in agents.TurnInput, st agents.ThreadSettings) (*http.Request, error) {
	if isV2() {
		body := map[string]any{"text": strings.TrimSpace(in.Prompt)}
		var files []map[string]string
		for _, p := range in.Attachments {
			if strings.TrimSpace(p) != "" {
				files = append(files, map[string]string{"uri": "file://" + p, "name": filepath.Base(p)})
			}
		}
		if len(files) > 0 {
			body["files"] = files
		}
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		return http.NewRequest("POST", addr+"/api/session/"+url.PathEscape(ses)+"/prompt", bytes.NewReader(buf))
	}
	body := map[string]any{"parts": buildParts(in)}
	if ag := agentForMode(st.Mode); ag != "" {
		body["agent"] = ag
	}
	if prov, model, ok := splitModel(st.Model); ok {
		body["model"] = map[string]string{"providerID": prov, "modelID": model}
	}
	if st.Effort != "" {
		body["variant"] = st.Effort
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return http.NewRequest("POST", dirQ(addr+"/session/"+url.PathEscape(ses)+"/message", dir), bytes.NewReader(buf))
}

// v2 turn pacing. /prompt only queues the input (its answer is the stored user message), so
// the end of the turn is read off /api/session/active: it has to be seen running first, and
// then gone. Not measured against a model turn (no credentials in the measuring sandbox);
// the exit conditions below are the conservative reading of the spec.
var (
	v2PollEvery   = 500 * time.Millisecond
	v2StartWithin = 10 * time.Second
	v2PollMisses  = 3
)

// sendTurn submits the input and returns once the turn has ended, shaped like v1's blocking
// answer so the caller's state machine is the same on both majors. A transport failure
// (the daemon went away) is an error, exactly as on v1.
func sendTurn(addr, ses, dir string, req *http.Request, st agents.ThreadSettings) (*http.Response, error) {
	if !isV2() {
		return turnClient.Do(req)
	}
	if err := applyV2Settings(addr, ses, st); err != nil {
		return nil, err
	}
	res, err := turnClient.Do(req)
	if err != nil || res.StatusCode >= 300 {
		return res, err
	}
	promptBody, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	res.Body.Close()
	var userMsg struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(promptBody, &userMsg)
	gen := Serve().Generation()
	seen, misses := false, 0
	started := time.Now()
	for {
		m, err := activeSessions(addr, dir)
		// A new generation means the daemon this turn ran on is gone, whatever the new one
		// reports (v1 sees the same thing as a transport break). Checked after the read so
		// that an empty list from the replacement is never taken for "finished".
		if Serve().Generation() != gen {
			return nil, errors.New("opencode serve was replaced during the turn")
		}
		switch {
		case err != nil:
			if misses++; misses >= v2PollMisses {
				return nil, err
			}
		case m[ses] != "":
			seen, misses = true, 0
		case seen:
			// Provider failures are not visible here (v2 reports them on the event stream,
			// #2103), so an ended turn reads as completed.
			return v2Done(), nil
		case time.Since(started) > v2StartWithin:
			// Never seen running: either it finished before the first poll or it never
			// started. Only an assistant message newer than our input says which.
			if v2Answered(addr, ses, userMsg.Data.ID) {
				return v2Done(), nil
			}
			return nil, errors.New("opencode v2 turn state unknown: the session never ran")
		default:
			misses = 0
		}
		time.Sleep(v2PollEvery)
	}
}

func v2Done() *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}
}

// v2Answered reports whether the session holds an assistant message newer than after (the
// id /prompt returned; message ids sort by time).
func v2Answered(addr, ses, after string) bool {
	res, err := serveClient.Get(addr + "/api/session/" + url.PathEscape(ses) + "/message?order=desc&limit=5")
	if err != nil {
		return false
	}
	defer res.Body.Close()
	var w struct {
		Data []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"data"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&w) != nil || after == "" {
		return false
	}
	for _, m := range w.Data {
		if m.Type == "assistant" && m.ID > after {
			return true
		}
	}
	return false
}

// applyV2Settings pushes the settings v1 carried in each message body: v2 keeps agent and
// model on the session.
func applyV2Settings(addr, ses string, st agents.ThreadSettings) error {
	post := func(path string, body any) error {
		buf, _ := json.Marshal(body)
		res, err := serveClient.Post(addr+"/api/session/"+url.PathEscape(ses)+path, "application/json", bytes.NewReader(buf))
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if res.StatusCode >= 300 {
			return fmt.Errorf("opencode session%s: HTTP %d", path, res.StatusCode)
		}
		return nil
	}
	if ag := agentForMode(st.Mode); ag != "" {
		if err := post("/agent", map[string]string{"agent": ag}); err != nil {
			return err
		}
	}
	if prov, model, ok := splitModel(st.Model); ok {
		ref := map[string]string{"providerID": prov, "id": model}
		if st.Effort != "" {
			ref["variant"] = st.Effort
		}
		return post("/model", map[string]any{"model": ref})
	}
	return nil
}

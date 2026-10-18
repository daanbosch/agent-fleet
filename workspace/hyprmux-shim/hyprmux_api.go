package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// hyprmuxCLI is the only route the shim has into the host's hyprmux: the installed CLI, run
// with explicit argv and never through a shell or the toolbar.
type hyprmuxCLI interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

const (
	cliTimeout      = 30 * time.Second
	maxBodyBytes    = 64 << 10
	maxContentBytes = 8 << 10
	maxTitleBytes   = 512
)

var (
	itemKind     = regexp.MustCompile(`^[a-z][a-z-]{0,31}$`)
	launchAgents = map[string]bool{"pi": true, "codex": true, "claude": true}
	launchModes  = map[string]bool{"normal": true, "yolo": true}
)

type execCLI struct{ bin string }

func (c execCLI) Run(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, cliTimeout)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, c.bin, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("hyprmux %s failed: %s", args[0], cliError(stderr.String(), err))
	}
	return out, nil
}

// cliError keeps the first stderr line the CLI wrote, bounded, so a failure reads as the
// CLI's own words without dumping its output.
func cliError(stderr string, err error) string {
	line := strings.TrimSpace(strings.SplitN(stderr, "\n", 2)[0])
	if line == "" {
		return err.Error()
	}
	if len(line) > 200 {
		line = line[:200]
	}
	return line
}

// discoverHyprmuxCLI returns nil when no hyprmux is on PATH; the handlers then answer 503.
func discoverHyprmuxCLI() hyprmuxCLI {
	bin, err := exec.LookPath("hyprmux")
	if err != nil {
		return nil
	}
	return execCLI{bin: bin}
}

// operationKey is derived from the request itself, so a retried POST carries the same key and
// hyprmux treats it as the same operation.
func operationKey(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = io.WriteString(h, p)
		_, _ = h.Write([]byte{0})
	}
	return "shim-" + hex.EncodeToString(h.Sum(nil))[:40]
}

// cliOr503 is the fail-closed gate: without the CLI nothing can be read or written.
func (s *server) cliOr503(w http.ResponseWriter) (hyprmuxCLI, bool) {
	if s.cfg.CLI == nil {
		writeError(w, http.StatusServiceUnavailable, "hyprmux CLI is not on PATH for the shim; the hyprmux API is unavailable")
		return nil, false
	}
	return s.cfg.CLI, true
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// writeCLIJSON relays the CLI's JSON answer. Anything else is a CLI fault, reported as one.
func writeCLIJSON(w http.ResponseWriter, out []byte, err error) {
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if !json.Valid(out) {
		writeError(w, http.StatusBadGateway, "hyprmux returned output that is not JSON")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// statusWire is the console-facing part of `hyprmux status --json`. The rest of that output
// carries session prompts and project roots, which nothing here needs.
type statusWire struct {
	UpdatedAt string          `json:"updatedAt"`
	Hall      json.RawMessage `json:"hall"`
	Projects  []projectWire   `json:"projects"`
}

type projectWire struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Slug  string `json:"slug"`
	Group string `json:"group"`
}

func (s *server) loadStatus(w http.ResponseWriter, r *http.Request) (statusWire, bool) {
	cli, ok := s.cliOr503(w)
	if !ok {
		return statusWire{}, false
	}
	out, err := cli.Run(r.Context(), "status", "--json")
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return statusWire{}, false
	}
	var st statusWire
	if err := json.Unmarshal(out, &st); err != nil {
		writeError(w, http.StatusBadGateway, "hyprmux status is not the expected JSON")
		return statusWire{}, false
	}
	return st, true
}

func (s *server) hyprmuxStatus(w http.ResponseWriter, r *http.Request) {
	if st, ok := s.loadStatus(w, r); ok {
		writeJSON(w, http.StatusOK, st)
	}
}

func (s *server) hyprmuxProjects(w http.ResponseWriter, r *http.Request) {
	if st, ok := s.loadStatus(w, r); ok {
		writeJSON(w, http.StatusOK, map[string]any{"projects": st.Projects})
	}
}

// sessionWireRow is the part of `hyprmux session list --json` the console reads. Prompts,
// transcripts and working directories are left out.
type sessionWireRow struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	ProjectID string `json:"projectId"`
	Agent     string `json:"agent"`
	UpdatedAt string `json:"updatedAt"`
	External  bool   `json:"external"`
}

func listSessions(r *http.Request, cli hyprmuxCLI) ([]sessionWireRow, error) {
	out, err := cli.Run(r.Context(), "session", "list", "--json")
	if err != nil {
		return nil, err
	}
	var rows []sessionWireRow
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, errors.New("hyprmux session list is not the expected JSON")
	}
	return rows, nil
}

func (s *server) roomItems(w http.ResponseWriter, r *http.Request) {
	cli, ok := s.cliOr503(w)
	if !ok {
		return
	}
	project := r.URL.Query().Get("project")
	if !validName.MatchString(project) {
		writeError(w, http.StatusBadRequest, "project query parameter is required and must be a project id")
		return
	}
	out, err := cli.Run(r.Context(), "room", "item", "list", "--project", project, "--json")
	writeCLIJSON(w, out, err)
}

type roomItemReq struct {
	Project string `json:"project"`
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Content string `json:"content"`
}

func (s *server) roomItemCreate(w http.ResponseWriter, r *http.Request) {
	cli, ok := s.cliOr503(w)
	if !ok {
		return
	}
	var req roomItemReq
	if !decodeBody(w, r, &req) {
		return
	}
	switch {
	case !validName.MatchString(req.Project):
		writeError(w, http.StatusBadRequest, "project must be a project id")
	case !itemKind.MatchString(req.Kind):
		writeError(w, http.StatusBadRequest, "kind must be lowercase letters and hyphens")
	case !validText(req.Title, maxTitleBytes):
		writeError(w, http.StatusBadRequest, "title is required and must be at most 512 bytes of text")
	case !validText(req.Content, maxContentBytes):
		writeError(w, http.StatusBadRequest, "content is required and must be at most 8192 bytes of text")
	default:
		key := operationKey("room-item", req.Project, req.Kind, req.Title, req.Content)
		out, err := cli.Run(r.Context(), "room", "item", "create", "--project", req.Project,
			"--kind", req.Kind, "--title", req.Title, "--content", req.Content, "--operation-key", key, "--json")
		writeCLIJSON(w, out, err)
	}
}

// validText accepts non-empty UTF-8 text without NUL bytes that fits in max bytes.
func validText(s string, max int) bool {
	return s != "" && len(s) <= max && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

type sessionLaunchReq struct {
	Project  string  `json:"project"`
	Agent    string  `json:"agent"`
	Mode     string  `json:"mode"`
	Prompt   *string `json:"prompt"`
	Headless *bool   `json:"headless"`
}

// sessionLaunch runs `hyprmux session launch`, which has no prompt or headless option. A
// request that asks for either is refused rather than launched without it.
func (s *server) sessionLaunch(w http.ResponseWriter, r *http.Request) {
	cli, ok := s.cliOr503(w)
	if !ok {
		return
	}
	var req sessionLaunchReq
	if !decodeBody(w, r, &req) {
		return
	}
	switch {
	case !validName.MatchString(req.Project):
		writeError(w, http.StatusBadRequest, "project must be a project id")
	case !launchAgents[req.Agent]:
		writeError(w, http.StatusBadRequest, "agent must be pi, codex or claude")
	case !launchModes[req.Mode]:
		writeError(w, http.StatusBadRequest, "mode must be normal or yolo")
	case req.Prompt != nil || req.Headless != nil:
		writeError(w, http.StatusNotImplemented, "hyprmux session launch takes no prompt and no headless option (only -project, -agent, -mode, -slot); a prompt-bearing run is hyprmux room run start --item, which this shim does not start")
	default:
		out, err := cli.Run(r.Context(), "session", "launch", "-project", req.Project, "-agent", req.Agent, "-mode", req.Mode, "-json")
		writeCLIJSON(w, out, err)
	}
}

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// continueAgents are the Pi profiles the toolbar's continue-pi accepts (piAgentProfiles in
// tools/agent-toolbar/main.go). A profile added there must be added here too.
var continueAgents = map[string]bool{
	"pi": true, "pi-astra": true, "pi-glm53-flash": true, "pi-opus55": true, "pi-opus5-max": true,
	"pi-sonnet5-max": true, "pi-daybreak": true, "pi-sol61": true, "pi-sol": true, "pi-luna": true,
	"pi-terra": true, "pi-gpt55": true, "pi-gpt54": true, "pi-gpt54-mini": true, "pi-spark": true,
	"pi-fable": true, "pi-fable-5-1": true, "pi-fable-loop": true, "pi-fable-loop-kimi": true,
	"pi-kimi-k3": true, "pi-haiku55": true, "pi-opus5": true, "pi-max": true,
}

type continueReq struct {
	Prompt *string `json:"prompt"`
	Agent  string  `json:"agent"`
	Mode   string  `json:"mode"`
}

// continueSession routes by the source's agent. A Pi session is resumed in place with
// `hyprmux session resume`, so the prompt becomes its next turn and its profile and mode stay.
// Any other toolbar-managed session is continued into a new Pi session with
// `hyprmux session continue-pi`, which leaves the source running. Only toolbar-managed sessions
// can be continued; an imported host session is refused before anything spawns.
func (s *server) continueSession(w http.ResponseWriter, r *http.Request) {
	row, ok := s.findRow(w, r)
	if !ok {
		return
	}
	cli, ok := s.cliOr503(w)
	if !ok {
		return
	}
	var req continueReq
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Agent == "" {
		req.Agent = "pi"
	}
	if req.Mode == "" {
		req.Mode = "normal"
	}
	if msg := continueRefusal(req); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	source, managed, err := toolbarManaged(r, cli, row.ID)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if !managed {
		writeError(w, http.StatusConflict, fmt.Sprintf("session %s is an imported host session, not toolbar-managed; only toolbar-managed sessions can be continued", row.ID))
		return
	}
	if continueAgents[source.Agent] {
		resumePiSource(w, r, cli, source, req.Prompt)
		return
	}
	args := []string{"session", "continue-pi", "-json", "-agent=" + req.Agent, "-mode=" + req.Mode}
	if req.Prompt != nil {
		args = append(args, "-prompt="+strings.TrimSpace(*req.Prompt))
	}
	out, err := cli.Run(r.Context(), append(args, row.ID)...)
	writeContinued(w, out, err)
}

// resumePiSource resumes a Pi session in place. Its agent and mode apply only to continue-pi,
// so they are not sent here. A prompt must be confirmed by the daemon, which an older daemon
// does not do: it ignores the unknown key, and the shim then refuses rather than report success.
func resumePiSource(w http.ResponseWriter, r *http.Request, cli hyprmuxCLI, source sessionWireRow, prompt *string) {
	args := []string{"session", "resume", "-json"}
	if prompt != nil {
		args = append(args, "-prompt="+strings.TrimSpace(*prompt))
	}
	out, err := cli.Run(r.Context(), append(args, source.ID)...)
	if err != nil {
		writeError(w, http.StatusBadGateway, "route session resume: "+err.Error())
		return
	}
	var result struct {
		Prompt bool `json:"prompt"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		writeError(w, http.StatusBadGateway, "route session resume: hyprmux session resume did not answer JSON")
		return
	}
	if prompt != nil && !result.Prompt {
		writeError(w, http.StatusBadGateway, "route session resume: hyprmux did not confirm the prompt was delivered; upgrade the hyprmux daemon")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": source.ID, "title": source.Title, "agent": source.Agent, "route": "session resume"})
}

func continueRefusal(req continueReq) string {
	switch {
	case !continueAgents[req.Agent]:
		return "agent must be a Pi profile such as pi or pi-sol"
	case !launchModes[req.Mode]:
		return "mode must be normal or yolo"
	case req.Prompt != nil && !validText(strings.TrimSpace(*req.Prompt), maxContentBytes):
		return "prompt must be at most 8192 bytes of text, not blank and without NUL"
	}
	return ""
}

// toolbarManaged returns the CLI's session row for id and whether it is toolbar-managed. An id the
// list does not know, or one flagged external (a manually launched session), is not managed.
// Version note: this refusal depends on the hyprmux CLI's session list emitting `external`
// (daemon 4d39ea3 or newer). Older CLIs mark every row unmarked, so an external session would
// parse as managed here; the shim therefore requires the CLI to be current (see README) before
// enabling continue endpoints.
func toolbarManaged(r *http.Request, cli hyprmuxCLI, id string) (sessionWireRow, bool, error) {
	rows, err := listSessions(r, cli)
	if err != nil {
		return sessionWireRow{}, false, err
	}
	for _, row := range rows {
		if row.ID == id {
			return row, !row.External, nil
		}
	}
	return sessionWireRow{}, false, nil
}

// writeContinued answers with the new session's id, title and agent. The CLI's session also
// carries the continuation prompt and host paths, which the console does not need.
func writeContinued(w http.ResponseWriter, out []byte, err error) {
	if err != nil {
		writeError(w, http.StatusBadGateway, "route continue-pi: "+err.Error())
		return
	}
	var created struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		Agent string `json:"agent"`
	}
	if err := json.Unmarshal(out, &created); err != nil || created.ID == "" {
		writeError(w, http.StatusBadGateway, "hyprmux session continue-pi did not return a session id")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": created.ID, "title": created.Title, "agent": created.Agent})
}

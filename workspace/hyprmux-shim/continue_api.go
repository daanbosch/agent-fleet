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

// continueSession runs `hyprmux session continue-pi`, which starts a new Pi session from this
// session's transcript and leaves this one running. Only toolbar-managed sessions can be
// continued; an imported host session is refused before anything spawns.
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
	managed, err := toolbarManaged(r, cli, row.ID)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if !managed {
		writeError(w, http.StatusConflict, fmt.Sprintf("session %s is an imported host session, not toolbar-managed; only toolbar-managed sessions can be continued", row.ID))
		return
	}
	args := []string{"session", "continue-pi", "-json", "-agent=" + req.Agent, "-mode=" + req.Mode}
	if req.Prompt != nil {
		args = append(args, "-prompt="+strings.TrimSpace(*req.Prompt))
	}
	out, err := cli.Run(r.Context(), append(args, row.ID)...)
	writeContinued(w, out, err)
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

// toolbarManaged reports whether the CLI's session list has id as a toolbar session. An id the
// list does not know, or one flagged external (a manually launched session), is not managed.
func toolbarManaged(r *http.Request, cli hyprmuxCLI, id string) (bool, error) {
	rows, err := listSessions(r, cli)
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		if row.ID == id {
			return !row.External, nil
		}
	}
	return false, nil
}

// writeContinued answers with the new session's id, title and agent. The CLI's session also
// carries the continuation prompt and host paths, which the console does not need.
func writeContinued(w http.ResponseWriter, out []byte, err error) {
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
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

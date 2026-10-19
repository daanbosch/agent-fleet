package main

import (
	"net/http"
	"strings"
)

type launchFastVMReq struct {
	Project string  `json:"project"`
	Mode    string  `json:"mode"`
	Prompt  *string `json:"prompt"`
}

// launchFastVM starts a Pi session inside a fastvm warm clone through `hyprmux session launch
// -fastvm`. A fastvm session runs Pi only and needs a prompt, so a request without one is
// refused before the CLI runs.
func (s *server) launchFastVM(w http.ResponseWriter, r *http.Request) {
	cli, ok := s.cliOr503(w)
	if !ok {
		return
	}
	var req launchFastVMReq
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Mode == "" {
		req.Mode = "normal"
	}
	switch {
	case !validName.MatchString(req.Project):
		writeError(w, http.StatusBadRequest, "project must be a project id")
	case !launchModes[req.Mode]:
		writeError(w, http.StatusBadRequest, "mode must be normal or yolo")
	case req.Prompt == nil || !validText(strings.TrimSpace(*req.Prompt), maxContentBytes):
		writeError(w, http.StatusBadRequest, "prompt is required: at most 8192 bytes of text, not blank and without NUL")
	default:
		out, err := cli.Run(r.Context(), "session", "launch", "-project", req.Project, "-agent", "pi", "-mode", req.Mode, "-fastvm", "-json", "--", strings.TrimSpace(*req.Prompt))
		writeCLIJSON(w, out, err)
	}
}

package main

import (
	"net/http"
	"regexp"
	"strings"
)

// transcriptPart and transcriptTurn are the subset of the workspace agent's transcript wire
// shape the Console's chat mirror renders.
type transcriptPart struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

type transcriptTurn struct {
	Role  string           `json:"role"`
	Parts []transcriptPart `json:"parts"`
}

// ansiSeq matches the escape sequences a terminal writer leaves in a typescript, and the
// carriage returns of its CRLF line endings.
var ansiSeq = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-Z\\-_]|\r`)

var backtickRun = regexp.MustCompile("`+")

// messages answers the Console's chat mirror. A headless writer's log is the only transcript
// the shim has, so it comes back as one assistant turn. The mirror's cursor means nothing
// here, so every answer is a reset of the tail (at most defaultTailBytes). A kitty window
// writes no log and answers an empty transcript.
func (s *server) messages(w http.ResponseWriter, r *http.Request) {
	row, ok := s.findRow(w, r)
	if !ok {
		return
	}
	turns := []transcriptTurn{}
	if isHeadless(row) {
		tail, _, err := s.readLog(row.ID, defaultTailBytes)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if text := strings.TrimSpace(stripANSI(tail)); text != "" {
			turns = append(turns, transcriptTurn{
				Role:  "assistant",
				Parts: []transcriptPart{{Kind: "text", Text: fenceLog(text)}},
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name": row.ID, "messages": turns, "cursor": 0, "reset": true, "firstLine": 0, "hasMore": false,
	})
}

func stripANSI(s string) string { return ansiSeq.ReplaceAllString(s, "") }

// fenceLog wraps text in a Markdown code fence longer than any backtick run inside it, so
// the log cannot close its own fence.
func fenceLog(text string) string {
	longest := 0
	for _, run := range backtickRun.FindAllString(text, -1) {
		longest = max(longest, len(run))
	}
	fence := strings.Repeat("`", max(3, longest+1))
	return fence + "text\n" + text + "\n" + fence
}

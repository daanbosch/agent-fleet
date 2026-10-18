package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"sort"
	"strings"
)

// checkpointWire is the part of the hall's Telegram checkpoint the console reads. Chat and
// topic ids, the hub socket and the source cursors stay on the host; only counts and booleans
// leave the shim.
type checkpointWire struct {
	Version     int `json:"Version"`
	Destination struct {
		Chat  string `json:"Chat"`
		Topic string `json:"Topic"`
	} `json:"Destination"`
	Projects map[string]struct {
		Topic string `json:"Topic"`
	} `json:"Projects"`
	Sources map[string]json.RawMessage `json:"Sources"`
}

// loadCheckpoint reports present=false for a missing checkpoint: a host that has never bound
// Telegram is a state, not a fault. Errors never carry the path.
func (s *server) loadCheckpoint() (checkpointWire, bool, error) {
	var cp checkpointWire
	b, err := os.ReadFile(s.cfg.TelegramCheckpoint)
	if errors.Is(err, fs.ErrNotExist) {
		return cp, false, nil
	}
	if err != nil {
		return cp, false, errors.New("telegram checkpoint is unreadable")
	}
	if err := json.Unmarshal(b, &cp); err != nil {
		return cp, false, errors.New("telegram checkpoint is not the expected JSON")
	}
	return cp, true, nil
}

func (s *server) telegramStatus(w http.ResponseWriter, r *http.Request) {
	cli, ok := s.cliOr503(w)
	if !ok {
		return
	}
	if s.cfg.TelegramCheckpoint == "" {
		writeError(w, http.StatusServiceUnavailable, "HOME is unset for the shim, so the Telegram checkpoint location is unknown")
		return
	}
	cp, present, err := s.loadCheckpoint()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	rows, err := listSessions(r, cli)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	supervisors := []sessionWireRow{}
	for _, row := range rows {
		if strings.Contains(row.Title, "· Supervisor ·") {
			supervisors = append(supervisors, row)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"checkpoint": map[string]any{
			"present":               present,
			"version":               cp.Version,
			"projects":              len(cp.Projects),
			"sources":               len(cp.Sources),
			"destinationConfigured": cp.Destination.Chat != "" && cp.Destination.Topic != "",
		},
		"supervisors": supervisors,
	})
}

func (s *server) telegramTopics(w http.ResponseWriter, _ *http.Request) {
	if s.cfg.TelegramCheckpoint == "" {
		writeError(w, http.StatusServiceUnavailable, "HOME is unset for the shim, so the Telegram checkpoint location is unknown")
		return
	}
	cp, present, err := s.loadCheckpoint()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	ids := make([]string, 0, len(cp.Projects))
	for id := range cp.Projects {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	topics := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		topics = append(topics, map[string]any{"projectId": id, "hasTopic": cp.Projects[id].Topic != ""})
	}
	writeJSON(w, http.StatusOK, map[string]any{"present": present, "topics": topics})
}

// telegramTopicMessage is refused, not sent. hub.ts registers a sendOnly connection after
// checking only the shape of chatId and topicId (hub.ts:340-349), so the shim could name any
// topic. Which topic it may speak for is an authorization decision this slice does not make,
// and a Telegram post to a live chat is not something to send on a guess.
func (s *server) telegramTopicMessage(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotImplemented, "not sent: the hub's sendOnly registration (hub.ts:340-349) checks only the shape of chatId and topicId, so the shim would be choosing the topic itself; which route it may speak for is not decided yet")
}

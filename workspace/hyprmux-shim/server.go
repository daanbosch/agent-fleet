package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
)

const (
	defaultTailBytes = 64 << 10
	maxTailBytes     = 1 << 20
)

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type config struct {
	Token              string
	RegistryPath       string
	LogsDir            string
	CLI                hyprmuxCLI
	TelegramCheckpoint string
}

type server struct {
	cfg config
}

// newHandler serves the read-only surface the console uses. /healthz is open because the
// Control Plane's health probe sends no credentials; everything else needs the bearer token.
func newHandler(cfg config) http.Handler {
	s := &server{cfg: cfg}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /sessions", s.auth(s.list))
	mux.HandleFunc("GET /sessions/{name}", s.auth(s.detail))
	mux.HandleFunc("GET /sessions/{name}/log", s.auth(s.logTail))
	mux.HandleFunc("GET /sessions/{name}/messages", s.auth(s.messages))
	mux.HandleFunc("GET /ws/pty", s.auth(s.attachRefused))
	mux.HandleFunc("GET /api/hyprmux/status", s.auth(s.hyprmuxStatus))
	mux.HandleFunc("GET /api/hyprmux/projects", s.auth(s.hyprmuxProjects))
	mux.HandleFunc("GET /api/hyprmux/room-items", s.auth(s.roomItems))
	mux.HandleFunc("POST /api/hyprmux/room-item", s.auth(s.roomItemCreate))
	mux.HandleFunc("POST /api/hyprmux/session-launch", s.auth(s.sessionLaunch))
	mux.HandleFunc("POST /sessions/{name}/continue", s.auth(s.continueSession))
	mux.HandleFunc("POST /sessions/launch-fastvm", s.auth(s.launchFastVM))
	mux.HandleFunc("GET /api/telegram/status", s.auth(s.telegramStatus))
	mux.HandleFunc("GET /api/telegram/topics", s.auth(s.telegramTopics))
	mux.HandleFunc("POST /api/telegram/topic-message", s.auth(s.telegramTopicMessage))
	return mux
}

func (s *server) auth(h http.HandlerFunc) http.HandlerFunc {
	want := []byte("Bearer " + s.cfg.Token)
	return func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			writeError(w, http.StatusUnauthorized, "bearer token required")
			return
		}
		h(w, r)
	}
}

func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{"ok": true, "sessions": 0, "alive": 0, "headless": 0}
	rows, err := loadRegistry(s.cfg.RegistryPath)
	body["registryError"] = err != nil
	alive, headless := 0, 0
	for _, row := range rows {
		if viewOf(row).Alive {
			alive++
		}
		if isHeadless(row) {
			headless++
		}
	}
	body["sessions"] = len(rows)
	body["alive"] = alive
	body["headless"] = headless
	writeJSON(w, http.StatusOK, body)
}

func (s *server) list(w http.ResponseWriter, r *http.Request) {
	rows, err := loadRegistry(s.cfg.RegistryPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]sessionView, 0, len(rows))
	for _, row := range rows {
		out = append(out, viewOf(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

type sessionDetail struct {
	sessionView
	LogAvailable bool   `json:"logAvailable"`
	LogTail      string `json:"logTail,omitempty"`
}

func (s *server) detail(w http.ResponseWriter, r *http.Request) {
	row, ok := s.findRow(w, r)
	if !ok {
		return
	}
	d := sessionDetail{sessionView: viewOf(row)}
	if isHeadless(row) {
		tail, found, err := s.readLog(row.ID, defaultTailBytes)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		d.LogAvailable = found
		d.LogTail = tail
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *server) logTail(w http.ResponseWriter, r *http.Request) {
	row, ok := s.findRow(w, r)
	if !ok {
		return
	}
	if !isHeadless(row) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("session %s is a kitty window and writes no log the shim can read", row.ID))
		return
	}
	n, err := tailBytesParam(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	tail, found, err := s.readLog(row.ID, n)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, fmt.Sprintf("session %s has no log yet", row.ID))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, tail)
}

// attachRefused is the honest answer to a terminal attach: host sessions are kitty windows
// or detached headless writers, and neither has a PTY the shim can reach.
func (s *server) attachRefused(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("session")
	if name == "" {
		writeError(w, http.StatusBadRequest, "session query parameter is required")
		return
	}
	if !validName.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid session name")
		return
	}
	row, found, err := s.row(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "no such session")
		return
	}
	writeError(w, http.StatusConflict, fmt.Sprintf("host session %s is a kitty window or a detached headless writer; log view only", row.ID))
}

// findRow validates {name} and resolves it to its registry row, writing the 400/404/500
// answer itself when there is none.
func (s *server) findRow(w http.ResponseWriter, r *http.Request) (registryRow, bool) {
	name := r.PathValue("name")
	if !validName.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid session name")
		return registryRow{}, false
	}
	row, found, err := s.row(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return registryRow{}, false
	}
	if !found {
		writeError(w, http.StatusNotFound, "no such session")
		return registryRow{}, false
	}
	return row, true
}

func (s *server) row(name string) (registryRow, bool, error) {
	rows, err := loadRegistry(s.cfg.RegistryPath)
	if err != nil {
		return registryRow{}, false, err
	}
	for _, row := range rows {
		if row.ID == name {
			return row, true, nil
		}
	}
	return registryRow{}, false, nil
}

// logPath re-checks the id against validName itself, so a crafted id cannot step outside
// LogsDir even where a caller skipped the route's validation.
func (s *server) logPath(id string) (string, error) {
	if !validName.MatchString(id) {
		return "", errors.New("invalid session name")
	}
	return filepath.Join(s.cfg.LogsDir, id+".log"), nil
}

func (s *server) readLog(id string, n int) (string, bool, error) {
	path, err := s.logPath(id)
	if err != nil {
		return "", false, err
	}
	return readTail(path, n)
}

func tailBytesParam(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("bytes")
	if raw == "" {
		return defaultTailBytes, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 || n > maxTailBytes {
		return 0, fmt.Errorf("bytes must be between 1 and %d", maxTailBytes)
	}
	return n, nil
}

// readTail returns up to the last n bytes of path. A tail that starts mid-line loses that
// partial line. found is false when the log does not exist.
func readTail(path string, n int) (string, bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", false, err
	}
	start := max(fi.Size()-int64(n), 0)
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return "", false, err
	}
	buf, err := io.ReadAll(io.LimitReader(f, fi.Size()-start))
	if err != nil {
		return "", false, err
	}
	if start > 0 {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			return "", true, nil
		}
		buf = buf[i+1:]
	}
	return string(buf), true, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// registryRow is one entry of the hyprmux session registry. The prompt is not decoded:
// nothing the console shows needs it, and it is user content.
type registryRow struct {
	ID           string `json:"id"`
	Agent        string `json:"agent"`
	AgentVersion string `json:"agentVersion"`
	Mode         string `json:"mode"`
	Terminal     string `json:"terminal"`
	Title        string `json:"title"`
	Workdir      string `json:"workdir"`
	Status       string `json:"status"`
	PID          int    `json:"pid"`
	CreatedAt    string `json:"createdAt"`
	// PiSessionFile is the toolbar-persisted pi session JSONL of a pi row. A kitty row
	// has no log the shim can read — this file is the only real transcript it has.
	PiSessionFile string `json:"piSessionFile,omitempty"`
}

type registryFile struct {
	Sessions []registryRow `json:"sessions"`
}

// loadRegistry reads the registry fresh on every call: the toolbar rewrites it as sessions
// start and exit, and the shim keeps no copy of its own.
func loadRegistry(path string) ([]registryRow, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read registry: %w", err)
	}
	var f registryFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse registry %s: %w", path, err)
	}
	return f.Sessions, nil
}

// sessionView is the console-facing projection of a registry row. Name is the registry id.
// Kind is the agent name; the console maps kinds it does not know to a generic descriptor.
// Attachable is always false: no terminal attach path exists for host sessions yet.
type sessionView struct {
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	Agent        string `json:"agent"`
	AgentVersion string `json:"agentVersion,omitempty"`
	Mode         string `json:"mode"`
	Terminal     string `json:"terminal"`
	Title        string `json:"title"`
	Display      string `json:"display"`
	Dir          string `json:"dir"`
	Repo         string `json:"repo"`
	Status       string `json:"status"`
	Alive        bool   `json:"alive"`
	Attachable   bool   `json:"attachable"`
	CreatedAt    string `json:"createdAt"`
}

// viewOf maps a registry row. A row without a terminal field is a kitty window: the
// toolbar omits the field for the default.
func viewOf(r registryRow) sessionView {
	terminal := r.Terminal
	if terminal == "" {
		terminal = "kitty"
	}
	repo := ""
	if r.Workdir != "" {
		repo = filepath.Base(r.Workdir)
	}
	display := r.Title
	if display == "" {
		display = repo
	}
	if display == "" {
		display = r.ID
	}
	return sessionView{
		Name:         r.ID,
		Kind:         r.Agent,
		Agent:        r.Agent,
		AgentVersion: r.AgentVersion,
		Mode:         r.Mode,
		Terminal:     terminal,
		Title:        r.Title,
		Display:      display,
		Dir:          r.Workdir,
		Repo:         repo,
		Status:       r.Status,
		Alive:        r.Status == "running" && pidAlive(r.PID),
		CreatedAt:    r.CreatedAt,
	}
}

func isHeadless(r registryRow) bool { return r.Terminal == "headless" }

// pidAlive reports whether pid exists (EPERM means it exists under another uid). A reused
// pid reads as alive; the registry is display-only, so that is accepted.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

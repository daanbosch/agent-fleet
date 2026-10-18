package main

import (
	"os"
	"path/filepath"
	"testing"
)

const fixtureRegistry = "testdata/sessions-v4.json"

func TestLoadRegistryReadsRows(t *testing.T) {
	rows, err := loadRegistry(fixtureRegistry)
	if err != nil {
		t.Fatalf("loadRegistry: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4", len(rows))
	}
	if rows[2].ID != "h-pi-0003" || rows[2].Terminal != "headless" {
		t.Fatalf("row 3 = %+v", rows[2])
	}
}

func TestLoadRegistryMissingFileIsEmpty(t *testing.T) {
	rows, err := loadRegistry(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("missing registry should read as empty, got %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %d, want 0", len(rows))
	}
}

func TestLoadRegistryMalformedFails(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sessions-v4.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRegistry(p); err == nil {
		t.Fatal("malformed registry: want error, got nil")
	}
}

func TestViewMapsRegistryRow(t *testing.T) {
	rows, err := loadRegistry(fixtureRegistry)
	if err != nil {
		t.Fatal(err)
	}

	claude := viewOf(rows[0])
	if claude.Name != "h-claude-0001" || claude.Kind != "claude" || claude.Terminal != "kitty" {
		t.Errorf("row 1 view = %+v", claude)
	}
	if claude.Dir != "/work/alpha" || claude.Repo != "alpha" || claude.Display != "alpha work" {
		t.Errorf("row 1 paths/display = %+v", claude)
	}
	if !claude.Alive || claude.Attachable {
		t.Errorf("row 1 alive=%v attachable=%v, want alive and not attachable", claude.Alive, claude.Attachable)
	}

	codex := viewOf(rows[1])
	if codex.Alive || codex.Terminal != "kitty" {
		t.Errorf("row 2 (exited, explicit kitty) = %+v", codex)
	}

	headless := viewOf(rows[2])
	if !headless.Alive || headless.Kind != "pi" || headless.Terminal != "headless" || headless.Display != "gamma pi" {
		t.Errorf("row 3 = %+v", headless)
	}

	bare := viewOf(rows[3])
	if bare.Dir != "" || bare.Repo != "" || bare.Display != "h-pi-0004" {
		t.Errorf("row 4 without workdir or title = %+v", bare)
	}
}

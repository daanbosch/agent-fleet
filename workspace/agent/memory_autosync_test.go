package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents/claude"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/paths"
)

// The ui-prefs PUT moves the reconciler's phase before it answers, so a claude session launched
// right after switching off still gets native memory off until the write-back has run (#1972).
// The loop is not started in tests: the write-back has not happened when the PUT returns.
func TestUIPrefsPutKeepsNativeMemoryOffUntilTheWriteBack(t *testing.T) {
	instrEnv(t)
	put := func(body string) {
		t.Helper()
		rec := httptest.NewRecorder()
		handlePutUIPrefs(rec, httptest.NewRequest("PUT", "/env/ui-prefs", strings.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("PUT %s: %d %s", body, rec.Code, rec.Body)
		}
	}
	if claude.AutoMemoryOff() {
		t.Fatal("native memory should be on before the switch")
	}
	put(`{"agentMemory":true}`)
	if !claude.AutoMemoryOff() {
		t.Fatal("switch on: native memory should be off")
	}
	put(`{"agentMemory":false}`)
	if !claude.AutoMemoryOff() {
		t.Fatal("switch off before the write-back: native memory must stay off, the raw pref is not the state")
	}
}

// Stage A of codex: AF memory going on switches features.memories off in config.toml and leaves
// the rest of the file alone. (Putting the member's value back is the reconciler's, tested in
// memoryx.)
func TestAutoSyncWiresTheCodexFlagToConfigToml(t *testing.T) {
	instrEnv(t)
	cfg := filepath.Join(paths.CodexHome(), "config.toml")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("# mine\n[features]\nmemories = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	put := func(body string) {
		t.Helper()
		rec := httptest.NewRecorder()
		handlePutUIPrefs(rec, httptest.NewRequest("PUT", "/env/ui-prefs", strings.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("PUT %s: %d", body, rec.Code)
		}
	}
	put(`{"agentMemory":true}`)
	if b, _ := os.ReadFile(cfg); !strings.Contains(string(b), "memories = false") || !strings.Contains(string(b), "# mine") {
		t.Fatalf("config.toml after switching on:\n%s", b)
	}
}

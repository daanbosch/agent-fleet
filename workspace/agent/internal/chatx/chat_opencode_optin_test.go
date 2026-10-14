package chatx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents/opencode"
)

// An assistant chat turn runs the opt-in's opencode 2 binary, with its own database, once the
// workspace has switched; without the switch it runs the `opencode` on PATH as before.
func TestOpencodeChatFollowsTheOptIn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(opencode.MajorEnv, "")
	prev := opencode.UsagePref
	opencode.UsagePref = func() string { return opencode.UsageFree }
	t.Cleanup(func() { opencode.UsagePref = prev })

	// A recorder script: writes "<who> <OPENCODE_DB>" to the log, answers with one text event.
	log := filepath.Join(home, "ran.log")
	script := func(who string) string {
		return "#!/bin/sh\necho \"" + who + " ${OPENCODE_DB:-none}\" >> " + log +
			"\necho '{\"type\":\"text\",\"sessionID\":\"ses_1\",\"part\":{\"type\":\"text\",\"text\":\"ok\"}}'\n"
	}
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "opencode"), []byte(script("v1")), 0o755); err != nil {
		t.Fatal(err)
	}
	v2dir := filepath.Join(opencode.V2Prefix(), "node_modules", "@opencode", "cli", "bin")
	if err := os.MkdirAll(v2dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v2dir, "opencode.exe"), []byte(script("v2")), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	send := func() string {
		c := &ChatConversation{ID: "c1"}
		if _, err := (opencodeChat{}).Send(context.Background(), c, "hi"); err != nil {
			t.Fatalf("send: %v", err)
		}
		b, _ := os.ReadFile(log)
		_ = os.Remove(log)
		return strings.TrimSpace(string(b))
	}

	if got := send(); got != "v1 none" {
		t.Fatalf("opt-in off: ran %q, want the PATH opencode without OPENCODE_DB", got)
	}
	if err := opencode.SetWantsV2(true); err != nil {
		t.Fatal(err)
	}
	if got, want := send(), "v2 "+opencode.V2DBPath(); got != want {
		t.Fatalf("opt-in on: ran %q, want %q", got, want)
	}
}

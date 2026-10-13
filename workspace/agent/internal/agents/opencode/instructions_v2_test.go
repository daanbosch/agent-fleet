package opencode

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/mdblock"
)

// The member also edits the global AGENTS.md. A user-notes block whose end marker is missing
// would, with the plain Set, be stripped to the end of the file together with the member's own
// text below it.
func TestApplyUserInstructionsKeepsADamagedAgentsMD(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	start, _ := mdblock.Markers("user-notes")
	damaged := "member header\n" + start + "\nold body\nmember personal suffix\n"
	if err := os.MkdirAll(filepath.Dir(AgentsPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, major := range []int{2, 1} {
		useMajor(t, major)
		if err := os.WriteFile(AgentsPath(), []byte(damaged), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, body := range []string{"", "new text"} {
			err := ApplyUserInstructions(filepath.Join(t.TempDir(), "opencode.md"), body)
			if !errors.Is(err, mdblock.ErrDamaged) {
				t.Fatalf("major %d body %q: err = %v, want ErrDamaged", major, body, err)
			}
			if b, _ := os.ReadFile(AgentsPath()); string(b) != damaged {
				t.Fatalf("major %d body %q: AGENTS.md changed:\n%s", major, body, b)
			}
		}
	}
}

// Two well-formed copies: clearing removes both, so no stale instruction stays in effect.
func TestApplyUserInstructionsRemovesEveryCopy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	start, end := mdblock.Markers("user-notes")
	block := start + "\nold\n" + end + "\n"
	if err := os.MkdirAll(filepath.Dir(AgentsPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, major := range []int{2, 1} {
		useMajor(t, major)
		if err := os.WriteFile(AgentsPath(), []byte("head\n"+block+"mid\n"+block+"tail\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := ApplyUserInstructions(filepath.Join(t.TempDir(), "opencode.md"), ""); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(AgentsPath())
		if strings.Contains(string(b), "old") || strings.Contains(string(b), start) {
			t.Fatalf("major %d: a copy survived:\n%s", major, b)
		}
		for _, keep := range []string{"head", "mid", "tail"} {
			if !strings.Contains(string(b), keep) {
				t.Fatalf("major %d: the member's %q was lost:\n%s", major, keep, b)
			}
		}
	}
}

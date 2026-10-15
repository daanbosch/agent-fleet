package sessionx

import (
	"strings"
	"testing"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/bridge"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/transcript"
)

// A token-shaped string in tool output or in assistant text must not survive into the file,
// whichever option the member picked.
func TestExportScrubsSecrets(t *testing.T) {
	tok := "ghp_" + strings.Repeat("A1b2C3d4", 5)
	turns := []transcript.Turn{{Role: "assistant", Parts: []transcript.Part{
		{Kind: "text", Text: "the token is " + tok},
		{Kind: "tool", Tool: "Bash", Info: "env", Output: "GITHUB_TOKEN=" + tok},
	}}}
	md := renderSessionExport(session.Meta{Name: "s1", Kind: "claude"}, turns, true, "Asia/Tokyo", time.Now())
	if strings.Contains(md, tok) {
		t.Fatalf("token survived the export:\n%s", md)
	}
	if !strings.Contains(md, bridge.RedactedMark) {
		t.Fatalf("no redaction mark in:\n%s", md)
	}
}

func TestExportUsesTheRequestedZone(t *testing.T) {
	turns := []transcript.Turn{{Role: "user", TS: "2026-10-15T01:00:00Z", Parts: []transcript.Part{{Kind: "text", Text: "hi"}}}}
	md := renderSessionExport(session.Meta{Name: "s1", Kind: "claude"}, turns, false, "Asia/Tokyo", time.Now())
	if !strings.Contains(md, "2026-10-15 10:00") {
		t.Fatalf("turn time not shown in the requested zone:\n%s", md)
	}
}

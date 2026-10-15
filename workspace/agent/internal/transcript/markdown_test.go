package transcript

import (
	"strings"
	"testing"
	"time"
)

func sampleTurns() []Turn {
	return []Turn{
		{Role: "user", TS: "2026-10-15T10:00:00Z", Parts: []Part{{Kind: "text", Text: "Fix the build"}}},
		{Role: "assistant", Model: "m1", Parts: []Part{
			{Kind: "thinking", Text: "SECRET-THOUGHT"},
			{Kind: "text", Text: "Looking."},
			{Kind: "tool", Tool: "Bash", Info: "go build ./...", Output: "ok\n```\ninner fence"},
		}},
		// A second row of the same assistant turn (claude writes one turn as many rows).
		{Role: "assistant", Model: "m1", Parts: []Part{
			{Kind: "tool", Tool: "Edit", File: "main.go", Edits: []Edit{{Old: "a", New: "b"}}},
			{Kind: "text", Text: "Done."},
		}},
		{Role: "assistant", Sidechain: true, Parts: []Part{{Kind: "text", Text: "SIDECHAIN"}}},
		{Role: "user", Source: SourcePeer, PeerFrom: "other", Parts: []Part{{Kind: "text", Text: "hello from peer"}}},
	}
}

func TestMarkdownNamesOnly(t *testing.T) {
	md := Markdown(sampleTurns(), MarkdownOptions{Title: "T", Header: []string{"Session: s1"}, Lang: "en", Loc: time.FixedZone("JST", 9*3600)})
	for _, want := range []string{
		"# T\n", "- Session: s1\n",
		"## User · 2026-10-15 19:00", "Fix the build",
		"## Assistant (m1)", "Looking.", "> Tool: `Bash` — go build ./...", "> Tool: `Edit` — main.go", "Done.",
		"## Peer session (other)", "hello from peer",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in:\n%s", want, md)
		}
	}
	for _, bad := range []string{"SECRET-THOUGHT", "SIDECHAIN", "inner fence", "-a\n+b"} {
		if strings.Contains(md, bad) {
			t.Errorf("unexpected %q in:\n%s", bad, md)
		}
	}
	if n := strings.Count(md, "## Assistant"); n != 1 {
		t.Errorf("assistant rows of one turn split into %d sections:\n%s", n, md)
	}
}

func TestMarkdownToolOutput(t *testing.T) {
	md := Markdown(sampleTurns(), MarkdownOptions{ToolOutput: true, Lang: "en"})
	if !strings.Contains(md, "````\nok\n```\ninner fence\n````") {
		t.Errorf("tool output containing a fence must get a longer fence:\n%s", md)
	}
	if !strings.Contains(md, "```diff\n-a\n+b\n```") {
		t.Errorf("edit diff missing:\n%s", md)
	}
}

func TestMarkdownScrub(t *testing.T) {
	turns := []Turn{{Role: "assistant", Parts: []Part{{Kind: "tool", Tool: "Bash", Output: "TOKEN=abc123"}}}}
	md := Markdown(turns, MarkdownOptions{ToolOutput: true, Scrub: func(s string) string {
		return strings.ReplaceAll(s, "abc123", "[redacted]")
	}})
	if strings.Contains(md, "abc123") || !strings.Contains(md, "[redacted]") {
		t.Errorf("scrub not applied:\n%s", md)
	}
}

func TestMarkdownEmpty(t *testing.T) {
	if md := Markdown(nil, MarkdownOptions{Lang: "en"}); md != "(no conversation yet)\n" {
		t.Errorf("got %q", md)
	}
}

func TestShortTimeKeepsUnparsable(t *testing.T) {
	if got := ShortTime("yesterday", time.UTC); got != "yesterday" {
		t.Errorf("got %q", got)
	}
	if got := ShortTime("2026-10-15T01:02:03.456Z", time.UTC); got != "2026-10-15 01:02" {
		t.Errorf("got %q", got)
	}
}

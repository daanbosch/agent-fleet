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

// Scrub sees the finished document, so fields that are not prose cannot slip past it.
func TestMarkdownScrubCoversEveryField(t *testing.T) {
	const tok = "TOKENSENTINEL"
	turns := []Turn{
		{Role: "user", Source: "x" + tok, Parts: []Part{{Kind: "userfile", Files: []string{"uploads/" + tok + "/a.txt"}}}},
		{Role: "assistant", Model: "m" + tok, TS: "bad" + tok, Parts: []Part{
			{Kind: "tool", Tool: "T" + tok},
			{Kind: "delegation", AgentType: "A" + tok},
		}},
		{Role: "user", Source: SourcePeer, PeerFrom: "p" + tok, Parts: []Part{{Kind: "text", Text: "hi"}}},
	}
	for _, full := range []bool{false, true} {
		md := Markdown(turns, MarkdownOptions{Title: tok, Header: []string{tok}, ToolOutput: full,
			Scrub: func(s string) string { return strings.ReplaceAll(s, tok, "[x]") }})
		if strings.Contains(md, tok) {
			t.Errorf("full=%v: token survived:\n%s", full, md)
		}
	}
}

// A turn that opens a fence and never closes it must not swallow the turns after it.
func TestMarkdownClosesAnOpenFence(t *testing.T) {
	for _, fence := range []string{"```", "~~~~"} {
		turns := []Turn{
			{Role: "user", Parts: []Part{{Kind: "text", Text: fence + "text\nunclosed"}}},
			{Role: "assistant", Parts: []Part{{Kind: "text", Text: "answer"}}},
		}
		md := Markdown(turns, MarkdownOptions{Lang: "en"})
		want := "unclosed\n\n" + fence + "\n\n---\n\n## Assistant"
		if !strings.Contains(md, want) {
			t.Errorf("fence %q left open:\n%s", fence, md)
		}
	}
}

func TestMarkdownPlanAnswerAndOptionPreview(t *testing.T) {
	turns := []Turn{{Role: "assistant", Parts: []Part{
		{Kind: "plan", Plan: "do X", Answer: "User has approved your plan"},
		{Kind: "question", Questions: []Question{{Question: "Which?", Options: []Option{
			{Label: "A", Preview: "+--+\n```\n|A |"},
		}}}, Answer: "A"},
	}}}
	md := Markdown(turns, MarkdownOptions{Lang: "en", ToolOutput: true})
	for _, want := range []string{"**Answer:** User has approved your plan", "  ````\n  +--+\n  ```\n  |A |\n  ````"} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in:\n%s", want, md)
		}
	}
	if md := Markdown(turns, MarkdownOptions{Lang: "en"}); strings.Contains(md, "+--+") {
		t.Errorf("preview belongs to the full export only:\n%s", md)
	}
}

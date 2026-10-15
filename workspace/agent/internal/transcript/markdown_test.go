package transcript

import (
	"reflect"
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

// Fields that are not prose (paths, names, sources, stamps) are scrubbed too.
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

// The scrubber must not reach the fences the renderer adds: bridge's assignment pattern
// spans a newline, so an empty PASSWORD= right before a closing fence took the fence.
func TestMarkdownScrubCannotEatAFence(t *testing.T) {
	assign := regexpScrub()
	turns := []Turn{
		{Role: "assistant", Parts: []Part{{Kind: "tool", Tool: "Bash", Output: "`````\nPASSWORD="}}},
		{Role: "user", Parts: []Part{{Kind: "text", Text: "next"}}},
	}
	md := Markdown(turns, MarkdownOptions{Lang: "en", ToolOutput: true, Scrub: assign})
	if !strings.Contains(md, "PASSWORD=\n``````\n\n---\n\n## User") {
		t.Errorf("closing fence lost:\n%s", md)
	}
}

// regexpScrub mimics the bridge's assignment rule (the value may follow a newline).
func regexpScrub() func(string) string {
	return func(s string) string {
		i := strings.Index(s, "PASSWORD=")
		if i < 0 {
			return s
		}
		rest := strings.TrimLeft(s[i+len("PASSWORD="):], " \n")
		if j := strings.IndexAny(rest, " \n"); j >= 0 {
			rest = rest[:j]
		}
		if len(rest) < 6 {
			return s
		}
		return strings.Replace(s, rest, "[x]", 1)
	}
}

// Every string a turn can hold goes through Scrub, including fields added after this test.
func TestMarkdownScrubReachesEveryStringField(t *testing.T) {
	const tok = "TOKSENTINEL"
	var fill func(v reflect.Value)
	fill = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.String:
			v.SetString(tok)
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				if v.Field(i).CanSet() {
					fill(v.Field(i))
				}
			}
		case reflect.Slice:
			v.Set(reflect.MakeSlice(v.Type(), 1, 1))
			fill(v.Index(0))
		}
	}
	var turns []Turn
	for _, kind := range []string{"text", "tool", "delegation", "question", "plan", "userfile", "error"} {
		var tr Turn
		fill(reflect.ValueOf(&tr).Elem())
		tr.Role, tr.Parts[0].Kind, tr.Sidechain, tr.Compact, tr.CostOnly = "assistant", kind, false, false, false
		turns = append(turns, tr)
	}
	for _, full := range []bool{false, true} {
		md := Markdown(turns, MarkdownOptions{ToolOutput: full, Scrub: func(s string) string { return strings.ReplaceAll(s, tok, "[x]") }})
		if strings.Contains(md, tok) {
			t.Errorf("full=%v: an unscrubbed field reached the file:\n%s", full, md)
		}
	}
}

// A turn's own Markdown goes through untouched; these valid bodies are the ones line-level
// fence repairs used to break.
func TestMarkdownPassesTurnTextThrough(t *testing.T) {
	for _, body := range []string{
		"```inline```",
		"- item\n\n  ```\n  code\n    ```\n\noutside",
		"```text\n    ```\n```",
		"intro\n\n ```text\n    ```\n ```",
		"<!--\n```\n-->",
		"- item\n\n  ```\n  code\n\noutside",
	} {
		turns := []Turn{
			{Role: "user", Parts: []Part{{Kind: "text", Text: body}}},
			{Role: "assistant", Parts: []Part{{Kind: "text", Text: "answer"}}},
		}
		md := Markdown(turns, MarkdownOptions{Lang: "en"})
		if !strings.Contains(md, body+"\n\n---\n\n## Assistant") {
			t.Errorf("turn text changed for %q:\n%s", body, md)
		}
	}
}

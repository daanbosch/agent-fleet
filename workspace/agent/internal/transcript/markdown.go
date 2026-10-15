package transcript

import (
	"fmt"
	"reflect"
	"strings"
	"time"
)

// MarkdownOptions shapes a Markdown rendering of a conversation (issue #2183). The same
// renderer is meant to feed the cross-agent move (#2182), so everything that differs
// between a person's download and an agent's input is an option here, not a second
// renderer.
type MarkdownOptions struct {
	Title string
	// Header lines are written as a bullet list under the title, already formatted.
	Header []string
	// ToolOutput includes tool output, edit diffs and delegation prompts. Without it a
	// tool call is reduced to its name and one-line label.
	ToolOutput bool
	// Lang picks the labels ("en", anything else = Japanese), as uiprefs.Locale() does.
	Lang string
	// Loc is the zone turn times are shown in; nil = time.Local.
	Loc *time.Location
	// Scrub runs over every string of the turns, the title and the header before anything
	// is laid out: field by field so none (a path, a tool name, a model) slips past it, and
	// before the fences are sized so it cannot eat one (a value pattern may span a newline).
	// Tool output carries tokens even when the user downloads the file themselves, so a
	// caller that leaves this nil writes them verbatim.
	Scrub func(string) string
}

type mdLabels struct {
	user, assistant, peer, operator, injected, compacted, tool, subagent,
	question, answer, declined, plan, files, errorL, empty string
}

var mdLabelsJA = mdLabels{
	user: "ユーザー", assistant: "アシスタント", peer: "ピアセッション", operator: "オペレーター経由",
	injected: "自動送信", compacted: "コンテキスト圧縮の要約", tool: "ツール", subagent: "サブエージェント",
	question: "質問", answer: "回答", declined: "回答されませんでした", plan: "計画", files: "ファイル",
	errorL: "エラー", empty: "（会話はまだありません）",
}

var mdLabelsEN = mdLabels{
	user: "User", assistant: "Assistant", peer: "Peer session", operator: "via operator",
	injected: "sent automatically", compacted: "Context compaction summary", tool: "Tool", subagent: "Subagent",
	question: "Question", answer: "Answer", declined: "not answered", plan: "Plan", files: "Files",
	errorL: "Error", empty: "(no conversation yet)",
}

// Markdown renders turns, oldest first, as one Markdown document. Thinking is never
// written: it is the model's scratch work, not part of what was said. Sidechain turns
// (a subagent's own conversation) are left out too; the delegation part in the parent
// turn carries the instruction and the result.
func Markdown(turns []Turn, o MarkdownOptions) string {
	lb := mdLabelsJA
	if o.Lang == "en" {
		lb = mdLabelsEN
	}
	if o.Scrub != nil {
		turns = scrubStrings(turns, o.Scrub)
		o.Title = o.Scrub(o.Title)
		o.Header = scrubStrings(o.Header, o.Scrub)
	}
	var b strings.Builder
	if t := strings.TrimSpace(o.Title); t != "" {
		b.WriteString("# " + oneLine(t) + "\n\n")
	}
	for _, h := range o.Header {
		b.WriteString("- " + oneLine(h) + "\n")
	}
	if len(o.Header) > 0 {
		b.WriteString("\n")
	}

	wrote := false
	prevRole := ""
	for _, t := range turns {
		if t.Sidechain || t.CostOnly {
			continue
		}
		var body strings.Builder
		if t.Compact {
			writeCompact(&body, t, o, lb)
		} else {
			for _, p := range t.Parts {
				writePart(&body, p, o, lb)
			}
			if len(t.Parts) == 0 && strings.TrimSpace(t.Text) != "" {
				body.WriteString(strings.TrimSpace(t.Text) + "\n\n")
			}
		}
		if body.Len() == 0 {
			continue
		}
		closeOpenFence(&body)
		// claude and codex write one assistant turn as many rows; a heading per row would
		// split one answer into a dozen sections.
		if t.Role != "assistant" || prevRole != "assistant" || t.Compact {
			if wrote {
				b.WriteString("---\n\n")
			}
			b.WriteString("## " + turnHeading(t, lb, o.Loc) + "\n\n")
		}
		b.WriteString(body.String())
		wrote = true
		prevRole = t.Role
		if t.Compact {
			prevRole = ""
		}
	}
	if !wrote {
		b.WriteString(lb.empty + "\n")
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// scrubStrings returns a deep copy of v with f applied to every string in it, so a field
// added to Turn or Part later is covered without anyone remembering to list it here.
func scrubStrings[T any](v T, f func(string) string) T {
	return scrubValue(reflect.ValueOf(v), f).Interface().(T)
}

func scrubValue(v reflect.Value, f func(string) string) reflect.Value {
	switch v.Kind() {
	case reflect.String:
		return reflect.ValueOf(f(v.String())).Convert(v.Type())
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(scrubValue(v.Index(i), f))
		}
		return out
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		out.Set(v)
		for i := 0; i < v.NumField(); i++ {
			if out.Field(i).CanSet() {
				out.Field(i).Set(scrubValue(v.Field(i), f))
			}
		}
		return out
	}
	return v
}

func turnHeading(t Turn, lb mdLabels, loc *time.Location) string {
	var h string
	switch {
	case t.Compact:
		h = lb.compacted
	case t.Role == "assistant":
		h = lb.assistant
		if t.Model != "" {
			h += " (" + t.Model + ")"
		}
	case t.Source == SourcePeer:
		h = lb.peer
		if t.PeerFrom != "" {
			h += " (" + t.PeerFrom + ")"
		}
	case t.Source == "operator":
		h = lb.user + " (" + lb.operator + ")"
	case t.Source != "":
		h = lb.user + " (" + lb.injected + ": " + t.Source + ")"
	default:
		h = lb.user
	}
	if t.TS != "" {
		h += " · " + ShortTime(t.TS, loc)
	}
	return oneLine(h)
}

func writeCompact(b *strings.Builder, t Turn, o MarkdownOptions, lb mdLabels) {
	if !o.ToolOutput {
		// The heading alone says a compaction happened; its summary repeats what the
		// turns above already hold.
		b.WriteString("_" + lb.compacted + "_\n\n")
		return
	}
	writeFenced(b, "", t.Text)
}

func writePart(b *strings.Builder, p Part, o MarkdownOptions, lb mdLabels) {
	switch p.Kind {
	case "thinking":
	case "text":
		if s := strings.TrimSpace(p.Text); s != "" {
			b.WriteString(s + "\n\n")
		}
	case "tool":
		b.WriteString("> " + lb.tool + ": `" + oneLine(p.Tool) + "`")
		if label := firstNonEmpty(p.Info, p.File); label != "" {
			b.WriteString(" — " + oneLine(label))
		}
		b.WriteString("\n\n")
		if !o.ToolOutput {
			return
		}
		for _, e := range p.Edits {
			writeFenced(b, "diff", editDiff(e))
		}
		if s := strings.TrimSpace(p.Output); s != "" {
			writeFenced(b, "", s)
		}
	case "delegation":
		h := lb.subagent
		if p.AgentType != "" {
			h += " (" + p.AgentType + ")"
		}
		b.WriteString("> " + oneLine(h))
		if p.Info != "" {
			b.WriteString(" — " + oneLine(p.Info))
		}
		b.WriteString("\n\n")
		if !o.ToolOutput {
			return
		}
		if s := strings.TrimSpace(p.Prompt); s != "" {
			writeFenced(b, "", s)
		}
		if s := strings.TrimSpace(p.Output); s != "" {
			writeFenced(b, "", s)
		}
	case "question":
		for _, q := range p.Questions {
			b.WriteString("**" + lb.question + ":** " + strings.TrimSpace(q.Question) + "\n\n")
			for _, opt := range q.Options {
				line := "- " + oneLine(opt.Label)
				if opt.Description != "" {
					line += " — " + oneLine(opt.Description)
				}
				b.WriteString(line + "\n")
				if o.ToolOutput && strings.TrimSpace(opt.Preview) != "" {
					// The preview is what the options were compared on; indented so it
					// stays inside its list item.
					var pb strings.Builder
					writeFenced(&pb, "", opt.Preview)
					b.WriteString("\n" + indent(pb.String(), "  "))
				}
			}
			if len(q.Options) > 0 {
				b.WriteString("\n")
			}
		}
		switch {
		case p.Declined:
			b.WriteString("**" + lb.answer + ":** _" + lb.declined + "_\n\n")
		case strings.TrimSpace(p.Answer) != "":
			b.WriteString("**" + lb.answer + ":** " + strings.TrimSpace(p.Answer) + "\n\n")
		}
	case "plan":
		// A plan is what the agent proposed to do, so it is conversation, not tool output.
		if s := strings.TrimSpace(p.Plan); s != "" {
			b.WriteString("**" + lb.plan + ":**\n\n" + s + "\n\n")
		}
		if s := strings.TrimSpace(p.Answer); s != "" {
			b.WriteString("**" + lb.answer + ":** " + s + "\n\n")
		}
	case "userfile":
		b.WriteString("**" + lb.files + ":**")
		if p.Caption != "" {
			b.WriteString(" " + oneLine(p.Caption))
		}
		b.WriteString("\n\n")
		for _, f := range p.Files {
			b.WriteString("- `" + oneLine(f) + "`\n")
		}
		b.WriteString("\n")
	case "error":
		b.WriteString("> **" + lb.errorL + ":** " + oneLine(firstNonEmpty(p.Info, p.Text)) + "\n\n")
		if p.Info != "" && strings.TrimSpace(p.Text) != "" {
			writeFenced(b, "", strings.TrimSpace(p.Text))
		}
	}
}

// ShortTime shows an RFC3339 stamp to the minute in loc (nil = time.Local). A stamp that
// does not parse is returned as it is rather than dropped.
func ShortTime(ts string, loc *time.Location) string {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return ts
	}
	if loc == nil {
		loc = time.Local
	}
	return t.In(loc).Format("2006-01-02 15:04")
}

// closeOpenFence appends a closing fence when a turn's own Markdown left a code block open,
// so it cannot swallow the separator, the next heading and every turn after it.
func closeOpenFence(b *strings.Builder) {
	var open string
	openIndent := 0
	for _, line := range strings.Split(b.String(), "\n") {
		t := strings.TrimLeft(line, " ")
		f := fenceRun(t)
		if f == "" {
			continue
		}
		rest := t[len(f):]
		if open == "" {
			// CommonMark: an opening fence is indented at most three spaces, and a backtick
			// fence's info string holds no backtick (```inline``` is inline code). Deeper
			// indentation is a list item's fence or an indented block; leaving it alone can
			// only miss a close, never add a stray fence.
			if len(line)-len(t) <= 3 && (f[0] != '`' || !strings.Contains(rest, "`")) {
				open, openIndent = f, len(line)-len(t)
			}
			continue
		}
		// A close is indented at most three spaces relative to its container. This scan
		// cannot see containers, so the opening fence's own indent stands in for one: a
		// list item's close indented past the item still closes, while an indented run of
		// backticks inside a top-level block stays content.
		if f[0] == open[0] && len(f) >= len(open) && strings.TrimSpace(rest) == "" && len(line)-len(t)-openIndent <= 3 {
			open = ""
		}
	}
	if open != "" {
		b.WriteString(open + "\n\n")
	}
}

// fenceRun returns the leading run of ` or ~ when it is long enough to be a fence.
func fenceRun(line string) string {
	if line == "" || (line[0] != '`' && line[0] != '~') {
		return ""
	}
	n := 0
	for n < len(line) && line[n] == line[0] {
		n++
	}
	if n < 3 {
		return ""
	}
	return line[:n]
}

func indent(s, pad string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = pad + l
		}
	}
	return strings.Join(lines, "\n") + "\n\n"
}

func editDiff(e Edit) string {
	var b strings.Builder
	for _, l := range splitLines(e.Old) {
		b.WriteString("-" + l + "\n")
	}
	for _, l := range splitLines(e.New) {
		b.WriteString("+" + l + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
}

// writeFenced writes s as a code block whose fence is longer than any backtick run inside
// it, so tool output that itself contains ``` cannot close the block early.
func writeFenced(b *strings.Builder, lang, s string) {
	if strings.TrimSpace(s) == "" {
		return
	}
	fence := strings.Repeat("`", max(3, longestRun(s, '`')+1))
	fmt.Fprintf(b, "%s%s\n%s\n%s\n\n", fence, lang, strings.TrimRight(s, "\n"), fence)
}

func longestRun(s string, c byte) int {
	best, cur := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			cur++
			best = max(best, cur)
		} else {
			cur = 0
		}
	}
	return best
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

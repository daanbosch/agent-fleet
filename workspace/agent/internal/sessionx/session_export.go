package sessionx

import (
	"net/http"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents/claude"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/bridge"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/httpx"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/transcript"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/uiprefs"
)

// HandleSessionExport serves a session's whole conversation as one Markdown file (#2183).
// It reads the full transcript rather than the mirror's tail window, so the file does not
// depend on how far the member has scrolled. ?tools=full adds tool output and edit diffs;
// the default reduces every tool call to its name and label. ?tz= is the browser's IANA zone,
// so the file shows the same times as the mirror; an unknown zone falls back to the Agent's.
//
// Secrets are scrubbed with the chat bridge's scrubber whatever the option: tool output
// routinely carries tokens, and a downloaded file is easy to paste somewhere public.
func HandleSessionExport(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !session.ValidName(name) {
		httpx.WriteErr(w, http.StatusBadRequest, "bad_name", "invalid session name")
		return
	}
	meta, ok := session.ReadMeta(name)
	if !ok {
		httpx.WriteErr(w, http.StatusNotFound, "not_found", "no such session: "+name)
		return
	}
	if !AgentOf(meta.Kind).Caps().CanTranscript {
		httpx.WriteErr(w, http.StatusBadRequest, "unsupported_kind", "export is available for transcript-capable sessions only")
		return
	}
	turns := exportTurns(meta)
	tagInjectedTurns(name, turns)
	q := r.URL.Query()
	md := renderSessionExport(meta, turns, q.Get("tools") == "full", q.Get("tz"), time.Now())
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+exportFileName(name)+`"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(md))
}

// exportTurns reads the whole conversation. claude goes through CollectAllTurns, because the
// mirror's collector keeps only the newest 1 MiB and leaves ordinary tool results out. A
// question the agent is still waiting on is held apart from the turns (TranscriptData.Pending)
// so the mirror can render it as a live card; the file appends it, since it was asked.
func exportTurns(m session.Meta) []transcript.Turn {
	if m.Kind == session.KindClaude {
		lines, _, _ := claude.TranscriptRead(session.UUID(m.Dir, m.Name))
		return claude.CollectAllTurns(lines)
	}
	td, ok := AgentOf(m.Kind).Transcript(m)
	if !ok {
		return nil
	}
	return withPendingQuestions(td.Turns, td.Pending)
}

func withPendingQuestions(turns []transcript.Turn, pending []transcript.Question) []transcript.Turn {
	if len(pending) == 0 {
		return turns
	}
	if n := len(turns); n > 0 {
		for _, p := range turns[n-1].Parts {
			if p.Kind == "question" && len(p.Questions) > 0 && p.Answer == "" && p.Questions[0].Question == pending[0].Question {
				return turns // the kind left it in the turns as well
			}
		}
	}
	return append(turns, transcript.Turn{Role: "assistant", Parts: []transcript.Part{{Kind: "question", Questions: pending}}})
}

// exportFileName is the session slug, unless the scrubber would mask part of it: a slug can
// be any token-shaped string, and the body's masking means nothing if the file name keeps it.
func exportFileName(name string) string {
	if bridge.ScrubSecrets(name) != name {
		return "session.md"
	}
	return name + ".md"
}

// renderSessionExport is the handler minus I/O, so a test can pin that the scrubber is
// always applied.
func renderSessionExport(meta session.Meta, turns []transcript.Turn, toolOutput bool, tz string, now time.Time) string {
	lang := uiprefs.Locale()
	loc := time.Local
	if tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
	return transcript.Markdown(turns, transcript.MarkdownOptions{
		Title:      exportTitle(meta),
		Header:     exportHeader(meta, lang, now.In(loc)),
		ToolOutput: toolOutput,
		Lang:       lang,
		Loc:        loc,
		Scrub:      bridge.ScrubSecrets,
	})
}

func exportTitle(m session.Meta) string {
	if m.Title != "" {
		return m.Title
	}
	return m.Name
}

func exportHeader(m session.Meta, lang string, now time.Time) []string {
	label := map[string]string{"session": "セッション", "agent": "エージェント", "dir": "作業フォルダ", "branch": "ブランチ", "exported": "書き出し"}
	if lang == "en" {
		label = map[string]string{"session": "Session", "agent": "Agent", "dir": "Working folder", "branch": "Branch", "exported": "Exported"}
	}
	h := []string{label["session"] + ": " + m.Name, label["agent"] + ": " + m.Kind}
	if m.Dir != "" {
		h = append(h, label["dir"]+": "+m.Dir)
	}
	if m.Branch != "" {
		h = append(h, label["branch"]+": "+m.Branch)
	}
	return append(h, label["exported"]+": "+now.Format("2006-01-02 15:04 MST"))
}

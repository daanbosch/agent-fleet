package sessionx

// What a move reads from its source (ADR 0115 decisions 6 and 11.3): the conversation, the
// question and queues that never reached it, the pending peer spool, the working copy, the
// changed files and the open threads. Read with input closed, so nothing can be added to any of
// them while the move works; the preview reads the same without the gate.

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/agents/claude"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/gitx"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
	"github.com/k-k1/agent-fleet/workspace/agent/internal/transcript"
)

// readMoveSource is the snapshot as a HandoverInput, without the fields only the move knows
// (Earlier, Instructions, Now). A variable so tests need no transcript on disk.
var readMoveSource = func(m session.Meta) HandoverInput {
	in := HandoverInput{Source: m}
	var edits []transcript.FileEdit
	if m.Kind == session.KindClaude {
		// The whole jsonl, as the export reads it: the mirror's collector keeps only a tail.
		lines, _, _ := claude.TranscriptRead(session.UUID(m.Dir, m.Name))
		in.Turns = claude.CollectAllTurns(lines)
		edits = claude.CollectFileEdits(lines, 0)
	} else if td, ok := AgentOf(m.Kind).Transcript(m); ok {
		// td.Path is deliberately dropped: the file carries no path into a CLI store (decision 15).
		in.Turns, in.Pending = td.Turns, td.Pending
		in.Queued, in.QueuedItems = td.Queued, td.QueuedItems
		for _, t := range td.Turns {
			edits = append(edits, transcript.FileEditsInTurn(t)...)
		}
	}
	tagInjectedTurns(m.Name, in.Turns)
	in.ChangedFiles = foldAllFileEdits(m.Dir, edits)
	in.PeerMessages = agents.PendingPeers(m.Name)
	state, _ := moveSourceState(m)
	_, episode := RateLimitStates.Read(m.Name)
	in.CutOffByLimit = episode || state == agents.StateLimited || state == agents.StateSpendLimit
	in.Git = readMoveGit(m.Dir)
	in.Children = moveChildren(m.Name)
	in.Proposals = pendingProposalNames(m.Name)
	return in
}

// readMoveGit reads the working copy as decision 6.3 lists it. gitx.Run is not used for the
// status: it trims its output, and the first porcelain line starts with a meaningful space.
func readMoveGit(dir string) HandoverGit {
	if dir == "" || !gitx.IsGitRepo(dir) {
		return HandoverGit{}
	}
	out, err := gitx.Cmd(dir, "status", "--porcelain").Output()
	if err != nil {
		return HandoverGit{}
	}
	g := HandoverGit{Available: true, Status: strings.TrimRight(string(out), "\n")}
	// Against HEAD, so staged changes count too; an unborn branch has no HEAD and no diff.
	g.DiffStat, _ = gitx.Run(dir, "diff", "--stat", "HEAD")
	if ahead, err := gitx.Run(dir, "log", "--oneline", "--no-decorate", "@{upstream}..HEAD"); err == nil {
		g.Ahead = ahead
	} else {
		g.Ahead = "(the branch has no upstream)"
	}
	return g
}

// moveChildren lists the children the source started (ADR 0073) that are not archived, with
// their states. They stay the source's (decision 12); the file only names them.
func moveChildren(parent string) []HandoverChild {
	var out []HandoverChild
	for _, m := range session.ListMetas() {
		if m.Archived || m.OriginSession != parent || session.OriginOf(m) != session.OriginSession {
			continue
		}
		out = append(out, HandoverChild{Name: m.Name, State: DriveState(m, SessionAlive(m), false)})
	}
	slices.SortFunc(out, func(a, b HandoverChild) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// pendingProposalNames names the source's handoff proposals nobody launched yet, by title
// where there is one.
func pendingProposalNames(name string) []string {
	list, _ := ReadHandoffProposals(name)
	var out []string
	for _, p := range list {
		if p.LaunchedAt != 0 {
			continue
		}
		if t := strings.TrimSpace(p.Title); t != "" {
			out = append(out, t)
		} else {
			out = append(out, p.ID)
		}
	}
	return out
}

// projectInstructionFiles is what each kind loads by itself from the working folder, relative
// to it (decision 14). A kind is listed with the files it is known to read, and nothing it
// might: a file wrongly claimed for the target is one the target is never told to read, while
// a file wrongly left out only costs the target one extra read. opencode and muse read
// CLAUDE.md only when AGENTS.md is absent, so they are not credited with it.
var projectInstructionFiles = map[string][]string{
	session.KindClaude:   {"CLAUDE.md", "CLAUDE.local.md"},
	session.KindCodex:    {"AGENTS.md"},
	session.KindOpencode: {"AGENTS.md"},
	session.KindMuse:     {"AGENTS.md"},
	session.KindAgy:      {"AGENTS.md"},
	session.KindCopilot:  {"AGENTS.md", ".github/copilot-instructions.md"},
	session.KindCursor:   {"AGENTS.md", "CLAUDE.md", "CLAUDE.local.md", ".cursorrules", ".cursor/rules"},
	session.KindKiro:     {".kiro/steering"},
}

// instructionGap lists the files the source's kind loads from dir that the target's kind does
// not, among those that exist there.
func instructionGap(dir, sourceKind, targetKind string) []string {
	var out []string
	for _, f := range projectInstructionFiles[sourceKind] {
		if slices.Contains(projectInstructionFiles[targetKind], f) {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f))); err == nil {
			out = append(out, f)
		}
	}
	return out
}

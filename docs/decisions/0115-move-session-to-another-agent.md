# 0115. Move a session to another agent kind without an LLM: a rendered transcript file, pointed at by the first prompt

English | [日本語](0115-move-session-to-another-agent.ja.md)

- Status: **proposed** (2026-10-15). Nothing is implemented.
- Tracking: #2182. Builds on #2183 (the shared Markdown renderer, `transcript.Markdown`).
- See also: [0039](0039-fork-at-message.md) (fork, same kind only) /
  [0049](0049-session-changed-files.md) (the changed-files list) /
  [0042](0042-user-instructions.md) (instructions delivered per kind)

## Context

A session that hits its agent's usage limit stops until the window resets — days, for a weekly
cap. Two ways to continue elsewhere exist, and both fall short:

- **Hand over to another agent** asks the operator assistant to read the source and write a
  summary. That is an LLM call that can hit a limit itself, it is lossy, and what survives depends
  on the operator's judgement.
- **Fork** (ADR 0039) carries the whole conversation, but only into the same agent kind, which is
  the kind that just ran out.

P1 (#2183) gave every transcript-capable kind one renderer from the normalized
`transcript.Turn` to Markdown, with secrets scrubbed before layout. This record decides how that
file becomes the start of a session on a **different** kind.

### What each kind's transcript yields today

Read from `Agent.Transcript` / `claude.CollectAllTurns` on develop (2026-10-15). Tool output is
capped by the parser, not by the renderer.

| Kind | Source | Whole history? | Tool output | Usage-limit detection (`limited`) |
|---|---|---|---|---|
| claude | session jsonl via `CollectAllTurns` | yes, compaction summaries included | full | yes |
| codex | rollout | yes | capped at 4000 runes | managed only |
| opencode | SQLite store | yes | capped at 4000 runes | no |
| cursor | TUI: transcript file. **managed: the driver's memory** | managed: **empty once stopped** | capped at 4000 | no |
| kiro | managed: memory while alive, v2 JSONL after | yes | capped at 4000 | no |
| agy | conversation jsonl (+ DB probe for a pending question) | yes | parser's own | no |
| copilot | `events.jsonl` | yes | capped at 4000 | no |
| lcpp | its own store | yes | capped at 4000 | no |
| muse | its own store + commentary log | yes | parser's own | yes |
| shell / ssm | none | — | — | — |

Two consequences: the move is offered for every `CanTranscript` kind, but only claude, managed
codex and muse can **trigger** it from `limited`; and a managed cursor session must be moved while
its handle is alive, because the conversation does not exist anywhere else.

## Decisions

1. **The handover is a file the target reads, named in its first prompt.** The Agent renders the
   source, writes it, and starts the target with a short prompt that says where the file is, what
   it is, and to read the head before doing anything. The conversation never passes through an
   LLM on the way.
2. **Rejected: synthesizing the target CLI's native store.** Every store is a private,
   version-drifting contract (the opencode v2 store replaced its schema outright), and claude's
   thinking blocks are signed, so a synthesized history is either rejected or silently replayed
   without them. Fork already does this where it is safe: within one kind, through the CLI's own
   fork API.
3. **Rejected: sending the whole history as the first turn.** It overflows small context windows
   (lcpp) and makes the first turn's cost proportional to the history. A file lets the target read
   the head and page into the rest only when needed.
4. **Rejected: an LLM summary.** That is the existing handover. It stays, for when a summary is
   what the user wants; the move is the deterministic alternative next to it.
5. **Same-kind targets are not offered.** The move exists to leave a kind; within a kind, fork
   carries more (native history, thinking, tool state). The dialog points at fork instead.
6. **File layout: the state first, the history after.** A target that reads only the first
   screen must already know what to do. In order:
   1. A header: source session, kind, model, working folder, branch, the time of the move, and
      one sentence saying this file is the whole record and the source's own files must not be
      opened.
   2. **Where it stopped**: the last user message verbatim, the last assistant text, an unanswered
      question if any, and whether the last turn was cut off by the limit.
   3. **Working copy state** at the moment of the move: `git status --porcelain`, the diff stat,
      and the branch's commits ahead of its upstream.
   4. **Changed files** of the session (the ADR 0049 list, Agent-side aggregation).
   5. **Open threads**: children the source started and their states, and the source's pending
      handoff proposals, by name (decision 12).
   6. **The conversation**, rendered by `transcript.Markdown` and compressed by decision 7.
7. **Keep what the target needs to continue, drop the rest.** The question for every part is
   "would the target act differently without it?", not "did it happen?". The rules are
   mechanical, by part kind and position; no LLM and no guess at importance.
   - **Kept verbatim:** user messages (the intent), questions and their answers, plans, peer and
     operator messages, compaction summaries, and assistant text.
   - **Kept as one line:** an edit-family tool call (verb and file; the diff is in the working
     copy and decision 6.3 and 6.4 already list it), an `error` part, a delegation (its instruction
     and result, without the subagent's tool calls).
   - **Dropped:** thinking and sidechains (as in P1), reads, searches, listings and other tool
     calls that changed nothing, tool output, usage and cost rows (`CostOnly`), model badges,
     the Agent's own injected prompts (the usage-limit resume "continue" and similar, which are
     instructions to the source, not to the target).
   - **The exception is where it stopped:** the last 3 turns keep every tool call with its output
     (`ToolOutput: true`). A turn cut off by the limit is usually mid-investigation, and the
     output is what the target would otherwise run again.
   - If the file still exceeds 256 KiB, the oldest assistant texts are cut to their first
     paragraph, oldest first, until it fits; each cut leaves a visible marker. User messages are
     never cut.
   - 256 KiB and 3 are starting values, not measurements. They are constants in one place, to be
     revised after the first live moves.
   - Secrets are always scrubbed (`bridge.ScrubSecrets`), as in P1.
8. **No new on-disk format; one in-memory one.** The normalized `transcript.Turn` is already the
   kind-neutral representation, and the file the target reads is Markdown, because an LLM reads
   Markdown best. A JSON handover format would be a second contract to keep in step with the
   renderer. In between, the Agent builds a `Handover` value (the parts of decision 6, with the
   conversation already filtered by decision 7) and renders that; the filtering and the size
   bound are tested on the value, not by comparing Markdown. A machine-readable form, if one is
   ever needed, is a second rendering of the same value.
9. **The file lives in the target's throwaway directory**: `~/.af-work/<target>/handover-<source>.md`
   (`paths.SessionWorkDir`). The target can read it, it is not in the working copy so it can
   never be committed, and deleting the target session deletes it (`removeSessionSideFiles`).
   This holds for managed targets too, where `$AF_WORK_DIR` is not set: the path is computed, not
   read from the environment.
   The file is part of the target's conversation, not scratch: the target reads the head first
   and pages into the rest later, and its first prompt names the path. So moving the target to
   the trash archives the file with the session and restoring it puts the file back
   (`archiveSessionForDelete` gains it as a payload with its destination, the way it carries
   claude's jsonl). Without that, `removeSessionSideFiles` deletes `~/.af-work/<target>` and a
   restored target points at a file that no longer exists. Stopping or archiving the target
   without deleting it leaves the file in place, so only the trash path needs this.
   Open: whether every kind reads outside its working folder without
   a permission prompt (cursor and kiro in plan mode are the ones to measure); the fallback is to
   paste the head (decision 6, parts 1–5) into the prompt and keep only the history in the file.
10. **The target runs in the same working copy**, as fork does. The source is frozen first
    (decision 11), so two writers never share it; the `shared-working-copy` warning is not added.
11. **Freezing the source.** In this order, before the target starts:
    1. Record `movedTo: <target>` in the source's meta. `rateLimitTick` skips a moved session;
       without this the next tick books a fresh resume, because the transcript tail still reads
       as limited.
    2. Cancel the booked resume (`dropRateLimitSchedule`) and close the episode.
    3. Stop the source. It is at its limit, so nothing is lost; it is not archived, so its
       mirror stays readable and the user can still delete it.
    4. Resuming a moved session asks for confirmation, naming the target ("this conversation
       continues in X"). It is allowed: the user may want the source back once the window resets.
12. **Ownership does not move.** Children, peer messages addressed to the source and pending
    handoff proposals keep naming the source. Moving them is a separate, harder change (the
    spawn ledger, the child limit and the peer ACL are all keyed by name). The file lists them
    (decision 6.5) so the target knows they exist; the move dialog warns when there are children
    still running.
13. **A turn cut off mid-edit is shown before the move, not fixed.** The dialog shows the
    working copy's dirty state (the same data as decision 6.3). The user decides; the Agent does
    not commit, stash or reset anything.
14. **Instructions and skills are not copied.** Fleet's policy and the user's instructions already
    reach every kind (ADR 0042). Project instruction files differ by kind (`CLAUDE.md` vs
    `AGENTS.md`); the header names the files the source's kind reads that the target's kind does
    not, so the target can read them itself. Skills are not translated between CLIs.
15. **The target never reads the source CLI's private state.** The prompt says so, the file
    carries no path into a CLI store (the header omits `TranscriptData.Path`), and the target's
    only input is the rendered file.

## Phases

- **P2a — the move.** `POST /api/sessions/{name}/move {kind, model?, driver?}` on the Agent:
  build and render (decisions 6–8), write (9), freeze (11), create the target with `initial_prompt` (1).
  The CP route allow-list gains it. Console: a session-menu item next to the existing handover,
  a dialog with the agent picker (other kinds only, with `get_agent_usage` where the kind has a
  source), the dirty state and running children. Tests: compression bounds, freeze order (no
  booking after a move), a managed cursor source that is stopped is refused with a reason,
  and a target moved to the trash and restored still has its handover file.
- **P2b — the trigger.** When a session shows `limited` (or `spendLimit`), the mirror's limit
  banner offers the move with candidates that have quota left. Only claude, managed codex and
  muse reach this state today.
- **Not in P2:** moving ownership (decision 12); measuring the bounds in decision 7 on real moves.

## Consequences

- A limited session can continue on another kind in one action, without any model call before
  the target's own first turn.
- The target starts with less than the source had: no thinking, no native tool state, and no
  record of what was read or searched before the last turns. That is the price of not touching
  native stores, and why fork stays the answer within one kind. A target that needs a file the
  source read reads it again; the working copy is the same.
- The source's status keeps meaning "this conversation stopped here"; the `movedTo` mark is what
  tells it apart from a session that is simply waiting.

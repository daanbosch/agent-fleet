# 0117. A background review that curates agent memory and skills from experience

English | [日本語](0117-background-memory-skill-review.ja.md)

- Status: **proposed** (2026-10-18). Documentation only: nothing below is built. The code facts were read from the
  tree on this date; every size, cap and cost figure is a **proposal that was not measured**
  (phase P0 measures them).
- Tracking: #1559
- Follow-ups: #2319, #2320, #2321, #2322, #2323
- Related: [0108](0108-af-owned-agent-memory.md) (the store this record writes to; its decision 8 left
  "a review queue is built when #1559's automated review needs one") /
  [0022](0022-agent-memory-management.md) (history and rollback) /
  [0029](0029-usage-accounting.md) (the ledger, and its frozen `feature` enum) /
  [0042](0042-user-instructions.md) (user instructions stay human-only) /
  [0112](0112-codex-memory-import.md) (codex's own memory, off by default)

## Context

Issue #1559 asks for what hermes does: a review pass that reads what a session just did and
curates memory and skills from it. hermes forks the running agent's own turn. Agent Fleet does not
own the CLIs, so it cannot fork a turn; what it can run is a separate, tool-less one-shot model call
over the session's transcript. That is the shape of the existing title suggestion.

What the tree already provides, read for this record:

- **A store that every kind writes through one path.** After 0108 the five af MCP tools write
  AF's own store (`memoryx/agent_memory.go`, `agentMemSave`): revision check, secret scan, one
  commit per change with `AF-Author-Kind` / `AF-Author-Session` trailers in the 0022 history, and
  a Console change list with diff, revert and forget (`agent_memory_changes.go`). It is behind the
  per-user `agentMemory` switch, off by default. `memory_roots.go` (claude and codex native
  trees) is not a write path any more: while the switch is on claude's native writer is off
  (0108 decision 6 step 2), and codex's native memory is off by default.
- **A cheap helper-turn route.** `chatx.OneShotHeadless(ctx, feature, tier, persona, prompt, model)`
  picks a backend (a per-feature pin, else the shared priority order claude → codex → opencode),
  runs it headless, and records one ledger row from inside (`usagex.RecordCall`) tagged by the
  caller's `usagex.Tag{Feature, Trigger, Ref}`. The claude backend runs with `--tools ""` and
  `--no-session-persistence`; codex with `--ephemeral`. Titles, branch names, reply chips and
  translations already use it.
- **A frozen ledger enum.** A new consumer needs a `Feature*` constant in
  `internal/usagex/ledger.go`, a row in 0029 §2 (as a dated note), and Console catalogue and
  colour entries; `usage_features_catalog_test.go` fails when they drift.
- **A turn-end signal.** The Agent's per-session status record (`internal/status`) carries
  `TurnEnd`, `TurnEndAt` and `TurnEndReason`, written by the claude and codex hooks, the opencode
  plugin and managed drivers; the four hook-less TUI kinds are observed by polling the sessions
  list.
- **One trap in the existing trigger.** The automatic title suggestion starts from the
  transcript *read* handler (`sessionx/session_transcript.go`), i.e. only while a Console is
  looking at the session, and its backoff comment says as much. A background review copied from
  that pattern would silently never run for an unattended or scheduled session. The right
  precedent for a trigger is the level-triggered reconciler of the memory switch
  (`memoryx/agent_memory_autosync.go`): a loop in the Agent that compares persisted state with
  the world on every wake-up.
- **Skills have no write path today.** `fleetskills` copies the image's `notes/` topics into the
  user skills root of claude (`$CLAUDE_CONFIG_DIR/skills`), codex (`$CODEX_HOME/skills`), opencode
  (`~/.config/opencode/skills`) and muse (`~/.config/muse/skills`), as `af-<topic>` directories
  carrying an ownership marker; only marked directories are ever removed. agy reads only a
  project-side `.agents/skills`; copilot, cursor, kiro and lcpp have no user skills root.

## Decision

1. **Trigger: a level-triggered Agent loop on turn end plus a watermark; manual "Review now"
   beside it.** The loop wakes periodically and at Agent start, and considers a session when all
   of these hold: the review switch is on and AF memory (`agentMemory`) is on; the session has a
   transcript (`Caps().CanTranscript`, which excludes shell and ssm); its status shows a real
   turn end (`TurnEnd`, not an idle AF merely assumes) at least a settle time ago (proposed 120 s,
   longer than the title's 45 s because a review is dearer); at least K new turns exist after the
   session's persisted watermark (proposed K = 10, echoing hermes' "every 10 turns"); and the
   session is not itself a review. The watermark (the transcript position last reviewed) is stored
   beside the store and advances only when a run finishes, so a crash or a stop re-runs the same
   window. A failure backs off with the title suggestion's doubling shape. Any kind with a
   transcript is eligible, scheduled and spawned sessions included; they are where unattended
   experience accumulates.
2. **The input is a redacted window, built by AF.** The helper sees: the unreviewed turns (a
   character budget, newest kept, proposed 40,000 characters), the project's current
   `memory_index` lines, and the full bodies of up to five existing memories ranked by keyword
   overlap with the window (the `memory_search` ranking), so that "patch rather than create"
   and "read before write" are structural, not hoped for. Before the call every string passes the
   0022 secret rules; a hit is replaced by a placeholder in the window, never sent. Tool output
   and fetched pages are marked as data in the prompt. This is a mitigation, not a boundary:
   the transcript can contain content a third party wrote (a web page, an issue), and the
   gate in decision 6 exists for that reason.
3. **The helper turn is one `OneShotHeadless` call, tier `OneShotShort` unless P0 shows it is too
   weak, with a new ledger feature `memory.review`.** Trigger `auto` (or `manual` for "Review
   now"), `Ref` the reviewed session's name, so the ledger can show what each session's review
   cost and the usage view can show review as its own column. The backend is the feature's pin or
   the shared order, **not necessarily the reviewed session's kind**; the transcript therefore
   goes to whichever provider the member already uses for assistant features. The feature gets a
   row in Settings → AI assist like the other one-shot features, where the member can pin the
   agent and model. The ledger enum, the Console catalogue, the colours and a dated note in 0029 §2
   land in one commit (the test enforces it). Per-run bounds: one call, one retry at most, output
   capped (proposed 5 operations, 8 KiB each).
4. **The model proposes; AF applies.** The reply is a JSON list of operations — memory
   `create` / `update` / `forget`, and later skill operations (decision 7) — each with the target
   name, the new text, a one-line reason, and the turn indexes that are its evidence. AF parses and
   validates it (names, limits of 0108 decision 3, base revision from the input it built); a
   malformed reply is dropped whole. The helper never touches a file and has no tools, so a
   confused or injected helper can produce only text that AF then checks. Write targets:
   - **Memory, every kind: the AF store**, through the same `agentMemSave` / `agentMemForget`
     path as the af MCP tools, scoped by the *reviewed session's* project (0108 decision 2) with
     the user scope for kind-independent facts. The reviewed session's kind needs no write
     target of its own, because every kind reads the store through the af MCP; this is why the
     kinds outside `memory_roots.go` (opencode, agy, copilot, cursor, kiro, lcpp, muse) are
     covered by the same code as claude and codex.
   - **Native memory trees (claude `*/memory/**`, codex `memories/**`): not written.** claude's
     native writer is already off under the AF switch; codex's is the member's separate choice
     (0112).
   - **Skills: see decision 7.**
5. **Versioning and rollback reuse the 0022/0108 history; nothing new is invented.** An applied
   proposal is an ordinary one-commit publish, authored `review` (a value the existing
   `agentMemKindRe` already admits) with the reviewed session as author session and a
   `AF-Review-Run: <id>` trailer. The change list shows it, per-change revert and forget work as
   they do, and "revert this run" reverts every commit that carries the id as new commits. Nothing
   rewrites history. A proposal rejected by the member is remembered by content hash so the same
   idea is not proposed again; a forgotten memory is never revived (the 0108 tombstone).
6. **The approval gate: proposals queue by default; publishing directly is a separate, later,
   member-chosen mode.** Three values of a per-user setting `memoryReview`: `off` (default),
   `propose`, `auto`. 0108 decision 8 argued against an approval queue because every session
   holds `AGENT_TOKEN` and a shell, so a gate cannot stop a hostile agent. That argument does not
   transfer: this gate is not against a hostile session, it is against an *unattended model
   writing on its own initiative* on the strength of text a third party may have written. Nobody
   asked the helper to write anything, so unlike an agent's `memory_save` there is no intention
   to respect. In `propose`, proposals are files in a pending queue beside the store (outside the
   memory repo, so unapproved text is never in the published history); the member approves,
   rejects or rejects all in Settings → Agent memory, next to the existing change list, reusing its
   diff; a Console notification (the `memory-sync` notice pattern) carries the pending count.
   Approving applies through the normal publish with the proposal's stored base revision; a
   stale base marks the proposal stale and does not overwrite. In `auto` memory proposals publish
   at once through the same checks and are found and undone afterwards, exactly as 0108 decision
   8 handles agents' writes. **Skills never use `auto`.** What the queue costs (approval load, a
   queue that is approved unread or switched off) is the cost 0108 named; it is accepted here
   because the default is off and `auto` exists for the member who finds it too heavy.
7. **Skills: a separate AF-owned store, always approval-gated, distributed only where a user
   skills root exists, and not before the memory path works.** A skill is read as an order
   (0108 decision 7 makes memory "evidence, not an order"), so a bad skill is worse than a bad
   memory and gets the stricter treatment. User scope only: a project-specific lesson goes to
   memory. Writing into a working copy's `.agents/skills` is not done, because it leaves
   uncommitted changes in the member's repository. The store lives under the same mount, its
   history in the memory repo under a path class of its own (`agentMemRepoPathRe` admits only
   `af/(user|projects/…)/<name>.md`, so a skill needs its own pattern or the change list would
   withhold it), scanned with the same rules. Distribution mirrors `fleetskills`: one directory
   per skill in the claude, codex, opencode and muse roots, with a **different prefix and a
   different marker** from fleetskills' `af-` notes, so neither sync removes the other's
   directories. agy, copilot, cursor, kiro and lcpp receive nothing; they still get the lesson if
   it is also saved as a memory. The patch-over-create preference is enforced by input (decision
   2): the existing skills with matching keywords are in the prompt, and a `create` whose name
   or description is near-identical to an existing one is converted to a proposed `update`.
8. **Opt-in, and the failure modes.** Off by default, per user, with a plain statement next to
   the switch that conversation excerpts are sent to the pinned or default assistant provider and
   that it costs tokens. Failures, each with the behaviour that makes it harmless:
   - no usable backend, or the call fails: back off; the watermark does not move; nothing is
     published; the notification is not raised for transient failures.
   - malformed or oversized reply: dropped whole, counted, backed off.
   - a secret in a proposal: refused by the existing scan, as for an agent's write; the helper is
     not retried.
   - stale base revision, name clash: the proposal is marked stale, never merged.
   - churn (the same proposal each window): the rejected-hash memory of decision 5, plus the
     per-day run cap of decision 9's P3.
   - concurrent reviews of the same project: the store lock and the revision check serialise;
     the loser's proposal goes stale.
   - the workspace stops mid-run: the run is lost, the watermark is unmoved, the next wake-up
     repeats it; the cost already incurred stays in the ledger.
   - a review of a review: helper calls are not sessions and have no transcript, so they are
     never reviewed.
   - the helper's text echoes the transcript into the pending queue: the queue file is a
     secret-scanned artefact in the same private mount as the store, deleted on approval or
     rejection; only the hash survives.
9. **Phases.** Sizes are estimates.
   - **P0, S (#2319): measure.** Transcript sizes per idle window, the cost of one helper turn,
     and the volume of hand-run proposals on a real workspace; correct the proposed caps.
   - **P1, M (#2320): engine without publish.** Loop, watermark, redacted input, helper turn
     with `memory.review` in the ledger, validation, the pending queue, the `memoryReview`
     setting (`off` / `propose`), "Review now". Includes the ledger enum, catalogue and AI-assist
     row.
   - **P2, M (#2321): approval UI and publish.** The pending list in Settings → Agent memory,
     approve / reject, the notification, the `review` author and run id, "revert this run", the
     rejected-hash memory.
   - **P3, S (#2322): `auto` mode and budgets.** Only after the owner answers open questions 1
     and 2; includes the daily run cap and per-run token cap.
   - **P4, L (#2323): learned skills.** Store, scan, always-gated approval, distribution to the
     four roots with the distinct prefix and marker.

## Rejected

- **A hermes-style fork of the live turn ("same model, same cached prompt").** AF does not own the
  CLIs, and four kinds have no hook seam at all.
- **Triggering on the transcript read, as the auto-title does.** It runs only while a Console is
  open, so unattended sessions would never be reviewed (Context).
- **Every N tool calls or every turn.** The cost scales with activity rather than with what there
  is to learn, and the mid-turn state is the wrong moment to summarise. A turn-end plus a window
  of K turns is the same intent at a lower rate.
- **Review only at session stop or delete.** Many sessions are never stopped, and a stop or a
  recreate would drop the window.
- **A nightly sweep over cold transcripts as the only trigger.** It works for kinds without
  hooks but delays the lesson and wakes a stopped workspace; it may be added as a second trigger
  later (not planned in this record).
- **Writing into claude's native memory or codex's `memories/`.** Bypasses the revision check,
  scan and attribution (0108 Rejected), and for codex fights its own integration pipeline.
- **Letting the helper hold the af MCP tools and call `memory_save` itself.** Publishes by
  acting, with the model's mistakes and any injected text going straight to every kind's next
  session; the structured-proposal contract keeps AF as the only writer.
- **Auto-publish as the default.** See decision 6; it stays available as an explicit mode.
- **Project-scope skills in the working copy.** Dirties the member's repository.
- **Writing skills into the 0042 user instructions.** Voids 0042 decision 8.

## Consequences

- Experience can be turned into memory without anyone asking an agent to save it, with the same
  history, scan and revert the member already has, and the cost shows up as its own ledger
  column per reviewed session.
- A new failure class appears: a helper that writes plausible but wrong lessons. The default is a
  queue, so the member sees it before any other session does, at the price of an inbox.
- The transcript, redacted, goes to a provider the member did not choose per session. The
  switch's text says so; the feature pin lets the member choose which.
- Review does not run while the workspace is stopped, like every Agent-side feature (0022).
- The `memory.review` value is appended to 0029's enum with a dated note in 0029, not by
  rewriting it.

## Open questions for the owner

Each is a choice about cost, default or approval experience and is deliberately not decided here.

1. **When the switch is turned on, which mode: `propose` (the recommendation) or `auto`?** The
   record defaults to `propose` for memory and always for skills; whether `auto` may ever be the
   offered default is the owner's call.
2. **The cost ceiling.** Proposed starting points are one review per session per 10 new turns,
   at most one in flight, and a daily cap on runs; the numbers, and whether to apply them to
   unattended and scheduled sessions at all, are the owner's. P0 supplies the measured figures.
3. **May the transcript go to a provider other than the reviewed session's kind?** The default
   follows the shared one-shot order. Pinning the review to the session's own kind avoids sending
   a kind's transcript to another vendor but drops kinds that cannot run headless.
4. **Where does approval live?** Settings → Agent memory only (the recommendation: one place, no new
   chrome), or also a banner in the session's mirror when its review produced proposals.
5. **Are skills in scope for the first release?** The recommendation is memory first (P1–P2),
   skills after (P4) and only if memory review proves worth its cost.
6. **How long are rejected proposals' hashes and pending text kept?** Proposed: pending text is
   deleted on decision, the hash is kept until the memory it concerned is forgotten.

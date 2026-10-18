# 134. Background memory review, phase P0 — transcript sizes and helper-turn cost (#2319)

status: **Measured (2026-10-18).** ADR 0117 (decisions 1–3 and 9) proposed a 120 s settle, K = 10
turns, a 40,000-character input, 5 output operations of 8 KiB, one review in flight and a daily
run cap, none of them measured. This journal replaces those proposals with figures from one real
workspace and recommends corrected caps (section 6). The caps are **recommendations awaiting the
owner's confirmation**; the ADR text is unchanged and carries a dated note that points here.
Sizes come from the claude and codex transcripts of one development workspace; the helper cost
comes from three hand-run calls. Both are small and unrepresentative in ways listed in section 7.

## 1. Method

### 1.1 What a "turn" is

ADR 0117 counts turns with the unit AF's transcript normalizer produces, so that is what was
counted. It is **not** a user prompt and not a JSONL line:

- **claude** (`agents/claude/transcript.go`, `parseTurn`): one displayable line = one turn. A
  user line with text (or a queued/steering prompt) is a user turn; every assistant line that
  carries text, a tool call or a non-empty thinking block is its own assistant turn. Claude
  writes one response as several lines (text, each tool call), so one response is several turns.
  A user line holding only tool results is **not** a turn; the result is attached to its call.
  Meta lines, bookkeeping lines and the compaction summary are not counted.
- **codex** (`agents/codex/transcript.go`, `parseRolloutFull`): each displayable `response_item`
  (user or assistant message, reasoning with a summary, a function or custom tool call) is a
  turn. Tool outputs attach to their call; developer messages and injected context blocks are
  dropped.

Consequence: one user prompt produced about 12 assistant turns in claude (98,462 assistant
turns for 8,164 user turns) and about 11 in codex (44,074 for 3,984). "K = 10 turns" therefore
means roughly one prompt and its tool chain, not ten exchanges.

### 1.2 Part A — sizes (every claude and codex transcript on the workspace)

Two scripts streamed the files once each and wrote **only numbers** (one TSV row per normalized
turn, one per file). A second script computed the aggregates. No transcript text was printed,
searched or written anywhere. Where the normalizer's Go code decides something (what is a turn,
the 4,000-rune cap on a tool output in `transcript.CapOutput`, the 80-rune tool label), the
script re-implements the rule; it was checked against the Go source, not against the Go binary.

Per turn the scripts record the character counts of: text (user and assistant text), the tool
trace (tool name plus the one-line label), the delegation/edit payload, the tool result
(capped at 4,000 characters as `CapOutput` does, and separately at 500), the thinking text, and
the raw JSONL bytes of the turn's own line plus the lines of its tool results. Five "levels" are
reported:

| Level | Content | Why |
|---|---|---|
| L1 | text only | the conversation |
| L2 | text + tool trace | what happened, without tool output |
| L4 | L2 + each tool result clipped to 500 characters | the recommended input shape (section 6) |
| L3 | L2 + payloads + each tool result at the normalizer's 4,000-character cap | what the mirror export carries |
| raw | JSONL bytes | the naive upper bound |

Windows were measured two ways. **Fixed blocks:** consecutive non-overlapping blocks of K turns.
**Policy simulation:** a window closes at the first *quiet point* (no activity for S seconds after
the last turn) at which at least K turns are unreviewed, and the watermark then moves to the end
of the window; a session's last turn counts as a quiet point. This is ADR decision 1 minus the
status-record details. "Activity" is any user or assistant line including tool results;
bookkeeping lines are ignored because both CLIs write some of them when the *next* prompt
arrives (codex writes `task_complete` there), which made every gap read as 0 s until they were
excluded.

Sessions whose working directory is under the per-session scratch root (probe runs) are
excluded from the headline figures ("real"); including them changes no figure by more than a few
per cent.

### 1.3 Part B — one hand-run helper trial

Material: **only** the sessions of the #1559 work itself (the parent conductor and two other
claude sessions identified by working directory, and one codex session). One had only 7 turns
and produced no window. Three windows were sent, one tool-less call each:

1. **Window.** Built by a script from the policy simulation (K = 10, S = 120 s), turns numbered,
   tool results clipped to 500 characters and marked as data, cut at 40,000 characters (newest
   kept; none was that large).
2. **Redaction before anything was sent** (ADR decision 2, option (b)): every decoded string
   (turn text, tool label, tool result) was scanned line by line with the rules of
   `memoryx/memory_secrets.go`. The script **loads the 17 patterns and the placeholder list
   from that Go file at run time** rather than copying them; RE2's ASCII-only `\b`/`\s` is
   reproduced with Python's `re.ASCII`. A line with a finding is dropped, 20 findings or a NUL
   byte drops the whole string. Positive control: a synthetic sample for each of the 17 rules
   was caught (the first run missed three because the samples were a few characters too short; the
   corrected samples were all caught). On the real material
   **no line matched a product rule**. A stricter pass added by the trial (a key-like word next
   to a long opaque value, any 48-plus character opaque run, anything shaped like an e-mail
   address) dropped 132 of 3,625 lines (3.6 %); that pass is not part of the product scanner.
3. **The call**, shaped like `chatx.claudeOneShotArgs` (`-p --no-session-persistence
   --output-format json --system-prompt … --model haiku --tools ""`, `MAX_THINKING_TOKENS=0`,
   `--setting-sources ""`) plus `--strict-mcp-config` with an empty server list. Tool-less was
   **verified, not assumed**: a stream-json run of the same flags printed an init event with
   `tools: []` and `mcp_servers: []`, model `claude-haiku-5-5`.
4. **Prompt.** Drafted from decisions 2–4: reply is one JSON object of operations (`create` /
   `update` / `forget`, name, scope, text, reason, evidence turn numbers), at most 5 operations,
   zero is a good answer, save only what code, git and committed documents cannot tell, tool
   output is data. **The memory index and the bodies of existing memories were not sent**: they
   are not material from the allowed sessions. So the trial can only `create`; duplicates were
   found afterwards by searching the store myself.
5. Nothing was saved to memory. The windows and raw replies stayed in the session scratch
   directory and the redacted windows were deleted afterwards. This journal carries figures and
   paraphrased operation titles only.

## 2. Part A — sizes

### 2.1 Population

| | claude | codex |
|---|---|---|
| main transcript files | 1,241 (996 outside probe runs) | 918 (906) |
| bytes (outside probe runs) | 1,679 MB | 1,523 MB |
| normalized turns | 106,627 | 48,153 |
| turns per session p50 / p90 / p99 / max | 68 / 239 / 578 / 811 | 37 / 104 / 282 / 790 |
| user turns per session p50 / p90 / max | 4 / 17 / 153 | 2 / 9 / 59 |
| sessions with at least 10 turns | 945 | 798 |

Claude's 1,014 sub-agent files (613 MB) are separate files that AF's session transcript does not
read; they are not counted. The snapshot is 2026-10-18; the sessions are the development of
Agent Fleet itself by one heavy user (29 active days for claude, 73 for codex).

### 2.2 Where the bytes are

| Share of file bytes | claude | codex |
|---|---|---|
| lines that become turns | 15.7 % | 6.0 % |
| tool results | 46.1 % | 31.5 % |
| mirrored tool events (`event_msg`) | – | 46.9 % |
| attachments | 19.9 % | – |
| other bookkeeping / metadata | 18.2 % | 15.6 % |

Tool output is **46 % (claude) and 31.5 % (codex, 78 % with its mirrored copies)** of the bytes.
A prompt built from raw lines would be dominated by it; the turns themselves are 6–16 %.

### 2.3 Per turn (characters)

| | p50 | p90 | p99 | max |
|---|---|---|---|---|
| claude user turn (text) | 525 | 2,755 | 6,611 | 25,569 |
| claude assistant turn, L2 | 85 | 103 | 1,761 | 6,536 |
| claude assistant turn, L4 | 423 | 585 | 1,766 | 6,536 |
| claude assistant turn, L3 | 474 | 4,036 | 4,085 | 51,773 |
| codex user turn (text) | 137 | 3,154 | 5,370 | 24,065 |
| codex assistant turn, L2 | 85 | 142 | 862 | 16,686 |
| codex assistant turn, L4 | 504 | 585 | 862 | 16,686 |

Claude text is 1.53 UTF-8 bytes per character (Japanese and English mixed). Claude's thinking
blocks are almost always empty in the transcript (p99 179 characters).

### 2.4 Per fixed block of K turns (characters; p50 / p90 / p99)

| K | claude L2 | claude L4 | claude L3 | codex L2 | codex L4 | codex L3 |
|---|---|---|---|---|---|---|
| 5 | 425 / 2,688 / 6,134 | 1,965 / 3,533 / 7,244 | 4,169 / 13,008 / 20,425 | 463 / 2,333 / 5,430 | 2,187 / 3,324 / 6,592 | 6,986 / 16,365 / 20,425 |
| 10 | 981 / 4,637 / 8,860 | 4,069 / 7,002 / 11,039 | 8,876 / 24,441 / 37,957 | 1,069 / 4,301 / 7,035 | 4,462 / 7,124 / 9,826 | 14,928 / 29,217 / 36,401 |
| 20 | 3,392 / 7,702 / 15,378 | 8,406 / 13,268 / 19,142 | 18,573 / 46,409 / 68,201 | 3,238 / 6,953 / 10,175 | 9,133 / 13,221 / 16,256 | 31,518 / 54,502 / 68,731 |

Raw JSONL bytes for K = 10 blocks: claude p50 57 KB, p90 161 KB, p99 977 KB, max 5.3 MB; codex p50
52 KB, p90 189 KB, p99 438 KB, max 15.7 MB. **A prompt cannot be built from raw lines.**

### 2.5 Time between turns (the settle figure)

| | claude | codex |
|---|---|---|
| gap inside a response, p50 / p90 / p99 | 2 / 9 / 27 s | 3 / 11 / 46 s |
| gaps inside a response longer than 45 s | 0.38 % | 1.07 % |
| gaps inside a response longer than 120 s | 0.03 % | 0.14 % |
| gaps inside a response longer than 300 s | 0.01 % | 0.02 % |
| gap before the next user prompt, p50 / p90 / p99 | 189 s / 35 min / 13.6 h | 39 s / 17 min / 4.7 h |
| prompt arrives within 120 s of the last activity | 37.4 % | 61.1 % |
| prompt arrives within 300 s | 61.3 % | 73.4 % |
| prompt arrives within 600 s | 75.7 % | 83.3 % |

A 45 s settle (the title's) would sometimes fire inside a long tool call; 120 s almost never
does. But the person comes back quickly, so *any* short settle reviews a conversation that is
still going: with S = 120 s, 63 % (claude) and 59 % (codex) of closed windows are followed by
more activity within ten minutes; with S = 300 s, 39.5 % and 38.1 %.

### 2.6 Policy simulation (runs and window sizes)

"Active day" is a UTC day with at least one run. Window sizes are characters; L4 is the
recommended shape. The last two rows of each kind are the recommendation of section 6.

| Trigger | kind | runs | runs per active day p50 / p90 / max | window turns p50 | L2 p50 / p99 | L4 p50 / p90 / p99 | over 40k: L2 / L4 |
|---|---|---|---|---|---|---|---|
| 10 turns, S = 45 s | claude | 3,928 | – | 17 | 4,502 / 20,623 | 8,402 / 22,702 / 59,316 | 0.2 % / 3.2 % |
| 10 turns, S = 120 s (proposal) | claude | 3,324 | 116 / 194 / 261 | 20 | 5,176 / 25,958 | 9,669 / 28,664 / 72,111 | 0.4 % / 5.7 % |
| 10 turns, S = 300 s | claude | 2,565 | 91 / 153 / 208 | 25 | 6,569 / 36,741 | 12,349 / 40,271 / 84,841 | 0.9 % / 10.2 % |
| 20 turns, S = 300 s | claude | 1,933 | 69 / 111 / 167 | 36 | 8,896 / 41,389 | 17,666 / 45,696 / 90,114 | 1.1 % / 13.9 % |
| 30 turns **or** 3 prompts, S = 300 s | claude | 1,923 | 71 / 109 / 159 | 38 | 8,965 / 44,075 | 17,704 / 46,306 / 92,929 | 1.1 % / 14.1 % |
| 10 turns, S = 120 s (proposal) | codex | 1,627 | 12 / 56 / 81 | 21 | 4,424 / 18,649 | 10,443 / 24,129 / 52,840 | 0.0 % / 2.6 % |
| 30 turns **or** 3 prompts, S = 300 s | codex | 856 | 7 / 29 / 71 | 40 | 8,236 / 24,836 | 19,696 / 37,185 / 74,518 | 0.1 % / 7.7 % |

Counting prompts alone starves unattended sessions: with a threshold of 3 user prompts only 622
of 996 claude sessions ever get a run (451 of 906 for codex), because a spawned or scheduled
session often has one prompt and hundreds of tool turns. That is why the recommended trigger has
both a turn count and a prompt count.

Combined across both kinds with the recommended trigger: 2,779 runs over 70 active days; per day
p50 13, p90 109, p99 181, **max 184**. Bursts under the proposed trigger: at most 5 windows
closing in the same minute for claude and 6 for codex.

## 3. Part B — the helper trial

### 3.1 Which model OneShotShort is

`chatx.OneShotHeadless(…, OneShotShort, …)` resolves `kind` by the feature's pin or the shared
order, then the model by the feature pin, the member's "short text" setting, or the computed
recommendation (`recommendedUtilityModelV`, `chat_providers.go`):

| kind | model the Short tier picks | tool-less? |
|---|---|---|
| claude | first visible of `haiku`, `sonnet`, `opus`, `fable` — the `haiku` alias, **measured as `claude-haiku-5-5`** on this workspace | yes: `--tools ""` |
| codex | cheapest priced model in the live catalog (`cheapestListedModel`), else the first id with a size marker; reasoning effort `low` | no (`--ephemeral` only) |
| opencode | not auto-picked: the member's default unless `AF_TITLE_MODEL_OPENCODE` names one | no |
| agy | cheapest priced model of its catalog, else the Flash default | no (`read_url`) |
| cursor | `AF_TITLE_MODEL_CURSOR`, else the CLI default (no recommendation case) | no |
| muse | `museSafeOneShotModel` | – |

Only claude meets decision 3's tool-less requirement today, so the trial ran on it.

### 3.2 Results

| | window A (claude) | window B (claude) | window C (codex) | floor call |
|---|---|---|---|---|
| window turns / characters sent | 34 / 19,893 | 26 / 14,555 | 11 / 5,850 | – |
| whole prompt, characters | 21,337 | 19,434 | 7,816 | 30 |
| input tokens | 15,700 | 14,880 | 9,765 | 6,657 |
| output tokens (of which hidden thinking) | 336 (326) | 3,795 (2,825) | 290 (280) | 4 (0) |
| cost (API-equivalent) | $0.0033 | $0.0049 | $0.0021 | $0.0013 |
| latency | 2.7 s | 19.5 s | 2.7 s | 1.2 s |
| operations proposed | 0 | 3 | 0 | – |

- **Floor:** a one-word prompt already costs 6.66k input tokens (the managed instructions and
  skill list the CLI loads), so every run pays about $0.0013 before any window.
- **Tokens per character:** (input − 6.66k) / prompt characters = 0.42, 0.42 and 0.40, so about
  **0.41 tokens per character** for this Japanese and English mix.
- **Rates implied:** the four costs fit exactly $0.2 per million input tokens (all of them were
  written to the 1-hour cache) and $0.5 per million output tokens; no price list was assumed.
  These are API-equivalent figures, not what the member's plan meters.
- **Thinking was not off.** `MAX_THINKING_TOKENS=0`, which `claudeOneShotEnv` sets for every
  one-shot, did not stop haiku-5-5 thinking on a task: 280–2,825 hidden thinking tokens, 74–97 %
  of the output (the one-word call used none). It cost little here, but the output ceiling of
  ADR decision 3 cannot be read from the visible reply, and the title and branch one-shots
  presumably carry the same hidden cost. Not retested with `--effort`, to keep to one call per
  window.
- **Operations (window B, paraphrased; text 383–1,165 characters; evidence turn numbers all
  inside the window):**

| Operation | Plausibly useful? |
|---|---|
| create (project): the owner's answers to the six open questions of the ADR | No — the answers are now a dated note in the committed ADR, and half of the text is task status |
| create (project): the effort rule for reviewers of privacy-related work | Plausibly — a durable workflow rule, no existing memory states it; the example of a lapse inside the text is noise |
| create (user): reporting-language and branch conventions for the conductor role | No — a duplicate of the existing `commit-message-language` memory and of the workspace policy and user instructions |

  Windows A and C returned `{"ops":[]}`. Window A is the session that drafted the ADR, whose
  conclusions are in the committed document, so empty is plausible; I did not re-read the windows
  to look for misses, so the miss rate is not measured. **1 of 3 proposals is plausibly useful**;
  with the index in the prompt the two duplicates would at best become an `update`. Three
  samples do not show that the Short tier is too weak, but they do show why the default must be
  `propose`: the helper does not check what the repository or an existing memory already says.

## 4. Findings that change reading of the ADR

1. **"Turn" means displayable row** (section 1.1). K = 10 fires about once per prompt.
2. **Raw lines are unusable** as input (section 2.4); the window must be built from turn text,
   labels and clipped tool output.
3. **Hidden thinking tokens** are most of the output cost and are not capped by the existing
   one-shot environment (section 3.2).
4. **A fixed 6.7k-token floor** per call makes the number of runs, not the window size, the cost
   driver at the Short tier: a 40k-character window adds about $0.003 to a $0.0013 floor.
5. **Cost is not the binding constraint.** At the Short tier a run is $0.003–0.006, so even
   184 runs a day is about a dollar; the limits that matter are the approval load (2 of 3
   proposals were useless), the member's plan quota, and the single in-flight slot.
6. **Settle does not mean finished.** Even at 300 s, 38–40 % of windows are followed by more
   activity within ten minutes; the watermark design already tolerates that.
7. `chatx.claudeOneShotArgs` has no `--strict-mcp-config`. Whether user-level MCP servers load
   under `--setting-sources ""` was **not tested**; P1's deny-all contract must pass an empty
   MCP config and check the init event for `tools: []` and `mcp_servers: []`, as this trial did.

## 5. What was not measured

- Kinds other than claude and codex. opencode, copilot, muse, agy, cursor, kiro and lcpp all
  report `CanTranscript`, and none was measured: the owner's exception covered claude and codex
  transcripts only. Their normalizers differ (store-backed kinds update a message after the
  fact), so these figures should not be read across.
- The memory index and existing-memory bodies in the prompt. The default `memory_index` budget is
  24,576 bytes, which alone is more than half of the proposed 40,000 characters; the bodies of
  ADR decision 2 were not sized.
- Any non-claude helper backend, a second tier above Short, and `--effort` control.
- Helper quality beyond three windows, and the false-negative rate.
- Plan-quota use. Costs are API-equivalent.

## 6. Corrected caps

All values are **recommendations awaiting the owner's confirmation** (ADR 0117 open question 2
says the numbers are confirmed after P0).

| Proposal (ADR 0117) | Measured basis | Recommended |
|---|---|---|
| Settle 120 s | Gaps inside a response exceed 120 s in 0.03–0.14 % of cases and 300 s in 0.01–0.02 %; at 120 s, 59–63 % of windows are followed by activity within ten minutes, at 300 s 38–40 %; 300 s removes 23 % of claude runs relative to 120 s (2,565 against 3,324) | **300 s** |
| K = 10 turns | A turn is a displayable row, about 12 per prompt; 10 rows means about one run per prompt, 116 runs per active day at the median for claude (max 261). Counting prompts alone leaves 37–50 % of sessions unreviewed | **30 turns or 3 user prompts**, whichever comes first (71 runs per active day at the median for claude, max 159; codex max 71) |
| 40,000-character input | L2 fits 98.9 % (claude) of windows, L4 fits 85.9 % (claude) and 92.3 % (codex); raw bytes are unusable; 0.41 tokens per character | **Keep 40,000 characters for the window**, built as L4 (each tool result clipped to 500 characters, redacted before the clip). When a window is longer, **split it at a turn boundary and review the oldest part first** instead of dropping it (the proposed "newest kept" never reviews the dropped turns) |
| Index and up to five memory bodies (unsized) | Index budget 24,576 bytes; bodies not measured | **Assumption for P1 to confirm:** index at most 12,000 characters, at most 2,000 characters per body, so the whole prompt stays near 62,000 characters (about 25k tokens plus the 6.7k floor) |
| Output of 5 operations of 8 KiB | 0, 0 and 3 operations; texts 383–1,165 characters; output 290–3,795 tokens, 74–97 % hidden thinking | **Keep 5 operations; 4 KiB per operation text; 16 KiB per reply**; a larger reply is dropped whole. Do not rely on the thinking switch |
| One review in flight | Latency 2.7–19.5 s; at most 5–6 windows close in one minute | **Keep one in flight**; one pending window per session (it just grows), a global queue bound of 20 sessions |
| Daily run cap (P3) | Recommended trigger, both kinds combined: p90 109, p99 181, max 184 runs per day; $0.003–0.006 per run | **200 runs per day per workspace** (about $0.8 a day API-equivalent at the Short tier); a runaway guard, not a limit anyone should meet |
| Tier `OneShotShort` | claude resolves to `claude-haiku-5-5`; only claude is tool-less; 1 of 3 proposals useful, no proof of weakness | **Keep `OneShotShort` for P1.** Count approved against rejected proposals in P1 and revisit with a pin to the Prose tier if fewer than a third are accepted |

## 7. Limits of these figures

- One workspace, one heavy user working on this repository. Active days: 29 (claude), 73 (codex).
  The runs per day are a high-use ceiling, not an average member.
- The simulation counts a session's last turn as a quiet point, treats every kind as having
  turn-end evidence, and ignores the `agentMemory` switch, the status record and restarts.
- Claude transcripts of resumed or forked sessions repeat history; that inflates turn counts
  slightly and was not de-duplicated.
- Three helper calls. Cost per run is a fit to three points plus a floor call.

Follow-ups: #2320 (P1 owns the thinking and MCP controls of the findings above and the unsized
index and bodies); #2319 stays open until the owner confirms the caps.

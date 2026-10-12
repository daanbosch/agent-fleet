# 0113. A member-origin mark the Agent can verify: the CP signs a member's relayed write, the Agent holds only the public key

English | [日本語](0113-member-origin-mark.ja.md)

- Status: **accepted** (2026-10-12), built with #1997: the CP signs three relayed routes
  (`control-plane/member_origin.go`), the Agent verifies (`workspace/agent/internal/memberorigin`) and
  requires the mark on the member-only memory writes. **Not verified end to end**: no real CP↔Agent run
  was possible in the workspace this was built in; both halves are tested against a shared wire vector.
- Tracking: #1997. Follow-ups: #2014. Related: [0108](0108-af-owned-agent-memory.md) decisions 4 and 8 (the switch, and why an
  approval step is not a boundary) / [0102](0102-aws-login-through-the-console.md) (binding a result to a
  person's press when a route cannot tell who called it) / [07-security §7.2, §7.5](../build/07-security.md#75-cp--agent-authentication).

## Context

Every Agent route accepts the workspace's `AGENT_TOKEN`, and every session's shell holds it: the af MCP
server needs it. The CP adds `X-AF-Relay: cp` to what it relays, but that header is a log hint any caller can
set. So a route meant only for the member's Console cannot tell the member from a session.

For AF memory that matters in three places. `entries/pin` and `entries/revert` (revert also forgets) record the
member as the author by route and are not gated by the `agentMemory` switch, because the switch governs what
sessions may do. A member create/edit route would do the same with arbitrary new text, so a session could write
memory as the member while the switch is off; it was pulled out of #1985 for that reason.

The constraint that shapes the answer: a session runs as the **same uid** as the Agent and shares its
filesystem, process table and network namespace. Anything the Agent can read — its environment,
`/proc/<pid>/environ`, a file, a key handed to it — a session can read too. A shared HMAC key delivered to the
Agent is therefore a key the session holds as well.

## Decisions

1. **The mark is an Ed25519 signature made by the CP; the Agent holds only public keys.** Verifying needs
   nothing secret, so it does not matter that a session can read what the Agent reads. The private key never
   leaves the CP process.
2. **The key is derived, not stored.** Version *N*'s seed is `HMAC-SHA256(token-signing master,
   "af-member-origin-sign/vN")`, the same master the per-membership bridge tokens derive from
   (`tokenSignMaster`: `AF_MASTER_KEY`'s digest, or the deployment's random dev master). Nothing new to
   provision or back up, and a leaked seed of one version says nothing about the next.
3. **Delivery reuses the start env.** `workspaceExtraEnv` injects, at every container start,
   `AF_MEMBER_ORIGIN_KEYS=v<N>:<public key>,v<N+1>:<public key>`, `AF_WORKSPACE_ID` and `AF_MEMBERSHIP_ID`.
   None of it is secret. The Agent reads them once; a session cannot change a running process's environment.
4. **What is signed.** The header `X-AF-Member-Origin: <kid>.<base64url(claims)>.<base64url(signature)>`,
   signature over `"af-member-origin/v1\n" + claims`, where the claims are the JSON bytes as sent (no
   canonicalisation): method, the Agent-side path, the raw query, SHA-256 of the body, workspace id,
   membership id, the CP identity id, `iat`, `exp` (60 s after `iat`) and a 128-bit nonce. The Agent rejects
   any mismatch with the request it received, a lifetime over 2 minutes, an `iat` more than 30 s ahead of its
   clock, an expired mark, and a nonce it has seen.
5. **Replay cache.** The Agent keeps each nonce until its `exp`, at most 4,096. When full and nothing has
   expired it refuses (503) rather than accept a mark it cannot remember: a full cache must never become a
   replay window.
6. **Which routes.** The CP signs exactly `POST /api/agents/memory/entries/{pin,revert,member-save}`
   (`memberOriginRoutes`) and only when the workspace is the caller's own membership's. On every relayed
   request (REST and SSE) it first deletes any `X-AF-Member-Origin` the browser sent. The Agent wraps the same
   three routes in `memberorigin.Require`, and their handlers refuse again when no verified claims are in the
   request context, so a route registered without the middleware stays shut.
7. **Unsigned pin and revert are refused.** No compatibility window: the Console reaches these routes only
   through the CP, which signs them, so nothing legitimate sends them unsigned. An Agent older than the CP
   ignores the header (pin and revert behave as before, and `member-save` is a 404); a CP older than the Agent
   sends no mark and the Agent refuses — the feature is off, not open.
8. **Author only when verified.** `member` / `console` is written as the author only by these three routes,
   so only behind a valid mark. `member-save` runs the tools' own save (revision check, secret scan, limits),
   is not gated by the switch (like pin and revert), cannot start a project, and leaves the project's
   `project.json` as the sessions wrote it. The CP audits it as `memory.entry.save` with an id-shaped project
   hint only.
9. **Rotation.** `AF_MEMBER_ORIGIN_KEY_VERSION` (CP env, default 1) selects the signing key. Because a
   workspace starts with the current and the next public key, raising it by one keeps every running workspace
   verifying; the old private key is never used again, and workspaces started afterwards no longer accept it.
   Raising it by more than one, or rotating `AF_MASTER_KEY`, leaves running workspaces refusing member writes
   until they restart. `0` turns signing off (nothing injected, every member write refused).

## Threat model

- **In scope**: a session, or anything running in it, that holds `AGENT_TOKEN`, can set any header, can read
  the Agent's environment and files, and can capture a mark it sees in transit inside the workspace. It cannot
  produce a mark (no private key), reuse one for another route, query, body, workspace or member (all
  signed), or replay one (nonce, expiry).
- **What the mark does not do.** It decides what the Agent's own routes accept; it does not make the store
  tamper-proof. A session with a shell can write the store's files and history directly, as the uid that owns
  them, and can replace binaries in `~/.local/bin` that the next start runs. That was already true of every
  author field (decision 8 of 0108 records the same limit), and no in-workspace mechanism closes it.
  `ptrace_scope=1` (measured in this workspace) keeps a session from reading the Agent's memory, but there is
  no private key there to read anyway.
- **As strong as the CP's authentication.** The CP signs for whoever it resolves as the member. Under
  `AUTH=dev` it trusts every caller, so the mark adds nothing there; under `oauth` it needs the member's
  session cookie, which is not in the workspace.
- **Out of scope**: a compromised CP (it holds the master), and a stolen master key.

## Failure modes

| Situation | Result |
|---|---|
| Workspace started before this change, or by a CP with signing off | 503 `member_origin_unavailable` on the three routes until it restarts |
| No mark, or only `AGENT_TOKEN` / `X-AF-Relay` | 403 `member_origin_required` |
| Mark for another request, workspace, member, key; expired; replayed | 403 `member_origin_invalid` |
| Body over the CP's 2 MiB buffer | 413 at the CP; nothing reaches the Agent |
| Request for another member's workspace | 403 at the CP; nothing is signed |

## Rejected

- **A shared HMAC key in the Agent's env or a file.** Readable by every session (same uid); forgeable.
- **Keeping the Agent's key in another process or uid.** No such process exists in a workspace, and adding a
  root helper contradicts the no-root image.
- **A per-request token the Agent asks the CP to confirm.** Needs a CP round trip on every member write and a
  CP endpoint a session can also reach with its own credentials; the signature needs neither.
- **An unsigned grace period for pin and revert.** Every legitimate caller is the CP, which signs.

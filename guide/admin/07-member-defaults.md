---
audience: "a tenant administrator pointing new members at the team's agent"
updated: "2026-10"
---

# 07. Member defaults

English | [日本語](07-member-defaults.ja.md)

A new member's first screen is Home, with a welcome card at the top. Its second step is
**Connect an agent**. Agent Fleet supports several agents (Claude, Codex, opencode and others),
and a new member usually cannot tell which one the team pays for. Here you name it, and the step
then says **"Your team's default is Claude (any other agent works too)"**.

It **names an agent; it does not restrict anything**. A member who connects another agent still
completes the step, and every agent stays available in the launch dialog. Which agents members
may use at all is decided by their own accounts and by the deployment, not here.

## Setting the default agent

Tenant settings → **Member defaults** (under Operations). Pick one of Claude, Codex, opencode,
Cursor, Copilot, Kiro or Muse, or **None**, and press **Save**. **None** brings back the generic
wording that lists the choices.

Agents that need no account of the member's own are not offered: the shell and SSM, and the
deployment's own engine (who may use that is set under [who may use the inference engines](02-limits.md#who-may-use-the-inference-engines-you-can-set-this)).

## When members see it

Members read the default each time the Console loads, even while their workspace is stopped.
A member who already has the page open sees the change after a reload. The welcome card shows
only until a member creates a first session or starts a first chat, so the default matters for
members who have not started yet.

Saving is recorded in the [audit log](03-audit-usage.md) as `tenant.member_defaults`, with the
chosen agent.

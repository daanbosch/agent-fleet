// The limit board's wire shapes and wording (ADR 0111 decision 6, issue #1962).
//
// Everything a viewer receives was already reduced by the CP (control-plane/limit_board.go);
// this file only words it. Three rules the wording must keep:
//   - a value is a lower bound from when it was observed ("at least X% (N min ago)"), never a
//     statement about now;
//   - past its reset a unit reads "reset not yet observed", never 0% and never "available";
//   - no data is "unknown", never "available". Nothing here ranks or compares members.
import { fmtDateTime, TIME_HM } from "../../lib/intl.ts";
import type { MsgKey } from "../../lib/i18n/index.ts";

export interface LimitUnit {
  unit: string;
  windowMins?: number;
  status: "current" | "stale" | "reset_not_observed";
  band?: "lt50" | "lt80" | "lt95" | "ge95";
  usedPct?: number;
  resetsAt?: string;
  observedAt?: string;
}

export interface LimitSession {
  state: "limited" | "spend_limit" | "blocked" | "auth";
  resumeAt?: string;
}

export interface LimitKind {
  kind: string;
  units?: LimitUnit[];
  sessions?: LimitSession[];
}

export interface LimitView {
  kinds: LimitKind[];
  sessionsObservedAt?: string;
  sessionsStale?: boolean;
}

export interface BoardMember extends LimitView {
  userKey: string;
  email: string;
}

export interface BoardPage {
  enabled: boolean;
  members: BoardMember[];
  next: string;
}

export type Audience = "none" | "tenant" | "members";
export type Level = "state" | "band" | "exact";

export interface BoardMe {
  enabled: boolean;
  audience: Audience;
  level: Level;
  members: { userKey: string; email: string }[];
  own: LimitView;
  publishes: { slug: string; name: string; audience: Audience; level: Level }[];
}

// Decision 6's P1 coverage, as the guide's table states it. The board shows it so that a kind
// missing from a member's entry reads as "not collected" or "unknown", never as fine.
export const COVERAGE: { kind: string; quota: MsgKey; sessions: MsgKey }[] = [
  { kind: "claude", quota: "limits.cov.windows", sessions: "limits.cov.yes" },
  { kind: "codex", quota: "limits.cov.windows", sessions: "limits.cov.managed_only" },
  { kind: "muse", quota: "limits.cov.windows_len", sessions: "limits.cov.yes" },
  { kind: "copilot", quota: "limits.cov.pools", sessions: "limits.cov.unknown" },
  { kind: "agy", quota: "limits.cov.not_collected", sessions: "limits.cov.unknown" },
];

const KIND_NAMES: Record<string, string> = {
  claude: "Claude",
  codex: "Codex",
  muse: "Muse",
  copilot: "Copilot",
  agy: "Antigravity",
};

export const kindName = (kind: string) => KIND_NAMES[kind] ?? kind;

type Tr = (key: MsgKey, vars?: Record<string, string | number>) => string;

// agoText words an observation's age in whole minutes, the board's unit.
export function agoText(tr: Tr, iso: string | undefined, now: number): string {
  if (!iso) return tr("limits.unknown");
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return tr("limits.unknown");
  const min = Math.max(0, Math.floor((now - t) / 60000));
  if (min < 1) return tr("limits.ago.now");
  if (min < 120) return tr("limits.ago.min", { n: min });
  return tr("limits.ago.hour", { n: Math.floor(min / 60) });
}

export function unitLabel(tr: Tr, kind: string, u: LimitUnit): string {
  switch (u.unit) {
    case "5h":
      return tr("limits.unit.5h");
    case "7d":
    case "weekly":
      return tr("limits.unit.weekly");
    case "window":
      return u.windowMins ? tr("limits.unit.window_h", { h: Math.round(u.windowMins / 60) }) : tr("limits.unit.window");
  }
  if (kind === "copilot") {
    if (u.unit === "premium_interactions") return tr("limits.pool.premium");
    if (u.unit === "chat") return tr("limits.pool.chat");
    if (u.unit === "completions") return tr("limits.pool.completions");
  }
  return u.unit;
}

const BAND_KEY: Record<NonNullable<LimitUnit["band"]>, MsgKey> = {
  lt50: "limits.band.lt50",
  lt80: "limits.band.lt80",
  lt95: "limits.band.lt95",
  ge95: "limits.band.ge95",
};

// STALE_MS is the CP's limitStaleAfter (ADR 0111 addendum item 6).
export const STALE_MS = 60 * 60_000;

// effectiveStatus re-applies the CP's time boundaries at `now`: a board left open keeps its
// entries, and an hour later a "current" value is stale and a value past its reset time no
// longer means anything. Only ever moves towards less certain, never back.
export function effectiveStatus(u: LimitUnit, now: number): LimitUnit["status"] {
  if (u.status === "reset_not_observed") return u.status;
  const reset = u.resetsAt ? Date.parse(u.resetsAt) : NaN;
  if (!Number.isNaN(reset) && now >= reset) return "reset_not_observed";
  const obs = u.observedAt ? Date.parse(u.observedAt) : NaN;
  if (!Number.isNaN(obs) && now - obs > STALE_MS) return "stale";
  return u.status;
}

// unitText is one unit's value as the board words it.
export function unitText(tr: Tr, u: LimitUnit, now: number): string {
  const status = effectiveStatus(u, now);
  if (status === "reset_not_observed") return tr("limits.reset_not_observed");
  const ago = agoText(tr, u.observedAt, now);
  let s: string;
  if (u.usedPct !== undefined) s = tr("limits.at_least", { pct: Math.round(u.usedPct), ago });
  else if (u.band) s = tr(BAND_KEY[u.band], { ago });
  else return tr("limits.unknown");
  return status === "stale" ? `${s} · ${tr("limits.stale")}` : s;
}

export function sessionText(tr: Tr, s: LimitSession): string {
  switch (s.state) {
    case "limited":
      return s.resumeAt
        ? tr("limits.session.limited_until", { time: fmtDateTime(s.resumeAt, TIME_HM) })
        : tr("limits.session.limited");
    // Never with a resume time: waiting does not clear a spend limit.
    case "spend_limit":
      return tr("limits.session.spend_limit");
    case "blocked":
      return tr("limits.session.blocked");
    case "auth":
      return tr("limits.session.auth");
  }
  return s.state;
}

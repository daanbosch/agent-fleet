// Moving a session to another agent kind (ADR 0115): the wire shapes of the Agent's
// /sessions/{name}/move routes (workspace/agent/internal/sessionx/session_move*.go) and the
// pure rules the dialog and the row draw from them; the calls are moveApi.ts. The Agent decides
// everything, and nothing here second-guesses an answer it gave: it only words it.
import type { ApiError } from "../../core/api/client.ts";
import type { Session, SessionKind } from "../../types/session.ts";

/** Why a kind is not offered as a target (moveTargetReason on the Agent). */
export type MoveTargetReason = "same_kind" | "cannot_read_file" | "not_measured" | "not_an_agent";

export interface MoveTarget {
  kind: SessionKind;
  eligible: boolean;
  reason?: MoveTargetReason | string;
}

export interface MoveRefusal extends ApiError {
  target?: string;
  file?: string;
  state?: string;
  canFork?: boolean;
}

export interface MoveGit {
  available: boolean;
  dirty: boolean;
  status?: string;
  diffStat?: string;
  ahead?: string;
}

export interface MovePending {
  target: string;
  kind?: string;
  file?: string;
  state?: string;
  error?: string;
  targetExists: boolean;
  orphan?: boolean;
  busy?: boolean;
}

export interface MoveDelivery {
  target: string;
  file: string;
  initialPromptState: "" | "pending" | "failed" | "unknown" | string;
  deliverable: boolean;
}

/** GET /sessions/{name}/move/preview. */
export interface MovePreview {
  source: string;
  kind: SessionKind;
  driver?: string;
  state: string;
  refusal?: MoveRefusal;
  targets: MoveTarget[];
  canFork?: boolean;
  git?: MoveGit;
  queued?: { text: string; origin?: string; attachments?: string[] }[];
  peers?: { from: string; intent?: string; message: string }[];
  children?: { name: string; state: string; running: boolean }[];
  proposals?: string[];
  sizeBytes?: number;
  overSizeTarget?: boolean;
  warnings?: string[];
  moving?: string;
  movedTo?: string;
  pending?: MovePending;
  delivery?: MoveDelivery;
}

/** The answer of a move or a retry that created (or found) its target. */
export interface MoveResult {
  source: string;
  target: Session | null;
  file: string;
  earlier?: string[];
  sizeBytes?: number;
  cut?: number;
  sourceStopped: boolean;
  stopError?: string;
  delivery?: MoveDelivery;
}

export type Answer<T> = (T & { error?: undefined }) | { error: MoveRefusal };

/** What the dialog shows for a source: a fresh move, a move to finish or cancel, or one done. */
export type MovePhase = "move" | "pending" | "moved";

export function movePhase(p: MovePreview): MovePhase {
  if (p.movedTo) return "moved";
  if (p.moving || p.pending) return "pending";
  return "move";
}

/** The session a row's move points at: the target while moving, the destination once moved. */
export function moveTargetOf(s: Pick<Session, "moving" | "movedTo">): string {
  return s.movedTo || s.moving || "";
}

/**
 * The kinds the picker offers: eligible on the Agent's word and, once the connection check has
 * answered, connected. A kind the Agent allows but nobody has logged into would only fail the
 * create, which a move then has to roll back.
 */
export function offeredTargets(p: MovePreview, connected: ((k: SessionKind) => boolean) | null): SessionKind[] {
  return p.targets.filter((t) => t.eligible && (!connected || connected(t.kind))).map((t) => t.kind);
}

/** Bytes as the dialog states a file size: KiB below a MiB, one decimal. */
export function fmtSize(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MiB`;
}

/**
 * Subscription usage the Console already reads, by kind: the same endpoints the WS bar's chips
 * poll (app/WsBar.tsx USAGE_SOURCES), all answering {ok, fiveHour, sevenDay}. A kind missing
 * here has no usage source, and the dialog says nothing about its quota rather than guess.
 */
export const MOVE_USAGE_ENDPOINTS: Partial<Record<SessionKind, string>> = {
  claude: "api/claude/usage",
  codex: "api/codex/usage",
  muse: "api/muse/usage",
};

export interface MoveUsage {
  fiveHour?: number;
  sevenDay?: number;
}

/** Reads a usage answer; null when it carries no observation worth showing. */
export function readMoveUsage(d: unknown): MoveUsage | null {
  const r = d as { ok?: boolean; fiveHour?: { pct?: number; stale?: boolean }; sevenDay?: { pct?: number; stale?: boolean } } | null;
  if (!r || !r.ok) return null;
  // A stale window rolled over after its capture: its percentage is a guess, not a reading.
  const pct = (w?: { pct?: number; stale?: boolean }) => (w && !w.stale && typeof w.pct === "number" ? Math.round(w.pct) : undefined);
  const u = { fiveHour: pct(r.fiveHour), sevenDay: pct(r.sevenDay) };
  return u.fiveHour === undefined && u.sevenDay === undefined ? null : u;
}

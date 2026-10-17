// MoveLimitBanner — the move offered where the limit is felt (ADR 0115 P2b). A session stopped at
// its agent's usage limit (limited, or spend_limit) either waits for the reset or goes on in
// another kind; this band offers the second, opening the same dialog as the session menu with the
// kinds that still have quota left in front.
//
// Nothing here decides eligibility: the Agent's preview does, and the band draws only when that
// leaves a target. The source's own kind is dropped again here (decision 5) so a preview that
// ever listed it could not put the exhausted account first.
import { useEffect, useMemo, useState } from "react";
import { Icon } from "../../../ui/Icon.tsx";
import { useT } from "../../../lib/i18n/index.ts";
import { kindDisplayName } from "../../../lib/sessionkind.ts";
import { agentOf } from "../../../agents/registry.ts";
import { movePreview } from "../../sessions/moveApi.ts";
import { offeredTargets, orderByQuota, quotaState, type MovePreview } from "../../sessions/move.ts";
import { useConnectedKinds, useMoveUsage } from "../../sessions/moveHooks.ts";
import { useSessionUI } from "../../sessions/ui.ts";
import type { Session } from "../../../types/session.ts";

/** The states the Agent reports for a usage limit; the second one a wait does not clear. */
export const isLimitState = (state?: string): boolean => state === "limited" || state === "spend_limit";

export function MoveLimitBanner({ session, state, running }: { session?: Session | null; state?: string; running: boolean }) {
  const tr = useT();
  const openMove = useSessionUI((u) => u.openMove);
  // A moving or moved source refuses every start; its dialog (retry, cancel, resume anyway) is
  // reached from the row, not from a limit it is no longer waiting on.
  const open = !!session && isLimitState(state) && running && agentOf(session.kind).caps.transcript && !session.moving && !session.movedTo;
  const name = session?.name ?? "";
  const [preview, setPreview] = useState<MovePreview | null>(null);
  useEffect(() => {
    setPreview(null);
    if (!open) return;
    let alive = true;
    movePreview(name)
      .then((d) => alive && setPreview(d && !d.error ? d : null))
      .catch(() => {});
    return () => {
      alive = false;
    };
  }, [open, name]);

  const connected = useConnectedKinds();
  const candidates = useMemo(
    () => (preview && !preview.refusal && !preview.movedTo && !preview.moving ? offeredTargets(preview, connected).filter((k) => k !== session?.kind) : []),
    [preview, connected, session?.kind],
  );
  const usage = useMoveUsage(candidates);
  const ordered = orderByQuota(candidates, usage);
  const left = ordered.filter((k) => quotaState(usage[k]) === "left");
  // Every candidate at its cap: moving there only trades one wall for another.
  if (!open || !session || ordered.length === 0 || ordered.every((k) => quotaState(usage[k]) === "full")) return null;

  return (
    <div className="mirror-attention mirror-move-limit" role="status">
      <Icon name="arrow-swap" />
      <span className="ma-text">
        {left.length > 0
          ? tr("mirror.move_limit_left", { kinds: left.map(kindDisplayName).join(", ") })
          : tr("mirror.move_limit")}
      </span>
      <button type="button" className="btn primary ma-btn" onClick={() => openMove(session, true)}>
        {tr("mirror.move_limit_action")}
      </button>
    </div>
  );
}

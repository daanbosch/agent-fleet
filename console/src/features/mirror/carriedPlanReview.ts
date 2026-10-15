// What follows a successful review launch of a carried plan: the carried card is discarded,
// so nothing can approve a plan whose review is under way (discard does not wake the
// session). A failed discard must not be silent — the review is already running and the card
// would stay approvable — so it is surfaced, and the card's own Discard button is the retry.
import { sessionCarriedAnswer } from "../../core/api/client.ts";
import { t as tr } from "../../lib/i18n/index.ts";

export function discardCarriedAfterReview({
  session,
  setCarried,
  toast,
}: {
  session: string;
  setCarried: (c: null) => void;
  toast: (message: string) => void;
}): () => void {
  return () => {
    void sessionCarriedAnswer(session, { decision: "discard" }).then((r) => {
      if (r.ok) setCarried(null);
      else toast(tr("mirror.carried_plan_discard_failed", { err: r.message || "" }));
    });
  };
}

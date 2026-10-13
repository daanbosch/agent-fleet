// Launching from a handoff another member offered (docs/log/77 / ADR 0057, issue #2020).
//
// The offer is TAKEN before the session is created: a conditional transition on the CP that
// the sender's withdraw cannot get past, and that fails once the sender has withdrawn. Taking
// it first is what keeps "exactly one side starts the work" (decision 6); reporting the
// acceptance after the launch let a just-withdrawn offer be launched anyway. The create
// carries the take, and the CP confirms it immediately before the Agent is asked: a Console
// that went quiet past the take window and lost the offer to the sender creates nothing when
// it resumes (handoff_take_lost). A launch that fails gives the take back, so the offer is
// pending again; a launch that succeeds settles it as accepted with the take's token.
//
// A launch whose create got no definite answer (no answer, a gateway's page) keeps the take
// (#2040): the session may exist, and giving the offer back would let the sender start the
// same work. The CP settles the take itself whenever it saw the Agent's answer, and the next
// press reconciles the rest: it sends the lost launch's token, and the CP either gives an
// unconfirmed take back and takes afresh, or asks the recipient's Agent about the confirmed
// create and accepts the session it finds (status "accepted" with its name).
//
// The CP still never launches anything (decision 3): the recipient's own Console creates the
// session in their own workspace, through the launch callback, between the take and the
// accept. The take happens on the launch press, not when the dialog opens, so a dialog that is
// cancelled never holds the offer.
//
// It lives in sharing, not in the launch flow, to avoid a circular import (an offer's lifetime
// is subordinate to the share ACL).
import { apiJSON, errText } from "../../core/api/client.ts";
import { t } from "../../lib/i18n/index.ts";
import type { LaunchResult } from "../repos/LaunchModal.tsx";
import type { HandoffTake } from "../repos/useStartWork.ts";
import { useSessionsStore } from "../sessions/store.ts";
import { useHandoffStore } from "./handoffStore.ts";

const offerPath = (id: string, verb: string) => `api/session-handoff-offers/${encodeURIComponent(id)}/${verb}`;

// The token of each offer's launch whose outcome is unknown, sent with the next take. Kept in
// memory only: after a reload the CP still reconciles a confirmed take on its own, and an
// unconfirmed one lapses after its take window.
const lostTakes = new Map<string, string>();

// The CP names the offer's status in offerStatus. Never read `status`: the API client stamps
// it with the HTTP status on every error (apiResult), so the CP's string never arrives there.
interface HandoffErrorBody {
  code?: string;
  offerStatus?: string;
  message?: string;
  sessionName?: string;
}

/** Why a take was refused, in the recipient's words. */
function takeRefusal(e: HandoffErrorBody | undefined): string {
  if (e?.code === "handoff_being_taken") return t("handoff.take_busy");
  if (e?.code === "handoff_launch_unknown") return t("handoff.take_unknown");
  if (e?.code === "handoff_session_after_give_back") return t("handoff.take_after_give_back", { name: e.sessionName || "" });
  if (e?.code === "handoff_already_decided") {
    switch (e.offerStatus) {
      case "withdrawn":
        return t("handoff.take_withdrawn");
      case "declined":
        return t("handoff.take_declined");
      case "accepted":
        return t("handoff.take_accepted");
      default:
        // expired, or a row the share revocation removed (status "")
        return t("handoff.take_expired");
    }
  }
  if (e?.code === "not_found") return t("handoff.take_expired");
  return t("handoff.take_failed", { err: e?.code || e?.message ? errText({ code: e.code, message: e.message }) : t("err.network") });
}

/**
 * Takes the offer, runs the launch, then accepts or gives it back. A refused take launches
 * nothing and tells the user why through `notify`. The launch's own result is returned as it
 * is, so the dialog behaves exactly as for any other launch.
 */
export async function launchFromHandoffOffer(
  offerId: string,
  launch: (take: HandoffTake) => Promise<LaunchResult>,
  notify: (msg: string) => void,
): Promise<LaunchResult> {
  const prev = lostTakes.get(offerId);
  const taken = await apiJSON(offerPath(offerId, "take"), "POST", prev ? { token: prev } : {}).catch(() => ({ error: {} }));
  let token = typeof taken?.token === "string" ? taken.token : "";
  if (taken?.status === "accepted" && typeof taken?.sessionName === "string" && taken.sessionName) {
    // The lost launch did create its session; the CP has now accepted the offer with it.
    lostTakes.delete(offerId);
    notify(t("handoff.launch_recovered", { name: taken.sessionName }));
    void useHandoffStore.getState().refresh();
    void useSessionsStore.getState().refresh();
    return { ok: true, name: taken.sessionName };
  }
  if (!token) {
    notify(takeRefusal(taken?.error));
    void useHandoffStore.getState().refresh();
    return { ok: false };
  }
  lostTakes.delete(offerId);
  let r: LaunchResult = { ok: false };
  try {
    // The CP gave a refused create's take back already; the release is for any other case.
    const renew = async (): Promise<HandoffTake | null> => {
      await apiJSON(offerPath(offerId, "release"), "POST", { token }).catch(() => undefined);
      const again = await apiJSON(offerPath(offerId, "take"), "POST", {}).catch(() => ({ error: {} }));
      if (typeof again?.token !== "string" || !again.token) {
        notify(takeRefusal(again?.error));
        return null;
      }
      token = again.token;
      return { offerId, token, renew };
    };
    r = await launch({ offerId, token, renew });
  } catch {
    // A launch that threw part-way may have created its session.
    r = { ok: false, unknown: true };
  } finally {
    if (r.unknown) {
      lostTakes.set(offerId, token);
      notify(t("handoff.launch_unknown"));
    } else if (r.ok) {
      const d = await apiJSON(offerPath(offerId, "accept"), "POST", { sessionName: r.name || "", token }).catch(() => ({
        error: { message: t("err.network") },
      }));
      if (d?.error) {
        const e = d.error as HandoffErrorBody;
        notify(t("handoff.accept_unrecorded", { err: e.offerStatus || errText({ code: e.code, message: e.message }) }));
      }
    } else {
      // Best-effort: an unreleased take stops holding the offer after the CP's take window.
      // The CP keeps a take whose create reached the Agent unless the Agent itself refused it:
      // what read as a failure here (a gateway's error, say) may have made the session.
      const rel = await apiJSON(offerPath(offerId, "release"), "POST", { token }).catch(() => undefined);
      if ((rel?.error as HandoffErrorBody | undefined)?.code === "handoff_launch_unknown") {
        r = { ...r, unknown: true };
        lostTakes.set(offerId, token);
        notify(t("handoff.launch_unknown"));
      }
    }
    void useHandoffStore.getState().refresh();
  }
  return r;
}

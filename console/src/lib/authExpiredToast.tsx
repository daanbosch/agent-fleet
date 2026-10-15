// The one place a failed send turns into a toast. An `auth_expired` refusal (the Agent's
// promptBlocker: the workspace's Claude login is dead) gets a sticky toast with a
// Re-authenticate button, because a member with no session has no other way to the login
// flow: the transcript's error card only appears after a turn failed with a 401, and with the
// credentials already dead no turn ever starts. Every other failure stays a plain toast.
import type { ReactNode } from "react";
import { useSettingsUI } from "../features/settings/store.ts";
import { dismissToast } from "../ui/toast.ts";
import type { ToastOptions } from "../ui/ToastProvider.tsx";
import { useT } from "./i18n/index.ts";

export const AUTH_EXPIRED_CODE = "auth_expired";
const TOAST_KEY = "auth-expired";

type ToastSink = (message: ReactNode, opts?: ToastOptions) => unknown;

/** openAgentsSettings opens Settings on the Agents section, where the Claude card's login lives. */
export function openAgentsSettings(): void {
  useSettingsUI.getState().openSettings("agents", { target: "claude" });
}

export function AuthExpiredToast() {
  const tr = useT();
  return (
    <span className="update-toast">
      <span className="update-toast-txt">
        <span>{tr("err.auth_expired")}</span>
      </span>
      <button
        type="button"
        className="update-toast-btn"
        onClick={() => {
          dismissToast(TOAST_KEY);
          openAgentsSettings();
        }}
      >
        {tr("mirror.error_auth_action")}
      </button>
    </span>
  );
}

/** toastSendFailure reports a rejected send. `message` is the already-localized reason
 *  (errText), `code` the wire code when the caller has one. */
export function toastSendFailure(toast: ToastSink, fail: { code?: string; message?: string }, fallback: string): void {
  if (fail.code === AUTH_EXPIRED_CODE) {
    toast(<AuthExpiredToast />, { kind: "error", duration: 0, key: TOAST_KEY });
    return;
  }
  toast(fail.message || fallback);
}


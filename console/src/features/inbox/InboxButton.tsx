// InboxButton is the top bar's entry to the member inbox (ADR 0111), beside the notification
// bell but separate from it: the inbox has its own badge and its items never go into the
// notification center (decision 8).
//
// Pending handoffs from other members (ADR 0057) are counted on the same badge: members look for
// them here. They come from the handoff store and its one refcounted poll, never from
// GET /api/inbox, so nothing is counted twice. They are a backlog, cleared only by deciding them
// (docs/log/77 §77.10), so opening the inbox does not clear them, and the inbox's receive
// controls do not reach them: do-not-disturb mutes inbox items only, and a new handoff makes no
// inbox toast because it already raised a notification of its own.
import { useEffect, useRef, useState } from "react";
import { Button } from "../../ui/Button.tsx";
import { Icon } from "../../ui/Icon.tsx";
import { useToast } from "../../ui/ToastProvider.tsx";
import { useT } from "../../lib/i18n/index.ts";
import { useTenantStore } from "../../core/store/tenant.ts";
import { useLaunchSeed, useLaunchTarget } from "../repos/store.ts";
import { pendingHandoffCount, startHandoffPolling, useHandoffStore } from "../sharing/handoffStore.ts";
import { inboxBadge, onInboxScopeChange, setInboxScope, startInboxChannel, useInboxStore } from "./store.ts";
import { InboxModal } from "./InboxModal.tsx";
import "./inbox.css";

export function InboxButton() {
  const tr = useT();
  const toast = useToast();
  const [open, setOpen] = useState(false);
  const mailbox = useInboxStore((s) => s.mailbox);
  const loaded = useInboxStore((s) => s.loaded);
  const badge = inboxBadge(mailbox);
  const handoffs = useHandoffStore((s) => pendingHandoffCount(s.received));
  const total = badge + handoffs;
  // A toast when the count rises while the inbox is closed. Do-not-disturb mutes it along
  // with the badge (inboxBadge is 0 then), and the first load of a scope is not news.
  const prev = useRef<number | null>(null);
  const tenant = useTenantStore((s) => s.tenant);
  const user = useTenantStore((s) => s.whoami?.user ?? "");
  const scope = user + "|" + tenant;
  // A tenant or account switch drops the previous scope's inbox at once: list, badge, stream,
  // the open dialog with its drafts, and a launch seeded from one of its requests.
  useEffect(() => {
    onInboxScopeChange(() => {
      if (useLaunchSeed.getState().inboxItemId) {
        useLaunchSeed.getState().clear();
        useLaunchTarget.getState().clear();
      }
    });
    return () => onInboxScopeChange(null);
  }, []);
  useEffect(() => {
    setInboxScope(scope);
    setOpen(false);
    prev.current = null;
  }, [scope]);
  useEffect(() => startInboxChannel(), []);
  useEffect(() => startHandoffPolling(), []);
  useEffect(() => {
    if (!loaded) return;
    if (prev.current !== null && badge > prev.current && !open) toast(tr("inbox.toast_new"), { kind: "info" });
    prev.current = badge;
  }, [badge, loaded, open, toast, tr]);
  return (
    <div className="inbox-wrap">
      <Button
        className="gear notification-btn inbox-btn"
        title={tr("inbox.title")}
        aria-label={
          total > 0
            ? [tr("inbox.title"), badge > 0 && tr("inbox.badge", { n: badge }), handoffs > 0 && tr("inbox.badge_handoffs", { n: handoffs })]
                .filter(Boolean)
                .join(" — ")
            : tr("inbox.open")
        }
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        <Icon name="inbox" />
        {total > 0 && <span className="notification-badge inbox-badge">{total > 9 ? "9+" : total}</span>}
      </Button>
      {open && <InboxModal key={scope} onClose={() => setOpen(false)} />}
    </div>
  );
}

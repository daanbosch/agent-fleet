// InboxButton is the top bar's entry to the member inbox (ADR 0111), beside the notification
// bell but separate from it: the inbox has its own badge and its items never go into the
// notification center (decision 8).
import { useEffect, useRef, useState } from "react";
import { Button } from "../../ui/Button.tsx";
import { Icon } from "../../ui/Icon.tsx";
import { useToast } from "../../ui/ToastProvider.tsx";
import { useT } from "../../lib/i18n/index.ts";
import { inboxBadge, startInboxChannel, useInboxStore } from "./store.ts";
import { InboxModal } from "./InboxModal.tsx";
import "./inbox.css";

export function InboxButton() {
  const tr = useT();
  const toast = useToast();
  const [open, setOpen] = useState(false);
  const mailbox = useInboxStore((s) => s.mailbox);
  const loaded = useInboxStore((s) => s.loaded);
  const badge = inboxBadge(mailbox);
  useEffect(() => startInboxChannel(), []);
  // A toast when the count rises while the inbox is closed. Do-not-disturb mutes it along
  // with the badge (inboxBadge is 0 then), and the first load after boot is not news.
  const prev = useRef<number | null>(null);
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
        aria-label={badge > 0 ? tr("inbox.title") + " — " + tr("inbox.badge", { n: badge }) : tr("inbox.open")}
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        <Icon name="inbox" />
        {badge > 0 && <span className="notification-badge inbox-badge">{badge > 9 ? "9+" : badge}</span>}
      </Button>
      {open && <InboxModal onClose={() => setOpen(false)} />}
    </div>
  );
}

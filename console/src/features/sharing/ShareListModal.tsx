// ShareListModal lists the shares the owner created (opened from the broadcast icon in the
// left pane, or the manage icon of the sharing section). Row appearance and the "primary
// action + close" footer follow ArchivedModal (sessions/ArchivedModal.tsx). Creation lives
// in ShareCreateModal, reached from the "new share" button here or a row's context menu.
import { useEffect, useMemo, useState } from "react";
import { Modal } from "../../ui/Modal.tsx";
import { Button, IconButton } from "../../ui/Button.tsx";
import { Icon } from "../../ui/Icon.tsx";
import { useConfirm } from "../../ui/ConfirmProvider.tsx";
import { useToast } from "../../ui/ToastProvider.tsx";
import { api, errText } from "../../core/api/client.ts";
import { useT } from "../../lib/i18n/index.ts";
import { useReposStore } from "../repos/store.ts";
import { useSessionsStore } from "../sessions/store.ts";
import { ShareCreateModal } from "./ShareCreateModal.tsx";
import { useMySharesStore } from "./store.ts";
import { boundedRequest } from "./boundedRequest.ts";
import "./sharing.css";

export function ShareListModal({ onClose }: { onClose: () => void }) {
  const tr = useT();
  const repos = useReposStore((s) => s.repos);
  const sessions = useSessionsStore((s) => s.sessions);
  const shares = useMySharesStore((s) => s.shares);
  const askConfirm = useConfirm();
  const toast = useToast();
  const [createOpen, setCreateOpen] = useState(false);
  const [busy, setBusy] = useState<string | null>(null);
  useEffect(() => { void useMySharesStore.getState().refresh(); }, []);

  const targetInfo = useMemo(() => (scope: { type: string; key: string }) => {
    if (scope.type === "session") {
      const s = sessions.find((x) => x.name === scope.key);
      return { icon: "comment-discussion", label: s ? s.title || s.label || s.name : scope.key };
    }
    const r = repos.find((x) => x.workingCopyId === scope.key);
    return {
      icon: scope.type === "worktree" ? "git-branch" : "root-folder",
      label: r ? r.name : scope.key,
    };
  }, [sessions, repos]);

  const remove = async (id: string) => {
    if (!(await askConfirm({
      title: tr("share.unshare_confirm_title"),
      body: tr("share.unshare_confirm_body"),
      confirmLabel: tr("share.unshare"),
      danger: true,
    }))) return;
    await mutate(id, { method: "DELETE" }, "share.unshare_failed");
  };
  const change = async (id: string, p: "ro" | "rw") => {
    await mutate(id, {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ permission: p }),
    }, "share.permission_failed");
  };
  // mutate runs one unshare / permission change with a deadline and says when it failed:
  // a silent failure (a 409 while an approved operation runs, say) left the row looking
  // changed until the next refresh put it back.
  const mutate = async (id: string, init: RequestInit, failKey: "share.unshare_failed" | "share.permission_failed") => {
    setBusy(id);
    const req = boundedRequest();
    const d = await api(`api/session-shares/${encodeURIComponent(id)}`, { ...init, signal: req.signal })
      .catch(() => ({ error: { message: tr(req.timedOut() ? "share.request_timeout" : failKey) } }));
    req.settle();
    setBusy(null);
    if (d?.error) toast(errText(d.error));
    await useMySharesStore.getState().refresh();
  };

  return (
    <Modal title={tr("share.list_title")} onClose={onClose} className="share-list-modal">
      <div className="ui-modal-body">
        {shares.length === 0 ? (
          <p className="sm-muted">{tr("share.no_shares")}</p>
        ) : (
          <ul className="share-list">
            {shares.map((s) => {
              const info = targetInfo(s.scope);
              return (
                <li key={s.id} className="share-row" aria-busy={busy === s.id}>
                  <div className="share-info" title={info.label}>
                    <Icon name={info.icon} />
                    <span className="share-target-name">{info.label}</span>
                    <span className="share-recipient">{s.recipientUserKey}</span>
                  </div>
                  <div className="share-actions">
                    <select value={s.permission} disabled={busy === s.id} onChange={(e) => void change(s.id, e.target.value as "ro" | "rw")}>
                      <option value="ro">{tr("share.permission_ro")}</option>
                      <option value="rw">{tr("share.permission_rw")}</option>
                    </select>
                    <IconButton icon={busy === s.id ? "loading" : "trash"} spin={busy === s.id} label={tr("share.unshare")} variant="danger" disabled={busy === s.id} onClick={() => void remove(s.id)} />
                  </div>
                </li>
              );
            })}
          </ul>
        )}
      </div>
      <footer className="ui-modal-foot">
        <Button variant="primary" icon="add" onClick={() => setCreateOpen(true)}>{tr("share.new")}</Button>
        <Button variant="ghost" onClick={onClose}>{tr("common.close")}</Button>
      </footer>
      {createOpen && (
        <ShareCreateModal
          onClose={() => setCreateOpen(false)}
          onCreated={() => void useMySharesStore.getState().refresh()}
        />
      )}
    </Modal>
  );
}

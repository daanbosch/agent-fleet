// Inbox attachments (ADR 0111 decisions 4 and 5): composing a recipe and attaching a session,
// and showing them on a card.
//
// A recipe is shown with its repository apart from the prompt, so the recipient sees what it
// points at; receiving one fetches and clones nothing. "Take in" copies it into the memo queue
// as the recipient's own memo, with an origin the server sets. A session attachment shows what
// the server decided this reader may see; a session they cannot see arrives as an opaque
// placeholder and is drawn as one, with nothing to open.
import { useState } from "react";
import { Button } from "../../ui/Button.tsx";
import { Icon } from "../../ui/Icon.tsx";
import { useToast } from "../../ui/ToastProvider.tsx";
import { useT } from "../../lib/i18n/index.ts";
import { openSessionChat } from "../sessions/open.ts";
import { openSharedSession } from "../sharing/open.ts";
import { useSessionsStore } from "../sessions/store.ts";
import { useSharedSessionsStore } from "../sharing/store.ts";
import { displayName } from "../../lib/sessionview.ts";
import { useInboxStore, type InboxAttachment, type InboxAttachmentIn, type InboxItem, type InboxPerson } from "./store.ts";

/** The recipe fields as the sender fills them in. */
export interface RecipeDraft {
  prompt: string;
  kind: string;
  model: string;
  effort: string;
  repo: string;
}

export const emptyRecipe = (): RecipeDraft => ({ prompt: "", kind: "", model: "", effort: "", repo: "" });

export function RecipeEditor({ value, onChange }: { value: RecipeDraft; onChange: (r: RecipeDraft) => void }) {
  const tr = useT();
  const field = (k: keyof RecipeDraft, label: string, placeholder = "") => (
    <label className="ui-field">
      <span className="ui-field-label">{label}</span>
      <input value={value[k]} placeholder={placeholder} onChange={(e) => onChange({ ...value, [k]: e.target.value })} />
    </label>
  );
  return (
    <div className="inbox-recipe-editor">
      <label className="ui-field">
        <span className="ui-field-label">{tr("inbox.recipe_prompt")}</span>
        <textarea rows={4} value={value.prompt} onChange={(e) => onChange({ ...value, prompt: e.target.value })} />
      </label>
      <div className="inbox-recipe-hints">
        {field("kind", tr("inbox.recipe_kind"), "claude")}
        {field("model", tr("inbox.recipe_model"))}
        {field("effort", tr("inbox.recipe_effort"))}
      </div>
      {field("repo", tr("inbox.recipe_repo"), "github.com/owner/repo")}
      <p className="ui-field-hint">{tr("inbox.recipe_repo_hint")}</p>
    </div>
  );
}

/** A session chosen to attach: the sender's own (by name) or one shared with them. */
export type SessionChoice = { own: true; name: string; label: string } | { own: false; catalogId: string; label: string };

export function SessionAttachEditor({
  value,
  share,
  recipients,
  onChange,
  onShare,
}: {
  value: SessionChoice | null;
  share: boolean;
  recipients: InboxPerson[];
  onChange: (c: SessionChoice | null) => void;
  onShare: (v: boolean) => void;
}) {
  const tr = useT();
  const mine = useSessionsStore((s) => s.sessions);
  const shared = useSharedSessionsStore((s) => s.sessions);
  const options: SessionChoice[] = [
    ...mine.map((s) => ({ own: true as const, name: s.name, label: displayName(s) })),
    ...shared.map((s) => ({ own: false as const, catalogId: s.id, label: (s.title || s.name) + " — " + (s.ownerEmail || s.ownerUserKey) })),
  ];
  const keyOf = (c: SessionChoice) => (c.own ? "own:" + c.name : "shared:" + c.catalogId);
  return (
    <div className="inbox-session-editor">
      <label className="ui-field">
        <span className="ui-field-label">{tr("inbox.attach_session")}</span>
        <select
          value={value ? keyOf(value) : ""}
          onChange={(e) => {
            const c = options.find((o) => keyOf(o) === e.target.value) ?? null;
            onChange(c);
            if (!c?.own) onShare(false);
          }}
        >
          <option value="">{tr("inbox.attach_none")}</option>
          {options.map((o) => (
            <option key={keyOf(o)} value={keyOf(o)}>
              {o.label}
            </option>
          ))}
        </select>
      </label>
      {value?.own && (
        // Decision 5: the owner confirms the session, every recipient and the scope before a
        // share is created with the message.
        <label className="inbox-check">
          <input type="checkbox" checked={share} onChange={(e) => onShare(e.target.checked)} />
          <span>
            {tr("inbox.attach_share_confirm", {
              session: value.label,
              who: recipients.map((r) => r.email || r.userKey).join(", ") || "—",
            })}
          </span>
        </label>
      )}
      {value && !value.own && <p className="ui-field-hint">{tr("inbox.attach_shared_hint")}</p>}
      {value?.own && !share && <p className="ui-field-hint">{tr("inbox.attach_no_share_hint")}</p>}
    </div>
  );
}

/** Builds the send's attachments from the composer's state. */
export function buildAttachments(recipe: RecipeDraft | null, session: SessionChoice | null, share: boolean, recipients: InboxPerson[]): InboxAttachmentIn[] {
  const out: InboxAttachmentIn[] = [];
  if (recipe && recipe.prompt.trim()) out.push({ type: "recipe", recipe: { ...recipe } });
  if (session) {
    if (session.own) {
      out.push({
        type: "session",
        sessionName: session.name,
        ...(share ? { share: { recipients: recipients.map((r) => r.userKey), scope: "session" } } : {}),
      });
    } else {
      out.push({ type: "session", catalogId: session.catalogId });
    }
  }
  return out;
}

export function AttachmentView({ item, att }: { item: InboxItem; att: InboxAttachment }) {
  const tr = useT();
  const toast = useToast();
  const [busy, setBusy] = useState(false);
  if (att.type === "recipe") {
    const r = att.recipe;
    if (!att.available || !r) return <div className="inbox-attachment inbox-placeholder">{tr("inbox.recipe_unreadable")}</div>;
    const takeIn = async () => {
      setBusy(true);
      const res = await useInboxStore.getState().takeIn(item.id);
      setBusy(false);
      toast(res.ok ? tr("inbox.taken_in") : tr("inbox.take_in_failed"), res.ok ? { kind: "success" } : undefined);
    };
    return (
      <div className="inbox-attachment inbox-recipe">
        <div className="inbox-attachment-head">
          <Icon name="beaker" /> {tr("inbox.recipe")}
          {(r.kind || r.model || r.effort) && <span className="muted">{[r.kind, r.model, r.effort].filter(Boolean).join(" · ")}</span>}
        </div>
        {/* The repository, apart from the prompt: what it points at, never a link that fetches. */}
        {r.repo && (
          <div className="inbox-recipe-repo">
            <Icon name="repo" /> <code>{r.repo}</code>
          </div>
        )}
        <pre className="inbox-body">{r.prompt}</pre>
        {r.memoId ? (
          <p className="ui-field-hint">{tr("inbox.taken_in_already")}</p>
        ) : (
          r.canTakeIn && (
            <div className="inbox-actions">
              <Button disabled={busy} onClick={() => void takeIn()}>
                <Icon name="add" /> {tr("inbox.take_in")}
              </Button>
            </div>
          )
        )}
        {!r.canTakeIn && !r.memoId && item.role === "recipient" && item.kind === "request" && (
          <p className="ui-field-hint">{tr("inbox.take_in_assignee_only")}</p>
        )}
      </div>
    );
  }
  const s = att.session;
  if (!att.available || !s) {
    return (
      <div className="inbox-attachment inbox-placeholder">
        <Icon name="lock" /> {tr("inbox.session_placeholder")}
      </div>
    );
  }
  return (
    <div className="inbox-attachment inbox-session">
      <Icon name="comment-discussion" /> <strong>{s.title || s.name}</strong>
      <span className="muted">
        {[s.repo, s.branch, s.mine ? "" : s.owner.email || s.owner.userKey].filter(Boolean).join(" · ")}
      </span>
      <Button small variant="ghost" onClick={() => (s.mine ? openSessionChat(s.name) : openSharedSession(s.catalogId))}>
        {tr("inbox.session_open")}
      </Button>
    </div>
  );
}

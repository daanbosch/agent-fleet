// MemoryBrowserPanel — the member's browser of AF-owned agent memory (#1985): the memories of one
// scope with their author, use count and pin, a search, the body of one, forget, and the
// forgotten ones with a way back. Writing by hand goes through the same revision check and secret
// scan as the agents' memory_save; the author is recorded as the Console, like a revert.
//
// A memory whose text fails the secret scan is never listed (the Agent sends a count), so no
// name or body of one reaches this DOM. Search matches what the list carries (name, description,
// type, author), not bodies.

import { useCallback, useEffect, useMemo, useState } from "react";
import { api, apiJSON, errDetail, errText, isTransientErr } from "../../../core/api/client.ts";
import { useRetryLoad } from "../../../lib/retryLoad.ts";
import { useToast } from "../../../ui/ToastProvider.tsx";
import { useConfirm } from "../../../ui/ConfirmProvider.tsx";
import { useT } from "../../../lib/i18n/index.ts";
import { fmtDateTime, DATETIME_FULL } from "../../../lib/intl.ts";
import type { MemoryBody, MemoryChange, MemoryListed, SecretFinding } from "./memoryTypes.ts";

const TYPES = ["user", "feedback", "project", "reference"] as const;
const projectQuery = (project?: string) => (project ? "?project=" + encodeURIComponent(project) : "");

// "user" or "project:<id>": one browsable scope.
const scopeKey = (m: { scope: string; project?: { id: string } }) =>
  m.scope === "user" ? "user" : "project:" + (m.project?.id ?? "");

interface Draft {
  /** null: a new memory. */
  base: MemoryBody | null;
  /** Where the form was opened, fixed: the scope selector may move while the form is open, and a
   *  save must never land in another scope with this revision and text. */
  scope: "user" | "project";
  project: string;
  kinds: string[];
  name: string;
  description: string;
  type: string;
  body: string;
}

export function MemoryBrowserPanel({ reload, onChanged }: { reload: number; onChanged: () => void }) {
  const tr = useT();
  const toast = useToast();
  const askConfirm = useConfirm();
  const [rows, setRows] = useState<MemoryListed[] | null>(null);
  const [withheld, setWithheld] = useState(0);
  const [forgotten, setForgotten] = useState<MemoryChange[]>([]);
  const [loadErr, setLoadErr] = useState("");
  const [scope, setScope] = useState("user");
  const [query, setQuery] = useState("");
  const [open, setOpen] = useState<MemoryBody | null>(null);
  const [openErr, setOpenErr] = useState("");
  const [draft, setDraft] = useState<Draft | null>(null);
  const [findings, setFindings] = useState<SecretFinding[]>([]);
  const [formErr, setFormErr] = useState("");
  const [busy, setBusy] = useState(false);
  const [mine, setMine] = useState(0);

  const load = useCallback(async (signal: AbortSignal) => {
    const [list, ch] = await Promise.all([
      api("api/agents/memory/entries/list"),
      api("api/agents/memory/entries/changes?limit=300"),
    ]);
    if (signal.aborted) return true;
    if (isTransientErr(list) || isTransientErr(ch)) return false;
    setLoadErr(list?.error ? errText(list.error) : "");
    setRows(list?.error ? [] : (list?.entries ?? []));
    setWithheld(list?.error ? 0 : (list?.withheld ?? 0));
    // The newest change of a memory that does not exist now is the one that took it away; its
    // revert brings the memory back.
    const changes: MemoryChange[] = ch?.error ? [] : (ch?.changes ?? []);
    setForgotten(changes.filter((c) => c.latest && !c.live && c.revertible));
    return true;
  }, []);
  useRetryLoad(load, [reload, mine]);

  // The scopes there is something to browse in. User scope is always offered: a new memory can
  // start there. A project is offered once it holds a memory (the Console cannot start one).
  const scopes = useMemo(() => {
    const out = new Map<string, string>([["user", tr("mem.af_scope_user")]]);
    for (const m of [...(rows ?? []), ...forgotten]) {
      if (m.scope === "project" && m.project) out.set(scopeKey(m), m.project.display);
    }
    return [...out.entries()];
  }, [rows, forgotten, tr]);
  useEffect(() => {
    if (!scopes.some(([k]) => k === scope)) setScope("user");
  }, [scopes, scope]);

  const project = scope.startsWith("project:") ? scope.slice("project:".length) : "";
  const q = query.trim().toLowerCase();
  const shown = (rows ?? []).filter(
    (m) =>
      scopeKey(m) === scope &&
      (!q || `${m.name}\n${m.description}\n${m.type ?? ""}\n${m.authorKind}\n${m.authorSession}`.toLowerCase().includes(q)),
  );
  const gone = forgotten.filter((c) => scopeKey(c) === scope);
  const who = (m: { authorKind: string; authorSession: string }) =>
    m.authorKind === "member" ? tr("mem.af_by_member") : `${m.authorKind} · ${m.authorSession}`;

  const view = async (m: MemoryListed) => {
    setDraft(null);
    setOpenErr("");
    setOpen(null);
    try {
      const res = await api(
        `api/agents/memory/entries/member-read?scope=${m.scope}&project=${encodeURIComponent(m.project?.id ?? "")}&name=${encodeURIComponent(m.name)}`,
      );
      if (res?.error) setOpenErr(errDetail(res.error));
      else setOpen({ ...m, ...res });
    } catch {
      setOpenErr(tr("mem.af_failed"));
    }
  };

  const forget = async (m: MemoryListed) => {
    const ok = await askConfirm({
      title: tr("mem.af_forget_title"),
      body: <p>{tr("mem.af_forget_body", { name: m.name, where: m.scope === "user" ? tr("mem.af_scope_user") : (m.project?.display ?? "") })}</p>,
      confirmLabel: tr("mem.af_forget"),
      danger: true,
    });
    if (!ok) return;
    setBusy(true);
    try {
      // The project id also goes in the query: the CP audit ledger reads the URL only.
      const res = await apiJSON("api/agents/memory/entries/member-forget" + projectQuery(m.project?.id), "POST", {
        scope: m.scope,
        project: m.project?.id ?? "",
        name: m.name,
        revision: m.revision,
      });
      if (res?.error) toast(errDetail(res.error));
      else {
        toast(tr("mem.af_forgotten", { name: m.name }), { kind: "success" });
        if (open?.name === m.name && scopeKey(open) === scopeKey(m)) setOpen(null);
        onChanged();
      }
    } catch {
      toast(tr("mem.af_failed"));
    } finally {
      setBusy(false);
      setMine((n) => n + 1);
    }
  };

  const restore = async (c: MemoryChange) => {
    setBusy(true);
    try {
      const res = await apiJSON("api/agents/memory/entries/revert?commit=" + encodeURIComponent(c.commit), "POST", {
        commit: c.commit,
        forget: false,
        ack: false,
      });
      if (res?.error) toast(res.error.code === "memory_secret_detected" ? tr("mem.br_restore_secret") : errDetail(res.error));
      else {
        toast(tr("mem.br_restored", { name: c.name }), { kind: "success" });
        onChanged();
      }
    } catch {
      toast(tr("mem.af_failed"));
    } finally {
      setBusy(false);
      setMine((n) => n + 1);
    }
  };

  const edit = (base: MemoryBody | null) => {
    setFindings([]);
    setFormErr("");
    setDraft({
      base,
      scope: base ? base.scope : project ? "project" : "user",
      project: base ? (base.project?.id ?? "") : project,
      // Kept as read: a save replaces the kinds, and an edit of the body must not widen who sees it.
      kinds: base?.kinds ?? [],
      name: base?.name ?? "",
      description: base?.description ?? "",
      type: base?.type || "feedback",
      body: base?.body ?? "",
    });
  };

  const save = async () => {
    if (!draft) return;
    setBusy(true);
    setFindings([]);
    setFormErr("");
    try {
      const res = await apiJSON("api/agents/memory/entries/member-save" + projectQuery(draft.project), "POST", {
        scope: draft.scope,
        project: draft.project,
        name: draft.name.trim(),
        description: draft.description,
        type: draft.type,
        kinds: draft.kinds,
        body: draft.body,
        revision: draft.base?.revision ?? 0,
      });
      if (res?.error?.code === "memory_secret_detected") {
        setFindings(res.findings ?? []);
        setFormErr(tr("mem.br_secret"));
      } else if (res?.error) {
        setFormErr(errDetail(res.error));
      } else {
        toast(tr("mem.br_saved", { name: draft.name.trim() }), { kind: "success" });
        setDraft(null);
        setOpen(null);
        onChanged();
        setMine((n) => n + 1);
      }
    } catch {
      setFormErr(tr("mem.af_failed"));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="mem-browser">
      <h4>{tr("mem.br_title")}</h4>
      <p className="muted ds-hint">{tr("mem.br_intro")}</p>
      {loadErr && <p className="mem-warn">{loadErr}</p>}
      {withheld > 0 && <p className="mem-warn">{tr("mem.br_withheld", { n: withheld })}</p>}
      <div className="mem-br-bar">
        <select aria-label={tr("mem.br_scope")} value={scope} onChange={(e) => setScope(e.target.value)}>
          {scopes.map(([k, label]) => (
            <option key={k} value={k}>
              {label}
            </option>
          ))}
        </select>
        <input
          type="search"
          aria-label={tr("mem.br_search")}
          placeholder={tr("mem.br_search_ph")}
          value={query}
          onChange={(e) => setQuery(e.target.value)}
        />
        <button type="button" disabled={busy} onClick={() => edit(null)}>
          {tr("mem.br_new")}
        </button>
      </div>
      <ul className="mem-pin-list mem-br-list">
        {rows === null ? (
          <li className="muted pad">{tr("common.loading")}</li>
        ) : shown.length === 0 ? (
          <li className="muted pad">{tr(q ? "mem.br_no_match" : "mem.br_empty")}</li>
        ) : (
          shown.map((m) => (
            <li key={scopeKey(m) + "/" + m.name} className="mem-br-row">
              <span className="mem-af-name">{m.name}</span>
              {m.pinned && <span className="mem-badge">{tr("mem.br_pinned")}</span>}
              {m.type && <span className="mem-badge">{m.type}</span>}
              <span className="mem-snap-what muted" title={m.description}>
                {m.description}
              </span>
              <span className="mem-snap-what muted">
                {who(m)}
                {" · "}
                {fmtDateTime(m.updated, DATETIME_FULL)}
                {" · "}
                {tr("mem.pin_uses", { n: m.uses })}
              </span>
              <button type="button" disabled={busy} onClick={() => void view(m)}>
                {tr("mem.br_view")}
              </button>
              <button type="button" disabled={busy} onClick={() => void forget(m)}>
                {tr("mem.af_forget")}
              </button>
            </li>
          ))
        )}
      </ul>
      {openErr && <p className="mem-warn">{openErr}</p>}
      {open && !draft && (
        <div className="mem-br-detail">
          <div className="mem-diff-head">
            <strong className="mem-af-name">{open.name}</strong>
            <span className="muted">{open.description}</span>
            <button type="button" disabled={busy} onClick={() => edit(open)}>
              {tr("mem.br_edit")}
            </button>
            <button type="button" onClick={() => setOpen(null)}>
              {tr("mem.br_close")}
            </button>
          </div>
          <pre className="mem-br-body">{open.body}</pre>
        </div>
      )}
      {draft && (
        <form
          className="mem-br-form"
          onSubmit={(e) => {
            e.preventDefault();
            void save();
          }}
        >
          <strong>{draft.base ? tr("mem.br_edit_title", { name: draft.base.name }) : tr("mem.br_new_title")}</strong>
          {!draft.base && (
            <label>
              {tr("mem.br_f_name")}
              <input value={draft.name} onChange={(e) => setDraft({ ...draft, name: e.target.value })} />
            </label>
          )}
          <label>
            {tr("mem.br_f_description")}
            <input value={draft.description} onChange={(e) => setDraft({ ...draft, description: e.target.value })} />
          </label>
          <label>
            {tr("mem.br_f_type")}
            <select value={draft.type} onChange={(e) => setDraft({ ...draft, type: e.target.value })}>
              {TYPES.map((t) => (
                <option key={t} value={t}>
                  {t}
                </option>
              ))}
            </select>
          </label>
          <label>
            {tr("mem.br_f_body")}
            <textarea rows={8} value={draft.body} onChange={(e) => setDraft({ ...draft, body: e.target.value })} />
          </label>
          {formErr && <p className="mem-warn">{formErr}</p>}
          {findings.length > 0 && (
            <ul className="mem-findings">
              {findings.map((f, i) => (
                <li key={i}>
                  <code>{f.rule}</code> {tr("mem.af_secret_line", { line: f.line })} <code>{f.hint}</code>
                </li>
              ))}
            </ul>
          )}
          <div className="mem-br-actions">
            <button type="submit" disabled={busy}>
              {tr("mem.br_save")}
            </button>
            <button type="button" disabled={busy} onClick={() => setDraft(null)}>
              {tr("mem.br_cancel")}
            </button>
          </div>
        </form>
      )}
      {gone.length > 0 && (
        <>
          <h5>{tr("mem.br_forgotten")}</h5>
          <p className="muted ds-hint">{tr("mem.br_forgotten_note")}</p>
          <ul className="mem-pin-list mem-br-list">
            {gone.map((c) => (
              <li key={c.commit} className="mem-br-row">
                <span className="mem-af-name">{c.name}</span>
                <span className="mem-snap-what muted">
                  {fmtDateTime(c.at, DATETIME_FULL)}
                  {" · "}
                  {who(c)}
                </span>
                <button type="button" disabled={busy} onClick={() => void restore(c)}>
                  {tr("mem.br_restore")}
                </button>
              </li>
            ))}
          </ul>
        </>
      )}
    </div>
  );
}

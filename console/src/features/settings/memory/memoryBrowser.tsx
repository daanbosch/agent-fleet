// MemoryBrowserPanel — the member's browser of AF-owned agent memory (#1985): the memories of all
// scopes at once (the default) or of one scope with their author, use count and pin, a search, the body of one, forget, and the
// forgotten ones with a way back. It adds no write route: forget and restore are the changes
// list's own revert route, on the newest change of the memory. A forget resolves that change on
// demand (member-latest) and the forgotten list comes from member-forgotten (the store's
// tombstones), so neither depends on how many recent changes were loaded. Writing a memory
// by hand waits for a member mark the Agent can verify (#1997).
//
// A memory whose text fails the secret scan is never listed (the Agent sends a count), so no
// name or body of one reaches this DOM. Search matches the list's own fields at once (name,
// description, type, author) and, after a pause, bodies through member-search, which counts no
// use of the memories it finds.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { api, apiJSON, errDetail, errText, isTransientErr } from "../../../core/api/client.ts";
import { useRetryLoad } from "../../../lib/retryLoad.ts";
import { useToast } from "../../../ui/ToastProvider.tsx";
import { useConfirm } from "../../../ui/ConfirmProvider.tsx";
import { useT } from "../../../lib/i18n/index.ts";
import { fmtDateTime, DATETIME_FULL } from "../../../lib/intl.ts";
import type { MemoryBody, MemoryChange, MemoryListed, MemorySearchHit } from "./memoryTypes.ts";


/** Pause after the last keystroke before the body search is asked. */
const SEARCH_DELAY_MS = 300;

// The default selection: every scope at once (list, field filter and body search).
const ALL = "all";

// "user" or "project:<id>": one browsable scope.
const scopeKey = (m: { scope: string; project?: { id: string } }) =>
  m.scope === "user" ? "user" : "project:" + (m.project?.id ?? "");

export function MemoryBrowserPanel({ reload, onChanged }: { reload: number; onChanged: () => void }) {
  const tr = useT();
  const toast = useToast();
  const askConfirm = useConfirm();
  const [rows, setRows] = useState<MemoryListed[] | null>(null);
  const [withheld, setWithheld] = useState(0);
  const [forgotten, setForgotten] = useState<MemoryChange[]>([]);
  const [goneWithheld, setGoneWithheld] = useState(0);
  const [goneTruncated, setGoneTruncated] = useState(false);
  const [goneErr, setGoneErr] = useState(false);
  // The projects that hold a forgotten memory, as the Agent last named them (it names them even
  // when their rows did not fit). Kept across scope changes so a project stays selectable.
  const [goneProjects, setGoneProjects] = useState<Map<string, string>>(new Map());
  const [loadErr, setLoadErr] = useState("");
  const [scope, setScope] = useState(ALL);
  const [query, setQuery] = useState("");
  const [open, setOpen] = useState<MemoryBody | null>(null);
  const [openErr, setOpenErr] = useState("");
  const [hits, setHits] = useState<MemorySearchHit[]>([]);
  const [searching, setSearching] = useState(false);
  const [searchErr, setSearchErr] = useState(false);
  const [busy, setBusy] = useState(false);
  const [mine, setMine] = useState(0);

  const load = useCallback(async (signal: AbortSignal) => {
    const list = await api("api/agents/memory/entries/list");
    if (signal.aborted) return true;
    if (isTransientErr(list)) return false;
    setLoadErr(list?.error ? errText(list.error) : "");
    setRows(list?.error ? [] : (list?.entries ?? []));
    setWithheld(list?.error ? 0 : (list?.withheld ?? 0));
    return true;
  }, []);
  useRetryLoad(load, [reload, mine]);

  // The forgotten memories of the scope on screen ("all" sends no scope param, which makes the Agent
  // list the user scope and every project). Each memory's newest change is the one that took it
  // away; its revert brings the memory back.
  const loadGone = useCallback(async (signal: AbortSignal) => {
    const [kind, projectId = ""] = scope.split(":");
    const scopeQ =
      kind === ALL ? "" : kind === "user" ? "?scope=user" : `?scope=project&project=${encodeURIComponent(projectId)}`;
    const res = await api("api/agents/memory/entries/member-forgotten" + scopeQ);
    if (signal.aborted) return true;
    if (isTransientErr(res)) return false;
    setGoneErr(!!res?.error);
    setForgotten(res?.error ? [] : (res?.forgotten ?? []));
    if (!res?.error) {
      const named: { id: string; display: string }[] = res?.projects ?? [];
      // An answer for all scopes is the whole truth; one for a single scope only adds to it.
      setGoneProjects((prev) => {
        const next = new Map(kind === ALL ? [] : prev);
        for (const p of named) next.set("project:" + p.id, p.display);
        return next;
      });
    }
    setGoneWithheld(res?.error ? 0 : (res?.withheld ?? 0));
    setGoneTruncated(!res?.error && !!res?.truncated);
    return true;
  }, [scope]);
  useRetryLoad(loadGone, [reload, mine, scope]);

  // The scopes there is something to browse in. "All scopes" comes first and is the default: a
  // query is expected to cover everything, and the user scope alone is often empty. User scope is
  // always offered: a new memory can start there. A project is offered once it holds a memory
  // (the Console cannot start one).
  const scopes = useMemo(() => {
    const out = new Map<string, string>([
      [ALL, tr("mem.br_scope_all")],
      ["user", tr("mem.af_scope_user")],
    ]);
    for (const m of rows ?? []) {
      if (m.scope === "project" && m.project) out.set(scopeKey(m), m.project.display);
    }
    for (const [k, display] of goneProjects) if (!out.has(k)) out.set(k, display);
    return [...out.entries()];
  }, [rows, goneProjects, tr]);
  useEffect(() => {
    if (!scopes.some(([k]) => k === scope)) setScope(ALL);
  }, [scopes, scope]);

  const q = query.trim().toLowerCase();
  // Body search: asked after a pause, for the scope on screen. "All" sends no scope param, which makes the
  // Agent search the user scope and every project. A newer question or a scope change
  // drops the answer of an older one.
  const searchSeq = useRef(0);
  useEffect(() => {
    const seq = ++searchSeq.current;
    setHits([]);
    setSearchErr(false);
    if (!q) {
      setSearching(false);
      return;
    }
    setSearching(true);
    const [kind, projectId = ""] = scope.split(":");
    const timer = setTimeout(() => {
      const scopeQ =
        kind === ALL ? "" : kind === "user" ? "scope=user&" : `scope=project&project=${encodeURIComponent(projectId)}&`;
      api(`api/agents/memory/entries/member-search?${scopeQ}limit=50&q=${encodeURIComponent(q)}`)
        .then((res) => {
          if (seq !== searchSeq.current) return;
          if (res?.error) setSearchErr(true);
          else setHits(res?.hits ?? []);
        })
        .catch(() => {
          if (seq === searchSeq.current) setSearchErr(true);
        })
        .finally(() => {
          if (seq === searchSeq.current) setSearching(false);
        });
    }, SEARCH_DELAY_MS);
    return () => clearTimeout(timer);
  }, [q, scope]);

  // The rows of the scope that match by a listed field (instant) or by a body hit; a body hit
  // brings its matching lines. Order: listed matches as listed, then body-only hits in rank order.
  const snippets = new Map(hits.map((h) => [scopeKey(h) + "/" + h.name, h.snippets ?? []]));
  const inScope = (rows ?? []).filter((m) => scope === ALL || scopeKey(m) === scope);
  const byField = (m: MemoryListed) =>
    `${m.name}\n${m.description}\n${m.type ?? ""}\n${m.authorKind}\n${m.authorSession}`.toLowerCase().includes(q);
  const keyOf = (m: { scope: string; project?: { id: string }; name: string }) => scopeKey(m) + "/" + m.name;
  const fieldRows = inScope.filter(byField);
  const seen = new Set(fieldRows.map(keyOf));
  const byKey = new Map(inScope.map((m) => [keyOf(m), m]));
  const bodyRows = hits.flatMap((h) => {
    const m = byKey.get(keyOf(h));
    if (!m || seen.has(keyOf(h))) return [];
    seen.add(keyOf(h));
    return [m];
  });
  const shown = q ? [...fieldRows, ...bodyRows] : inScope;
  const gone = forgotten;
  const whereOf = (m: { scope: string; project?: { display: string } }) =>
    m.scope === "user" ? tr("mem.af_scope_user") : (m.project?.display ?? "");
  const who = (m: { authorKind: string; authorSession: string }) =>
    m.authorKind === "member" ? tr("mem.af_by_member") : `${m.authorKind} · ${m.authorSession}`;

  const view = async (m: MemoryListed) => {
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

  // The newest change of a memory, wherever it sits in the history. null: none could be resolved
  // (a toast says why).
  const newest = async (m: MemoryListed): Promise<MemoryChange | null> => {
    try {
      const res = await api(
        `api/agents/memory/entries/member-latest?scope=${m.scope}&project=${encodeURIComponent(m.project?.id ?? "")}&name=${encodeURIComponent(m.name)}`,
      );
      if (res?.error) toast(errDetail(res.error));
      else if (!res?.commit || !res.live || !res.revertible) toast(tr("mem.br_forget_unavailable", { name: m.name }));
      else return res as MemoryChange;
    } catch {
      toast(tr("mem.af_failed"));
    }
    return null;
  };

  const forget = async (m: MemoryListed) => {
    setBusy(true);
    const c = await newest(m);
    setBusy(false);
    if (!c) return;
    const ok = await askConfirm({
      title: tr("mem.af_forget_title"),
      body: <p>{tr("mem.af_forget_body", { name: m.name, where: whereOf(m) })}</p>,
      confirmLabel: tr("mem.af_forget"),
      danger: true,
    });
    if (!ok) return;
    setBusy(true);
    try {
      // The same revert route as the changes list; the commit also goes in the query for the audit.
      const res = await apiJSON("api/agents/memory/entries/revert?commit=" + encodeURIComponent(c.commit), "POST", {
        commit: c.commit,
        forget: true,
        ack: false,
        // The list and the changes are read apart: pin the forget to the revision this row showed.
        expectRevision: m.revision,
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
      </div>
      {searchErr && <p className="mem-warn">{tr("mem.br_search_failed")}</p>}
      <ul className="mem-pin-list mem-br-list">
        {rows === null ? (
          <li className="muted pad">{tr("common.loading")}</li>
        ) : shown.length === 0 ? (
          <li className="muted pad">{tr(q ? (searching ? "mem.br_searching" : "mem.br_no_match") : "mem.br_empty")}</li>
        ) : (
          shown.map((m) => (
            <li key={scopeKey(m) + "/" + m.name} className="mem-br-row">
              <span className="mem-af-name">{m.name}</span>
              {scope === ALL && <span className="mem-badge mem-br-where">{whereOf(m)}</span>}
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
              {(snippets.get(scopeKey(m) + "/" + m.name) ?? []).length > 0 && (
                <span className="mem-snap-what muted mem-br-snip">
                  {(snippets.get(scopeKey(m) + "/" + m.name) ?? []).join(" … ")}
                </span>
              )}
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
      {open && (
        <div className="mem-br-detail">
          <div className="mem-diff-head">
            <strong className="mem-af-name">{open.name}</strong>
            <span className="muted">{open.description}</span>
            <button type="button" onClick={() => setOpen(null)}>
              {tr("mem.br_close")}
            </button>
          </div>
          <pre className="mem-br-body">{open.body}</pre>
        </div>
      )}
      {(gone.length > 0 || goneWithheld > 0 || goneTruncated || goneErr) && (
        <>
          <h5>{tr("mem.br_forgotten")}</h5>
          <p className="muted ds-hint">{tr("mem.br_forgotten_note")}</p>
          {goneErr && <p className="mem-warn">{tr("mem.br_forgotten_failed")}</p>}
          {goneWithheld > 0 && <p className="mem-warn">{tr("mem.br_forgotten_withheld", { n: goneWithheld })}</p>}
          {goneTruncated && <p className="mem-warn">{tr("mem.br_forgotten_truncated", { n: gone.length })}</p>}
          <ul className="mem-pin-list mem-br-list">
            {gone.map((c) => (
              <li key={c.commit} className="mem-br-row">
                <span className="mem-af-name">{c.name}</span>
                {scope === ALL && <span className="mem-badge mem-br-where">{whereOf(c)}</span>}
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

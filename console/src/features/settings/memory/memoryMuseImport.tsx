// MuseImportPanel — the one-time import of muse's project-scope native memory
// (`<working copy>/.agents/memory`) into AF memory (ADR 0108 decision 6, #1991). It follows the
// claude import panel: preview first, confirm sends the previewed items in batches.
//
// muse's personal scopes are not read (their location is not established), and nothing here can
// reach AF's user scope. A file git tracks may be shared with teammates, so the preview says so
// before the member confirms. Applying needs the Agent Fleet memory switch on.

import { useCallback, useRef, useState } from "react";
import { api, apiJSON, errDetail, errText, isTransientErr } from "../../../core/api/client.ts";
import { useRetryLoad } from "../../../lib/retryLoad.ts";
import { useToast } from "../../../ui/ToastProvider.tsx";
import { Button } from "../../../ui/Button.tsx";
import { useT, tMaybe } from "../../../lib/i18n/index.ts";
import { fmtDateTime, DATETIME_FULL } from "../../../lib/intl.ts";
import { useSettings } from "../../../lib/settings.ts";
import { IMPORT_BATCH } from "./memoryClaudeImport.tsx";
import type { ClaudeImportResult, MuseImportItem, MuseImportPreview, MuseImportSource, MuseImportStatus } from "./memoryTypes.ts";

const STATUSES: MuseImportStatus[] = ["new", "update", "unchanged", "diverged", "forgotten", "secret", "invalid"];
// An unknown reason from a newer Agent is printed raw rather than breaking the row.
const reasonLabel = (r: string): string => tMaybe("mem.mi_reason_" + r) ?? tMaybe("mem.ci_reason_" + r) ?? r;

export function MuseImportPanel({ reload, onChanged }: { reload: number; onChanged: () => void }) {
  const tr = useT();
  const toast = useToast();
  const enabled = useSettings().agentMemory;
  const [sources, setSources] = useState<MuseImportSource[] | null>(null);
  const [withheld, setWithheld] = useState(0);
  const [loadErr, setLoadErr] = useState("");
  const [dir, setDir] = useState("");
  const [preview, setPreview] = useState<MuseImportPreview | null>(null);
  const [previewErr, setPreviewErr] = useState("");
  const [busy, setBusy] = useState(false);
  const [progress, setProgress] = useState<{ done: number; total: number } | null>(null);
  const [again, setAgain] = useState(0);

  const load = useCallback(async (signal: AbortSignal) => {
    const res = await api("api/agents/memory/muse-import");
    if (signal.aborted) return true;
    if (isTransientErr(res)) return false;
    setLoadErr(res?.error ? errText(res.error) : "");
    setSources(res?.error ? [] : (res?.sources ?? []));
    setWithheld(res?.error ? 0 : (res?.withheld ?? 0));
    return true;
  }, []);
  useRetryLoad(load, [reload, again]);

  // Only the latest request may answer, or the confirm would pair one project with another dir.
  const previewSeq = useRef(0);
  const loadPreview = useCallback(async (d: string) => {
    const seq = ++previewSeq.current;
    setPreview(null);
    setPreviewErr("");
    if (!d) return;
    const res = await api("api/agents/memory/muse-import/preview?dir=" + encodeURIComponent(d));
    if (seq !== previewSeq.current) return;
    if (res?.error) {
      setPreviewErr(errDetail(res.error));
      return;
    }
    setPreview(res as MuseImportPreview);
  }, []);

  const pick = (d: string) => {
    setDir(d);
    void loadPreview(d);
  };

  const todo: MuseImportItem[] = (preview?.items ?? []).filter((i) => i.status === "new" || i.status === "update");
  const sharedTodo = todo.filter((i) => i.shared).length;

  const run = async () => {
    if (!preview?.project || todo.length === 0) return;
    const project = preview.project.id;
    setBusy(true);
    setProgress({ done: 0, total: todo.length });
    let imported = 0;
    let skipped = 0;
    try {
      for (let at = 0; at < todo.length; at += IMPORT_BATCH) {
        const items = todo.slice(at, at + IMPORT_BATCH).map((i) => ({ name: i.name, sourceHash: i.sourceHash }));
        // The project id also goes in the query: the CP audit ledger reads the URL only.
        const res = await apiJSON("api/agents/memory/muse-import?project=" + encodeURIComponent(project), "POST", {
          project,
          dir: preview.dir,
          items,
        });
        if (res?.error) {
          toast(errDetail(res.error));
          return;
        }
        for (const r of (res?.results ?? []) as ClaudeImportResult[]) {
          if (r.result === "skipped") skipped++;
          else imported++;
        }
        setProgress({ done: Math.min(at + IMPORT_BATCH, todo.length), total: todo.length });
      }
      toast(tr(skipped ? "mem.ci_done_skipped" : "mem.ci_done", { n: imported, skipped }), {
        kind: skipped ? undefined : "success",
      });
    } catch {
      // The request may or may not have reached the Agent: re-read what is there now.
      toast(tr("mem.ci_failed"));
    } finally {
      setBusy(false);
      setProgress(null);
      setAgain((n) => n + 1);
      onChanged();
      void loadPreview(dir);
    }
  };

  const sourceLabel = (s: MuseImportSource) =>
    `${s.project?.display ?? s.dir} (${s.count})${s.reason ? " — " + reasonLabel(s.reason) : ""}`;

  const statusLabel: Record<MuseImportStatus, string> = {
    new: tr("mem.mi_status_new"),
    update: tr("mem.mi_status_update"),
    unchanged: tr("mem.mi_status_unchanged"),
    diverged: tr("mem.mi_status_diverged"),
    forgotten: tr("mem.mi_status_forgotten"),
    secret: tr("mem.mi_status_secret"),
    invalid: tr("mem.mi_status_invalid"),
  };
  const group = (st: MuseImportStatus) => {
    const items = (preview?.items ?? []).filter((i) => i.status === st);
    if (items.length === 0) return null;
    return (
      <details key={st} open={st === "new" || st === "update" || st === "secret"} className="mem-ci-group">
        <summary>
          <span className={"mem-badge ci-" + st}>{statusLabel[st]}</span> {items.length}
        </summary>
        <ul className="mem-ci-items">
          {items.map((i) => (
            <li key={i.name}>
              <span className="mem-af-name">{i.name}</span>
              {i.shared && <span className="mem-badge"> {tr("mem.mi_shared_badge")}</span>}
              {st === "update" && i.sourceModified && i.afUpdated && (
                <span className="muted">
                  {" · "}
                  {tr("mem.mi_update_times", {
                    muse: fmtDateTime(i.sourceModified, DATETIME_FULL),
                    af: fmtDateTime(i.afUpdated, DATETIME_FULL),
                  })}
                </span>
              )}
              {(st === "invalid" || st === "diverged") && i.reason && (
                <span className="muted"> · {reasonLabel(i.reason)}</span>
              )}
              {st === "secret" && (
                <ul className="mem-findings">
                  {(i.findings ?? []).map((f, n) => (
                    <li key={n}>
                      <code>{f.rule}</code> {tr("mem.af_secret_line", { line: f.line })} <code>{f.hint}</code>
                    </li>
                  ))}
                </ul>
              )}
            </li>
          ))}
        </ul>
      </details>
    );
  };

  return (
    <div className="mem-ci">
      <h4>{tr("mem.mi_title")}</h4>
      <p className="muted ds-hint">{tr("mem.mi_intro")}</p>
      {loadErr && <p className="mem-warn">{loadErr}</p>}
      {withheld > 0 && <p className="mem-warn">{tr("mem.mi_withheld_sources", { n: withheld })}</p>}
      {sources === null ? (
        <p className="muted">{tr("common.loading")}</p>
      ) : sources.length === 0 ? (
        !loadErr && <p className="muted">{tr("mem.mi_none")}</p>
      ) : (
        <label className="mem-scope">
          <span className="muted">{tr("mem.mi_source")}</span>
          <select value={dir} disabled={busy} onChange={(e) => pick(e.target.value)}>
            <option value="">{tr("mem.ci_pick")}</option>
            {sources.map((s) => (
              <option key={s.dir} value={s.dir} disabled={!s.project}>
                {sourceLabel(s)}
              </option>
            ))}
          </select>
        </label>
      )}
      {previewErr && <p className="mem-warn">{previewErr}</p>}
      {dir && !preview && !previewErr && <p className="muted">{tr("common.loading")}</p>}
      {preview && (
        <>
          <p className="muted ds-hint">{tr("mem.ci_to", { project: preview.project?.display ?? "" })}</p>
          {(preview.items ?? []).some((i) => i.shared) && <p className="mem-warn">{tr("mem.mi_shared_warn")}</p>}
          {STATUSES.map(group)}
          {(preview.withheld ?? 0) > 0 && (
            <p className="mem-warn">{tr("mem.ci_withheld_files", { n: preview.withheld ?? 0 })}</p>
          )}
          {preview.truncated && <p className="mem-warn">{tr("mem.ci_truncated")}</p>}
          {!enabled && <p className="mem-warn">{tr("mem.ci_off")}</p>}
          <div className="mem-ci-actions">
            <Button variant="primary" disabled={busy || !enabled || todo.length === 0} onClick={() => void run()}>
              {tr("mem.ci_apply", { n: todo.length })}
            </Button>
            {sharedTodo > 0 && <span className="muted">{tr("mem.mi_apply_shared", { n: sharedTodo })}</span>}
            {progress && (
              <span className="muted" role="status">
                {tr("mem.ci_progress", { done: progress.done, total: progress.total })}
              </span>
            )}
          </div>
        </>
      )}
    </div>
  );
}

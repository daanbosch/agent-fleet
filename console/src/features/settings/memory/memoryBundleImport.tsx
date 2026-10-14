// BundleAfPanel — the one-shot import of AF memory out of a memory bundle (#1983, ADR 0108). It
// sits under the generic bundle import, which takes the file in and keeps its lineage under an
// import id; the generic apply never writes AF memory (no secret scan), so this is where it is
// brought in: previewed first, project by project, then sent in batches of 50.
//
// Only each memory's latest text comes across. The bundle's revision, authors, dates, pins and
// history do not (the Agent resets them and records where each came from), a memory that already
// exists here is never replaced, and one forgotten here is never brought back.

import { useCallback, useState } from "react";
import { api, apiJSON, errDetail, errText, isTransientErr } from "../../../core/api/client.ts";
import { useRetryLoad } from "../../../lib/retryLoad.ts";
import { useToast } from "../../../ui/ToastProvider.tsx";
import { Button } from "../../../ui/Button.tsx";
import { useT, tMaybe } from "../../../lib/i18n/index.ts";
import type { BundleItem, BundlePreview, BundleResult, BundleScope } from "./memoryTypes.ts";

/** The Agent's per-request cap (agentMemBundleBatch). */
export const BUNDLE_BATCH = 50;

const STATUSES: BundleItem["status"][] = ["new", "exists", "forgotten", "secret", "invalid", "unchanged"];
const statusLabel = (s: string): string => tMaybe("mem.bi_status_" + s) ?? s;
const reasonLabel = (r: string): string => tMaybe("mem.bi_reason_" + r) ?? tMaybe("mem.ci_reason_" + r) ?? r;
const scopeName = (s: BundleScope, tr: (k: "mem.bi_user_scope") => string): string =>
  s.source === "user" ? tr("mem.bi_user_scope") : (s.display ?? s.source);

export function BundleAfPanel({ importId, onChanged }: { importId: string; onChanged: () => void }) {
  const tr = useT();
  const toast = useToast();
  const [preview, setPreview] = useState<BundlePreview | null>(null);
  const [loadErr, setLoadErr] = useState("");
  const [busy, setBusy] = useState(false);
  const [progress, setProgress] = useState<{ done: number; total: number } | null>(null);
  const [again, setAgain] = useState(0);

  const load = useCallback(
    async (signal: AbortSignal) => {
      const res = await api("api/agents/memory/af-import/preview?importId=" + encodeURIComponent(importId));
      if (signal.aborted) return true;
      if (isTransientErr(res)) return false;
      setLoadErr(res?.error ? errText(res.error) : "");
      setPreview(res?.error ? null : (res as BundlePreview));
      return true;
    },
    [importId],
  );
  useRetryLoad(load, [importId, again]);

  if (loadErr) return <p className="mem-warn">{loadErr}</p>;
  if (!preview || !preview.hasAF) return null;

  // What would be sent: the new memories of every scope that has a local target.
  const todo = preview.scopes.flatMap((s) =>
    s.target || s.source === "user"
      ? s.items
          .filter((i) => i.status === "new")
          .map((i) => ({ scope: s.source, target: s.target?.id ?? "", name: i.name, sourceHash: i.sourceHash ?? "" }))
      : [],
  );

  const run = async () => {
    setBusy(true);
    setProgress({ done: 0, total: todo.length });
    let imported = 0;
    let skipped = 0;
    try {
      for (let at = 0; at < todo.length; at += BUNDLE_BATCH) {
        const items = todo.slice(at, at + BUNDLE_BATCH);
        // The import id also goes in the query: the CP audit ledger reads the URL only.
        const res = await apiJSON("api/agents/memory/af-import?importId=" + encodeURIComponent(importId), "POST", {
          importId,
          items,
        });
        if (res?.error) {
          toast(errDetail(res.error));
          return;
        }
        for (const r of (res?.results ?? []) as BundleResult[]) {
          if (r.result === "imported") imported++;
          else skipped++;
        }
        setProgress({ done: Math.min(at + BUNDLE_BATCH, todo.length), total: todo.length });
      }
      toast(tr(skipped ? "mem.bi_done_skipped" : "mem.bi_done", { n: imported, skipped }), {
        kind: skipped ? undefined : "success",
      });
    } catch {
      // The request may or may not have reached the Agent: read what is there now.
      toast(tr("mem.bi_failed"));
    } finally {
      setBusy(false);
      setProgress(null);
      setAgain((n) => n + 1);
      onChanged();
    }
  };

  return (
    <div className="mem-ci">
      <h4>{tr("mem.bi_title")}</h4>
      <p className="muted ds-hint">{tr("mem.bi_intro")}</p>
      {preview.incomplete && <p className="mem-warn">{tr("mem.bi_incomplete")}</p>}
      {preview.truncated && <p className="mem-warn">{tr("mem.ci_truncated")}</p>}
      {preview.scopes.map((s) => (
        <details key={s.source} className="mem-ci-group" open={!!s.target || s.source === "user"}>
          <summary>
            <span className="mem-af-name">{scopeName(s, tr)}</span>
            {s.target ? (
              <span className="muted">
                {" → "}
                {s.target.display} {s.match === "remote" ? tr("mem.bi_by_remote") : ""}
              </span>
            ) : s.source !== "user" ? (
              <span className="muted">
                {" — "}
                {tr(s.reason === "ambiguous" ? "mem.bi_ambiguous" : "mem.bi_no_local", {
                  n: s.counts["skipped"] ?? 0,
                })}
              </span>
            ) : null}
          </summary>
          {STATUSES.map((st) => {
            const items = s.items.filter((i) => i.status === st);
            if (items.length === 0) return null;
            return (
              <details key={st} open={st !== "unchanged"} className="mem-ci-group">
                <summary>
                  <span className={"mem-badge ci-" + st}>{statusLabel(st)}</span> {items.length}
                </summary>
                <ul className="mem-ci-items">
                  {items.map((i) => (
                    <li key={i.name}>
                      <span className="mem-af-name">{i.name}</span>
                      {i.reason && <span className="muted"> · {reasonLabel(i.reason)}</span>}
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
          })}
          {(s.withheld ?? 0) > 0 && <p className="mem-warn">{tr("mem.ce_withheld", { n: s.withheld ?? 0 })}</p>}
        </details>
      ))}
      <div className="mem-ci-actions">
        <Button variant="primary" disabled={busy || todo.length === 0} onClick={() => void run()}>
          {progress ? tr("mem.ci_progress", { done: progress.done, total: progress.total }) : tr("mem.bi_apply", { n: todo.length })}
        </Button>
      </div>
    </div>
  );
}

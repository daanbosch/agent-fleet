// AgentMemoryStatus — the one status header of the Agent memory entry: is it on, how much is in
// it, when it last changed, and whether the background sync needs attention. It only reads
// endpoints the panels below already call (entries/list, entries/changes, auto-sync); nothing
// here is new API. A read that fails leaves its figure out rather than showing a wrong zero.

import { useCallback, useState } from "react";
import { api, isTransientErr } from "../../../../core/api/client.ts";
import { useRetryLoad } from "../../../../lib/retryLoad.ts";
import { useSettings } from "../../../../lib/settings.ts";
import { useT } from "../../../../lib/i18n/index.ts";
import { fmtDateTime, DATETIME_FULL } from "../../../../lib/intl.ts";
import type { AutoSyncStatus, MemoryChange, MemoryListed } from "../memoryTypes.ts";

interface Figures {
  count: number | null;
  projects: number | null;
  lastChange: string | null;
  syncProblem: boolean;
}

// The reconciler needs the member when a run failed, the Agent reported a problem it cannot fix
// by itself, or the last run left conflicts or skipped secrets.
function needsAttention(st: AutoSyncStatus | undefined): boolean {
  if (!st) return false;
  const left = (st.run?.projects ?? []).reduce((n, p) => n + (p.counts.conflict ?? 0) + (p.counts.secret ?? 0), 0);
  return !!st.failing || (st.problems ?? []).length > 0 || left > 0;
}

export function AgentMemoryStatus({
  running,
  reload,
  onOpenSettings,
}: {
  running: boolean;
  reload: number;
  onOpenSettings: () => void;
}) {
  const tr = useT();
  const enabled = useSettings().agentMemory;
  const [fig, setFig] = useState<Figures | null>(null);

  const load = useCallback(
    async (signal: AbortSignal) => {
      // Nothing to ask while stopped; the deps re-run this once the workspace is up.
      if (!running) return true;
      const [list, changes, sync] = await Promise.all([
        api("api/agents/memory/entries/list"),
        api("api/agents/memory/entries/changes?limit=1"),
        api("api/agents/memory/auto-sync"),
      ]);
      if (signal.aborted) return true;
      if (isTransientErr(list) || isTransientErr(changes) || isTransientErr(sync)) return false;
      const entries: MemoryListed[] | null = list?.error ? null : (list?.entries ?? []);
      const last: MemoryChange | undefined = changes?.error ? undefined : changes?.changes?.[0];
      setFig({
        count: entries ? entries.length : null,
        projects: entries ? new Set(entries.flatMap((m) => (m.project ? [m.project.id] : []))).size : null,
        lastChange: last?.at ?? null,
        syncProblem: !sync?.error && needsAttention(sync as AutoSyncStatus),
      });
      return true;
    },
    [running],
  );
  useRetryLoad(load, [running, reload]);

  return (
    <div className="mem-status">
      <div className="mem-status-line">
        <span className={"mem-badge " + (enabled ? "mem-on" : "mem-off")}>
          {tr(enabled ? "mem.status_on" : "mem.status_off")}
        </span>
        {running && fig?.count != null && <span>{tr("mem.status_count", { n: fig.count })}</span>}
        {running && fig?.projects != null && <span>{tr("mem.status_projects", { n: fig.projects })}</span>}
        {running && fig?.lastChange && (
          <span className="muted">{tr("mem.status_last_change", { when: fmtDateTime(fig.lastChange, DATETIME_FULL) })}</span>
        )}
      </div>
      {running && fig?.syncProblem && (
        <p className="mem-warn mem-status-warn">
          {tr("mem.status_sync_problem")}{" "}
          <button type="button" className="ghost" onClick={onOpenSettings}>
            {tr("mem.status_sync_review")}
          </button>
        </p>
      )}
    </div>
  );
}

// The tenant's defaults for its members' Console (ADR 0114 decision 8): today the agent the
// first-run card names. Tenants pay for different agents, so a new member is pointed at the
// one their team uses instead of being asked to choose. It names, never restricts.
import { useCallback, useEffect, useState } from "react";
import { api, apiJSON, errDetail } from "../../../core/api/client.ts";
import { Icon } from "../../../ui/Icon.tsx";
import { useT } from "../../../lib/i18n/index.ts";
import { MEMBER_DEFAULT_AGENTS, agentOf } from "../../../agents/registry.ts";
import type { TenantMemberDefaults } from "../../../core/store/uiState.ts";

/** GET/PUT /api/admin/tenants/{slug}/member-defaults. */
export interface TenantMemberDefaultsView {
  tenant: string;
  defaults: TenantMemberDefaults;
  updated_by?: string;
  updated_at?: string;
}

export function TenantMemberDefaultsPanel({ slug }: { slug: string }) {
  const tr = useT();
  const [view, setView] = useState<TenantMemberDefaultsView | null>(null);
  const [agent, setAgent] = useState("");
  const [error, setError] = useState("");
  const [loadFailed, setLoadFailed] = useState(false);
  const [busy, setBusy] = useState(false);
  const [saved, setSaved] = useState(false);
  const path = `api/admin/tenants/${encodeURIComponent(slug)}/member-defaults`;
  const message = (e: unknown) => (e instanceof Error ? e.message : String(e));

  // A failed load says so and offers a retry: a panel stuck on "loading" gives the admin nothing
  // to act on.
  const load = useCallback(async () => {
    setLoadFailed(false);
    try {
      const d = await api(path);
      if (d && !d.error && d.defaults) {
        setView(d);
        setAgent(d.defaults.agent || "");
        setError("");
        return;
      }
      setError(d?.error ? errDetail(d.error) : tr("tenant.member_defaults_load_failed", { error: "" }));
    } catch (e) {
      setError(tr("tenant.member_defaults_load_failed", { error: message(e) }));
    }
    setLoadFailed(true);
  }, [path]);
  useEffect(() => {
    load();
  }, [load]);

  const save = async () => {
    setBusy(true);
    setError("");
    try {
      const res = await apiJSON(path, "PUT", { agent });
      if (res?.error) {
        setError(errDetail(res.error));
        return;
      }
      setView(res);
      setAgent(res.defaults?.agent || "");
      setSaved(true);
      setTimeout(() => setSaved(false), 1500);
    } catch (e) {
      // The selection stays, so pressing Save again retries it.
      setError(tr("tenant.member_defaults_save_failed", { error: message(e) }));
    } finally {
      setBusy(false);
    }
  };

  if (!view)
    return loadFailed ? (
      <div className="pad">
        <p className="admin-hint warn" role="alert">
          {error}
        </p>
        <button onClick={load}>{tr("tenant.member_defaults_retry")}</button>
      </div>
    ) : (
      <p className="muted pad">{tr("common.loading")}</p>
    );

  return (
    <section className="admin-panel">
      <div className="admin-fgroup">
        <h4>{tr("tenant.member_defaults_title")}</h4>
        <p className="admin-hint">{tr("tenant.member_defaults_note")}</p>
        <label className="admin-fld">
          <span className="af-cap">{tr("tenant.member_defaults_agent")}</span>
          <select
            value={agent}
            onChange={(e) => {
              setAgent(e.target.value);
              setError("");
            }}
          >
            <option value="">{tr("tenant.member_defaults_none")}</option>
            {MEMBER_DEFAULT_AGENTS.map((k) => (
              <option key={k} value={k}>
                {agentOf(k).label}
              </option>
            ))}
          </select>
        </label>
        {error && (
          <p className="admin-hint warn" role="alert">
            {error}
          </p>
        )}
        <div className="le-actions">
          <button className="primary" disabled={busy} onClick={save}>
            {tr("common.save")}
          </button>
          {saved && (
            <span className="saved-note">
              <Icon name="check" /> {tr("admin.saved")}
            </span>
          )}
          {view.updated_at && <span className="muted">{tr("tenant.member_defaults_updated", { at: view.updated_at })}</span>}
        </div>
      </div>
    </section>
  );
}

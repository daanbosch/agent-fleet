// TenantLimitBoardView — the tenant switch for the limit board (ADR 0111 addendum item 4).
//
// On by default, and members stay hidden until each opts in. Turning it off is for an
// organisation whose policy does not want remaining quota shown at all; it stops collection
// and empties the board. Nothing here shows the admin any member's limits: admin views are not
// widened (decision 6).
import { useCallback, useEffect, useState } from "react";
import { api, apiJSON, errText } from "../../core/api/client.ts";
import { useT } from "../../lib/i18n/index.ts";
import { useToast } from "../../ui/ToastProvider.tsx";

export function TenantLimitBoardView({ slug }: { slug: string }) {
  const tr = useT();
  const toast = useToast();
  const [enabled, setEnabled] = useState<boolean | null>(null);
  const [busy, setBusy] = useState(false);
  const path = `api/admin/tenants/${encodeURIComponent(slug)}/limit-board`;

  const load = useCallback(async () => {
    const d = await api(path);
    if (d && !d.error) setEnabled(!!d.enabled);
  }, [path]);
  useEffect(() => {
    void load();
  }, [load]);

  const set = async (on: boolean) => {
    setBusy(true);
    try {
      const res = await apiJSON(path, "PUT", { enabled: on });
      if (res?.error) {
        toast(errText(res.error));
        return;
      }
      setEnabled(!!res.enabled);
    } finally {
      setBusy(false);
    }
  };

  if (enabled === null) return <p className="muted pad">{tr("common.loading")}</p>;
  return (
    <section className="admin-panel">
      <div className="admin-fgroup">
        <h4>
          {tr("limits.tenant_title")}
          <span className="af-note">{enabled ? tr("limits.tenant_on") : tr("limits.tenant_off_short")}</span>
        </h4>
        <label className="admin-fld wide">
          <span>
            <input type="checkbox" checked={enabled} disabled={busy} onChange={(e) => void set(e.target.checked)} />{" "}
            {tr("limits.tenant_toggle")}
          </span>
        </label>
        <p className="admin-hint">{tr("limits.tenant_hint")}</p>
        <p className="admin-hint">{tr("limits.tenant_pooling")}</p>
      </div>
    </section>
  );
}

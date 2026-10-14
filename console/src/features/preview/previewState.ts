// The workspace's preview state as the port controls need it (docs/log/81, ADR 0062): the
// issued subdomain URLs, which ports are listed, which unlisted ones the owner has open right
// now, and who else can see the listed ones. One store, so the session rows and the
// workspace bar agree on what a click does without each fetching it.
import { create } from "zustand";
import { api, getTenant } from "../../core/api/client.ts";
import { useWorkspaceStore } from "../../core/store/workspace.ts";

export interface PreviewInfo {
  /** The deployment's preview domain; "" when host-based previews are off. */
  domain: string;
  /** port → URL, only for ports with a URL issued for this start (empty while stopped). */
  urls: Record<string, string>;
  /** Listed ports (the allow-list). */
  ports: number[];
  /** Unlisted ports the owner has opened for this start (grants). */
  tempPorts: number[];
  public: boolean;
  tenantShare: boolean;
  maxPorts: number;
}

/** Reads the ws-settings payload; null when it carries no preview block. */
export function previewInfoFrom(res: any): PreviewInfo | null {
  if (!res || res.error || typeof res !== "object") return null;
  return {
    domain: typeof res.previewDomain === "string" ? res.previewDomain : "",
    urls: res.previewUrls && typeof res.previewUrls === "object" ? res.previewUrls : {},
    ports: Array.isArray(res.previewPorts) ? res.previewPorts : [],
    tempPorts: Array.isArray(res.previewTempPorts) ? res.previewTempPorts : [],
    public: !!res.previewPublic,
    tenantShare: !!res.previewTenantShare,
    maxPorts: Number(res.previewMaxPorts) || 8,
  };
}

interface PreviewStore {
  /** The last answer, and the tenant it was read for. */
  info: PreviewInfo | null;
  tenant: string;
  set(info: PreviewInfo | null): void;
  /** Forget everything (workspace stopped / switched) and ignore any answer still in flight. */
  reset(): void;
  /** Re-read from the CP. Without `force`, calls closer than MIN_GAP_MS, or while one is in
   *  flight for this tenant, share the previous answer. `force` starts a new read and makes
   *  every older one's answer void. */
  refresh(opts?: { force?: boolean }): Promise<void>;
}

const MIN_GAP_MS = 4000;
let lastAt = 0;
let inflight: Promise<void> | null = null;
let inflightTenant = "";
// Bumped by every forced read and every reset: an answer from an older generation (a slow
// reply from before a restart, or from another tenant) must not bring its slug back.
let gen = 0;

export const usePreviewStore = create<PreviewStore>((set, get) => ({
  info: null,
  tenant: "",
  set: (info) => set({ info, tenant: getTenant() }),
  reset() {
    gen++;
    inflight = null;
    lastAt = 0;
    set({ info: null, tenant: "" });
  },
  refresh({ force = false } = {}) {
    const tenant = getTenant();
    if (!force && inflight && inflightTenant === tenant) return inflight;
    if (!force && get().tenant === tenant && get().info && Date.now() - lastAt < MIN_GAP_MS) return Promise.resolve();
    const mine = ++gen;
    lastAt = Date.now();
    inflightTenant = tenant;
    const p: Promise<void> = api("api/env/ws-settings")
      .then(
        (res: any) => {
          if (mine === gen) set({ info: previewInfoFrom(res), tenant });
        },
        () => {
          if (mine === gen) set({ info: null, tenant });
        },
      )
      .finally(() => {
        if (inflight === p) inflight = null;
      });
    inflight = p;
    return p;
  },
}));

/** The info that is valid for the tenant the Console is on now, else null (so a control falls
 *  back to the pane / lightweight preview instead of opening another tenant's slug). */
export function useCurrentPreviewInfo(): PreviewInfo | null {
  return usePreviewStore((s) => (s.tenant === getTenant() ? s.info : null));
}

// A workspace that stops, restarts or is replaced issues a new slug: drop what was read for the
// old start, and read again once it runs. One subscription here rather than one per control.
let lastWsState = useWorkspaceStore.getState().state;
useWorkspaceStore.subscribe((s) => {
  if (s.state === lastWsState) return;
  lastWsState = s.state;
  if (s.state === "running") void usePreviewStore.getState().refresh({ force: true });
  else usePreviewStore.getState().reset();
});

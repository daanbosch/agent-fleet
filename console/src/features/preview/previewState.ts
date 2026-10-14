// The workspace's preview state as the port controls need it (docs/log/81, ADR 0062): the
// issued subdomain URLs, which ports are listed, which unlisted ones the owner has open right
// now, and who else can see the listed ones. One store, so the session rows and the
// workspace bar agree on what a click does without each fetching it.
import { create } from "zustand";
import { api } from "../../core/api/client.ts";

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
  info: PreviewInfo | null;
  set(info: PreviewInfo | null): void;
  /** Re-read from the CP. Calls closer than `minGapMs` share the previous answer. */
  refresh(opts?: { force?: boolean }): Promise<void>;
}

const MIN_GAP_MS = 4000;
let lastAt = 0;
let inflight: Promise<void> | null = null;

export const usePreviewStore = create<PreviewStore>((set) => ({
  info: null,
  set: (info) => set({ info }),
  refresh({ force = false } = {}) {
    if (inflight) return inflight;
    if (!force && Date.now() - lastAt < MIN_GAP_MS) return Promise.resolve();
    lastAt = Date.now();
    inflight = api("api/env/ws-settings").then(
      (res: any) => set({ info: previewInfoFrom(res) }),
      () => set({ info: null }),
    ).finally(() => {
      inflight = null;
    });
    return inflight;
  },
}));

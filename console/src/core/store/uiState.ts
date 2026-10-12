// uiState — the member's Console-only UI state, kept in the control plane (ADR 0114) so it is
// readable while the workspace is stopped and follows the member to another device. Settings
// the Agent acts on belong in ui-prefs (lib/settings.ts), never here as well.
//
// localStorage holds a copy for the first paint only, keyed per (user, tenant). The CP answer
// replaces it as soon as it arrives.
import { create } from "zustand";
import { api, apiJSON, getTenant, getUser } from "../api/client.ts";

/** The first-run state. Mirrors the CP's onboardingStateWire (control-plane/member_ui_state.go). */
export interface OnboardingState {
  /** The member put the setup checklist away ("later"). */
  dismissed: boolean;
  /** The goal track they opened: "dev" = the development steps are expanded. */
  track: "" | "dev";
}

/** The tenant's defaults for its members (ADR 0114 decision 8). Mirrors tenantMemberDefaultsWire
 *  (control-plane/tenant_member_defaults.go). Read-only here: only a tenant admin sets them. */
export interface TenantMemberDefaults {
  /** The agent the first-run card names; "" = none. */
  agent: string;
}

/** The whole document. Mirrors memberUIStateWire; a PATCH replaces the top-level keys it carries.
 *  `defaults` comes back on every answer and is never sent. */
export interface MemberUIState {
  onboarding?: OnboardingState;
  defaults?: TenantMemberDefaults;
}

// The keys the first-run card used before ADR 0114. Read once to migrate, then removed.
const LEGACY_DISMISS = "af.onboarding.dismissed";
const LEGACY_TRACK = "af.onboarding.track";

const cacheKey = () => `af.ui-state:${getUser()}:${getTenant()}`;

function readCache(): MemberUIState {
  try {
    const raw = localStorage.getItem(cacheKey());
    return raw ? (JSON.parse(raw) as MemberUIState) : {};
  } catch {
    return {};
  }
}

function writeCache(s: MemberUIState) {
  try {
    localStorage.setItem(cacheKey(), JSON.stringify(s));
  } catch {}
}

/** The pre-ADR-0114 onboarding keys, or null when this browser never wrote any. */
export function legacyOnboarding(): OnboardingState | null {
  try {
    const d = localStorage.getItem(LEGACY_DISMISS);
    const t = localStorage.getItem(LEGACY_TRACK);
    if (d === null && t === null) return null;
    return { dismissed: d === "1", track: t === "dev" ? "dev" : "" };
  } catch {
    return null;
  }
}

function clearLegacy() {
  try {
    localStorage.removeItem(LEGACY_DISMISS);
    localStorage.removeItem(LEGACY_TRACK);
  } catch {}
}

interface UIStateStore {
  state: MemberUIState;
  /** The CP has answered for the current (user, tenant). Until then `state` is the cache. */
  loaded: boolean;
  /** Re-reads the cache for the current (user, tenant) and asks the CP. Call on boot and on a
   *  tenant switch. */
  load(): Promise<void>;
  /** Applies locally at once, then stores. A failed store keeps the local value for this tab;
   *  the next load brings back whatever the CP holds. */
  patch(p: MemberUIState): Promise<void>;
}

export const useUIStateStore = create<UIStateStore>((set, get) => ({
  state: readCache(),
  loaded: false,
  async load() {
    const key = cacheKey();
    set({ state: readCache(), loaded: false });
    let s: MemberUIState;
    try {
      s = ((await api("api/me/ui-state")) || {}) as MemberUIState;
    } catch {
      return; // CP unreachable: keep the cache, stay unloaded so nobody reads it as fresh
    }
    if (cacheKey() !== key) return; // the tenant changed while this was in flight
    set({ state: s, loaded: true });
    writeCache(s);
    const legacy = legacyOnboarding();
    if (legacy && !s.onboarding) await get().patch({ onboarding: legacy });
    if (legacy) clearLegacy();
  },
  async patch(p) {
    const next = { ...get().state, ...p };
    set({ state: next });
    writeCache(next);
    try {
      const s = (await apiJSON("api/me/ui-state", "PATCH", p)) as MemberUIState;
      if (s) {
        set({ state: s });
        writeCache(s);
      }
    } catch {
      /* keep the local value; see the doc comment */
    }
  },
}));

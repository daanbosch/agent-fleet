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

function readCache(key: string): MemberUIState {
  try {
    const raw = localStorage.getItem(key);
    return raw ? (JSON.parse(raw) as MemberUIState) : {};
  } catch {
    return {};
  }
}

function writeCache(key: string, s: MemberUIState) {
  try {
    localStorage.setItem(key, JSON.stringify(s));
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

/** A CP answer is a document only when it is an object without `error`: api() resolves HTTP
 *  failures as `{error}` instead of throwing. */
const isDoc = (v: unknown): v is MemberUIState => !!v && typeof v === "object" && !("error" in v);

// Request bookkeeping, outside the store so it never re-renders anything.
//
// `owner` is the (user, tenant) cache key the state belongs to. Every answer is checked against
// the owner its request was made for, so an answer that arrives after a tenant switch is dropped
// instead of landing in the other tenant's state and cache.
//
// `pending` holds keys patched locally and not yet confirmed. Every answer is shown with them on
// top, so neither an earlier PATCH's answer nor a GET started before a PATCH can roll a newer
// choice back. A key leaves `pending` only when an answer confirms the value it still holds.
//
// `writes` counts confirmed PATCHes for the owner: a GET that started before one of them carries
// an older document than the PATCH answer already shown, so its document is not used.
let owner = "";
let pending: MemberUIState = {};
let writes = 0;
// PATCHes go one at a time, so their answers arrive in the order the choices were made.
let queue: Promise<unknown> = Promise.resolve();

function adopt(key: string, doc: MemberUIState, set: (s: Partial<UIStateStore>) => void) {
  const shown = { ...doc, ...pending };
  set({ state: shown });
  writeCache(key, shown);
}

interface UIStateStore {
  state: MemberUIState;
  /** The CP has answered for the current (user, tenant). Until then `state` is the cache. */
  loaded: boolean;
  /** Re-reads the cache for the current (user, tenant) and asks the CP. Call on boot, on a
   *  tenant switch and when the signed-in user is resolved. */
  load(): Promise<void>;
  /** Applies locally at once, then stores. Resolves true once the CP confirmed it. A failed
   *  store keeps the local value for this tab; the next load brings back what the CP holds.
   *  `ifAbsent` asks the CP to fill only keys it does not hold yet. */
  patch(p: MemberUIState, opts?: { ifAbsent?: boolean }): Promise<boolean>;
}

export const useUIStateStore = create<UIStateStore>((set, get) => ({
  state: readCache(cacheKey()),
  loaded: false,
  async load() {
    const key = cacheKey();
    if (key !== owner) {
      owner = key;
      pending = {};
      writes = 0;
      set({ state: readCache(key), loaded: false });
    }
    const startWrites = writes;
    let res: unknown;
    try {
      res = await api("api/me/ui-state");
    } catch {
      return; // CP unreachable: keep the cache, stay unloaded so nobody reads it as fresh
    }
    // Another owner took over, or the CP answered an error: keep what is shown, unloaded.
    if (owner !== key || !isDoc(res)) return;
    if (writes === startWrites) adopt(key, res, set);
    set({ loaded: true });
    const legacy = legacyOnboarding();
    if (!legacy) return;
    // The old keys go only once the CP holds an onboarding value: a failed upload keeps them
    // for the next load. if_absent keeps a late upload from another tab from undoing a value
    // stored in the meantime.
    if (get().state.onboarding && !pending.onboarding) clearLegacy();
    else if (await get().patch({ onboarding: legacy }, { ifAbsent: true })) clearLegacy();
  },
  patch(p, opts) {
    const key = cacheKey();
    if (key !== owner) {
      owner = key;
      pending = {};
      writes = 0;
    }
    pending = { ...pending, ...p };
    const shown = { ...get().state, ...p };
    set({ state: shown });
    writeCache(key, shown);
    const url = "api/me/ui-state" + (opts?.ifAbsent ? "?if_absent=1" : "");
    const send = async (): Promise<boolean> => {
      let res: unknown;
      try {
        res = await apiJSON(url, "PATCH", p);
      } catch {
        return false;
      }
      if (owner !== key || !isDoc(res)) return false;
      writes++;
      for (const k of Object.keys(p) as (keyof MemberUIState)[]) {
        if (pending[k] === p[k]) delete pending[k];
      }
      adopt(key, res, set);
      return true;
    };
    const done = queue.then(send, send);
    queue = done.catch(() => {});
    return done;
  },
}));

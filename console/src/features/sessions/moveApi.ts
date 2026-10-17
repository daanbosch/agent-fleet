// The Agent's move routes (ADR 0115), relayed by the CP as /api/sessions/{name}/move…; the
// shapes are move.ts's.
import { api, apiJSON } from "../../core/api/client.ts";
import type { Answer, MovePreview, MoveResult } from "./move.ts";

const base = (name: string) => `api/sessions/${encodeURIComponent(name)}/move`;

export const movePreview = (name: string): Promise<Answer<MovePreview>> => api(base(name) + "/preview");
export const moveSession = (name: string, kind: string, model: string, driver: string): Promise<Answer<MoveResult>> =>
  apiJSON(base(name), "POST", { kind, ...(model ? { model } : {}), ...(driver ? { driver } : {}) });
export const moveRetry = (name: string): Promise<Answer<MoveResult>> => apiJSON(base(name) + "/retry", "POST", {});
export const moveCancel = (name: string): Promise<Answer<{ cancelled: string; target: string }>> =>
  apiJSON(base(name) + "/cancel", "POST", {});
export const moveReopen = (name: string): Promise<Answer<{ reopened: string }>> => apiJSON(base(name) + "/reopen", "POST", {});
/** Resends the first prompt; called on the TARGET, not the source. */
export const moveDeliver = (target: string): Promise<Answer<{ target: string; initialPromptState: string; mayDuplicate?: boolean }>> =>
  apiJSON(base(target) + "/deliver", "POST", {});

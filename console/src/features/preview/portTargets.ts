// Which ways of opening a port are available, and which one a plain click takes. Pure, so the
// availability matrix (browsers unavailable × preview domain × listed / unlisted) is tested
// without rendering anything.
//
//   subdomain  a real origin in a new tab: absolute-path assets, cookies, OAuth callbacks, HMR
//   pane       inside the Console, next to the session
//   light      /preview/{port}/ in a new tab: plain HTTP pages, works everywhere
import { browserTarget } from "../browser/target.ts";
import { previewOwnOpenURL, previewURL } from "../../core/api/client.ts";
import type { PreviewInfo } from "./previewState.ts";

export type PortTarget = "subdomain" | "pane" | "light";
export type Unavailable = "" | "stopped" | "bad_port" | "no_domain" | "no_slug" | "no_browser";

export interface PortTargets {
  /** The port is something a preview can carry at all (1..65535, not the Agent's). */
  valid: boolean;
  /** On the preview list (visible to others when sharing / public mode is on). */
  listed: boolean;
  /** Per target: "" when available, else why not. */
  state: Record<PortTarget, Unavailable>;
  /** What a plain click does; null when nothing is available. */
  def: PortTarget | null;
  /** Who else can see this port. "only_you" = unlisted: the owner's own access. */
  badge: "" | "only_you" | "public" | "shared";
}

export function portTargets(a: {
  port: number;
  running: boolean;
  browserUnavailable: string;
  info: PreviewInfo | null;
}): PortTargets {
  const valid = browserTarget(a.port, "/") !== null;
  const info = a.info;
  const listed = !!info && info.ports.includes(a.port);
  const base: Unavailable = !a.running ? "stopped" : !valid ? "bad_port" : "";
  const issued = !!info && Object.keys(info.urls).length > 0;
  const state: Record<PortTarget, Unavailable> = {
    // A reason that will not go away by itself (no domain, no browser on this runtime) is
    // named before one that will (stopped, no port typed yet).
    subdomain: !info || !info.domain ? "no_domain" : base || (!issued ? "no_slug" : ""),
    pane: a.browserUnavailable ? "no_browser" : base,
    light: base,
  };
  const order: PortTarget[] = ["subdomain", "pane", "light"];
  const def = order.find((t) => state[t] === "") ?? null;
  let badge: PortTargets["badge"] = "";
  if (info && info.domain) {
    if (!listed) badge = "only_you";
    else if (info.public) badge = "public";
    else if (info.tenantShare) badge = "shared";
  }
  return { valid, listed, state, def, badge };
}

/** The URL a subdomain open goes to: the issued URL for a listed port, the owner-only entry
 *  (which makes the grant) for an unlisted one. */
export function subdomainOpenURL(info: PreviewInfo, port: number, path = "/"): string {
  const issued = info.urls[String(port)];
  if (info.ports.includes(port) && issued) {
    try {
      return new URL("." + new URL(path, "http://x/").pathname, issued).toString() + safeSearch(path);
    } catch {
      return issued;
    }
  }
  return previewOwnOpenURL(port, path);
}

function safeSearch(path: string): string {
  try {
    const u = new URL(path, "http://x/");
    return u.search + u.hash;
  } catch {
    return "";
  }
}

/** "Copy URL": the subdomain URL for a listed port; the lightweight URL for an unlisted one,
 *  because its subdomain URL only works for the owner after a grant and would look broken
 *  pasted elsewhere. The owner-only entry is never copied. */
export function copyableURL(info: PreviewInfo | null, port: number): string {
  if (info && info.ports.includes(port) && info.urls[String(port)]) return info.urls[String(port)];
  return previewURL(port, "/");
}

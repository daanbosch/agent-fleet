// The move offered at a usage limit (ADR 0115 P2b). Pinned: it shows for limited and spend_limit
// only, never for a moving or moved source, and only when the Agent's preview leaves a target
// that is not the source's own kind and not already at its cap; the action opens the move dialog
// flagged as coming from the limit.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";

let routes: Record<string, unknown> = {};
vi.mock("../../../core/api/client.ts", async (orig) => ({
  ...(await orig<Record<string, unknown>>()),
  api: async (path: string) => routes[`GET ${path}`] ?? { error: { code: "unrouted" } },
}));

const { MoveLimitBanner } = await import("./MoveLimitBanner.tsx");
const { useSessionUI } = await import("../../sessions/ui.ts");
const { setCachedConns } = await import("../../repos/connsCache.ts");
const { t } = await import("../../../lib/i18n/index.ts");
type Session = import("../../../types/session.ts").Session;
type MovePreview = import("../../sessions/move.ts").MovePreview;

const PREVIEW = "GET api/sessions/s1/move/preview";
let root: Root | null = null;
let host: HTMLDivElement;
const src = (over: Partial<Session> = {}): Session => ({ name: "s1", kind: "claude", alive: true, state: "limited", title: "作業", ...over }) as Session;
const preview = (over: Partial<MovePreview> = {}): MovePreview => ({
  source: "s1",
  kind: "claude",
  state: "limited",
  targets: [
    { kind: "claude", eligible: false, reason: "same_kind" },
    { kind: "codex", eligible: true },
    { kind: "opencode", eligible: true },
  ],
  ...over,
});
// A fresh tree per call: a mounted banner keeps the usage it read, as it does in the app.
const render = async (s: Session, state: string | undefined = s.state, running = true) => {
  act(() => root!.unmount());
  root = createRoot(host);
  await act(async () => root!.render(<MoveLimitBanner session={s} state={state} running={running} />));
  for (let i = 0; i < 6; i++) await act(async () => await Promise.resolve());
};
const banner = () => host.querySelector(".mirror-move-limit");

beforeEach(() => {
  routes = { [PREVIEW]: preview() };
  setCachedConns(null);
  useSessionUI.setState({ move: null, moveFromLimit: false });
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
});
afterEach(() => {
  act(() => root?.unmount());
  host.remove();
  root = null;
});

describe("MoveLimitBanner", () => {
  it.each(["limited", "spend_limit"])("is shown for %s and opens the dialog from the limit", async (state) => {
    await render(src({ state }), state);
    expect(banner()).not.toBeNull();
    await act(async () => host.querySelector<HTMLElement>(".ma-btn")!.click());
    expect(useSessionUI.getState().move?.name).toBe("s1");
    expect(useSessionUI.getState().moveFromLimit).toBe(true);
  });

  it("is not shown for a session that is not at a limit", async () => {
    await render(src({ state: "working" }));
    expect(banner()).toBeNull();
  });

  it("is not shown for a source that is moving or already moved", async () => {
    await render(src({ moving: "t9" }));
    expect(banner()).toBeNull();
    await render(src({ movedTo: "t9" }));
    expect(banner()).toBeNull();
  });

  it("is not shown while the Workspace is not running", async () => {
    await render(src(), "limited", false);
    expect(banner()).toBeNull();
  });

  it("is not shown when the preview leaves no eligible target, or refuses the source", async () => {
    routes[PREVIEW] = preview({ targets: [{ kind: "claude", eligible: false, reason: "same_kind" }] });
    await render(src());
    expect(banner()).toBeNull();
    routes[PREVIEW] = preview({ refusal: { code: "move_source_state", message: "x" } });
    await render(src());
    expect(banner()).toBeNull();
  });

  it("never offers the source's own kind, even if a preview listed it as eligible", async () => {
    routes[PREVIEW] = preview({ targets: [{ kind: "claude", eligible: true }] });
    await render(src());
    expect(banner()).toBeNull();
  });

  it("is not shown when every candidate is at its cap", async () => {
    routes["GET api/codex/usage"] = { ok: true, fiveHour: { pct: 100 } };
    routes[PREVIEW] = preview({ targets: [{ kind: "codex", eligible: true }] });
    await render(src());
    expect(banner()).toBeNull();
  });

  it("names the kinds with quota left, and still offers when no kind can be judged", async () => {
    routes["GET api/codex/usage"] = { ok: true, fiveHour: { pct: 20 } };
    await render(src());
    expect(banner()?.textContent).toContain("Codex");
    expect(banner()?.textContent).not.toContain("OpenCode");
    routes["GET api/codex/usage"] = { error: { code: "x" } };
    await render(src());
    expect(banner()?.textContent).toContain(t("mirror.move_limit"));
  });
});

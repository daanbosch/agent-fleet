// Launching from a handoff another member offered (issue #2020, ADR 0057 decision 6).
//
// The offer is taken before the session is created, so an offer the sender withdrew while the
// recipient's launch dialog was open is never launched, and the recipient is told why. A launch
// that fails gives the take back. These replace run-sheet step H-14 on #1960.
//
// StartHost and useStartWork are the real ones; the dialog is a stub that presses launch, and
// the API is a fake that answers per route.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import type { LaunchOpts } from "./LaunchModal.tsx";

const apiJSON = vi.fn();
vi.mock("../../core/api/client.ts", async (orig) => {
  const real = (await orig()) as Record<string, unknown>;
  return { ...real, apiJSON: (...a: unknown[]) => apiJSON(...a), api: vi.fn(async () => ({ offers: [] })), pasteImage: vi.fn() };
});
vi.mock("../sessions/open.ts", () => ({ openSessionChat: vi.fn(), openSessionTerminal: vi.fn() }));
vi.mock("./useRepoRail.ts", () => ({ useRepoRailContext: () => ({ launchKinds: ["claude"], connsSettling: false }) }));
vi.mock("./StartModal.tsx", () => ({ StartModal: () => null }));
vi.mock("../mirror/HandoffProposal.tsx", () => ({ markHandoffLaunched: vi.fn() }));
vi.mock("./LaunchModal.tsx", () => ({
  LaunchModal: (p: { onLaunch: (o: LaunchOpts) => Promise<{ ok: boolean }> }) => (
    <button
      type="button"
      data-testid="launch"
      onClick={() =>
        void p
          .onLaunch({
            kind: "claude", driver: "", model: "", effort: "", startMode: "normal", prompt: "carry on",
            title: "Carry on", images: [], subdir: "", base: "feature/x", newBranch: "take/x", worktree: true,
          } as LaunchOpts)
          .catch(() => undefined) // a thrown launch is the dialog's to show; here only the offer matters
      }
    >
      launch
    </button>
  ),
}));

const { StartHost } = await import("./StartHost.tsx");
const { useLaunchSeed, useLaunchTarget } = await import("./store.ts");
const { ToastProvider } = await import("../../ui/ToastProvider.tsx");
const { t } = await import("../../lib/i18n/index.ts");

let root: Root | null = null;
let host: HTMLDivElement;

/** Answers apiJSON per path suffix; anything unlisted fails the test. */
function routes(answers: Record<string, unknown>) {
  apiJSON.mockImplementation(async (path: string) => {
    for (const [suffix, answer] of Object.entries(answers)) {
      if (path.endsWith(suffix)) return typeof answer === "function" ? (answer as () => unknown)() : answer;
    }
    throw new Error("unexpected call " + path);
  });
}

const calls = (suffix: string) => apiJSON.mock.calls.filter((c) => String(c[0]).endsWith(suffix));

async function pressLaunch() {
  await act(async () => {
    host.querySelector<HTMLButtonElement>("[data-testid=launch]")!.click();
  });
  // Let the take → create → accept/release chain settle.
  for (let i = 0; i < 5; i++) await act(async () => await Promise.resolve());
}

beforeEach(async () => {
  apiJSON.mockReset();
  useLaunchSeed.getState().set("carry on", "Carry on", "", "", "offer-1");
  useLaunchTarget.getState().open({ name: "private", path: "/home/dev/repos/private", branch: "feature/x" } as never);
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  await act(async () => {
    root!.render(
      <ToastProvider>
        <StartHost />
      </ToastProvider>,
    );
  });
});

afterEach(async () => {
  await act(async () => root?.unmount());
  host.remove();
  root = null;
  useLaunchSeed.getState().clear();
  useLaunchTarget.getState().clear();
});

describe("launching from a received handoff", () => {
  it("refuses the launch of an offer withdrawn while the dialog was open, creates nothing, and says why", async () => {
    routes({
      "/offer-1/take": { error: { code: "handoff_already_decided", message: "the handoff was already decided", status: "withdrawn" } },
    });
    await pressLaunch();
    expect(calls("api/sessions")).toHaveLength(0);
    expect(calls("/accept")).toHaveLength(0);
    expect(document.body.textContent).toContain(t("handoff.take_withdrawn"));
    // The dialog stays up (no launch happened), so the user can read why and close it.
    expect(host.querySelector("[data-testid=launch]")).not.toBeNull();
  });

  it("takes the offer before the session is created and accepts it with the take's token", async () => {
    routes({
      "/offer-1/take": { status: "pending", token: "tok-1" },
      "api/sessions": { name: "s-taken" },
      "/offer-1/accept": { status: "accepted" },
    });
    await pressLaunch();
    const order = apiJSON.mock.calls.map((c) => String(c[0]));
    expect(order.indexOf("api/session-handoff-offers/offer-1/take")).toBeLessThan(order.indexOf("api/sessions"));
    expect(calls("/offer-1/accept")[0][2]).toEqual({ sessionName: "s-taken", token: "tok-1" });
    expect(calls("/release")).toHaveLength(0);
  });

  it("gives the offer back when the launch fails, and does not accept it", async () => {
    routes({
      "/offer-1/take": { status: "pending", token: "tok-2" },
      "api/sessions": { error: { code: "worktree_failed", message: "worktree add: invalid reference" } },
      "/offer-1/release": { status: "pending" },
    });
    await pressLaunch();
    expect(calls("/offer-1/release")[0][2]).toEqual({ token: "tok-2" });
    expect(calls("/accept")).toHaveLength(0);
  });

  it("gives the offer back when the launch request itself throws", async () => {
    routes({
      "/offer-1/take": { status: "pending", token: "tok-3" },
      "api/sessions": () => Promise.reject(new Error("network down")),
      "/offer-1/release": { status: "pending" },
    });
    await pressLaunch();
    expect(calls("/offer-1/release")[0][2]).toEqual({ token: "tok-3" });
    expect(calls("/accept")).toHaveLength(0);
  });

  it("tells the user when the session started but the acceptance could not be recorded", async () => {
    routes({
      "/offer-1/take": { status: "pending", token: "tok-4" },
      "api/sessions": { name: "s-late" },
      "/offer-1/accept": { error: { code: "handoff_not_taken", message: "x", status: "expired" } },
    });
    await pressLaunch();
    expect(document.body.textContent).toContain(t("handoff.accept_unrecorded", { err: "expired" }));
  });

  it("does not take anything for a launch that is not from an offer", async () => {
    await act(async () => useLaunchSeed.getState().clear());
    routes({ "api/sessions": { name: "s-plain" } });
    await pressLaunch();
    expect(calls("/take")).toHaveLength(0);
    expect(calls("api/sessions")).toHaveLength(1);
  });
});

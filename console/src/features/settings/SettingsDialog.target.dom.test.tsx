// On a phone the section content is display:none until the layout is `entered`, and a scroll to
// an element inside it does nothing. A request carrying a scroll target must therefore open
// straight into its section, while a plain open keeps starting at the list.
import { describe, it, expect, afterEach, vi } from "vitest";
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";

vi.mock("../../core/api/client.ts", async (importActual) => ({
  ...(await importActual<typeof import("../../core/api/client.ts")>()),
  api: () => Promise.resolve({}),
  apiJSON: () => Promise.resolve({}),
}));
vi.mock("../../ui/ToastProvider.tsx", () => ({ useToast: () => () => {} }));
vi.mock("./agents/AgentsTab.tsx", () => ({ AgentsTab: () => <div data-testid="agents-tab" /> }));

const { SettingsDialog } = await import("./SettingsDialog.tsx");
const { useSettingsUI } = await import("./store.ts");

let root: Root | null = null;
let host: HTMLDivElement | null = null;

async function open(target?: string) {
  act(() => useSettingsUI.getState().openSettings("agents", target ? { target } : undefined));
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  await act(async () => root!.render(<SettingsDialog />));
}
const layout = () => document.querySelector(".settings-layout");

afterEach(() => {
  act(() => root?.unmount());
  act(() => useSettingsUI.getState().closeSettings());
  host?.remove();
  root = host = null;
});

describe("SettingsDialog target open", () => {
  it("enters the agents detail view directly when a target is requested", async () => {
    await open("claude");
    expect(layout()!.classList.contains("entered")).toBe(true);
    expect(document.querySelector("[data-testid=agents-tab]")).not.toBeNull();
  });

  it("a plain open still starts at the list", async () => {
    await open();
    expect(layout()!.classList.contains("entered")).toBe(false);
  });
});

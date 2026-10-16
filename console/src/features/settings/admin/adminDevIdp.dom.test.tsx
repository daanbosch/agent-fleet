// The development OIDC IdP switch (control-plane/dev_idp_admin.go). The server's answer drives
// everything on screen, so each state is a fixture: served, off, locked by the environment,
// and no PUBLIC_BASE_URL.
import { describe, it, expect, afterEach, beforeEach, vi } from "vitest";
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";

const api = vi.fn();
const apiJSON = vi.fn();
vi.mock("../../../core/api/client.ts", async (importActual) => ({
  ...(await importActual<typeof import("../../../core/api/client.ts")>()),
  api: (...args: unknown[]) => api(...args),
  apiJSON: (...args: unknown[]) => apiJSON(...args),
}));

import { DevIdpAdminView } from "./adminDevIdp.tsx";

let root: Root | null = null;
let host: HTMLDivElement | null = null;

const status = (over: Record<string, unknown> = {}) => ({
  enabled: true,
  setting: true,
  locked: false,
  missing_base_url: false,
  issuer: "https://af.example.com/dev-idp",
  opted_in: 3,
  ...over,
});

async function mount() {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  await act(async () => {
    root!.render(<DevIdpAdminView />);
  });
  await act(async () => {
    await Promise.resolve();
  });
}

const box = () => host!.querySelector<HTMLInputElement>("#devidp-enabled")!;
const text = () => host!.textContent ?? "";

beforeEach(() => {
  api.mockReset();
  apiJSON.mockReset();
});

afterEach(() => {
  act(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
});

describe("DevIdpAdminView", () => {
  it("shows it on, with the issuer and the opted-in count", async () => {
    api.mockResolvedValue(status());
    await mount();
    expect(box().checked).toBe(true);
    expect(box().disabled).toBe(false);
    expect(text()).toContain("https://af.example.com/dev-idp");
    expect(text()).toContain("3");
    expect(host!.querySelector(".form-err")).not.toBeNull(); // the "never trust in production" line
  });

  it("PUTs the opposite of the box and draws the answer", async () => {
    api.mockResolvedValue(status());
    apiJSON.mockResolvedValue(status({ enabled: false, setting: false }));
    await mount();
    await act(async () => {
      box().click();
    });
    expect(apiJSON).toHaveBeenCalledWith("api/admin/dev-idp", "PUT", { enabled: false });
    expect(box().checked).toBe(false);
    expect(text()).not.toContain("https://af.example.com/dev-idp");
  });

  it("is locked, and says why, when AF_DEV_IDP=0", async () => {
    api.mockResolvedValue(status({ enabled: false, locked: true }));
    await mount();
    expect(box().disabled).toBe(true);
    expect(box().checked).toBe(false);
    expect(text()).toContain("AF_DEV_IDP=0");
  });

  it("cannot be switched on without PUBLIC_BASE_URL, and says why", async () => {
    api.mockResolvedValue(status({ enabled: false, missing_base_url: true, issuer: "" }));
    await mount();
    expect(box().disabled).toBe(true);
    expect(text()).toContain("PUBLIC_BASE_URL");
  });

  it("shows a refusal from the server", async () => {
    api.mockResolvedValue(status());
    apiJSON.mockResolvedValue({ error: { code: "forbidden", message: "super_admin required" } });
    await mount();
    await act(async () => {
      box().click();
    });
    expect(text()).toContain("super_admin required");
    expect(box().checked).toBe(true);
  });
});

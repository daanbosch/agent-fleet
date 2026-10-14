import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";

const fake = vi.hoisted(() => ({
  controller: {
    target: { port: 5173, path: "/app" },
    snapshot: {
      state: "ready",
      url: "http://127.0.0.1:5173/app",
      title: "Fake App",
      width: 900,
      height: 600,
      canBack: true,
      canForward: false,
      errorCode: "",
      errorMessage: "",
      console: [],
    },
    sendInput: vi.fn(),
    changeTarget: vi.fn(),
    adoptTarget: vi.fn(),
    subscribe: vi.fn(() => () => {}),
    mount: vi.fn(),
    unmount: vi.fn(),
    setVisible: vi.fn(),
    setViewport: vi.fn(),
    history: vi.fn(),
    reload: vi.fn(),
    reconnect: vi.fn(),
  },
}));

vi.mock("./service.ts", () => ({ ensureBrowser: () => fake.controller }));
vi.mock("../../layout/store.ts", () => ({
  useLayoutStore: (selector: (state: { setPaneTarget: () => void }) => unknown) => selector({ setPaneTarget: () => {} }),
}));
vi.mock("../../lib/i18n/index.ts", () => ({ useT: () => (key: string) => key }));

import { BrowserPane } from "./BrowserPane.tsx";

describe("BrowserPane markup", () => {
  it("renders the toolbar, canvas, and transparent IME input against a fake controller", () => {
    const html = renderToStaticMarkup(<BrowserPane paneId="p3" port={5173} path="/app" />);
    expect(html).toContain("127.0.0.1:");
    expect(html).toContain("browser-port");
    expect(html).toContain("browser-path");
    expect(html).toContain("browser-canvas");
    expect(html).toContain("browser-ime");
    expect(html).not.toContain("browserId");
  });

  it("says why an unreachable port failed, and keeps the generic text otherwise", () => {
    const render = (reason?: string) => {
      Object.assign(fake.controller.snapshot, { state: "target-unreachable", reason });
      return renderToStaticMarkup(<BrowserPane paneId="p3" port={5173} path="/app" />);
    };
    expect(render("preview_not_http")).toContain("browser.target_not_http");
    expect(render("preview_tls")).toContain("browser.target_tls");
    expect(render("")).toContain("browser.target_unreachable");
    expect(render(undefined)).toContain("browser.target_unreachable");
    expect(render("preview_unreachable")).toContain("browser.target_unreachable");
  });
});

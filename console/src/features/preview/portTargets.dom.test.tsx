// The availability matrix behind PortOpen: browsers unavailable × preview domain × a URL issued
// for this start × listed / unlisted. The default a plain click takes is subdomain > pane >
// lightweight, and a target that cannot work says why instead of being offered.
import { describe, expect, it } from "vitest";
import { copyableURL, portTargets, subdomainOpenURL } from "./portTargets.ts";
import type { PreviewInfo } from "./previewState.ts";

const info = (over: Partial<PreviewInfo> = {}): PreviewInfo => ({
  domain: "pv.example.com",
  urls: { "3000": "https://abc-3000.pv.example.com", "8080": "https://abc-8080.pv.example.com" },
  ports: [3000, 8080],
  tempPorts: [],
  public: false,
  tenantShare: false,
  maxPorts: 8,
  ...over,
});
const base = { running: true, browserUnavailable: "" };

describe("portTargets default target", () => {
  it("is the subdomain when a domain is set and a URL is issued, listed or not", () => {
    expect(portTargets({ ...base, port: 3000, info: info() }).def).toBe("subdomain");
    expect(portTargets({ ...base, port: 5173, info: info() }).def).toBe("subdomain");
  });
  it("falls to the pane when there is no domain", () => {
    const t = portTargets({ ...base, port: 5173, info: info({ domain: "", urls: {} }) });
    expect(t.def).toBe("pane");
    expect(t.state.subdomain).toBe("no_domain");
    expect(t.badge).toBe(""); // no preview domain: nothing to say about who sees it
  });
  it("does not default to a subdomain that has no slug for this start", () => {
    const t = portTargets({ ...base, port: 5173, info: info({ urls: {} }) });
    expect(t.state.subdomain).toBe("no_slug");
    expect(t.def).toBe("pane");
  });
  it("falls to the lightweight preview when the runtime has no browser", () => {
    const t = portTargets({ port: 5173, running: true, browserUnavailable: "kubernetes", info: info({ domain: "", urls: {} }) });
    expect(t.def).toBe("light");
    expect(t.state.pane).toBe("no_browser");
    expect(portTargets({ port: 5173, running: true, browserUnavailable: "kubernetes", info: info() }).def).toBe("subdomain");
  });
  it("is unknown-info safe: before the state arrives, pane (or light) like before", () => {
    expect(portTargets({ ...base, port: 5173, info: null }).def).toBe("pane");
    expect(portTargets({ port: 5173, running: true, browserUnavailable: "k", info: null }).def).toBe("light");
  });
  it("offers nothing while stopped or for the Agent's own port", () => {
    expect(portTargets({ port: 5173, running: false, browserUnavailable: "", info: info() }).def).toBeNull();
    const agent = portTargets({ ...base, port: 7700, info: info() });
    expect(agent.def).toBeNull();
    expect(agent.state.light).toBe("bad_port");
  });
});

describe("portTargets badge", () => {
  it("marks an unlisted port as only-you, and a listed one by its exposure", () => {
    expect(portTargets({ ...base, port: 5173, info: info() }).badge).toBe("only_you");
    expect(portTargets({ ...base, port: 3000, info: info() }).badge).toBe("");
    expect(portTargets({ ...base, port: 3000, info: info({ tenantShare: true }) }).badge).toBe("shared");
    expect(portTargets({ ...base, port: 3000, info: info({ tenantShare: true, public: true }) }).badge).toBe("public");
  });
});

describe("urls", () => {
  it("opens a listed port at its issued URL and an unlisted one through the owner-only entry", () => {
    expect(subdomainOpenURL(info(), 3000, "/a/b?x=1")).toBe("https://abc-3000.pv.example.com/a/b?x=1");
    const own = new URL(subdomainOpenURL(info(), 5173, "/a"));
    expect(own.pathname).toMatch(/preview-open-own$/);
    expect(own.searchParams.get("port")).toBe("5173");
    expect(own.searchParams.get("next")).toBe("/a");
  });
  it("copies the subdomain URL for a listed port and the lightweight one for an unlisted port", () => {
    expect(copyableURL(info(), 3000)).toBe("https://abc-3000.pv.example.com");
    expect(copyableURL(info(), 5173)).toMatch(/\/preview\/5173\/$/);
    expect(copyableURL(info(), 5173)).not.toContain("preview-open-own");
  });
});

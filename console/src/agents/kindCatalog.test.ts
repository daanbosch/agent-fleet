import { readFileSync, existsSync } from "node:fs";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const apiMock = vi.fn();
vi.mock("../core/api/client.ts", () => ({ api: (...a: unknown[]) => apiMock(...a) }));

const { AGENTS, agentOf, availableKinds, repoLaunchKinds } = await import("./registry.ts");
const { FALLBACK_KIND_FACTS, catalogKinds, kindFacts, parseKindCatalog, setKindCatalog } = await import("./kindCatalog.ts");
const { loadKindCatalog, resetKindCatalog } = await import("./kindCatalogLoad.ts");
const { MCP_KINDS, mcpKinds } = await import("../features/settings/mcp/mcpWire.ts");
const { SESSION_KINDS } = await import("../types/session.ts");

// The build-time snapshot of GET /agents/kinds, embedded by the Control Plane and held equal to the
// Agent's registry by TestKindCatalogSnapshot. A checkout without it skips rather than fails, like
// the Go guard this one replaces for the Console side.
const SNAPSHOT = new URL("../../../control-plane/internal/agentkinds/catalog.json", import.meta.url);
const haveSnapshot = existsSync(SNAPSHOT);
const snapshot: { kinds: { kind: string; managedOnly: boolean; managedDefault: boolean; mcpClient: string }[] } = haveSnapshot
  ? JSON.parse(readFileSync(SNAPSHOT, "utf8"))
  : { kinds: [] };

describe.skipIf(!haveSnapshot)("static kind lists against the catalog snapshot", () => {
  it("SESSION_KINDS names exactly the kinds the Agent registers", () => {
    expect([...SESSION_KINDS].sort()).toEqual(snapshot.kinds.map((k) => k.kind).sort());
  });

  it("FALLBACK_KIND_FACTS equals the snapshot for every kind", () => {
    for (const e of snapshot.kinds) {
      expect(FALLBACK_KIND_FACTS[e.kind as keyof typeof FALLBACK_KIND_FACTS], e.kind).toEqual({
        managedOnly: e.managedOnly,
        managedDefault: e.managedDefault,
        mcpClient: e.mcpClient,
      });
    }
  });

  it("MCP_KINDS lists exactly the kinds whose mcpClient is yes", () => {
    expect([...MCP_KINDS].sort()).toEqual(
      snapshot.kinds
        .filter((k) => k.mcpClient === "yes")
        .map((k) => k.kind)
        .sort(),
    );
  });
});

beforeEach(() => {
  apiMock.mockReset();
  resetKindCatalog();
});
afterEach(() => {
  resetKindCatalog();
  vi.useRealTimers();
});

const body = (kinds: unknown[]) => ({ kinds });

describe("descriptors without a catalog (fallback)", () => {
  it("answer the driver facts the hand-kept fields used to", () => {
    const managed = SESSION_KINDS.filter((k) => AGENTS[k].managedDriver);
    expect(managed).toEqual(["codex", "cursor", "copilot", "kiro", "opencode", "lcpp", "muse"]);
    expect(SESSION_KINDS.filter((k) => AGENTS[k].terminalDriver === false)).toEqual(["lcpp", "muse"]);
    expect(AGENTS.claude.terminalDriver).toBe(true);
  });

  it("keep the property enumerable, so a spread copies the answer", () => {
    expect({ ...AGENTS.codex }.managedDriver).toBe(true);
  });
});

describe("a loaded catalog", () => {
  it("overrides the fallback per kind, and falls back for a kind it omits", () => {
    setKindCatalog(parseKindCatalog(body([{ kind: "claude", managedOnly: false, managedDefault: true, mcpClient: "yes" }])));
    expect(AGENTS.claude.managedDriver).toBe(true);
    expect(AGENTS.codex.managedDriver).toBe(true); // omitted: the fallback answers
    expect(AGENTS.lcpp.terminalDriver).toBe(false);
  });

  it("treats managedOnly as managed-default even if the entry forgets the flag", () => {
    setKindCatalog(parseKindCatalog(body([{ kind: "claude", managedOnly: true, managedDefault: false, mcpClient: "yes" }])));
    expect(AGENTS.claude.managedDriver).toBe(true);
    expect(AGENTS.claude.terminalDriver).toBe(false);
  });

  it("drops malformed entries and treats an empty answer as no catalog", () => {
    expect(parseKindCatalog(null)).toBeNull();
    expect(parseKindCatalog({})).toBeNull();
    expect(parseKindCatalog(body([]))).toBeNull();
    expect(parseKindCatalog(body([null, { kind: 3 }, { kind: "" }]))).toBeNull();
    expect(parseKindCatalog(body([{ kind: "x" }, null]))?.get("x")).toEqual({ managedOnly: false, managedDefault: false, mcpClient: "unverified" });
  });
});

describe("a kind the Console has no presentation for", () => {
  const entry = { kind: "newcli", managedOnly: false, managedDefault: true, mcpClient: "yes" };

  it("renders generically, claims no capability, and is not launchable", () => {
    setKindCatalog(parseKindCatalog(body([entry])));
    const a = agentOf("newcli");
    expect(a.label).toBe("newcli");
    expect(a.icon).toBe("symbol-misc");
    expect(a.managedDriver).toBe(true);
    const { chat, headlessChat, scheduledRuns, launchableFromRepo, planMode, forkAt } = a.caps;
    expect([chat, headlessChat, scheduledRuns, launchableFromRepo, planMode, forkAt]).toEqual([false, false, false, false, false, false]);
    expect(a.available({})).toBe(false);
    expect(agentOf("newcli")).toBe(a); // stable identity between renders
    expect(repoLaunchKinds).not.toContain("newcli");
    expect(Object.keys(availableKinds({}))).not.toContain("newcli");
  });

  it("is claude before a catalog names it, exactly as an unknown kind was", () => {
    expect(agentOf("newcli")).toBe(AGENTS.claude);
  });

  it("is dropped again with the catalog", () => {
    setKindCatalog(parseKindCatalog(body([entry])));
    setKindCatalog(null);
    expect(agentOf("newcli")).toBe(AGENTS.claude);
  });
});

describe("alternate spellings behave as before", () => {
  const spellings = ["Claude", " claude", "claude ", "claude-code", "kiro-cli", "cursor-agent", "llamacpp", "antigravity", "CODEX", ""];

  it.each(spellings)("%j resolves to claude and has no facts", (s) => {
    for (const cat of [false, true]) {
      if (cat) setKindCatalog(parseKindCatalog(body(snapshot.kinds.length ? snapshot.kinds : [{ kind: "claude" }])));
      expect(agentOf(s)).toBe(AGENTS.claude);
      expect(kindFacts(s)).toBeUndefined();
      expect(mcpKinds()).not.toContain(s);
    }
  });
});

describe("mcpKinds", () => {
  it("is MCP_KINDS in order without a catalog", () => {
    expect(mcpKinds()).toEqual([...MCP_KINDS]);
  });

  it("follows the catalog: no and unverified drop out, an unknown yes kind is appended", () => {
    setKindCatalog(
      parseKindCatalog(
        body([
          { kind: "codex", mcpClient: "no" },
          { kind: "kiro", mcpClient: "unverified" },
          { kind: "zeta", mcpClient: "yes" },
          { kind: "alpha", mcpClient: "yes" },
        ]),
      ),
    );
    const got = mcpKinds();
    expect(got).not.toContain("codex");
    expect(got).not.toContain("kiro");
    expect(got.slice(-2)).toEqual(["zeta", "alpha"]);
    expect(got.slice(0, 3)).toEqual(["claude", "opencode", "cursor"]);
    expect(catalogKinds()).toEqual(["codex", "kiro", "zeta", "alpha"]);
  });
});

describe("loadKindCatalog", () => {
  it("installs a good answer once and does not ask again", async () => {
    apiMock.mockResolvedValue(body([{ kind: "claude", managedOnly: false, managedDefault: true, mcpClient: "yes" }]));
    await loadKindCatalog();
    await loadKindCatalog();
    expect(apiMock).toHaveBeenCalledTimes(1);
    expect(apiMock).toHaveBeenCalledWith("/api/agents/kinds");
    expect(AGENTS.claude.managedDriver).toBe(true);
  });

  it("keeps the fallback on a failure and retries later with a growing delay", async () => {
    vi.useFakeTimers();
    apiMock.mockRejectedValueOnce(new Error("502")).mockResolvedValueOnce({ nope: 1 }).mockResolvedValue(
      body([{ kind: "claude", managedOnly: false, managedDefault: true, mcpClient: "yes" }]),
    );
    await loadKindCatalog();
    expect(AGENTS.claude.managedDriver).toBe(false);
    expect(AGENTS.codex.managedDriver).toBe(true);
    await vi.advanceTimersByTimeAsync(2_999);
    expect(apiMock).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(2); // 3 s: the unusable answer
    expect(apiMock).toHaveBeenCalledTimes(2);
    expect(AGENTS.claude.managedDriver).toBe(false);
    await vi.advanceTimersByTimeAsync(9_990);
    expect(apiMock).toHaveBeenCalledTimes(2);
    await vi.advanceTimersByTimeAsync(20); // +10 s
    expect(apiMock).toHaveBeenCalledTimes(3);
    expect(AGENTS.claude.managedDriver).toBe(true);
    await vi.advanceTimersByTimeAsync(120_000);
    expect(apiMock).toHaveBeenCalledTimes(3); // success stops the retries
  });

  it("drops an answer that was in flight when the tenant changed", async () => {
    let resolve!: (v: unknown) => void;
    apiMock.mockReturnValueOnce(new Promise((r) => (resolve = r)));
    const p = loadKindCatalog();
    resetKindCatalog();
    resolve(body([{ kind: "claude", managedOnly: false, managedDefault: true, mcpClient: "yes" }]));
    await p;
    expect(AGENTS.claude.managedDriver).toBe(false);
  });

  it("reset forgets the catalog and cancels the retry", async () => {
    vi.useFakeTimers();
    apiMock.mockRejectedValue(new Error("down"));
    await loadKindCatalog();
    resetKindCatalog();
    await vi.advanceTimersByTimeAsync(120_000);
    expect(apiMock).toHaveBeenCalledTimes(1);
  });
});

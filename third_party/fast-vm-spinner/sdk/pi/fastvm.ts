// fastvm.ts — pi extension registering the fastvm sandbox MCP server.
//
// Gives pi agents fastvm warm-clone microVM lifecycle tools (launch, exec,
// pause, resume, destroy, list, get) served by ~/.local/bin/fastvm-mcp
// (built from team-telnyx/fast-vm-spinner feat/agent-substrate,
// cmd/fastvm-mcp; itself a thin wrapper over the official Go SDK).
//
// Exposure:
//   - default "codemode": tools are callable (codemode scripts,
//     ctx.executeTool, tool pages) but not auto-declared to every model turn.
//   - FASTVM_PI_TOOLS=direct declares them to the model in every session.
//
// Env:
//   FASTVM_API          fastvm REST API (default the atl1 lab node)
//   FASTVM_DEFAULT_USER VM-owning user for launches (default dav)
//   FASTVM_MCP_BIN      server binary path
import os from "node:os";
import fs from "node:fs";

export default function (pi: ExtensionAPI) {
  const bin = process.env.FASTVM_MCP_BIN ?? `${os.homedir()}/.local/bin/fastvm-mcp`;
  if (!fs.existsSync(bin)) {
    // Binary not built yet: register nothing rather than fail the whole
    // extension runtime. Build with:
    //   go build -o ~/.local/bin/fastvm-mcp ./cmd/fastvm-mcp  (in team-telnyx/fast-vm-spinner)
    console.error(`[fastvm] MCP binary missing at ${bin}; skipping registration`);
    return;
  }
  pi.registerMcpServer("fastvm", {
    command: bin,
    args: [],
    env: {
      FASTVM_API: process.env.FASTVM_API ?? "http://10.242.128.198:8090",
      FASTVM_DEFAULT_USER: process.env.FASTVM_DEFAULT_USER ?? "dav",
    },
    exposure: (process.env.FASTVM_PI_TOOLS as "direct" | "codemode" | "deferred") ?? "codemode",
    description:
      "fastvm sandbox: warm-clone Firecracker microVMs for running untrusted/guest code — launch (sub-second restore), exec, pause, resume, destroy, list. VMs are name-scoped agpoc-* for leak-guard cleanup.",
  });
}

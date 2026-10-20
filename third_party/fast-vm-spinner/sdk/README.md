# fastvm SDKs, MCP server, pi plugin

The agent-facing surface of fastvm, as of feat/agent-substrate commit 71228c6.

```
pi / any MCP client ──stdio──▶ cmd/fastvm-mcp ──uses──▶ sdk/fastvm (Go) ──REST──▶ fastvm API
sdk/python/fastvm.py ────────────────────────────REST──▶ fastvm API
```

| Piece | What |
|---|---|
| `sdk/fastvm` (Go) | Official stdlib-only REST client. Forward-compat fields (warm-launch TTL, per-VM egress, fork) ride in request bodies; older daemons ignore them silently. `NamePrefix = "agpoc-"` scopes every SDK-created VM for the leak-guard sweeper. |
| `sdk/python/fastvm.py` | Matching Python SDK (stdlib only). `python3 sdk/python/fastvm.py` = built-in smoke. |
| `cmd/fastvm-mcp` | stdio MCP server, no deps (hand-rolled JSON-RPC 2.0): launch / exec / pause / resume / destroy / list / get. Honest MCP annotations (`destroy` = destructive, `get`/`list` = readOnly) and a destroy-all guard. |
| `sdk/pi/fastvm.ts` | pi extension registering the MCP server (installed copy: `~/.pi/agent/extensions/fastvm.ts`). Default exposure codemode; `FASTVM_PI_TOOLS=direct` to declare to the model. |
| `scripts/smoke_mcp.py` | Deterministic, no-LLM conformance + live lifecycle smoke. |

## Build & run

```bash
go build -o ~/.local/bin/fastvm-mcp ./cmd/fastvm-mcp
FASTVM_API=http://10.242.128.198:8090 python3 scripts/smoke_mcp.py   # MCP-SMOKE-OK
python3 sdk/python/fastvm.py                                        # PYSDK-SMOKE-OK
FASTVM_PI_TOOLS=direct pi --extension sdk/pi/fastvm.ts -p '<lifecycle prompt>'  # end-to-end via a model
```

## Proven (2026-10-18, live atl1-pc1-003)

- `MCP-SMOKE-OK` and `PYSDK-SMOKE-OK`: launch → exec (guest identity `ubuntu-fc-uvm`) → pause (exec correctly refused) → resume (guest state persisted) → destroy → list clean.
- End-to-end pi plugin test: a fresh headless pi session with the extension loaded drove all lifecycle steps itself through `mcp__fastvm__*` tools; final node inventory showed zero leaks. Note: pi reports `exposure` passed via `registerMcpServer` as "ignored settings", and the tools were model-visible regardless — the `FASTVM_PI_TOOLS` toggle is a forward-compat nicety, not a security control.

## Forward-compat gaps (server work, follow-up commits)

The SDK/MCP layers send these today; the feat/agent-substrate server line implements them:
- `ttl_minutes` on warm-launch (server-side TTL sweeper for warm clones)
- `egress` per-VM allowlist (enforced per-tap)
- `fork` (live E2B-style fork: pause→dump→reflink→resume→warm-launch N clones)

#!/usr/bin/env python3
"""Deterministic fastvm-mcp conformance + lifecycle smoke (no LLM).

Drives the MCP server over stdio JSON-RPC: initialize, tools/list, then a
full guest lifecycle against the live node — launch -> exec (hostname check)
-> pause -> exec-refused -> resume -> destroy -> list-clean.

Run:  FASTVM_API=http://10.242.128.198:8090 python3 scripts/smoke_mcp.py [path-to-fastvm-mcp]
Prints MCP-SMOKE-OK on success. Creates exactly one VM (agpoc- scope) and
destroys it.
"""
import json
import os
import subprocess
import sys
import time
import uuid

BIN = sys.argv[1] if len(sys.argv) > 1 else os.path.join(os.path.dirname(__file__), "..", "bin", "fastvm-mcp")


class McpSession:
    def __init__(self, env=None):
        e = dict(os.environ)
        e.update(env or {})
        self.p = subprocess.Popen([BIN], stdin=subprocess.PIPE, stdout=subprocess.PIPE, env=e)

    def send(self, obj):
        self.p.stdin.write((json.dumps(obj) + "\n").encode())
        self.p.stdin.flush()

    def recv(self):
        line = self.p.stdout.readline()
        if not line:
            raise RuntimeError("MCP server closed stdout")
        return json.loads(line)

    def rpc(self, id, method, params=None):
        self.send({"jsonrpc": "2.0", "id": id, "method": method, "params": params or {}})
        return self.recv()

    def notify(self, method, params=None):
        self.send({"jsonrpc": "2.0", "method": method, "params": params or {}})

    def call(self, name, args):
        return self.rpc(id=f"call-{name}-{uuid.uuid4().hex[:6]}", method="tools/call",
                        params={"name": name, "arguments": args})

    def close(self):
        self.p.stdin.close()
        self.p.wait(timeout=10)


def text_of(result):
    return "".join(c.get("text", "") for c in result["content"])


def main():
    ok = True
    try:
        s = McpSession()
        # 1. initialize
        init = s.rpc(id=1, method="initialize",
                     params={"protocolVersion": "2024-11-05", "capabilities": {}})
        assert init["result"]["serverInfo"]["name"] == "fastvm", init
        s.notify("initialized")

        # 2. tools/list
        tools = s.rpc(id=2, method="tools/list")
        names = [t["name"] for t in tools["result"]["tools"]]
        expected = ["launch", "exec", "pause", "resume", "destroy", "list", "get"]
        assert names == expected, names
        # tempo: every tool declares a schema
        for t in tools["result"]["tools"]:
            assert t["inputSchema"]["type"] == "object", t
            assert t["annotations"] is not None, t

        # 3. launch
        name = f"agpoc-mcp-{int(time.time() * 1000) % 1000000}"
        r = s.call("launch", {"name": name})
        assert not r["result"].get("isError"), r
        launched = text_of(r["result"])
        print(launched.splitlines()[0])
        assert name in launched, launched

        # 4. exec inside: guest identity proof
        r = s.call("exec", {"vm": name, "command": "hostname && cat /proc/uptime"})
        assert not r["result"].get("isError"), r
        out = text_of(r["result"])
        print(out.splitlines()[1])
        assert "ubuntu-fc-uvm" in out, out

        # 5. pause -> exec must be refused
        r = s.call("pause", {"vm": name})
        assert not r["result"].get("isError"), r
        r = s.call("exec", {"vm": name, "command": "true"})
        assert r["result"].get("isError"), "exec during pause must fail"
        assert "paused" in text_of(r["result"]).lower(), r
        print("exec correctly refused while paused")

        # 6. resume -> exec works again (state persisted)
        r = s.call("resume", {"vm": name})
        assert not r["result"].get("isError"), r
        r = s.call("exec", {"vm": name, "command": "date -u +%FT%TZ"})
        out = text_of(r["result"])
        print(out.splitlines()[1].strip())
        assert not r["result"].get("isError"), r

        # 7. get + list show the VM
        r = s.call("get", {"vm": name})
        assert name in text_of(r["result"])
        r = s.call("list", {})
        assert name in text_of(r["result"])

        # 8. destroy -> list clean
        r = s.call("destroy", {"vm": name})
        assert not r["result"].get("isError"), r
        r = s.call("list", {})
        assert name not in text_of(r["result"]), "VM still listed after destroy"

        # 9. ping still healthy
        r = s.rpc(id=9, method="ping")
        s.close()
        print("MCP-SMOKE-OK")
        return 0
    except AssertionError as e:
        print("FAIL:", e, file=sys.stderr)
    except Exception as e:
        print("ERROR:", e, file=sys.stderr)
    return 1

if __name__ == "__main__":
    sys.exit(main())

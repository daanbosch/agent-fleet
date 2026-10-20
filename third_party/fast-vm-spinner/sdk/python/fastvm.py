"""Official thin Python SDK for the fastvm REST API (stdlib only).

Usage:
    vm = fastvm.Client("http://10.242.128.198:8090", user="dav").warm_launch(
        template="warm-ubuntu-dev", ttl_minutes=30)
    res = c.exec(vm.name, "hostname")
    c.destroy(vm.name)

Forward-compatible options (ttl_minutes on warm launch, per-VM egress,
fork) ride along in request bodies; servers before feat/agent-substrate
ignore the unknown fields silently, servers on it enforce them.
"""
from __future__ import annotations

import json
import time
import urllib.request

NAME_PREFIX = "agpoc-"  # leak-guard scope: cleanup tooling only ever touches this prefix


class APIError(RuntimeError):
    def __init__(self, method, path, status, message):
        super().__init__(f"fastvm {method} {path}: HTTP {status}: {message}")
        self.method, self.path, self.status, self.message = method, path, status, message

    def not_found(self) -> bool:
        m = self.message.lower()
        return "not found" in m or "unknown vm" in m


class VM(dict):
    """VM record (vmid, name, user, ip, paused, expires_at, ...)."""

    @property
    def vmid(self) -> str:
        return self.get("vmid", "")

    @property
    def name(self) -> str:
        return self.get("name", "")


def _auto_name() -> str:
    return f"{NAME_PREFIX}{time.strftime('%Y%m%dT%H%M%S')}-{int(time.time() * 1000) % 1000000}"


class Client:
    def __init__(self, base_url: str, user: str = "dav", timeout: int = 180):
        self.base = base_url.rstrip("/")
        self.user = user
        self.timeout = timeout

    def _call(self, method: str, path: str, body=None):
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(self.base + path, data=data, method=method)
        if data:
            req.add_header("Content-Type", "application/json")
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as r:
                raw = r.read()
                status = r.status
        except urllib.error.HTTPError as e:
            raw = e.read()
            status = e.code
        ar = json.loads(raw)
        if status >= 400 or not ar.get("ok"):
            raise APIError(method, path, status, ar.get("error") or f"HTTP {status}")
        return ar.get("data")

    # --- lifecycle ---------------------------------------------------------

    def warm_launch(self, template="warm-ubuntu-dev", name=None, ssh_key="",
                    ttl_minutes=0, egress=None) -> VM:
        name = name or _auto_name()
        body = {"template": template, "user": self.user, "name": name,
                "ttl_minutes": ttl_minutes, "egress": egress or []}
        if ssh_key:
            body["ssh_key"] = ssh_key
        data = self._call("POST", "/api/v1/warm-launch", body)
        vms = data.get("vms", [])
        if len(vms) != 1:
            raise APIError("POST", "/api/v1/warm-launch", 200, f"expected 1 VM, got {len(vms)}")
        return VM(vms[0])

    def fork(self, vm: str, count=1, name=None, ttl_minutes=0, egress=None) -> list:
        """Live-fork a VM (requires feat/agent-substrate server)."""
        body = {"vm": vm, "count": count, "user": self.user,
                "ttl_minutes": ttl_minutes, "egress": egress or []}
        if name:
            body["name"] = name
        return self._call("POST", f"/api/v1/vms/{self._resolve(vm)}/fork", body).get("vms", [])

    def _resolve(self, vm: str) -> str:
        try:
            d = self._call("GET", f"/api/v1/vms/resolve/{vm}")
            return d.get("vmid") or vm
        except APIError:
            return vm

    def exec(self, vm: str, command: str, timeout_seconds: int = 0) -> dict:
        body = {"command": command}
        if timeout_seconds:
            body["timeout_s"] = timeout_seconds
        return self._call("POST", f"/api/v1/vms/{self._resolve(vm)}/exec", body)

    def pause(self, vm: str):
        self._call("POST", f"/api/v1/vms/{self._resolve(vm)}/pause", {})

    def resume(self, vm: str):
        self._call("POST", f"/api/v1/vms/{self._resolve(vm)}/resume", {})

    def destroy(self, vm: str):
        self._call("DELETE", f"/api/v1/vms/{self._resolve(vm)}")

    def get(self, vm: str) -> VM:
        return VM(self._call("GET", f"/api/v1/vms/resolve/{vm}"))

    def list(self) -> list:
        return self._call("GET", f"/api/v1/vms?user={self.user}").get("vms", [])


if __name__ == "__main__":  # minimal smoke: python3 fastvm.py
    import os

    c = Client(os.environ.get("FASTVM_API", "http://10.242.128.198:8090"), "dav")
    vm = c.warm_launch()
    print("launched", vm["name"], vm["vmid"], vm["ip"])
    res = c.exec(vm["name"], "hostname; python3 -c 'import platform; print(platform.python_version())'")
    print(res["exit_code"], res["output"].strip())
    c.pause(vm["name"])
    print("paused")
    c.resume(vm["name"])
    print("resumed")
    print(c.exec(vm["name"], "date -u +%FT%TZ")["output"].strip())
    c.destroy(vm["name"])
    names = [v["name"] for v in c.list()]
    assert vm["name"] not in names, "destroy failed"
    print("PYSDK-SMOKE-OK")

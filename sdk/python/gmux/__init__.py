"""gmux: share one GPU across many jobs, from Python.

    import gmux
    c = gmux.Client()
    job = c.run(["python", "train.py"], share=0.25, mem="20G")
    print(job["id"], job["state"])
    for u in c.usage(by="owner")["rows"]:
        print(u["key"], u["gpu_hours"])

The client talks to a running `gmux serve` over its unix socket.
"""
from __future__ import annotations

import http.client
import json
import os
import socket
from typing import Any


class GmuxError(RuntimeError):
    pass


class _UnixConnection(http.client.HTTPConnection):
    def __init__(self, path: str):
        super().__init__("localhost")
        self._path = path

    def connect(self) -> None:
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.connect(self._path)
        self.sock = s


class Client:
    """A gmux client bound to a daemon's socket."""

    def __init__(self, socket_path: str | None = None):
        self.path = socket_path or os.environ.get("GMUX_SOCKET") or os.path.join(
            os.environ.get("GMUX_STATE", "/tmp/gmux"), "gmux.sock"
        )

    def _call(self, method: str, path: str, body: Any = None) -> Any:
        conn = _UnixConnection(self.path)
        payload = json.dumps(body).encode() if body is not None else None
        headers = {"Content-Type": "application/json"} if payload else {}
        try:
            conn.request(method, path, body=payload, headers=headers)
            resp = conn.getresponse()
            data = resp.read()
        except OSError as exc:
            raise GmuxError(f"gmux daemon not reachable at {self.path} (is `gmux serve` running?): {exc}") from None
        finally:
            conn.close()
        parsed = json.loads(data) if data else {}
        if resp.status >= 400:
            raise GmuxError(parsed.get("error", f"HTTP {resp.status}"))
        return parsed

    def run(self, command: list[str], *, share: float, mem: str | int | None = None,
            gpus: int = 0, name: str = "", priority: int = 0, preemptible: bool = False,
            burst: bool = False, wait: bool = False, allow: list[str] | None = None,
            deny_net: bool = False) -> dict:
        """Start a job on a share of a GPU. Returns the job, which may be
        running or queued."""
        req: dict[str, Any] = {"command": command, "share": share, "name": name,
                               "priority": priority, "preemptible": preemptible, "burst": burst,
                               "wait": wait, "deny_net": deny_net}
        if mem is not None:
            req["mem_mib"] = _mem_mib(mem)
        if gpus:
            req["gpus"] = gpus
        if allow:
            req["allow"] = allow
        return self._call("POST", "/v1/jobs", req)

    def jobs(self) -> list[dict]:
        return self._call("GET", "/v1/jobs")["jobs"]

    def stop(self, job_id: str) -> None:
        self._call("DELETE", f"/v1/jobs/{job_id}")

    def resize(self, job_id: str, share: float, mem: str | int | None = None) -> dict:
        body = {"share": share, "mem_mib": _mem_mib(mem) if mem is not None else 0}
        return self._call("POST", f"/v1/jobs/{job_id}/resize", body)

    def cards(self) -> list[dict]:
        return self._call("GET", "/v1/cards")["cards"]

    def usage(self, *, since: str = "24h", by: str = "job") -> dict:
        return self._call("GET", f"/v1/usage?since={since}&by={by}")


def _mem_mib(v: str | int) -> int:
    if isinstance(v, int):
        return v
    t = v.strip().upper()
    mult = 1
    if t.endswith("G") or t.endswith("GB") or t.endswith("GIB"):
        mult, t = 1024, t.rstrip("GIB")
    elif t.endswith("M") or t.endswith("MB") or t.endswith("MIB"):
        t = t.rstrip("MIB")
    return int(float(t) * mult)

#!/usr/bin/env python3
import json
import os
import subprocess
import sys
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


def required_env(name):
    value = os.environ.get(name)
    if not value:
        raise RuntimeError(f"{name} is required")
    return value


NOMAD = required_env("NOMAD_ADDR").rstrip("/")
CONSUL = required_env("CONSUL_HTTP_ADDR").rstrip("/")
REGION = required_env("NOMAD_REGION")
JOB = required_env("PXC_JOB")


def get_json(url):
    with urllib.request.urlopen(url, timeout=10) as response:
        return json.load(response)


def nomad(path):
    sep = "&" if "?" in path else "?"
    return get_json(f"{NOMAD}{path}{sep}region={REGION}")


def consul(path):
    return get_json(f"{CONSUL}{path}")


def allocations():
    return [
        alloc
        for alloc in nomad(f"/v1/job/{JOB}/allocations")
        if alloc.get("DesiredStatus") == "run" and alloc.get("ClientStatus") == "running"
    ]


def service(name, passing=False):
    suffix = "?passing=1" if passing else ""
    return consul(f"/v1/health/service/{name}{suffix}")


def alloc_exec(alloc_id, task, command, timeout=20):
    cmd = [
        "nomad",
        "alloc",
        "exec",
        f"-address={NOMAD}",
        f"-region={REGION}",
        "-i=false",
        "-t=false",
        "-task",
        task,
        alloc_id,
        *command,
    ]
    result = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=timeout)
    return {"returncode": result.returncode, "stdout": result.stdout, "stderr": result.stderr}


def status():
    allocs = allocations()
    return {
        "job": JOB,
        "region": REGION,
        "allocations": [
            {
                "id": alloc["ID"],
                "name": alloc["Name"],
                "node": alloc["NodeName"],
                "group": alloc["TaskGroup"],
                "tasks": sorted(alloc.get("TaskStates", {}).keys()),
            }
            for alloc in allocs
        ],
        "services": {
            "pxc_cluster": len(service("pxc-cluster")),
            "pxc_primary_passing": len(service("pxc-primary", passing=True)),
            "pxc_ready_passing": len(service("pxc", passing=True)),
        },
    }


def recovery_candidates():
    if os.environ.get("ALLOW_LIVE_RECOVERY_PROBE") != "1":
        return {
            "enabled": False,
            "reason": "live recovery probes are disabled; run recover-position only in an offline/probe allocation",
            "candidates": [],
            "winner": None,
        }

    candidates = []
    for alloc in allocations():
        if alloc["TaskGroup"] != "cluster-member":
            continue
        result = alloc_exec(alloc["ID"], "pxc", ["pxc-runtime", "recover-position"])
        seqno = -1
        uuid = "00000000-0000-0000-0000-000000000000"
        if result["returncode"] == 0 and ":" in result["stdout"]:
            uuid, raw_seqno = result["stdout"].strip().split(":", 1)
            try:
                seqno = int(raw_seqno)
            except ValueError:
                seqno = -1
        candidates.append(
            {
                "allocation": alloc["ID"],
                "node": alloc["NodeName"],
                "uuid": uuid,
                "seqno": seqno,
                "exec": result,
            }
        )
    candidates.sort(key=lambda item: (item["seqno"], item["node"], item["allocation"]), reverse=True)
    return {"candidates": candidates, "winner": candidates[0] if candidates else None}


class Handler(BaseHTTPRequestHandler):
    def _json(self, code, payload):
        body = json.dumps(payload, indent=2, sort_keys=True).encode("utf-8")
        self.send_response(code)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        try:
            if self.path == "/health":
                self._json(200, {"ok": True})
            elif self.path == "/status":
                self._json(200, status())
            elif self.path == "/recovery/candidates":
                self._json(200, recovery_candidates())
            else:
                self._json(404, {"error": "not found"})
        except Exception as exc:
            self._json(500, {"error": str(exc)})

    def log_message(self, fmt, *args):
        print("pxc-control: " + fmt % args, file=sys.stderr)


def main():
    listen = os.environ.get("LISTEN", "0.0.0.0:8080")
    host, port = listen.rsplit(":", 1)
    ThreadingHTTPServer((host, int(port)), Handler).serve_forever()


if __name__ == "__main__":
    main()

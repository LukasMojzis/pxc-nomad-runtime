#!/usr/bin/env python3
import json
import os
import sys
import threading
import time
import urllib.request
import base64
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


def consul_put_raw(path, value):
    data = str(value).encode("utf-8")
    request = urllib.request.Request(f"{CONSUL}{path}", data=data, method="PUT")
    with urllib.request.urlopen(request, timeout=10) as response:
        return response.read().decode("utf-8")


def allocations():
    return [
        alloc
        for alloc in nomad(f"/v1/job/{JOB}/allocations")
        if alloc.get("DesiredStatus") == "run" and alloc.get("ClientStatus") == "running"
    ]


def service(name, passing=False):
    suffix = "?passing=1" if passing else ""
    return consul(f"/v1/health/service/{name}{suffix}")


def publish_db_size_once():
    records = consul("/v1/kv/pxc/node-classifier/?recurse")
    candidates = []
    for record in records or []:
        try:
            payload = json.loads(base64.b64decode(record["Value"]).decode("utf-8"))
        except Exception:
            continue
        if payload.get("member_role") == "data":
            candidates.append(payload)
    if not candidates:
        return {"ok": False, "reason": "no classifier data-member records found"}
    size = max(int(item.get("db_bytes") or 0) for item in candidates)
    consul_put_raw("/v1/kv/pxc/cluster/db_bytes", size)
    return {
        "ok": True,
        "db_bytes": size,
        "sources": [
            {"node": item.get("node_name"), "db_bytes": int(item.get("db_bytes") or 0)}
            for item in candidates
        ],
    }


def db_size_publisher():
    interval = int(os.environ.get("DB_SIZE_INTERVAL_SECONDS", "60"))
    while True:
        try:
            publish_db_size_once()
        except Exception as exc:
            print(f"pxc-control: db size publish failed: {exc}", file=sys.stderr)
        time.sleep(interval)


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
        "db_size": db_size_status(),
    }


def db_size_status():
    try:
        value = urllib.request.urlopen(f"{CONSUL}/v1/kv/pxc/cluster/db_bytes?raw", timeout=5).read()
        return {"published": True, "bytes": int(value.decode("utf-8").strip())}
    except Exception:
        return {"published": False, "bytes": 0}


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
        result = {
            "returncode": 1,
            "stdout": "",
            "stderr": "recovery probes require an external Nomad CLI; disabled in the control image",
        }
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
            elif self.path == "/db-size/publish":
                self._json(200, publish_db_size_once())
            else:
                self._json(404, {"error": "not found"})
        except Exception as exc:
            self._json(500, {"error": str(exc)})

    def log_message(self, fmt, *args):
        print("pxc-control: " + fmt % args, file=sys.stderr)


def main():
    listen = os.environ.get("LISTEN", "0.0.0.0:8080")
    host, port = listen.rsplit(":", 1)
    threading.Thread(target=db_size_publisher, daemon=True).start()
    ThreadingHTTPServer((host, int(port)), Handler).serve_forever()


if __name__ == "__main__":
    main()

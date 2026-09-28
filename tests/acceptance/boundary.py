#!/usr/bin/env python3
"""Disposable acceptance instrumentation, not a replacement target service.

Only the LLM response is synthetic. Docker requests are forwarded to the real
Engine after exact name + ownership-label validation. Alertmanager deliveries
are timestamped and immediately forwarded unchanged to the real backend.
"""
import datetime
import http.client
import http.server
import json
import os
from pathlib import Path
import socket
import socketserver
import threading
import urllib.parse

GOAL = "69a3ee64-5465-4d6c-a023-76ee5bebffbe"
TARGET = "oncall-execution-69a3ee64-sub2api"
ROOT = Path("/artifacts")
LOCK = threading.Lock()


def record(kind, **data):
    data = {"at": datetime.datetime.now(datetime.timezone.utc).isoformat(), "kind": kind, **data}
    with LOCK, (ROOT / "boundary.jsonl").open("a") as out:
        out.write(json.dumps(data) + "\n")


class Engine(http.client.HTTPConnection):
    def __init__(self):
        super().__init__("docker", timeout=40)

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect("/var/run/docker.sock")


def engine(method, path):
    conn = Engine()
    try:
        conn.request(method, path)
        response = conn.getresponse()
        return response.status, response.read(), response.getheader("Content-Type", "application/json")
    finally:
        conn.close()


class DockerHandler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_GET(self):
        self.forward()

    def do_POST(self):
        self.forward()

    def forward(self):
        path = urllib.parse.urlsplit(self.path).path
        allowed = ((self.command == "GET" and path in [f"/containers/{TARGET}/json", f"/containers/{TARGET}/logs"])
                   or (self.command == "POST" and path == f"/containers/{TARGET}/restart"))
        if not allowed:
            self.send_error(403, "acceptance exact target/method allowlist")
            return
        status, body, _ = engine("GET", f"/containers/{TARGET}/json")
        row = json.loads(body)
        if status != 200 or row.get("Name") != "/" + TARGET or row.get("Config", {}).get("Labels", {}).get("oncall.goal") != GOAL:
            self.send_error(403, "acceptance ownership mismatch")
            return
        if self.command == "POST":
            record("docker_action_start", method=self.command, path=self.path, container_id=row["Id"])
        status, body, content_type = engine(self.command, self.path)
        if self.command == "POST":
            record("docker_action_finish", status=status, path=self.path)
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


class UnixServer(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True


class Boundary(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_GET(self):
        self.respond(200, {"boundary": "acceptance instrumentation; NOT target health"})

    def respond(self, status, payload):
        raw = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        payload = json.loads(raw)
        if self.path == "/v1/chat/completions":
            record("llm_request", path=self.path, body=payload)
            assert payload["model"] == "acceptance-local"
            assert not payload.get("stream", False)
            # Deliberately proposes restart even for unsupported alerts: policy
            # scope is tested in the real server rather than hidden by the stub.
            result = {
                "rca": "Controlled process-exit acceptance fixture; alert_snapshot and docker identify the owned target.",
                "confidence": "high", "evidence_refs": ["alert_snapshot", "docker"],
                "plan": {"action": "docker_restart", "target": {"kind": "container", "name": TARGET},
                         "reason": "Restart the owned process-exit fixture only", "confidence": "high",
                         "risk": "low", "expected": "Direct Sub2API /health becomes HTTP 2xx"}}
            self.respond(200, {"id": "acceptance-local", "object": "chat.completion", "created": 1,
                               "model": "acceptance-local", "choices": [{"index": 0,
                               "message": {"role": "assistant", "content": json.dumps(result)},
                               "finish_reason": "stop"}], "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})
        elif self.path == "/alertmanager":
            record("alertmanager_arrival", body=payload)
            conn = http.client.HTTPConnection("server", 18080, timeout=10)
            try:
                conn.request("POST", "/webhook/alertmanager", raw,
                             {"Content-Type": "application/json", "Authorization": "Bearer disposable-acceptance-token"})
                response = conn.getresponse()
                body = response.read()
                record("alertmanager_forwarded", status=response.status, body=body.decode())
                self.respond(response.status, {"forwarded": response.status})
            finally:
                conn.close()
        else:
            self.respond(404, {"error": "unknown fixture route"})


if __name__ == "__main__":
    path = "/bridge/docker.sock"
    if os.path.exists(path):
        os.unlink(path)
    docker = UnixServer(path, DockerHandler)
    os.chmod(path, 0o666)
    threading.Thread(target=docker.serve_forever, daemon=True).start()
    # Container-only listener, no host publication.
    http.server.ThreadingHTTPServer(("0.0.0.0", 8888), Boundary).serve_forever()

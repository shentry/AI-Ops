#!/usr/bin/env python3
"""Controlled real-dependency acceptance harness (Python stdlib + Go/npm/Docker).

Commands: prepare, up, native, observe, real, report, snapshot, rebuild, stop, start, cleanup.
Artifacts: /tmp/oncall-execution-acceptance/live. No production files are written.
Fixture passwords below are disposable, local-only, NOT real credentials.
Read tests/acceptance/experiment.md before running. Cleanup never removes MySQL.
"""
import argparse
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request

REPO = Path(__file__).resolve().parents[2]
OUT = Path("/tmp/oncall-execution-acceptance/live")
PREFIX = "oncall-execution-69a3ee64-"
GOAL = "69a3ee64-5465-4d6c-a023-76ee5bebffbe"
DB = "oncall_live_69a3"
SERVICES = ["server", "boundary", "sub2api", "postgres", "redis", "prometheus", "blackbox", "alertmanager"]
BASE = "http://127.0.0.1:28080"
TOKEN = "disposable-acceptance-token"
# Console/operator identity: the machine TOKEN can no longer approve.
OPERATOR_TOKEN = "disposable-acceptance-operator-token-0123456789"
SUB_IMAGE = "weishaw/sub2api@sha256:ccf47a1c62e355f51f896e489f8253e119fe4101b103cd701ba458cc6c6f0f77"


def now():
    return dt.datetime.now(dt.timezone.utc).isoformat()


def record(kind, **data):
    OUT.mkdir(parents=True, exist_ok=True)
    with (OUT / "commands.jsonl").open("a") as stream:
        stream.write(json.dumps({"at": now(), "kind": kind, **data}) + "\n")


def run(*args, input=None, check=True, cwd=None, env=None):
    started = now()
    result = subprocess.run(args, input=input, text=True, capture_output=True, cwd=cwd, env=env)
    record("command", started_at=started, argv=list(args), cwd=str(cwd or REPO), stdin=input,
           returncode=result.returncode, stdout=result.stdout, stderr=result.stderr)
    if check and result.returncode:
        raise RuntimeError(f"command failed: {args}\n{result.stdout}\n{result.stderr}")
    return result.stdout.strip()


def owned(name, kind="container"):
    assert name.startswith(PREFIX), name
    value = json.loads(run("docker", kind, "inspect", name))[0]
    label = value.get("Config", {}).get("Labels", {}) if kind == "container" else value.get("Labels", {})
    assert label.get("oncall.goal") == GOAL, f"ownership mismatch: {name}"
    return value


def operation(service, action, *args):
    assert service in SERVICES
    owned(PREFIX + service)
    return run("docker", action, *args, PREFIX + service)


def mysql(sql, database=True):
    owned(PREFIX + "mysql")
    args = ["docker", "exec", "-i", PREFIX + "mysql", "mysql", "-uroot", "-poncall-test-root", "--default-character-set=utf8mb4", "-N", "-B"]
    if database:
        args.append(DB)
    return run(*args, input=sql)


def query(sql):
    return mysql(sql).splitlines()


def http(path, body=None, base=BASE, auth=False, expected=200):
    url = base + path
    # auth=True uses the machine token; everything else acts as the operator.
    headers = {"Content-Type": "application/json", "Authorization": "Bearer " + (TOKEN if auth else OPERATOR_TOKEN)}
    request = urllib.request.Request(url, data=None if body is None else json.dumps(body).encode(), headers=headers)
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            status, raw = response.status, response.read().decode()
    except urllib.error.HTTPError as response:
        status, raw = response.code, response.read().decode()
    record("http", url=url, request=body, status=status, response=raw)
    assert status == expected, (url, status, raw)
    try:
        return json.loads(raw)
    except json.JSONDecodeError:
        return raw


def wait(fn, timeout=120, interval=0.4):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        try:
            last = fn()
            if last:
                return last
        except (urllib.error.URLError, ConnectionError, TimeoutError):
            pass
        time.sleep(interval)
    raise TimeoutError(f"condition not met: {last}")


def write(name, value):
    (OUT / name).write_text(json.dumps(value, indent=2) if not isinstance(value, str) else value)


def config(mode="observe", native=False):
    return {
        "server": {"port": 28081 if native else 18080, "auth_token": TOKEN, **({} if native else {"listen_addr": "0.0.0.0"})},
        "mysql": {"dsn": f"oncall_live_69a3:disposable-live-db@tcp({'127.0.0.1:23306' if native else 'host.docker.internal:23306'})/{DB}?parseTime=true&loc=UTC"},
        "web": {"base_url": "http://127.0.0.1:28081" if native else BASE,
                "operators": [{"id": "acceptance", "role": "operator", "token_sha256": hashlib.sha256(OPERATOR_TOKEN.encode()).hexdigest()}]},
        "llm": {} if native else {"roles": {"reasoner": {"base_url": "http://boundary:8888/v1", "api_key": "disposable-llm-key", "model": "acceptance-local"}}},
        "correlate": {"group_by": ["labels.service", "labels.experiment"]},
        "diagnose": {"evidence": {"docker_socket": "/var/run/docker.sock" if native else "/bridge/docker.sock", "timeout_seconds": 2, "log_max_lines": 20}},
        "service": {"name": "sub2api", "env": "acceptance", "container": PREFIX + "sub2api",
                    "base_url": "http://127.0.0.1:28088" if native else "http://sub2api:8080",
                    **({} if native else {"postgres_dsn": "postgres://sub2api:disposable-pg@postgres:5432/sub2api?sslmode=disable", "redis_addr": "redis:6379"})},
        # One restart rule; observe proves the decision without writing, manual
        # needs the operator. The watch window is off so verification ends at passed.
        "remediation": {"rules_version": "acceptance-" + mode,
                        "rules": [{"id": "restart-stopped-process", "action": "docker_restart", "mode": mode, "alerts": ["Sub2APIDown"],
                                   "max_executions": 10, "window_minutes": 60}],
                        "verification": {"interval_seconds": 3, "window_seconds": 90, "timeout_seconds": 1, "required_passes": 1, "watch_seconds": 0}},
        "approval": {"ttl_minutes": 30},
        "tools": {"prometheus": {"base_url": "http://127.0.0.1:29090" if native else "http://prometheus:9090"}},
        "notify": {"im": {"provider": "", "webhook": ""}}}


def compose(*args):
    return run("docker", "compose", "-p", PREFIX + "live", "-f", str(OUT / "compose.json"), *args)


def build():
    source = OUT / "source"
    manifest = OUT / "build-manifest.json"
    if manifest.exists():
        shutil.copy2(manifest, OUT / f"build-manifest-before-{time.time_ns()}.json")
    if source.exists():
        shutil.rmtree(source)
    source.mkdir(parents=True)
    for name in ["cmd", "internal", "migrations", "web"]:
        shutil.copytree(REPO / name, source / name,
                        ignore=shutil.ignore_patterns("node_modules", "dist", "test-results", "playwright-report"))
    for name in ["go.mod", "go.sum"]:
        shutil.copy2(REPO / name, source / name)
    run("npm", "ci", cwd=source / "web")
    run("npm", "run", "build", cwd=source / "web")
    run("go", "build", "-o", str(OUT / "server-native"), "./cmd/server", cwd=source)
    run("go", "build", "-o", str(OUT / "server-linux"), "./cmd/server", cwd=source,
        env={**os.environ, "CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": "arm64"})
    write("build-manifest.json", {str(p.relative_to(source)): hashlib.sha256(p.read_bytes()).hexdigest()
                                  for p in source.rglob("*") if p.is_file() and "node_modules" not in p.parts})


def rebuild():
    operation("server", "stop")
    build()
    operation("server", "start")
    wait(lambda: http("/"), timeout=30)
    snapshot()  # Includes byte-for-byte checks of the freshly embedded assets.
    print("Rebuilt owned backend; served frontend matches current sources.")


def prepare():
    for port in [28080, 28081, 28088, 29090, 29093, 29115]:
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", port))
    record("port_preflight", ports=[28080, 28081, 28088, 29090, 29093, 29115], result="all free")
    assert mysql(f"SELECT SCHEMA_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='{DB}';", False) == "", "refuse to reuse existing acceptance DB"
    mysql(f"CREATE DATABASE {DB}; CREATE USER 'oncall_live_69a3'@'%' IDENTIFIED BY 'disposable-live-db'; GRANT ALL ON {DB}.* TO 'oncall_live_69a3'@'%';", False)
    for migration in sorted((REPO / "migrations").glob("*.sql")):
        mysql(migration.read_text())
    build()
    for directory in ["bridge", "pgdata", "redisdata", "subdata", "promdata", "amdata"]:
        (OUT / directory).mkdir(exist_ok=True)
        (OUT / directory).chmod(0o777)
    shutil.copy2(REPO / "tests/acceptance/boundary.py", OUT / "boundary.py")
    write("config.json", config())
    write("native.json", config(native=True))
    # Preserve real repository monitoring rules, adjust only owned DNS, labels
    # and documented test firing/evaluation timings. No dependency rules removed.
    prom = (REPO / "prometheus.yml").read_text().replace("5s", "2s").replace("host.docker.internal:8080", "sub2api:8080").replace("host.docker.internal:15432", "postgres:5432").replace("host.docker.internal:16379", "redis:6379")
    prom = re.sub(r"  - job_name: node-exporter\n.*?(?=  - job_name: sub2api-health)", "", prom, flags=re.S)
    prom = prom.replace("service: sub2api", "service: sub2api\n          experiment: live-prometheus")
    write("prometheus.yml", prom)
    alerts = (REPO / "alerts.yml").read_text().replace("interval: 10s", "interval: 2s").replace("for: 30s", "for: 4s").replace("container: sub2api", "container: " + PREFIX + "sub2api")
    write("alerts.yml", alerts)
    write("blackbox.yml", (REPO / "blackbox.yml").read_text())
    am = (REPO / "alertmanager.yml").read_text().replace("host.docker.internal:18080/webhook/alertmanager", "boundary:8888/alertmanager").replace('credentials: "devtoken"', f'credentials: "{TOKEN}"').replace("group_wait: 5s", "group_wait: 1s")
    write("alertmanager.yml", am)
    services = {}
    def service(name, image, **kwargs):
        services[name] = {"image": image, "container_name": PREFIX + name, "labels": {"oncall.goal": GOAL},
                          "networks": ["live"], "restart": "no", **kwargs}
    def bind(name, target, ro=False):
        return {"type": "bind", "source": str(OUT / name), "target": target, "read_only": ro}
    service("postgres", "postgres:18-alpine", volumes=[bind("pgdata", "/var/lib/postgresql")],
            environment={"PGDATA": "/var/lib/postgresql/data", "POSTGRES_USER": "sub2api", "POSTGRES_PASSWORD": "disposable-pg", "POSTGRES_DB": "sub2api", "TZ": "UTC"})
    service("redis", "redis:8-alpine", volumes=[bind("redisdata", "/data")])
    service("sub2api", SUB_IMAGE, volumes=[bind("subdata", "/app/data")], ports=["127.0.0.1:28088:8080"],
            environment={"AUTO_SETUP": "true", "SERVER_HOST": "0.0.0.0", "SERVER_PORT": "8080", "SERVER_MODE": "release",
                         "DATABASE_HOST": "postgres", "DATABASE_PORT": "5432", "DATABASE_USER": "sub2api", "DATABASE_PASSWORD": "disposable-pg", "DATABASE_DBNAME": "sub2api", "DATABASE_SSLMODE": "disable",
                         "REDIS_HOST": "redis", "REDIS_PORT": "6379", "ADMIN_EMAIL": "acceptance@example.invalid", "ADMIN_PASSWORD": "Disposable-Only-69a3!", "JWT_SECRET": "a" * 64, "TOTP_ENCRYPTION_KEY": "b" * 64, "TZ": "UTC"})
    service("boundary", "python:3.12-alpine", command=["python", "/boundary.py"],
            volumes=[bind("boundary.py", "/boundary.py", True), {"type": "volume", "source": "bridge", "target": "/bridge"}, bind(".", "/artifacts"),
                     {"type": "bind", "source": "/var/run/docker.sock", "target": "/var/run/docker.sock"}])
    service("server", "alpine:latest", command=["/server"], environment={"CONFIG_FILE": "/config.json"},
            extra_hosts=["host.docker.internal:host-gateway"], ports=["127.0.0.1:28080:18080"],
            volumes=[bind("server-linux", "/server", True), bind("config.json", "/config.json", True), {"type": "volume", "source": "bridge", "target": "/bridge", "read_only": True}])
    service("blackbox", "prom/blackbox-exporter:v0.25.0", ports=["127.0.0.1:29115:9115"],
            command=["--config.file=/config/blackbox.yml"], volumes=[bind("blackbox.yml", "/config/blackbox.yml", True)])
    service("alertmanager", "prom/alertmanager:v0.27.0", ports=["127.0.0.1:29093:9093"],
            command=["--config.file=/etc/alertmanager/alertmanager.yml", "--storage.path=/alertmanager"],
            volumes=[bind("alertmanager.yml", "/etc/alertmanager/alertmanager.yml", True), bind("amdata", "/alertmanager")])
    service("prometheus", "prom/prometheus:v2.54.1", ports=["127.0.0.1:29090:9090"],
            command=["--config.file=/etc/prometheus/prometheus.yml", "--storage.tsdb.path=/prometheus"],
            volumes=[bind("prometheus.yml", "/etc/prometheus/prometheus.yml", True), bind("alerts.yml", "/etc/prometheus/alerts.yml", True), bind("promdata", "/prometheus")])
    write("compose.json", {"services": services, "networks": {"live": {"name": PREFIX + "live-net", "labels": {"oncall.goal": GOAL}}},
                           "volumes": {"bridge": {"name": PREFIX + "bridge", "labels": {"oncall.goal": GOAL}}}})
    print("Prepared isolated database and fresh embedded frontend binaries.")


def up():
    for name in SERVICES:
        assert subprocess.run(["docker", "container", "inspect", PREFIX + name], capture_output=True).returncode != 0, "refuse existing container"
    assert subprocess.run(["docker", "network", "inspect", PREFIX + "live-net"], capture_output=True).returncode != 0, "refuse existing network"
    compose("up", "-d", "postgres", "redis", "boundary", "blackbox", "alertmanager")
    wait(lambda: "accepting connections" in run("docker", "exec", PREFIX + "postgres", "pg_isready", "-h", "127.0.0.1", "-U", "sub2api", "-d", "sub2api", check=False))
    compose("up", "-d", "sub2api")
    wait(lambda: http("/health", base="http://127.0.0.1:28088").get("status") == "ok", timeout=240)
    compose("up", "-d", "server")
    wait(lambda: http("/api/v1/approvals").get("approvals") == [])
    # Monitoring starts only in real phase to avoid bootstrap/observe alerts.
    print("Observe-mode real dependencies ready.")


def native():
    env = {**os.environ, "CONFIG_FILE": str(OUT / "native.json")}
    with (OUT / "native.log").open("w") as log:
        proc = subprocess.Popen([str(OUT / "server-native")], env=env, stdout=log, stderr=log)
    record("native_start", pid=proc.pid, executable=str(OUT / "server-native"))
    try:
        wait(lambda: http("/", base="http://127.0.0.1:28081"), timeout=30)
        listeners = run("lsof", "-nP", "-a", "-p", str(proc.pid), "-iTCP", "-sTCP:LISTEN")
        assert "127.0.0.1:28081" in listeners and "*:28081" not in listeners
        write("native-listener.txt", listeners)
    finally:
        proc.terminate()
        proc.wait(timeout=20)
        record("native_stop", pid=proc.pid, returncode=proc.returncode)
    for key in ["unknown_acceptance_key", "dry_run"]:
        cfg = config(native=True)
        cfg["approval"][key] = 30
        write("invalid.json", cfg)
        result = subprocess.run([str(OUT / "server-native")], env={**os.environ, "CONFIG_FILE": str(OUT / "invalid.json")}, text=True, capture_output=True, timeout=10)
        record("invalid_config", key=key, returncode=result.returncode, stderr=result.stderr)
        assert result.returncode != 0 and key in result.stderr


def alert(experiment, name="Sub2APIDown", status="firing", started=None):
    return {"status": status, "labels": {"alertname": name, "service": "sub2api", "container": PREFIX + "sub2api", "severity": "critical", "experiment": experiment},
            "annotations": {"summary": "Controlled acceptance fixture: " + experiment}, "startsAt": started or now(),
            "endsAt": now() if status == "resolved" else "0001-01-01T00:00:00Z", "generatorURL": "http://prometheus:9090/graph"}


def deliver(alerts):
    return http("/webhook/alertmanager", {"version": "4", "receiver": "acceptance", "status": alerts[0]["status"], "alerts": alerts}, auth=True, expected=202)


def pending(experiment):
    rows = query(f"SELECT a.id FROM approval a JOIN incident i ON i.id=a.incident_id WHERE i.group_key LIKE '%{experiment}%' AND a.status='pending' ORDER BY a.id DESC LIMIT 1;")
    return http("/api/v1/approvals/" + rows[0]) if rows else None


def approve(row):
    return http(f"/api/v1/approvals/{row['id']}/approve", {"plan_hash": row["plan_hash"], "reason": "Controlled owned-container acceptance"})


def status(approval_id, expected):
    row = http(f"/api/v1/approvals/{approval_id}")
    return row if row["status"] == expected else None


def boundary_events():
    path = OUT / "boundary.jsonl"
    return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []


def actions():
    return [e for e in boundary_events() if e["kind"] == "docker_action_start"]


def policy_output(experiment):
    rows = query(f"SELECT s.output_json FROM agent_run_step s JOIN agent_run r ON r.id=s.run_id JOIN incident i ON i.id=r.incident_id WHERE i.group_key LIKE '%{experiment}%' AND s.name='policy' AND r.status='succeeded' ORDER BY s.id DESC LIMIT 1;")
    return rows[0] if rows else None


def observe():
    assert config()["remediation"]["rules"][0]["mode"] == "observe"
    # A real stopped process: the observe rule decides exactly what it would do.
    operation("sub2api", "stop", "--time", "10")
    stopped = owned(PREFIX + "sub2api")["State"]["FinishedAt"]
    # A real container sends to the explicitly configured internal :18080 listener.
    body = {"version": "4", "receiver": "acceptance", "status": "firing", "alerts": [alert("observe")]}
    command = "import urllib.request,json; r=urllib.request.urlopen(urllib.request.Request('http://server:18080/webhook/alertmanager',data=" + repr(json.dumps(body).encode()) + ",headers={'Content-Type':'application/json','Authorization':'Bearer " + TOKEN + "'})); print(r.status); print(r.read().decode())"
    output = run("docker", "exec", PREFIX + "boundary", "python", "-c", command)
    assert output.startswith("202"), output
    decision = wait(lambda: policy_output("observe"))
    assert "decision=observe" in decision and "would docker_restart container/" + PREFIX + "sub2api" in decision, decision
    assert query("SELECT COUNT(*) FROM approval;") == ["0"]
    assert query("SELECT COUNT(*) FROM verify_task;") == ["0"]
    assert query("SELECT COUNT(*) FROM fault_memory;") == ["0"]
    assert actions() == [] and owned(PREFIX + "sub2api")["State"]["FinishedAt"] == stopped
    write("observe-result.json", {"policy": decision})
    deliver([alert("observe", status="resolved")])
    operation("sub2api", "start")
    wait(lambda: http("/health", base="http://127.0.0.1:28088").get("status") == "ok", timeout=120)
    record("gate_pass", gate="T1 container202, T15 observe: decision recorded, no approval/task/memory/Docker call")


def block_result(approval_id):
    mysql(f"DELIMITER //\nCREATE TRIGGER acceptance_block_result BEFORE UPDATE ON approval FOR EACH ROW BEGIN IF OLD.id={approval_id} AND OLD.status='executing' AND NEW.status='executed' THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='acceptance result commit failure'; END IF; END//\nDELIMITER ;\n")


def block_verification():
    mysql("CREATE TRIGGER acceptance_block_verify BEFORE UPDATE ON verify_task FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='acceptance verification claim blocked';")


def scope_tests():
    for label, names in [("scope-slow", ["Sub2APISlow"]), ("scope-postgres", ["Sub2APIPostgresUnreachable"]), ("scope-mixed", ["Sub2APIDown", "Sub2APIRedisUnreachable"])]:
        deliver([alert(label, name) for name in names])
        wait(lambda: query(f"SELECT r.id FROM agent_run r JOIN incident i ON i.id=r.incident_id WHERE i.group_key LIKE '%{label}%' AND r.status='succeeded';"))
        assert query(f"SELECT COUNT(*) FROM approval a JOIN incident i ON i.id=a.incident_id WHERE i.group_key LIKE '%{label}%';") == ["0"]
    # Snapshot exists for a really stopped process, then the source resolves
    # before the executor claims it.
    operation("sub2api", "stop", "--time", "10")
    deliver([alert("scope-resolved")])
    row = wait(lambda: pending("scope-resolved"))
    deliver([alert("scope-resolved", status="resolved")])
    wait(lambda: query(f"SELECT status FROM incident WHERE id={row['incident_id']};") == ["resolved"])
    approve(row)
    wait(lambda: status(row["id"], "expired"))
    assert len(actions()) == 0
    operation("sub2api", "start")
    wait(lambda: http("/health", base="http://127.0.0.1:28088").get("status") == "ok", timeout=120)
    record("gate_pass", gate="T8 Slow/dependency/mixed denied and resolved-before-claim expired")


def real():
    assert (OUT / "observe-result.json").exists(), "real execution requires a successful observe run first"
    operation("server", "stop", "--time", "20")
    write("config.json", config(mode="manual"))
    operation("server", "start")
    wait(lambda: http("/api/v1/approvals"))
    scope_tests()
    compose("up", "-d", "prometheus")
    owned(PREFIX + "prometheus")
    # Source entrypoint execs /app/sub2api as PID 1. No automatic restart policy.
    runtime = run("docker", "exec", PREFIX + "sub2api", "sh", "-c", "tr '\\0' ' ' </proc/1/cmdline; echo; ps")
    assert "/app/sub2api" in runtime
    assert owned(PREFIX + "sub2api")["HostConfig"]["RestartPolicy"]["Name"] == "no"
    record("fault_start", fault="stop owned actual Sub2API process/container")
    operation("sub2api", "stop", "--time", "10")
    record("fault_complete", state=owned(PREFIX + "sub2api")["State"])
    row = wait(lambda: pending("live-prometheus"), timeout=120)
    write("real-approval.json", row)
    with (OUT / "real-sse.txt").open("w") as stream:
        sse = subprocess.Popen(["curl", "-sS", "-N", "--max-time", "100", f"{BASE}/api/v1/incidents/{row['incident_id']}/stream"], stdout=stream, stderr=subprocess.DEVNULL)
    record("sse_capture", pid=sse.pid, incident_id=row["incident_id"], max_seconds=100)
    assert row["target"] == "container/" + PREFIX + "sub2api" and row["target_id"] and row["rule_id"] == "restart-stopped-process"
    assert row["mode"] == "manual" and "container" in row["checks"] and "health" in row["checks"]
    assert "risk" not in row and "base_url" not in json.dumps(row)
    http(f"/api/v1/approvals/{row['id']}/approve", {"plan_hash": "0" * 64, "reason": "wrong hash negative gate"}, expected=409)
    # Read-only verification claim is blocked in THIS DB ONLY to expose durable
    # execution/task state and permit a deterministic restart recovery test.
    block_verification()
    block_result(row["id"])
    approve(row)
    wait(lambda: len(actions()) == 1)
    wait(lambda: any(e["kind"] == "docker_action_finish" for e in boundary_events()))
    time.sleep(2.2)  # require at least one persistence-only retry
    assert query(f"SELECT status FROM approval WHERE id={row['id']};") == ["executing"]
    assert query(f"SELECT COUNT(*) FROM verify_task WHERE approval_id={row['id']};") == ["0"]
    assert query(f"SELECT COUNT(*) FROM fault_cmd_history WHERE approval_id={row['id']};") == ["0"]
    assert len(actions()) == 1
    write("result-rollback.json", {"approval_id": row["id"], "status": "executing", "task_count": 0, "history_count": 0, "action_count": 1})
    mysql("DROP TRIGGER acceptance_block_result;")
    wait(lambda: status(row["id"], "executed"))
    assert query(f"SELECT status FROM verify_task WHERE approval_id={row['id']};") == ["pending"]
    deadline = query(f"SELECT deadline_at FROM verify_task WHERE approval_id={row['id']};")[0]
    http(f"/api/v1/approvals/{row['id']}/approve", {"plan_hash": row["plan_hash"], "reason": "duplicate"}, expected=409)
    # One service has one disposition at a time: a second incident on a really
    # stopped process gets no approval while the first recovery is verifying.
    operation("sub2api", "stop", "--time", "10")
    deliver([alert("second-incident")])
    second = wait(lambda: policy_output("second-incident"))
    assert f"busy with approval {row['id']}" in second, second
    assert query("SELECT COUNT(*) FROM approval a JOIN incident i ON i.id=a.incident_id WHERE i.group_key LIKE '%second-incident%';") == ["0"]
    assert query(f"SELECT status FROM verify_task WHERE approval_id={row['id']};") == ["pending"]
    assert len(actions()) == 1
    operation("sub2api", "start")
    write("service-mutex.json", {"first": http(f"/api/v1/incidents/{row['incident_id']}/control-room"), "second_policy": second})
    # Persisted execution resumes only verification after real backend restart.
    operation("server", "stop", "--time", "20")
    mysql("DROP TRIGGER acceptance_block_verify;")
    operation("server", "start")
    wait(lambda: http("/api/v1/approvals"))
    wait(lambda: query(f"SELECT status FROM verify_task WHERE approval_id={row['id']};") == ["passed"])
    assert query(f"SELECT deadline_at FROM verify_task WHERE approval_id={row['id']};") == [deadline]
    assert len(actions()) == 1
    write("passed-before-resolved.json", {"real": http(f"/api/v1/incidents/{row['incident_id']}/control-room")})
    wait(lambda: any(e["kind"] == "alertmanager_arrival" and e["body"].get("status") == "resolved" for e in boundary_events()), timeout=120)
    wait(lambda: query(f"SELECT status FROM incident WHERE id={row['incident_id']};") == ["resolved"])
    # Crash after the external write: a real restart succeeds but its result is
    # not persisted. Restart reconciles against the container (its start time
    # moved), records the write once and never replays the action.
    operation("sub2api", "stop", "--time", "10")
    deliver([alert("unknown-result")])
    third = wait(lambda: pending("unknown-result"))
    block_result(third["id"])
    approve(third)
    wait(lambda: len(actions()) == 2)
    wait(lambda: len([e for e in boundary_events() if e["kind"] == "docker_action_finish"]) == 2)
    wait(lambda: query(f"SELECT status FROM approval WHERE id={third['id']};") == ["executing"])
    operation("server", "kill", "--signal", "KILL")
    mysql("DROP TRIGGER acceptance_block_result;")
    operation("server", "start")
    result = wait(lambda: status(third["id"], "executed"))
    time.sleep(3)
    assert len(actions()) == 2
    assert result["result"]["outcome"] == "written" and "interrupted" in result["result"]["detail"]
    wait(lambda: query(f"SELECT status FROM verify_task WHERE approval_id={third['id']};") == ["passed"])
    write("unknown-result-recovery.json", result)
    for item in [row, third]:
        assert query(f"SELECT COUNT(*) FROM fault_cmd_history WHERE approval_id={item['id']};") == ["1"]
        assert query(f"SELECT COUNT(*) FROM verify_task WHERE approval_id={item['id']};") == ["1"]
        assert query(f"SELECT COUNT(*) FROM incident_event WHERE approval_id={item['id']} AND event_type='execution.completed';") == ["1"]
    write("experiment-ids.json", {"real": row, "unknown": third})
    snapshot()
    report()
    record("gate_pass", gate="T2 projection, T3 wrong/duplicate hash, T5 service mutex, T9 rollback+persistence retry one physical action; T10 persisted verify recovery and crash reconciliation without replay; T16 real fault chain")
    print("Real experiment completed; leave backend live at " + BASE)


def snapshot():
    for table in ["incident", "approval", "verify_task", "fault_cmd_history", "fault_memory", "agent_run", "agent_run_step", "incident_event", "incident_problem", "raw_event", "alert", "last_alert"]:
        result = mysql("SELECT * FROM " + table + ";")
        write("db-" + table + ".tsv", result + "\n")
    for service in SERVICES:
        row = owned(PREFIX + service)
        # Do not dump image environment; even fixtures need not normalize that habit.
        write("container-" + service + ".json", {k: row[k] for k in ["Id", "Name", "Image", "State", "Mounts", "NetworkSettings"]})
        write(service + ".log", run("docker", "logs", "--timestamps", PREFIX + service))
    write("approvals.json", http("/api/v1/approvals"))
    write("incidents.json", http("/api/v1/control-room/incidents"))
    html = http("/")
    assert isinstance(html, str) and "/assets/" in html
    write("served-index.html", html)
    for asset in re.findall(r'(?:src|href)="(/assets/[^\"]+)"', html):
        with urllib.request.urlopen(BASE + asset) as response:
            body = response.read()
        built = (OUT / "source/web/dist" / asset.lstrip("/")).read_bytes()
        assert body == built
        record("embedded_asset_match", path=asset, sha256=hashlib.sha256(body).hexdigest(), bytes=len(body))
    write("handoff.json", {"url": BASE, "incident_url": BASE + "/incidents/" + (query("SELECT i.id FROM incident i JOIN approval a ON a.incident_id=i.id JOIN verify_task v ON v.approval_id=a.id WHERE i.status='firing' AND v.status='passed' ORDER BY i.id LIMIT 1;") or ["1"])[0],
                           "database": DB, "containers": [PREFIX + s for s in SERVICES], "network": PREFIX + "live-net", "config": str(OUT / "config.json"),
                           "start": "python3 tests/acceptance/live.py start", "stop": "python3 tests/acceptance/live.py stop", "cleanup": "python3 tests/acceptance/live.py cleanup"})


def report():
    ids = json.loads((OUT / "experiment-ids.json").read_text())
    first, third = ids["real"], ids["unknown"]
    observations = json.loads((OUT / "passed-before-resolved.json").read_text())
    for room in observations.values():
        assert room["incident"]["status"] == "firing"
        assert room["latest_action"]["status"] == "executed"
        assert room["latest_action"]["verification"]["status"] == "passed"
        assert room.get("pending_approval") is None
        assert "base_url" not in json.dumps(room["latest_action"])
    assert len(actions()) == 2
    owned(PREFIX + "live-net", "network")
    owned(PREFIX + "bridge", "volume")
    for service in SERVICES:
        container = owned(PREFIX + service)
        for mappings in container["NetworkSettings"]["Ports"].values():
            assert all(mapping["HostIp"] == "127.0.0.1" for mapping in mappings or [])
        assert all(mount.get("Name") == PREFIX + "bridge" for mount in container["Mounts"] if mount["Type"] == "volume")
    assert query(f"SELECT status FROM approval WHERE id={third['id']};") == ["executed"]
    assert query(f"SELECT JSON_UNQUOTE(JSON_EXTRACT(result_json,'$.outcome')) FROM approval WHERE id={third['id']};") == ["written"]
    assert query("SELECT TRIGGER_NAME FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA='oncall_live_69a3';") == []
    for item in [first, third]:
        assert query(f"SELECT COUNT(*) FROM fault_cmd_history WHERE approval_id={item['id']};") == ["1"]
        assert query(f"SELECT status FROM verify_task WHERE approval_id={item['id']};") == ["passed"]
        assert query(f"SELECT COUNT(*) FROM incident_event WHERE approval_id={item['id']} AND event_type='execution.completed';") == ["1"]
    assert query(f"SELECT status FROM incident WHERE id={first['incident_id']};") == ["resolved"]
    # Replay after backend restart resumes from the last durable SSE event ID.
    last = re.findall(r'^id: (\d+)$', (OUT / "real-sse.txt").read_text(), re.M)[-1]
    replay = run("curl", "-sS", "-N", "--max-time", "2", "-H", "Last-Event-ID: " + last,
                 f"{BASE}/api/v1/incidents/{first['incident_id']}/stream", check=False)
    assert "event: verify.passed" in replay and "event: incident.resolved" in replay
    write("sse-replayed.txt", replay)
    events = boundary_events()
    started = next(e["at"] for e in events if e["kind"] == "docker_action_start")
    finished = next(e["at"] for e in events if e["kind"] == "docker_action_finish")
    resolved = next(e["at"] for e in events if e["kind"] == "alertmanager_arrival" and e["body"]["status"] == "resolved")
    probe = observations["real"]["latest_action"]["verification"]["last_checked_at"]
    parse = dt.datetime.fromisoformat
    assert parse(probe) < parse(resolved)
    result = {"checked_at": now(), "target_image": SUB_IMAGE, "ids": {k: {"incident": v["incident_id"], "approval": v["id"]} for k,v in ids.items()},
              "timing": {"physical_action_start": started, "physical_action_finish": finished, "direct_probe_completion_db": probe,
                         "independent_resolved_arrival": resolved, "action_seconds": (parse(finished)-parse(started)).total_seconds(),
                         "resolved_after_probe_seconds": (parse(resolved)-parse(probe)).total_seconds()},
              "physical_restart_requests": len(actions()), "successful_executions": 2, "denied_by_service_mutex": 1, "reconciled_after_crash": 1,
              "observe": "decision recorded; no approval, task, memory or Docker request",
              "overrides": {"verification_timeout_interval_window_seconds": [1,3,90], "prometheus_scrape_evaluate_seconds": 2,
                            "rule_group_interval_seconds": 2, "down_firing_for_seconds": 4, "alertmanager_group_wait_seconds": 1,
                            "alertmanager_group_interval_seconds": 30, "watch_seconds": 0},
              "limitations": ["Single instrumented trial, not recovery latency calibration; default 5/10/120 not measured",
                              "The recovery watch window is disabled here; recurrence is covered by store and worker tests",
                              "Actual Sub2API health handler always emits 200; literal 503-to-200/persistent-503 gates not exercised",
                              "Verification pending was held with a claim-failing trigger in our DB, not a slow HTTP health response",
                              "T9 already-committed ambiguous retry is not available over public HTTP; tested precommit rollback and persistence-only retry",
                              "No URL redirection/input injection gate, legacy-data migration, stale-running lease, or browser interaction claim here",
                              "Only LLM is deterministic synthetic; alert/Engine forwarding instrumentation performs real I/O"]}
    write("result.json", result)
    print(json.dumps(result, indent=2))


def cleanup():
    # Exact names + goal labels, never compose down or broad label deletion.
    for service in SERVICES:
        name = PREFIX + service
        if subprocess.run(["docker", "container", "inspect", name], capture_output=True).returncode == 0:
            owned(name)
            run("docker", "rm", "-f", name)
    name = PREFIX + "live-net"
    if subprocess.run(["docker", "network", "inspect", name], capture_output=True).returncode == 0:
        owned(name, "network")
        run("docker", "network", "rm", name)
    name = PREFIX + "bridge"
    if subprocess.run(["docker", "volume", "inspect", name], capture_output=True).returncode == 0:
        owned(name, "volume")
        run("docker", "volume", "rm", name)
    # DB artifacts deliberately retained until parent has inspected. No operation
    # on sandbox service, parent DBs/triggers, server globals, or business services.
    print("Owned containers/network removed. Acceptance DB and /tmp evidence retained.")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=["prepare", "up", "native", "observe", "real", "report", "snapshot", "rebuild", "start", "stop", "cleanup"])
    args = parser.parse_args()
    if args.command in ["start", "stop"]:
        operation("server", args.command)
    else:
        globals()[args.command]()

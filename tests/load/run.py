#!/usr/bin/env python3
"""Run one bounded k6 stage on the isolated Linux/cgroup-v2 deployment.

Copy beside the deployed compose.yaml, then: python3 run.py duplicate 25 20 1
No credentials are read or printed here; Compose and MySQL use container env.
"""
import argparse
from datetime import datetime, timezone
import fcntl
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import time
import urllib.request

SCOPE = "aws-k6-20260917"
LOAD = "oncall-bench-k6"
SERVICES = ["oncall-bench-mysql", "oncall-bench-server"]
PROTECTED = ["sub2api", "sub2api-postgres", "sub2api-redis", "cli-proxy-api"]
POLL_SECONDS = 2
MYSQL = ["docker", "exec", "-i", SERVICES[0], "sh", "-c",
         'MYSQL_PWD="$MYSQL_PASSWORD" exec mysql -uoncall -D oncall_benchmark '
         '--batch --raw --skip-column-names --connect-timeout=3']
INSPECT = ('{"name":{{json .Name}},"id":{{json .Id}},'
           '"started_at":{{json .State.StartedAt}},"pid":{{.State.Pid}},'
           '"running":{{.State.Running}},"restarts":{{.RestartCount}},'
           '"oom":{{.State.OOMKilled}},"exit_code":{{.State.ExitCode}},'
           '"scope":{{json (index .Config.Labels "oncall.scope")}}}')
STATUS_SQL = """
SHOW GLOBAL STATUS WHERE Variable_name IN (
 'Threads_connected','Threads_running','Max_used_connections','Aborted_connects',
 'Slow_queries','Questions','Innodb_row_lock_current_waits','Innodb_row_lock_waits',
 'Innodb_row_lock_time','Innodb_buffer_pool_wait_free','Innodb_data_fsyncs');
"""
TOTALS_SQL = """
SELECT JSON_OBJECT(
 'raw_base', (SELECT COALESCE(MAX(id),0) FROM raw_event),
 'pending', (SELECT COUNT(*) FROM raw_event WHERE status='pending'),
 'alerts', (SELECT COUNT(*) FROM alert),
 'last_alerts', (SELECT COUNT(*) FROM last_alert),
 'incidents', (SELECT COUNT(*) FROM incident),
 'runs', (SELECT COUNT(*) FROM agent_run),
 'skip_succeeded', (SELECT COUNT(*) FROM agent_run WHERE mode='skip' AND status='succeeded'),
 'tokens', (SELECT COALESCE(SUM(tokens_in+tokens_out),0) FROM agent_run),
 'steps', (SELECT COUNT(*) FROM agent_run_step),
 'approvals', (SELECT COUNT(*) FROM approval));
"""


def command(args, stdin=None, timeout=10):
    return subprocess.check_output(args, input=stdin, text=True, timeout=timeout).strip()


def inspect(names):
    return [json.loads(line) for line in command(
        ["docker", "inspect", "--format", INSPECT, *names]).splitlines()]


def sql(query):
    return command(MYSQL, stdin=query, timeout=6).splitlines()


def counters(path):
    return {key: int(value) for key, value in
            (line.split() for line in path.read_text().splitlines())}


def cgroup(container):
    entry = Path(f"/proc/{container['pid']}/cgroup").read_text().strip()
    if not entry.startswith("0::/"):
        raise RuntimeError("This benchmark runner requires Linux cgroup v2")
    return Path("/sys/fs/cgroup") / entry[3:].lstrip("/")


def fetch(url):
    with urllib.request.urlopen(url, timeout=2) as response:
        return response.status, response.read().decode()


def sample(raw_base, groups, phase):
    started = time.monotonic()
    status, _ = fetch("http://127.0.0.1:8080/health")
    health_ms = (time.monotonic() - started) * 1000
    query = f"""
SELECT JSON_OBJECT('persisted',COUNT(*),
 'processed',COALESCE(SUM(status='processed'),0),
 'pending',COALESCE(SUM(status='pending'),0),
 'failed',COALESCE(SUM(status='failed'),0),
 'oldest_pending_s',COALESCE(TIMESTAMPDIFF(MICROSECOND,
     MIN(IF(status='pending',created_at,NULL)),UTC_TIMESTAMP(3))/1000000,0))
FROM oncall_benchmark.raw_event WHERE id>{raw_base};
"""
    db_started = time.monotonic()
    rows = sql(query + STATUS_SQL)
    mem = dict(line.split(":", 1) for line in Path("/proc/meminfo").read_text().splitlines())
    cpu = list(map(int, Path("/proc/stat").read_text().splitlines()[0].split()[1:9]))
    container_stats = {}
    for name, group in groups.items():
        # The load cgroup disappears on normal k6 exit; service cgroups must remain.
        if name == LOAD and not group.exists():
            continue
        memory_bytes = int((group / "memory.current").read_text())
        memory_stat = counters(group / "memory.stat")
        inactive_file = memory_stat["inactive_file"]
        # Same cgroup-v2 working-set calculation as Docker CLI 29.6.2.
        working_set = memory_bytes - inactive_file if inactive_file < memory_bytes else memory_bytes
        container_stats[name] = {
            "cpu": counters(group / "cpu.stat"),
            "memory_bytes": memory_bytes,
            "memory_inactive_file_bytes": inactive_file,
            "memory_working_set_bytes": working_set,
            "memory_limit": int((group / "memory.max").read_text()),
            "memory_events": counters(group / "memory.events"),
        }
    return {
        "at": datetime.now(timezone.utc).isoformat(), "monotonic_s": time.monotonic(),
        "phase": phase, "queue": json.loads(rows[0]),
        "mysql": {key: int(value) for key, value in (line.split("\t") for line in rows[1:])},
        "db_sample_ms": (time.monotonic() - db_started) * 1000,
        "sub2api_status": status, "sub2api_health_ms": health_ms,
        "available_memory_bytes": int(mem["MemAvailable"].split()[0]) * 1024,
        "disk_free_bytes": shutil.disk_usage("/").free,
        "host_cpu_ticks": cpu, "containers": container_stats,
    }


def safety(row, previous):
    if row["available_memory_bytes"] < 768 * 1024**2:
        return "host available memory below 768 MiB"
    if row["disk_free_bytes"] < 2 * 1024**3:
        return "host free disk below 2 GiB"
    if row["sub2api_status"] != 200 or row["sub2api_health_ms"] > 1000:
        return "Sub2API health failed or exceeded 1 second"
    if row["queue"]["pending"] > 5000 or row["queue"]["oldest_pending_s"] > 60:
        return "ingest backlog exceeded 5000 webhooks or 60 seconds"
    if row["queue"]["failed"]:
        return "raw_event contains failed webhooks"
    if row["mysql"]["Threads_connected"] > 80:
        return "MySQL connections exceeded 80 of the configured 100"
    for name, values in row["containers"].items():
        if values["memory_working_set_bytes"] > values["memory_limit"] * 0.95:
            return f"{name} memory working set exceeded 95% of its limit"
        if values["memory_events"]["oom_kill"]:
            return f"{name} has an OOM kill"
    if previous:
        delta = [a - b for a, b in zip(row["host_cpu_ticks"], previous["host_cpu_ticks"])]
        row["host_busy_pct"] = 100 * (1 - (delta[3] + delta[4]) / max(sum(delta), 1))
        if row["host_busy_pct"] > 90 and previous.get("host_busy_pct", 0) > 90:
            return "host CPU remained above 90% for two samples"
    return ""


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=["duplicate", "unique"])
    parser.add_argument("rate", type=int, choices=range(1, 251))
    parser.add_argument("seconds", type=int, choices=range(5, 61))
    parser.add_argument("batch_size", type=int, choices=range(1, 11))
    args = parser.parse_args()
    os.chdir(Path(__file__).resolve().parent)
    Path("results").mkdir(exist_ok=True)
    lock = Path("results/.runner.lock").open("w")
    fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    baseline = inspect(PROTECTED + SERVICES)
    if not all(c["running"] and not c["oom"] for c in baseline):
        raise RuntimeError("All protected and benchmark services must be running without OOM")
    if any(c["scope"] != SCOPE for c in baseline[-2:]):
        raise RuntimeError("Refusing to run outside the labeled isolated deployment")
    if command(["docker", "ps", "-aq", "--filter", f"name=^/{LOAD}$"]):
        raise RuntimeError("A prior k6 container exists; inspect it before another run")
    before = json.loads(sql(TOTALS_SQL)[0])
    if before["pending"]:
        raise RuntimeError("Drain the previous stage before starting another")
    stamp = datetime.now(timezone.utc)
    run_id = f"{args.mode}-r{args.rate}-b{args.batch_size}-{stamp:%Y%m%dT%H%M%S}"
    prefix = Path("results") / run_id
    result = {"run_id": run_id, "parameters": vars(args), "before": before,
              "containers_before": baseline, "poll_seconds": POLL_SECONDS, "stop_reason": "",
              "memory_guard": "memory.current minus inactive_file (Docker cgroup-v2 working set)"}
    groups = {c["name"].lstrip("/"): cgroup(c) for c in baseline[-2:]}
    previous = None
    monitor = prefix.with_suffix(".monitor.jsonl").open("w")

    def record(phase):
        nonlocal previous
        row = sample(before["raw_base"], groups, phase)
        violation = safety(row, previous)
        monitor.write(json.dumps(row) + "\n")
        monitor.flush()
        previous = row
        if violation:
            raise RuntimeError(violation)
        return row

    try:
        record("baseline")
        prefix.with_suffix(".metrics-before.txt").write_text(fetch("http://127.0.0.1:18081/metrics")[1])
        command(["docker", "compose", "--profile", "load", "run", "-d", "--no-deps",
                 "--name", LOAD, "-e", f"RUN_ID={run_id}", "-e", f"MODE={args.mode}",
                 "-e", f"RATE={args.rate}", "-e", f"SECONDS={args.seconds}",
                 "-e", f"BATCH_SIZE={args.batch_size}", "-e", f"STARTS_AT={stamp:%Y-%m-%dT%H:%M:%SZ}",
                 "k6", "run", "--quiet", "/scripts/alertmanager.k6.js"], timeout=20)
        load_state = inspect([LOAD])[0]
        groups[LOAD] = cgroup(load_state)
        started = time.monotonic()
        while inspect([LOAD])[0]["running"]:
            if time.monotonic() - started > args.seconds + 20:
                raise RuntimeError("k6 exceeded its bounded duration")
            record("load")
            time.sleep(POLL_SECONDS)
        result["k6_exit_code"] = inspect([LOAD])[0]["exit_code"]
        drain_started = time.monotonic()
        row = record("drain")
        while row["queue"]["pending"]:
            if time.monotonic() - drain_started > 120:
                raise RuntimeError("ingest did not drain within 120 seconds")
            time.sleep(POLL_SECONDS)
            row = record("drain")
        result["drain_observed_seconds"] = time.monotonic() - drain_started
        result["queue_final"] = row["queue"]
    except (Exception, KeyboardInterrupt) as exc:
        result["stop_reason"] = f"{type(exc).__name__}: {exc}"
    finally:
        # Only this runner's explicitly labeled k6 container is ever stopped/removed.
        if command(["docker", "ps", "-aq", "--filter", f"name=^/{LOAD}$"]):
            state = inspect([LOAD])[0]
            if state["scope"] != SCOPE:
                raise RuntimeError("k6 ownership changed; refusing cleanup")
            if state["running"]:
                command(["docker", "stop", "--time", "5", LOAD], timeout=12)
            with prefix.with_suffix(".k6.log").open("w") as log:
                subprocess.run(["docker", "logs", LOAD], stdout=log, stderr=log, check=True, timeout=10)
            result["k6_exit_code"] = inspect([LOAD])[0]["exit_code"]
            command(["docker", "rm", LOAD])
        monitor.close()
        prefix.with_suffix(".result.json").write_text(json.dumps(result, indent=2) + "\n")
    result["after"] = json.loads(sql(TOTALS_SQL)[0])
    result["containers_after"] = inspect(PROTECTED + SERVICES)
    prefix.with_suffix(".metrics-after.txt").write_text(fetch("http://127.0.0.1:18081/metrics")[1])
    summary_path = prefix.with_suffix(".summary.json")
    if summary_path.exists():
        result["k6"] = json.loads(summary_path.read_text())
    else:
        result["stop_reason"] = result["stop_reason"] or "k6 summary missing"
    queue = result.get("queue_final", {})
    accepted = result.get("k6", {}).get("metrics", {}).get("webhooks_accepted", {}).get("values", {}).get("count", 0)
    expected_incidents = 1 if args.mode == "duplicate" else accepted
    expected_alerts = expected_incidents * args.batch_size
    checks = {
        "containers_unchanged": baseline == result["containers_after"],
        "queue_drained": result["after"]["pending"] == 0,
        "accepted_and_processed_match": accepted > 0 and queue.get("processed") == queue.get("persisted") == accepted,
        "workload_shape_matches": all(result["after"][k] - before[k] == expected_incidents
                                      for k in ("incidents", "runs"))
        and all(result["after"][k] - before[k] == expected_alerts for k in ("alerts", "last_alerts")),
        "no_llm_or_execution": all(result["after"][k] == 0 for k in ("tokens", "steps", "approvals"))
        and result["after"]["runs"] == result["after"]["skip_succeeded"],
    }
    result["checks"] = checks
    prefix.with_suffix(".result.json").write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps({k: result.get(k) for k in
                      ("run_id", "k6_exit_code", "stop_reason", "queue_final", "drain_observed_seconds", "checks")}))
    return 0 if not result["stop_reason"] and result.get("k6_exit_code") == 0 and all(checks.values()) else 1


if __name__ == "__main__":
    def interrupted(signum, frame):
        raise KeyboardInterrupt(f"signal {signum}")
    signal.signal(signal.SIGTERM, interrupted)
    raise SystemExit(main())

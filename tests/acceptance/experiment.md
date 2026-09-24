# Controlled execution-trust live acceptance

> Final parent recheck completed on 2026-09-12 UTC. The owned live stack was then removed; the acceptance DB/user were explicitly dropped after export to `final/live-database.sql`. URLs below describe the reproducible handoff, not a currently running service. Browser and complete T1–T17 evidence are summarized in [the verification report](../../docs/execution-trust-verification.md).

This harness uses a freshly built embedded frontend + real Go server + real MySQL,
Sub2API, PostgreSQL, Redis, Docker Engine, Prometheus, blackbox-exporter and
Alertmanager. **Only the OpenAI-compatible LLM response is synthetic.** The
`boundary.py` fixture also records and immediately forwards real Docker requests
and Alertmanager webhooks; it is not a target `/health` replacement.

## Scope and safety

- Repository writes are confined to `tests/acceptance/`. Builds, configs, fixture
  data and evidence are in `/tmp/oncall-execution-acceptance/live/`.
- Goal label: `oncall.goal=69a3ee64-5465-4d6c-a023-76ee5bebffbe`.
- All created containers/network/volume names start `oncall-execution-69a3ee64-`.
  Operations check exact name and label before faults, restart or removal.
- Host publishes bind **127.0.0.1 only**. Server container listens explicitly on
  internal `0.0.0.0:18080`, published as `127.0.0.1:28080`. No host firewall edits.
- Existing sandbox `oncall-execution-69a3ee64-mysql:23306` is never restarted,
  reconfigured or attached to the test network. Only `oncall_live_69a3` and its
  same-named disposable user/grants are created. SQL triggers are local to this DB.
- No operations on business containers or any existing Sub2API. The Docker proxy
  permits only inspect/logs/restart of **our** exact Sub2API name and rechecks its
  label before forwarding. The application's restart whitelist contains only it.
- Credentials in fixture code/config are disposable examples, not real secrets.
  Admin address is `acceptance@example.invalid`. No model/provider accounts are
  configured in Sub2API. Notifications are the real backend's `NoopNotifier`;
  `notification.sent` events do not mean an external message was sent.
- Requires macOS/arm64 host + Linux/arm64 Docker (the tested environment), Go,
  Node/npm, Python 3, curl, lsof, and Docker Compose. Ports are checked before setup.

## Run

From the repository root, with the sandbox MySQL already running:

```sh
python3 tests/acceptance/live.py prepare
python3 tests/acceptance/live.py native
python3 tests/acceptance/live.py up
python3 tests/acceptance/live.py dry
python3 tests/acceptance/live.py real
python3 tests/acceptance/live.py report
```

`native` must run **before `up`**, avoiding two server instances on one business
DB. `prepare` refuses an existing test DB; `up` refuses existing named containers
or network. `real` requires a successful dry-run artifact, stops the dry server,
explicitly writes `dry_run: false`, then starts real mode. Approvals always use
public HTTP and the returned immutable `plan_hash`, recording `anonymous/web`.

The build copies current `cmd`, `internal`, migrations, `web`, go.mod/go.sum into
`/tmp`, runs `npm ci && npm run build` there, then builds native and Linux Go
binaries. It never modifies repository web build outputs. Served JS/CSS bytes are
compared with the fresh build; `build-manifest.json` records source/artifact hashes.

Sub2API uses the actual `weishaw/sub2api` image family present on this host, pinned
for this experiment to:

```
weishaw/sub2api@sha256:ccf47a1c62e355f51f896e489f8253e119fe4101b103cd701ba458cc6c6f0f77
org.opencontainers.image.version = 0.2.4
org.opencontainers.image.revision = 5de5e2bed035d43591a2e10e51f420ef6a84eb98
```

The formerly cached tag was an unmaterialized manifest; `docker pull
weishaw/sub2api:latest` fetched this real runnable image. Runtime config follows
that revision's deploy/docker-compose.yml: AUTO_SETUP, database env, Redis env,
fixed disposable JWT/TOTP values, `/app/data`, PostgreSQL 18 and Redis 8. The
entrypoint source `exec`s `/app/sub2api`; runtime `/proc/1/cmdline` and restart
policy `no` are checked before stopping the owned process/container. A real
Engine restart recovers it. No fake health service is used.

Upstream files inspected and retained in `live/upstream/pinned-*` were fetched via:

```sh
REV=5de5e2bed035d43591a2e10e51f420ef6a84eb98
for file in deploy/docker-compose.yml deploy/docker-entrypoint.sh \
  backend/internal/server/router.go Dockerfile \
  backend/internal/server/routes/common.go; do
  curl -fsSL --max-time 30 \
    "https://raw.githubusercontent.com/Wei-Shaw/sub2api/$REV/$file" \
    -o "/tmp/oncall-execution-acceptance/live/upstream/pinned-$(basename "$file")"
done
```

### Timing and instrumentation

Temporary overrides: verification **timeout 1s < interval 3s < window 90s**;
Prometheus scrape/evaluation 2s; rule-group interval 2s; Down firing `for: 4s`;
Alertmanager group_wait 1s, unchanged group_interval 30s; restart minimum 1s,
maximum 10/hour. The real repository dependency rules and blackbox health-body
check are retained; only the unrelated node-exporter job is omitted. This is one
instrumented functional trial, **not latency calibration of defaults 5/10/120**.

Two own-DB triggers deliberately expose otherwise short windows:

1. Before approval `executing -> executed`, reject result commit. Real restart
   occurs once; approval remains executing with **zero task and history** during
   repeated persistence failures. Removing trigger permits a single atomic result
   + task + history + event commit, with no extra Docker action.
2. Reject verification claims temporarily. First task stays pending while another
   Incident executes. Stop/restart backend, remove trigger, then only verification
   resumes with unchanged deadlines. This is a pending queue, not a slow HTTP probe.

Both first and second approvals target the same sole allowlisted test container,
but belong to independent Incidents. The first task's passing probe occurs after
both approved restarts: these timestamps must not be attributed to the first
restart alone, or generalized into distinct-target throughput measurements.

A third approval's real action succeeds while result commit is rejected; the
backend is killed with SIGKILL. Restart marks the old executing row failed with
`manual_check: true`, creates no verify task, and does not replay the action.

## Recorded run: 2026-09-11 UTC

The live experiment completed its action/state assertions. Initial evidence export
failed because the mysql CLI's default character set produced non-UTF-8 text;
setting `--default-character-set=utf8mb4` fixed the fixture exporter. The final
`snapshot` and independently rechecking `report` commands both exited 0. No
production source bug was found or patched by this experiment.

Other fixture-only setup corrections, retained in command logs: transient Docker
Hub EOF pulls; waiting for PostgreSQL TCP readiness rather than its bootstrap Unix
socket; using a labeled Docker volume for the instrumentation Unix socket because
Docker Desktop's host bind-mount socket chmod returned EINVAL. These are not
claimed as application failures.

| Fact | Evidence / observed result |
|---|---|
| Native listener | `native-listener.txt`: API `127.0.0.1:28081`; framework graceful-control socket also loopback. Native PID 51717 exited after test. |
| Container delivery | Boundary container posted to `http://server:18080/webhook/alertmanager`, received **202**, persisted incident/approval. |
| Dry run | Incident 1, approval 1: **simulated**, no verify task, no memory, no Docker request or target start-time change. |
| Real chain | Incident **6**, approval **3**; actual Prometheus/blackbox firing -> Alertmanager -> backend. |
| Actual action start | **17:32:15.741321**; forwarding real Docker POST `/containers/oncall-execution-69a3ee64-sub2api/restart?t=10`. |
| Actual action finish | **17:32:15.826843**, Engine **204**; 0.085522s request duration. |
| Durable executed + task | **17:32:18.881** after injected result-commit failures were released. |
| Direct health completed | verify_task last_checked_at **17:32:23.669**, actual Sub2API HTTP **200**, task **passed**. |
| Independently received resolved | Boundary **17:32:42.411708**, forwarded backend **202** at 17:32:42.420596. This is measured arrival, not inferred from intervals. |
| Separate incident fact | At passed observation, Incident 6 was still **firing**. Resolved arrived **18.742708s later**. |
| Executor isolation | Incident **7**, approval **4**, executed while approval 3's task remained pending. Its task passed but Incident 7 remains firing: no resolved message was fabricated. |
| Unknown-result recovery | Incident **8**, approval **5**, third physical restart; crash before result persistence -> failed/manual_check; no task and no replay. |
| No duplicate action | Exactly **3 real Engine restart requests for 3 distinct real approvals**; approvals 3/4 each have exactly one execution event, task and history. Approval 1 adds none. |
| Safe read model / SSE | `latest_action` retains executed + pending/passed with no pending_approval, and excludes verification base URL. Live SSE contains execution.completed/verify.queued; Last-Event-ID replay after restart contains verify.passed and incident.resolved. |

### Gates, without overclaiming

- **T1:** native default loopback, explicit container-reachable internal 18080 with
  actual 202 delivery, unknown/deleted keys rejected.
- **T5:** real second incident executes while first verification is pending;
  persisted latest_action and SSE/replay expose both facts. Browser rendering is
  parent-owned and not claimed here.
- **T6 (partial):** actual direct 200 passes before independently arriving resolved.
  Literal 503→200 and persistent-503 failure branches are **not exercised**. This
  Sub2API revision's common.go `/health` handler unconditionally returns 200 when
  running, and process exit gives unavailable, not 503. We did not substitute a
  fake target to claim those gates.
- **T8 (partial):** high-confidence stub proposes restart for Slow, PostgreSQL and
  mixed Down+Redis alerts; real policy creates no approvals. Resolved-before-claim
  approval expires without action. Redirect refusal, user-supplied probe URL and
  post-approval mixed-scope races are not claimed by this black-box run.
- **T9 (partial):** real result-transaction rollback and persistence-only retry
  prove no partial task/history and no second action. Replay after an ambiguously
  *already committed* transaction is not exposed by public HTTP and needs store
  integration testing, owned by the parent.
- **T10:** actual backend restart resumes persisted pending verification only;
  actual SIGKILL with unpersisted executing result yields manual check, no replay.
- **T15 (partial):** dry-run contract proved; migration/legacy-record branch is
  owned by the migration child, not claimed here.
- **T16 (partial):** own empty DB migrated 001–011, fresh embedded assets verified,
  real dependency/fault timestamps recorded. Old-data migration and full browser
  E2E are separately owned and not claimed here.
- Additional serial checks: approval DTO target/scope/L2/boolean mode, wrong Hash
  and repeated approval return 409. No Web/Feishu concurrency or real paid model
  quality claim. No stale-running lease, deadline-late 2xx, or retry-budget claim.

## Evidence and handoff

Live backend left running (no native server PID):

- `http://127.0.0.1:28080/` — embedded UI
- `http://127.0.0.1:28080/incidents/7` — executed + passed, Incident still firing
- `/incidents/6` — actual monitored fault, now resolved
- `/incidents/8` — failed/unknown execution, manual-check problem
- `/incidents/1` — simulated historical action

Containers (all prefixed `oncall-execution-69a3ee64-`): `server`, `boundary`,
`sub2api`, `postgres`, `redis`, `prometheus`, `blackbox`, `alertmanager`.
Network: `oncall-execution-69a3ee64-live-net`. Shared Unix-socket volume:
`oncall-execution-69a3ee64-bridge`. Other data are bind mounts under `live/`.
MySQL is the parent's existing sandbox, **not part of cleanup**.

Config: `/tmp/oncall-execution-acceptance/live/config.json` (real mode, automatic
execution disabled, only our Sub2API allowlisted). Compose: `live/compose.json`.

```sh
python3 tests/acceptance/live.py report    # recheck without new actions
python3 tests/acceptance/live.py snapshot  # refresh durable DB/DTO/resource evidence
python3 tests/acceptance/live.py rebuild   # stop only owned backend, fresh build, restart + asset check
python3 tests/acceptance/live.py stop      # checked stop of backend only
python3 tests/acceptance/live.py start     # checked start of backend only
# Only AFTER parent/browser inspection:
python3 tests/acceptance/live.py cleanup
```

Cleanup verifies every exact name/label and removes only those eight containers,
the test network and socket volume. It deliberately retains the acceptance DB,
disposable user and `/tmp` evidence; no broad `compose down`, Docker prune or PID
kill. For a new fresh run, after inspection and cleanup, explicitly drop only the
acceptance DB/user through the checked `mysql()` helper and archive `live/` under
`/tmp/oncall-execution-acceptance/` before rerunning `prepare`.

Key durable artifacts:

- `commands.jsonl`: exact command argv/stdin/stdout/stderr/return codes and HTTP
  requests/responses, ownership checks, faults, build outputs (fixture credentials).
- `boundary.jsonl`: independent Docker start/finish and Alertmanager arrival times,
  local LLM requests/contract. `result.json`: machine-readable checked summary.
- `db-*.tsv`: actual own-DB state, including raw webhook envelope and event history.
- `dry-result.json`, `result-rollback.json`, `execution-isolation.json`,
  `passed-before-resolved.json`, `unknown-result-recovery.json`.
- `real-sse.txt`, `sse-replayed.txt`, `build-manifest.json`, `served-index.html`,
  `container-*.json`, `handoff.json`, logs and pinned upstream source.
- Console logs keep initial fixture failures and final successful rechecks, rather
  than overwriting the record with an unqualified all-pass claim.

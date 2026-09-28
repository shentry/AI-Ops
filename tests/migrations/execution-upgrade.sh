#!/usr/bin/env bash
# Isolated, destructive-schema exercise ONLY on the labelled execution sandbox.
set -euo pipefail
cd "$(dirname "$0")/../.."
container=oncall-execution-69a3ee64-mysql
goal=69a3ee64-5465-4d6c-a023-76ee5bebffbe
: "${MYSQL_TEST_ROOT_PASSWORD:?set the sandbox root password}"
: "${MYSQL_TEST_APP_PASSWORD:?set the sandbox oncall_test password}"

verify_sandbox() {
  local identity
  identity=$(docker inspect --format '{{.Name}} {{index .Config.Labels "oncall.goal"}} {{range (index .NetworkSettings.Ports "3306/tcp")}}{{.HostIp}}:{{.HostPort}}{{end}}' "$container")
  if [[ "$identity" != "/$container $goal 127.0.0.1:23306" ]]; then
    echo "refusing unexpected sandbox identity: $identity" >&2
    exit 1
  fi
}

suffix="$(date -u +%Y%m%d%H%M%S)_$$"
empty_db="oncall_upgrade_69a3_empty_$suffix"
legacy_db="oncall_upgrade_69a3_legacy_$suffix"
evidence="${EXECUTION_UPGRADE_EVIDENCE_DIR:-/tmp/oncall-execution-acceptance/upgrade-$suffix}"
mkdir -p "$evidence"
exec > >(tee "$evidence/verification.log") 2>&1
verify_sandbox
printf 'UTC=%s\ncontainer=%s\nlabel=%s\nempty_db=%s\nlegacy_db=%s\n' "$(date -u +%FT%TZ)" "$container" "$goal" "$empty_db" "$legacy_db"

for database in "$empty_db" "$legacy_db"; do
  # No IF NOT EXISTS: a collision is an error, never permission to reuse a DB.
  # Escape underscores in GRANT's database pattern so grants are exact-scope.
  grant_database=${database//_/\\_}
  verify_sandbox
  docker exec -i -e MYSQL_PWD="$MYSQL_TEST_ROOT_PASSWORD" "$container" mysql -uroot <<SQL
CREATE DATABASE \`$database\` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;
GRANT ALL PRIVILEGES ON \`$grant_database\`.* TO 'oncall_test'@'%';
SQL
done

for mode in empty legacy; do
  if [[ "$mode" == empty ]]; then database=$empty_db; else database=$legacy_db; fi
  export TEST_MYSQL_DSN="oncall_test:${MYSQL_TEST_APP_PASSWORD}@tcp(127.0.0.1:23306)/${database}?parseTime=true&loc=UTC"
  TEST_EXECUTION_UPGRADE_MODE="$mode" go test -count=1 -v ./internal/store -run '^TestExecutionUpgradeMigrations$'
done
export TEST_MYSQL_DSN="oncall_test:${MYSQL_TEST_APP_PASSWORD}@tcp(127.0.0.1:23306)/${empty_db}?parseTime=true&loc=UTC"
unset TEST_EXECUTION_UPGRADE_MODE
go test -race -count=1 -v ./internal/store -run '^TestExecutionUpgrade(Retirement|Rollback|CommitFailureCount|ConcurrentRetirement)$'
go test -count=1 ./cmd/retire-approvals
printf 'PASS: offline migration/retirement scope only. No server or external action started.\nEvidence: %s/verification.log\nDisposable databases retained: %s %s\n' "$evidence" "$empty_db" "$legacy_db"

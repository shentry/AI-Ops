-- Immutable execution mode, safety level and verification binding.
-- OFFLINE: stop all writers and back up MySQL before upgrading a populated database.
-- MYSQL_DSN=... go run ./cmd/retire-approvals -apply (works on 001–008 or 009+).
-- See docs/execution-trust-upgrade.md for upgrade, preflight and rollback commands.
-- This DDL never decides approvals, rehashes history or backfills verification.
-- After 009–011, startup refuses any remaining active NULL-context approvals.
ALTER TABLE approval
  ADD COLUMN execution_context JSON NULL,
  MODIFY COLUMN status ENUM('pending','approved','denied','expired','executing','executed','simulated','failed') NOT NULL;

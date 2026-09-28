-- Multi-action remediation: rules, service mutex, operation receipts,
-- compensation, post-recovery watch, change records, reviews and control events.
-- OFFLINE: stop the server before applying. Execution snapshots move to
-- version 3; pending/approved older snapshots are expired at claim, unfinished
-- older verification tasks finish as inconclusive, and an interrupted older
-- execution is recovered as failed with a manual check. Nothing is replayed.
ALTER TABLE approval
  ADD COLUMN service VARCHAR(64) NULL AFTER run_id,
  ADD COLUMN rule_id VARCHAR(64) NULL AFTER service,
  ADD COLUMN parent_approval_id BIGINT NULL AFTER rule_id,
  ADD COLUMN operation_id VARCHAR(64) NULL AFTER result_json,
  ADD COLUMN operation_started_at DATETIME(3) NULL AFTER operation_id,
  MODIFY COLUMN status ENUM('pending','approved','denied','expired','executing','executed','simulated','failed','aborted') NOT NULL,
  ADD KEY idx_approval_service_status (service, status),
  ADD KEY idx_approval_rule_started (rule_id, operation_started_at),
  ADD KEY idx_approval_parent (parent_approval_id);

-- phase verify: consecutive healthy observations decide recovery.
-- phase watch: after recovery, consecutive unhealthy observations are a recurrence.
ALTER TABLE verify_task
  ADD COLUMN phase ENUM('verify','watch') NOT NULL DEFAULT 'verify' AFTER status,
  ADD COLUMN consecutive_failures INT NOT NULL DEFAULT 0 AFTER consecutive_passes,
  MODIFY COLUMN status ENUM('pending','running','passed','failed','inconclusive','stable','recurred') NOT NULL;

-- One row per service, locked while a claim decides whether another
-- disposition of that service is still executing or verifying.
CREATE TABLE service_lock (
  service    VARCHAR(64) PRIMARY KEY,
  created_at DATETIME(3) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- Global operator and system decisions: emergency stop/resume, rule block
-- resets and rule releases loaded at startup. Append-only.
CREATE TABLE control_event (
  id          BIGINT AUTO_INCREMENT PRIMARY KEY,
  kind        VARCHAR(32) NOT NULL,
  rule_id     VARCHAR(64) NULL,
  actor       VARCHAR(64) NOT NULL,
  reason      VARCHAR(512) NOT NULL,
  detail_json JSON NULL,
  created_at  DATETIME(3) NOT NULL,
  KEY idx_control_kind (kind, rule_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- Deployment changes: releases reported by CI/CD or people, and rollbacks made
-- by the agent. Configuration and secret bodies stay in the deployment system;
-- only references and digests are stored here.
CREATE TABLE change_event (
  id              BIGINT AUTO_INCREMENT PRIMARY KEY,
  env             VARCHAR(32) NOT NULL,
  service         VARCHAR(64) NOT NULL,
  change_type     ENUM('release','rollback','config') NOT NULL,
  release_id      VARCHAR(128) NULL,
  image_ref       VARCHAR(512) NULL,
  before_ref      VARCHAR(512) NULL,
  config_version  VARCHAR(128) NULL,
  db_migration    ENUM('none','compatible','incompatible','unknown') NOT NULL DEFAULT 'unknown',
  verified_at     DATETIME(3) NULL,
  occurred_at     DATETIME(3) NOT NULL,
  source          VARCHAR(32) NOT NULL,
  actor           VARCHAR(64) NOT NULL,
  idempotency_key VARCHAR(128) NOT NULL,
  approval_id     BIGINT NULL,
  created_at      DATETIME(3) NOT NULL,
  UNIQUE KEY uk_change_source_key (source, idempotency_key),
  KEY idx_change_service_time (service, occurred_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- Human review of an incident: the real root cause and fix, and whether the
-- diagnosis (run) or the action (approval) was right. unknown is not correct.
CREATE TABLE review (
  id          BIGINT AUTO_INCREMENT PRIMARY KEY,
  incident_id BIGINT NOT NULL,
  run_id      BIGINT NULL,
  approval_id BIGINT NULL,
  subject     ENUM('diagnosis','action') NOT NULL,
  verdict     ENUM('correct','partial','wrong','unknown') NOT NULL,
  root_cause  VARCHAR(1024) NOT NULL,
  actual_fix  VARCHAR(1024) NOT NULL,
  manual_minutes INT NOT NULL DEFAULT 0,
  reviewer    VARCHAR(64) NOT NULL,
  created_at  DATETIME(3) NOT NULL,
  KEY idx_review_incident (incident_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- D01 表结构。手工执行：mysql < migrations/001_init.sql
-- 不要 AutoMigrate：ENUM、索引、JSON 可空性必须能在这份 SQL 里审查。
-- 十张表：摄入 raw_event/alert/last_alert，聚合 incident*，
-- 记忆、审批、诊断 agent_run*。

-- webhook 信封。D03 先写 pending，再由 worker 扫出来补账。
CREATE TABLE IF NOT EXISTS raw_event (
  id            BIGINT AUTO_INCREMENT PRIMARY KEY,
  source        VARCHAR(64)  NOT NULL,
  payload       JSON         NOT NULL,
  status        ENUM('pending','processed','failed') NOT NULL DEFAULT 'pending',
  error         TEXT         NULL,
  created_at    DATETIME(3)  NOT NULL,
  processed_at  DATETIME(3)  NULL,
  KEY idx_status (status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- 归一化告警的追加历史。fingerprint=SHA-256，alert_hash=MD5（D02）。
CREATE TABLE IF NOT EXISTS alert (
  id            BIGINT AUTO_INCREMENT PRIMARY KEY,
  fingerprint   CHAR(64)     NOT NULL,
  alert_hash    CHAR(32)     NOT NULL,
  source        VARCHAR(64)  NOT NULL,
  name          VARCHAR(255) NOT NULL,
  severity      TINYINT      NOT NULL,
  status        ENUM('firing','resolved') NOT NULL,
  labels        JSON         NOT NULL,
  annotations   JSON         NOT NULL,
  starts_at     DATETIME(3)  NOT NULL,
  received_at   DATETIME(3)  NOT NULL,
  KEY idx_fp_time (fingerprint, received_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- 每个 fingerprint 的当前快照。D03 去重只跟这一行比。
CREATE TABLE IF NOT EXISTS last_alert (
  fingerprint   CHAR(64)     PRIMARY KEY,
  alert_id      BIGINT       NOT NULL,
  alert_hash    CHAR(32)     NOT NULL,
  status        ENUM('firing','resolved') NOT NULL,
  severity      TINYINT      NOT NULL,
  first_seen    DATETIME(3)  NOT NULL,
  last_seen     DATETIME(3)  NOT NULL,
  firing_count  INT          NOT NULL DEFAULT 1,
  incident_id   BIGINT       NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- D04/D05 聚合事件。idx_group_open 用来查仍开放的 incident。
CREATE TABLE IF NOT EXISTS incident (
  id            BIGINT AUTO_INCREMENT PRIMARY KEY,
  group_key     VARCHAR(255) NOT NULL,
  status        ENUM('candidate','firing','acknowledged','resolved') NOT NULL,
  severity      TINYINT      NOT NULL,
  alerts_count  INT          NOT NULL DEFAULT 0,
  title         VARCHAR(512) NOT NULL,
  started_at    DATETIME(3)  NOT NULL,
  last_seen_at  DATETIME(3)  NOT NULL,
  resolved_at   DATETIME(3)  NULL,
  KEY idx_group_open (group_key, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- 成员表。联合主键：同一 fingerprint 最多加入一个 incident 一次。
CREATE TABLE IF NOT EXISTS incident_alert (
  incident_id   BIGINT       NOT NULL,
  fingerprint   CHAR(64)     NOT NULL,
  linked_at     DATETIME(3)  NOT NULL,
  PRIMARY KEY (incident_id, fingerprint)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- D13 记忆键是 CHAR(12)，不是 D02 的 SHA-256 告警指纹。
CREATE TABLE IF NOT EXISTS fault_memory (
  fingerprint   CHAR(12)     PRIMARY KEY,
  group_key     VARCHAR(255) NOT NULL,
  alert_name    VARCHAR(255) NOT NULL,
  rca_text      TEXT         NOT NULL,
  plan_json     JSON         NOT NULL,
  confidence    ENUM('high','medium','low') NOT NULL,
  hits          INT          NOT NULL DEFAULT 0,
  first_seen    DATETIME(3)  NOT NULL,
  last_used     DATETIME(3)  NULL,
  last_success  DATETIME(3)  NOT NULL,
  ttl_sec       INT          NOT NULL DEFAULT 3600
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- 已审批命令结果，后续诊断会注入。
CREATE TABLE IF NOT EXISTS fault_cmd_history (
  id            BIGINT AUTO_INCREMENT PRIMARY KEY,
  fingerprint   CHAR(12)     NOT NULL,
  tool_name     VARCHAR(128) NOT NULL,
  args_json     JSON         NOT NULL,
  result_brief  VARCHAR(1024) NOT NULL,
  approval_id   BIGINT       NULL,
  created_at    DATETIME(3)  NOT NULL,
  KEY idx_fp_time (fingerprint, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- D10 的 L2 变更动作状态机。
CREATE TABLE IF NOT EXISTS approval (
  id            BIGINT AUTO_INCREMENT PRIMARY KEY,
  incident_id   BIGINT       NOT NULL,
  run_id        BIGINT       NOT NULL,
  tool_name     VARCHAR(128) NOT NULL,
  args_json     JSON         NOT NULL,
  reason        TEXT         NOT NULL,
  status        ENUM('pending','approved','denied','expired','executed','failed') NOT NULL,
  expires_at    DATETIME(3)  NOT NULL,
  decided_by    VARCHAR(64)  NULL,
  result_json   JSON         NULL,
  created_at    DATETIME(3)  NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- 一次诊断尝试，也是 D05 诊断 worker 的队列。
CREATE TABLE IF NOT EXISTS agent_run (
  id            BIGINT AUTO_INCREMENT PRIMARY KEY,
  incident_id   BIGINT       NOT NULL,
  mode          ENUM('full','light','skip','memory_hit') NOT NULL,
  status        ENUM('pending','running','succeeded','failed') NOT NULL,
  retry_of      BIGINT       NULL,
  rca_text      TEXT         NULL,
  plan_json     JSON         NULL,
  tokens_in     INT          NOT NULL DEFAULT 0,
  tokens_out    INT          NOT NULL DEFAULT 0,
  started_at    DATETIME(3)  NOT NULL,
  finished_at   DATETIME(3)  NULL,
  KEY idx_status (status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- 可回放的流水线一步。idx_run (run_id, seq) 是按序读取路径。
CREATE TABLE IF NOT EXISTS agent_run_step (
  id            BIGINT AUTO_INCREMENT PRIMARY KEY,
  run_id        BIGINT       NOT NULL,
  seq           INT          NOT NULL,
  kind          ENUM('evidence','llm','tool','guard','approval','verify') NOT NULL,
  name          VARCHAR(128) NOT NULL,
  input_json    JSON         NULL,
  output_json   JSON         NULL,
  error         TEXT         NULL,
  started_at    DATETIME(3)  NOT NULL,
  finished_at   DATETIME(3)  NULL,
  KEY idx_run (run_id, seq)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- D06 控制室持久化：事件、问题、对话、飞书绑定和 Web 会话。
-- 手工执行；保持表结构与 internal/store/models.go 一一对应，不使用 AutoMigrate。

-- 跨摄入、诊断、审批、执行、Verify、通知和对话的统一事实事件。
CREATE TABLE IF NOT EXISTS incident_event (
  id            BIGINT AUTO_INCREMENT PRIMARY KEY,
  incident_id   BIGINT       NOT NULL,
  run_id        BIGINT       NULL,
  approval_id   BIGINT       NULL,
  event_type    VARCHAR(64)  NOT NULL,
  phase         VARCHAR(32)  NOT NULL,
  status        VARCHAR(32)  NOT NULL,
  summary       VARCHAR(512) NOT NULL,
  payload_json  JSON         NULL,
  created_at    DATETIME(3)  NOT NULL,
  KEY idx_incident_event (incident_id, id),
  KEY idx_run_event (run_id, id),
  KEY idx_approval_event (approval_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- Incident 当前仍需关注的问题；incident_id/code 唯一，重现时更新同一行。
CREATE TABLE IF NOT EXISTS incident_problem (
  id             BIGINT AUTO_INCREMENT PRIMARY KEY,
  incident_id    BIGINT       NOT NULL,
  run_id         BIGINT       NULL,
  code           VARCHAR(64)  NOT NULL,
  severity       VARCHAR(16)  NOT NULL,
  status         VARCHAR(16)  NOT NULL,
  summary        VARCHAR(512) NOT NULL,
  detail_json    JSON         NULL,
  first_seen_at  DATETIME(3)  NOT NULL,
  last_seen_at   DATETIME(3)  NOT NULL,
  resolved_at    DATETIME(3)  NULL,
  UNIQUE KEY uk_incident_problem (incident_id, code),
  KEY idx_incident_problem (incident_id, status, severity)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- Incident 内的 Web、飞书和系统对话消息。
CREATE TABLE IF NOT EXISTS conversation_message (
  id             BIGINT AUTO_INCREMENT PRIMARY KEY,
  incident_id    BIGINT       NOT NULL,
  run_id         BIGINT       NULL,
  reply_to_id    BIGINT       NULL,
  channel        VARCHAR(16)  NOT NULL,
  role           VARCHAR(16)  NOT NULL,
  actor_id       VARCHAR(128) NULL,
  actor_name     VARCHAR(128) NULL,
  content        TEXT         NOT NULL,
  tool_name      VARCHAR(128) NULL,
  tool_call_id   VARCHAR(128) NULL,
  status         VARCHAR(16)  NOT NULL,
  metadata_json  JSON         NULL,
  created_at     DATETIME(3)  NOT NULL,
  finished_at    DATETIME(3)  NULL,
  KEY idx_incident_message (incident_id, id),
  KEY idx_run_message (run_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- 将飞书消息或线程绑定到 Incident、Run 或 Approval。
CREATE TABLE IF NOT EXISTS im_binding (
  id                BIGINT AUTO_INCREMENT PRIMARY KEY,
  provider          VARCHAR(16)  NOT NULL,
  chat_id           VARCHAR(128) NOT NULL,
  message_id        VARCHAR(128) NOT NULL,
  root_message_id   VARCHAR(128) NULL,
  thread_id         VARCHAR(128) NULL,
  incident_id       BIGINT       NOT NULL,
  run_id            BIGINT       NULL,
  approval_id       BIGINT       NULL,
  message_kind      VARCHAR(32)  NOT NULL,
  created_at        DATETIME(3)  NOT NULL,
  UNIQUE KEY uk_provider_message (provider, message_id),
  KEY idx_im_incident (incident_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- 第三方回调 event_id 去重记录。
CREATE TABLE IF NOT EXISTS integration_event_receipt (
  event_id       VARCHAR(128) PRIMARY KEY,
  provider       VARCHAR(16)  NOT NULL,
  event_type     VARCHAR(64)  NOT NULL,
  processed_at   DATETIME(3)  NOT NULL,
  result         VARCHAR(32)  NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- 一次性 OAuth state/PKCE 记录；只保存加密后的 verifier。
CREATE TABLE IF NOT EXISTS web_oauth_state (
  state_hash                 VARCHAR(128) PRIMARY KEY,
  code_verifier_ciphertext   TEXT         NOT NULL,
  redirect_uri               VARCHAR(512) NOT NULL,
  expires_at                 DATETIME(3)  NOT NULL,
  created_at                 DATETIME(3)  NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- 浏览器会话；token 和 CSRF 值只以哈希形式保存。
CREATE TABLE IF NOT EXISTS web_session (
  id               VARCHAR(64)  PRIMARY KEY,
  token_hash       VARCHAR(128) NOT NULL,
  actor_id         VARCHAR(128) NOT NULL,
  actor_name       VARCHAR(128) NOT NULL,
  tenant_key       VARCHAR(128) NULL,
  csrf_token_hash  VARCHAR(128) NOT NULL,
  expires_at       DATETIME(3)  NOT NULL,
  created_at       DATETIME(3)  NOT NULL,
  last_seen_at     DATETIME(3)  NOT NULL,
  revoked_at       DATETIME(3)  NULL,
  UNIQUE KEY uk_web_session_token (token_hash)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- D10 审批决策审计字段。
ALTER TABLE approval
  ADD COLUMN decided_at DATETIME(3) NULL,
  ADD COLUMN decision_reason TEXT NULL,
  ADD COLUMN decision_source VARCHAR(16) NULL;

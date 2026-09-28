-- Replayable diagnosis input, one row per agent_run. Written after evidence
-- collection and updated with the exact model input BEFORE the model is called;
-- a run whose snapshot cannot be persisted fails and publishes no plan.
-- Rows hold sanitized evidence and tool outputs; restrict read access like the
-- rest of the audit tables.
CREATE TABLE diagnosis_snapshot (
  run_id          BIGINT PRIMARY KEY,
  code_version    VARCHAR(64) NOT NULL,
  evidence_json   JSON NOT NULL,
  model           VARCHAR(128) NULL,
  prompt_sha256   CHAR(64) NULL,
  tools_json      JSON NULL,
  input_text      MEDIUMTEXT NULL,
  tool_calls_json JSON NULL,
  context_json    JSON NULL,
  created_at      DATETIME(3) NOT NULL,
  finished_at     DATETIME(3) NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

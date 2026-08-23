-- 全局当前 LLM 模型；allowlist、URL 和密钥仍只在 YAML 配置中保存。
-- 单例行由 internal/llm.ModelSwitcher 在启动时初始化，手工执行本迁移。
CREATE TABLE IF NOT EXISTS llm_model_selection (
  singleton_id  TINYINT UNSIGNED NOT NULL PRIMARY KEY,
  current_model VARCHAR(128)     NOT NULL,
  updated_at    DATETIME(3)      NOT NULL,
  CONSTRAINT chk_llm_model_selection_singleton CHECK (singleton_id = 1)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

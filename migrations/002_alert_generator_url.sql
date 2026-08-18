-- D03 保留 Alertmanager generatorURL，供 D07 解析原始 PromQL 回放。
SET @generator_url_exists = (
  SELECT COUNT(*)
  FROM information_schema.columns
  WHERE table_schema = DATABASE()
    AND table_name = 'alert'
    AND column_name = 'generator_url'
);
SET @generator_url_sql = IF(
  @generator_url_exists = 0,
  'ALTER TABLE alert ADD COLUMN generator_url TEXT NOT NULL AFTER annotations',
  'SELECT 1'
);
PREPARE generator_url_stmt FROM @generator_url_sql;
EXECUTE generator_url_stmt;
DEALLOCATE PREPARE generator_url_stmt;

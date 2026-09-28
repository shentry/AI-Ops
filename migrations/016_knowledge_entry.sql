-- Knowledge the model can search: repository handbook sections (source=repo,
-- ref "<file>#<heading>", synced from the binary at startup) and reviewed
-- incidents an operator added (source=incident, ref "incident/<id>").
-- Bodies are sanitized before insert and capped at 8 KB by the application.
-- The ngram parser (ngram_token_size defaults to 2) indexes Chinese text.
CREATE TABLE knowledge_entry (
  id         BIGINT AUTO_INCREMENT PRIMARY KEY,
  source     ENUM('repo','incident') NOT NULL,
  ref        VARCHAR(255) NOT NULL,
  title      VARCHAR(255) NOT NULL,
  body       TEXT NOT NULL,
  sha256     CHAR(64) NOT NULL,
  created_by VARCHAR(64) NOT NULL,
  updated_at DATETIME(3) NOT NULL,
  UNIQUE KEY uk_knowledge_ref (ref),
  FULLTEXT KEY ft_knowledge (title, body) WITH PARSER ngram
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

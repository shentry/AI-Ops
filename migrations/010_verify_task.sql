-- One recoverable read-only verification task per genuinely executed approval.
CREATE TABLE verify_task (
  approval_id      BIGINT PRIMARY KEY,
  status           ENUM('pending','running','passed','failed','inconclusive') NOT NULL,
  next_check_at    DATETIME(3) NOT NULL,
  deadline_at      DATETIME(3) NOT NULL,
  claimed_at       DATETIME(3) NULL,
  last_checked_at  DATETIME(3) NULL,
  last_result_json JSON NULL,
  created_at       DATETIME(3) NOT NULL,
  finished_at      DATETIME(3) NULL,
  KEY idx_verify_due (status, next_check_at, approval_id),
  KEY idx_verify_claim (status, claimed_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- Durable delivery of incidents that require human attention. The task is
-- inserted in the same transaction as its source event. Delivery is at-least-once;
-- a lost provider acknowledgement can produce a duplicate with the same event id.
CREATE TABLE notification_task (
  event_id BIGINT PRIMARY KEY,
  attempts INT NOT NULL DEFAULT 0,
  next_attempt_at DATETIME(3) NOT NULL,
  delivered_at DATETIME(3) NULL,
  last_error VARCHAR(512) NULL,
  CONSTRAINT fk_notification_event FOREIGN KEY (event_id) REFERENCES incident_event(id) ON DELETE CASCADE,
  KEY idx_notification_due (delivered_at, next_attempt_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

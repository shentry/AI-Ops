-- D11 执行态：approved 被领取后进入 executing，完成写回 executed/failed。
ALTER TABLE approval MODIFY COLUMN status ENUM('pending','approved','denied','expired','executing','executed','failed') NOT NULL;

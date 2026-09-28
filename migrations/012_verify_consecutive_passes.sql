-- Recovery verification requires consecutive healthy observations.
-- OFFLINE: stop the server before applying. Execution snapshots move to version 2;
-- pending/approved version-1 approvals are expired at claim and unfinished
-- version-1 verification tasks finish as inconclusive (manual check).
ALTER TABLE verify_task
  ADD COLUMN consecutive_passes INT NOT NULL DEFAULT 0 AFTER last_result_json;

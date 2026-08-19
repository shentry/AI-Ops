-- D10 审批完整性：plan_hash 绑定审批单与当时审批的 plan 内容。
-- 执行前重算比对，参数或目标被改过就不许执行（GC-13）。
ALTER TABLE approval ADD COLUMN plan_hash CHAR(64) NOT NULL DEFAULT '' AFTER reason;

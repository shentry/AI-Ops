-- 对话 worker 的领取租约：进程在 LLM 调用中退出后，超时 running 问题可安全回队。
ALTER TABLE conversation_message
  ADD COLUMN claimed_at DATETIME(3) NULL,
  ADD KEY idx_conversation_claim (status, claimed_at);

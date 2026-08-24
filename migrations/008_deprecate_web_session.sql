-- 008: 弃用浏览器会话与 OAuth 表。
--
-- 控制台是显式公开的匿名面（见 README「Web 控制台」），没有登录流程，
-- 因此 005 建的 web_session / web_oauth_state 两张表已无任何写入方，
-- 对应的 Go DAO 与 /auth/feishu/* 路由已从代码中移除。
--
-- 这里只做标记，不做 DROP：表里可能留有历史会话行，删表是不可逆操作，
-- 应由运维在确认无回滚需求后单独执行。确认后的清理语句：
--
--   DROP TABLE IF EXISTS web_session;
--   DROP TABLE IF EXISTS web_oauth_state;

ALTER TABLE web_session
  COMMENT = 'DEPRECATED: 控制台无登录，已无读写方；确认无回滚需求后可 DROP';

ALTER TABLE web_oauth_state
  COMMENT = 'DEPRECATED: 控制台无登录，已无读写方；确认无回滚需求后可 DROP';

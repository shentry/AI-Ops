// 后端返回的是稳定标识符（event_type、status、problem code 等），界面需要中文标签。
// 映射集中在这里而不是散在各组件，避免同一个标识符在不同面板出现两种译法。
// 未知值一律回退原文，宁可显示英文标识符，也不猜错语义。

const statusLabels: Record<string, string> = {
  queued: "排队中",
  running: "进行中",
  started: "已开始",
  succeeded: "成功",
  completed: "已完成",
  passed: "通过",
  sent: "已发送",
  degraded: "降级",
  failed: "失败",
  error: "错误",
  blocked: "阻塞",
  inconclusive: "不可判定",
  pending: "待处理",
  approved: "已批准",
  denied: "已拒绝",
  expired: "已过期",
  executing: "执行中",
  executed: "已执行",
  simulated: "演练完成",
  not_started: "尚未开始",
  not_applicable: "不适用",
  unknown: "未知",
  recorded: "已记录",
  open: "未解决",
  resolved: "已恢复",
  firing: "触发中",
  candidate: "候选",
};

const eventTypeLabels: Record<string, string> = {
  "incident.created": "Incident 已创建",
  "incident.promoted": "升级为正式 Incident",
  "incident.resolved": "Incident 已恢复",
  "run.queued": "诊断已排队",
  "run.started": "诊断开始",
  "run.succeeded": "诊断完成",
  "run.failed": "诊断失败",
  "run.stalled": "诊断卡住",
  "collector.started": "开始采集证据",
  "collector.completed": "证据采集完成",
  "collector.failed": "证据采集失败",
  "llm.started": "模型推理开始",
  "llm.tool_called": "模型调用工具",
  "llm.completed": "模型推理完成",
  "llm.failed": "模型推理失败",
  "guard.evaluated": "Guard 校验",
  "guard.overridden": "Guard 改写了计划",
  "policy.evaluated": "Policy 判定",
  "policy.degraded": "Policy 降级为人工审批",
  "approval.created": "创建审批单",
  "approval.approved": "审批通过",
  "approval.denied": "审批被拒绝",
  "approval.expired": "审批已过期",
  "execution.started": "开始执行动作",
  "execution.completed": "动作执行完成",
  "execution.failed": "动作执行失败",
  "execution.simulated": "演练完成（未执行真实变更）",
  "verify.queued": "恢复验证已排队",
  "verify.started": "开始恢复验证",
  "verify.checked": "完成一次健康观测",
  "verify.passed": "验证通过",
  "verify.failed": "验证失败",
  "verify.inconclusive": "验证不可判定",
  "retry.scheduled": "已安排重新诊断",
  "notification.sent": "通知已发送",
  "notification.failed": "通知发送失败",
  "conversation.asked": "提问已提交",
  "conversation.answered": "Agent 已回答",
  "conversation.failed": "回答失败",
  "escalation.required": "需要人工介入",
};

const problemCodeLabels: Record<string, string> = {
  collector_missing: "缺少证据源",
  collector_failed: "证据采集失败",
  reasoner_timeout: "模型推理超时",
  reasoner_parse_failed: "模型输出无法解析",
  tool_repeated: "工具被重复调用",
  tool_output_truncated: "工具输出被截断",
  run_stalled: "诊断卡住",
  guard_overridden: "Guard 改写了计划",
  policy_blocked: "Policy 拒绝执行",
  policy_degraded: "Policy 降级为人工审批",
  approval_near_expiry: "审批即将过期",
  execution_failed: "动作执行失败",
  verify_failed: "验证失败",
  verify_inconclusive: "验证不可判定",
  notification_failed: "通知发送失败",
  conversation_failed: "问答失败",
};

const phaseLabels: Record<string, string> = {
  ingest: "接入",
  incident: "Incident",
  memory: "记忆",
  evidence: "证据",
  collector: "证据采集",
  llm: "模型推理",
  reasoner: "模型推理",
  guard: "Guard",
  policy: "Policy",
  approval: "审批",
  execution: "执行",
  execute: "执行",
  verify: "验证",
  verification: "验证",
  retry: "重试",
  notification: "通知",
  notify: "通知",
  conversation: "对话",
  tool: "工具",
  system: "系统",
};

const problemSeverityLabels: Record<string, string> = {
  critical: "严重",
  high: "高",
  warning: "警告",
  medium: "中",
  info: "提示",
  low: "低",
};

const roleLabels: Record<string, string> = {
  user: "值班人",
  assistant: "排障 Agent",
  tool: "工具",
  system: "系统",
};

function lookup(table: Record<string, string>, value?: string | null, fallback = "—"): string {
  const key = (value ?? "").trim();
  if (!key) return fallback;
  return table[key.toLowerCase()] ?? table[key] ?? key;
}

export function statusLabel(value?: string | null, fallback = "—"): string {
  return lookup(statusLabels, value, fallback);
}

const verificationStatusLabels: Record<string, string> = {
  not_started: "尚未开始",
  not_applicable: "不适用",
  unknown: "未知",
  pending: "等待恢复验证",
  running: "恢复验证中",
  passed: "目标健康检查通过",
  failed: "观察窗口内未恢复",
  inconclusive: "恢复情况未知，需要人工核查",
};

export function verificationStatusLabel(value: string): string {
  return lookup(verificationStatusLabels, value, "未知");
}

export function eventTypeLabel(value?: string | null): string {
  const key = (value ?? "").trim();
  if (!key) return "事件";
  if (eventTypeLabels[key]) return eventTypeLabels[key];
  // 动态事件名形如 <phase>.started/.completed/.failed：按后缀合成，而不是显示原始标识符。
  const dot = key.lastIndexOf(".");
  if (dot > 0) {
    const phase = phaseLabels[key.slice(0, dot)];
    const action = statusLabels[key.slice(dot + 1)];
    if (phase && action) return `${phase} · ${action}`;
  }
  return key;
}

export function problemCodeLabel(value?: string | null): string {
  const key = (value ?? "").trim();
  if (!key) return "未知问题";
  if (problemCodeLabels[key]) return problemCodeLabels[key];
  // 部分问题码带来源后缀，例如 collector_failed_prom_replay。命中前缀后保留后缀，
  // 否则界面只会显示一串原始标识符，和旁边的 code 标签重复。
  for (const [code, label] of Object.entries(problemCodeLabels)) {
    if (key.startsWith(`${code}_`)) return `${label} · ${key.slice(code.length + 1)}`;
  }
  return key;
}

export function phaseLabel(value?: string | null): string {
  return lookup(phaseLabels, value, "系统");
}

export function problemSeverityLabel(value?: string | null): string {
  return lookup(problemSeverityLabels, value, "提示");
}

export function roleLabel(value?: string | null): string {
  return lookup(roleLabels, value, "参与者");
}

// severity 是告警严重度数字，S1 最重。运维口径通用，保留 S 记法并给出中文后缀。
export function severityLabel(severity: number | string): string {
  if (typeof severity === "number") return `S${severity}`;
  const trimmed = String(severity ?? "").trim();
  return trimmed ? statusLabel(trimmed, trimmed) : "未知";
}

export function durationLabel(milliseconds?: number | null): string {
  if (milliseconds === null || milliseconds === undefined || milliseconds < 0) return "—";
  if (milliseconds < 1000) return `${Math.round(milliseconds)} 毫秒`;
  const seconds = milliseconds / 1000;
  if (seconds < 60) return `${seconds.toFixed(1)} 秒`;
  const minutes = Math.floor(seconds / 60);
  const rest = Math.round(seconds % 60);
  if (minutes < 60) return `${minutes} 分 ${rest} 秒`;
  return `${Math.floor(minutes / 60)} 小时 ${minutes % 60} 分`;
}

export function timeLabel(value?: string | null, withSeconds = false): string {
  if (!value) return "—";
  const date = new Date(value);
  if (Number.isNaN(date.valueOf())) return value;
  return date.toLocaleString("zh-CN", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    ...(withSeconds ? { second: "2-digit" } : {}),
    hour12: false,
  });
}

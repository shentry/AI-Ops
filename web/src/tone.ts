// 后端状态标识符 → 界面色调。只表达状态的好坏与紧迫度，未知值一律中性。
export type Tone = "neutral" | "accent" | "ok" | "warn" | "danger" | "info";

const toneByStatus: Record<string, Tone> = {
  succeeded: "ok",
  completed: "ok",
  passed: "ok",
  stable: "ok",
  resolved: "ok",
  approved: "ok",
  executed: "ok",
  sent: "ok",
  recorded: "neutral",
  running: "info",
  started: "info",
  executing: "info",
  queued: "neutral",
  candidate: "neutral",
  not_started: "neutral",
  not_applicable: "neutral",
  unknown: "neutral",
  pending: "warn",
  blocked: "warn",
  degraded: "warn",
  inconclusive: "warn",
  expired: "warn",
  aborted: "warn",
  partial: "warn",
  missing: "warn",
  open: "warn",
  firing: "danger",
  failed: "danger",
  error: "danger",
  denied: "danger",
  recurred: "danger",
};

export function statusTone(status?: string | null): Tone {
  return toneByStatus[(status ?? "").trim().toLowerCase()] ?? "neutral";
}

// severity 数字越小越严重（S1 最重）；文本严重度按常见口径映射。
export function severityTone(severity: number | string): Tone {
  if (typeof severity === "number") return severity <= 1 ? "danger" : severity === 2 ? "warn" : "info";
  const value = String(severity).toLowerCase();
  if (value === "critical" || value === "high") return "danger";
  if (value === "warning" || value === "medium") return "warn";
  return "info";
}

export const toneText: Record<Tone, string> = {
  neutral: "text-fg-muted",
  accent: "text-accent",
  ok: "text-ok",
  warn: "text-warn",
  danger: "text-danger",
  info: "text-info",
};

export const toneDot: Record<Tone, string> = {
  neutral: "bg-fg-faint",
  accent: "bg-accent",
  ok: "bg-ok",
  warn: "bg-warn",
  danger: "bg-danger",
  info: "bg-info",
};

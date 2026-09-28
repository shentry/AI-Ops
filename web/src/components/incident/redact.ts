// 工具输入输出在浏览器里再脱敏一次：服务端已经脱敏，这里只是纵深防御，
// 防止某个新字段漏网后原样出现在屏幕和截图里。
const sensitiveKey = /(password|passwd|secret|token|authorization|cookie|dsn|credential|private[_-]?key|access[_-]?key)/i;
const maxChars = 4000;

export function safeJSON(value: unknown): string {
  const redacted = redact(value, 0);
  let output: string;
  if (typeof redacted === "string") output = redacted;
  else {
    try {
      output = JSON.stringify(redacted, null, 2) ?? "";
    } catch {
      output = "[内容不可用]";
    }
  }
  return output.length > maxChars ? `${output.slice(0, maxChars)}…` : output;
}

export function isEmptyValue(value: unknown): boolean {
  return value === undefined || value === null || value === "";
}

function redact(value: unknown, depth: number): unknown {
  if (depth > 4) return "[层级过深已省略]";
  if (Array.isArray(value)) return value.slice(0, 50).map((item) => redact(item, depth + 1));
  if (value && typeof value === "object") {
    const result: Record<string, unknown> = {};
    for (const [key, item] of Object.entries(value)) {
      result[key] = sensitiveKey.test(key) ? "[已脱敏]" : redact(item, depth + 1);
    }
    return result;
  }
  return value;
}

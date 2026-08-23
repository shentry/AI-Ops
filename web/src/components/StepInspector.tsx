import { RunStep } from "../api";
import { durationLabel, phaseLabel, statusLabel, timeLabel } from "../labels";

interface StepInspectorProps {
  step: RunStep | null;
}

export function StepInspector({ step }: StepInspectorProps) {
  return (
    <section className="panel inspector-panel" aria-labelledby="inspector-title">
      <div className="panel-heading">
        <div>
          <span className="eyebrow">单步详情</span>
          <h2 id="inspector-title">步骤检查器</h2>
        </div>
        {step && <span className="panel-caption">步骤 #{step.id}</span>}
      </div>
      {!step ? (
        <div className="empty-state compact-empty"><p>点上面流程图里的任一节点，看这一步的输入输出。</p></div>
      ) : (
        <div className="step-detail">
          <div className="step-title-row">
            <div>
              <strong>{step.name || phaseLabel(step.kind)}</strong>
              <span>{phaseLabel(step.kind)} · 第 {step.seq} 步</span>
            </div>
            <span className={`status-pill ${step.error ? "status-failed" : ""}`}>{statusLabel(step.status, step.error ? "失败" : "已记录")}</span>
          </div>
          <dl className="detail-list">
            <div><dt>开始</dt><dd>{timeLabel(step.started_at, true)}</dd></div>
            <div><dt>结束</dt><dd>{timeLabel(step.finished_at, true)}</dd></div>
            <div><dt>耗时</dt><dd>{durationLabel(step.duration_ms)}</dd></div>
          </dl>
          {step.error && <div className="step-error" role="alert">{step.error}</div>}
          <CodeSummary label="输入摘要" value={step.input} />
          <CodeSummary label="输出摘要" value={step.output} />
          {step.truncated && <p className="truncation-note">输出在服务端已被截断，浏览器拿到的不是全文。</p>}
        </div>
      )}
    </section>
  );
}

function CodeSummary({ label, value }: { label: string; value: unknown }) {
  if (value === undefined || value === null || value === "") return null;
  return (
    <div className="code-summary">
      <span>{label}</span>
      <pre>{safeJSON(value)}</pre>
    </div>
  );
}

const sensitiveKey = /(password|passwd|secret|token|authorization|cookie|dsn|credential|private[_-]?key|access[_-]?key)/i;

function safeJSON(value: unknown): string {
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
  return output.length > 3000 ? `${output.slice(0, 3000)}…` : output;
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

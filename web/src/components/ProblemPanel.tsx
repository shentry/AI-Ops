import { Check, TriangleAlert } from "lucide-react";

import { ProblemDTO } from "../api";
import { problemCodeLabel, problemSeverityLabel, statusLabel } from "../labels";

interface ProblemPanelProps {
  problems: ProblemDTO[];
}

export function ProblemPanel({ problems }: ProblemPanelProps) {
  return (
    <section className="panel problem-panel" aria-labelledby="problem-title">
      <div className="panel-heading">
        <div>
          <span className="eyebrow">待关注</span>
          <h2 id="problem-title">当前问题</h2>
        </div>
        <span className={`count-badge ${problems.length ? "has-problems" : ""}`}>{problems.length}</span>
      </div>
      {problems.length === 0 ? (
        <div className="empty-state compact-empty">
          <span className="empty-check"><Check size={15} strokeWidth={2.5} /></span>
          <p>没有待处理问题</p>
          <small>各阶段信号都在预期范围内。</small>
        </div>
      ) : (
        <ul className="problem-list">
          {problems.map((problem) => (
            <li className={`problem-card severity-${problem.severity.toLowerCase()}`} key={problem.id || `${problem.code}-${problem.summary}`}>
              <div className="problem-card-heading">
                <span className="problem-severity"><TriangleAlert size={12} strokeWidth={2.2} />{problemSeverityLabel(problem.severity)}</span>
                <code>{problem.code}</code>
              </div>
              <strong>{problemCodeLabel(problem.code)}</strong>
              <p>{problem.summary || "没有更多说明"}</p>
              {problem.detail !== undefined && problem.detail !== null && (
                <p>{safeDetail(problem.detail)}</p>
              )}
              <div className="problem-card-meta">
                <span>{statusLabel(problem.status)}</span>
                {problem.run_id ? <span>诊断 #{problem.run_id}</span> : <span>Incident 级</span>}
              </div>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

function safeDetail(detail: unknown): string {
  if (typeof detail === "string") return detail;
  try {
    return JSON.stringify(detail, null, 2) ?? "";
  } catch {
    return "详情不可用";
  }
}

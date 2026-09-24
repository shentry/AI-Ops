import { ApprovalDTO } from "../api";
import { statusLabel, timeLabel, verificationStatusLabel } from "../labels";

export function ActionPanel({ action, incidentStatus }: { action: ApprovalDTO | null; incidentStatus: string }) {
  return (
    <section className="panel" aria-labelledby="action-title">
      <div className="panel-heading">
        <div><span className="eyebrow">执行与验证</span><h2 id="action-title">最近变更</h2></div>
        {action && <span className="status-pill">#{action.id}</span>}
      </div>
      {!action ? (
        <div className="empty-state compact-empty"><p>没有变更记录</p><small>诊断完成不代表已执行变更或故障恢复。</small></div>
      ) : (
        <div className="action-details">
          <dl className="detail-list">
            <div><dt>动作</dt><dd>{action.tool_name || "未知"}</dd></div>
            <div><dt>目标</dt><dd>{action.target || "未知"}</dd></div>
            <div><dt>执行状态</dt><dd>{statusLabel(action.status)}</dd></div>
            <div><dt>恢复验证</dt><dd>{verificationStatusLabel(action.verification.status)}</dd></div>
            {action.verification.last_checked_at && <div><dt>最近检查</dt><dd>{timeLabel(action.verification.last_checked_at, true)}</dd></div>}
            {action.verification.deadline_at && <div><dt>观察窗口截止</dt><dd>{timeLabel(action.verification.deadline_at, true)}</dd></div>}
            {action.verification.detail && <div><dt>观测说明</dt><dd>{action.verification.detail}</dd></div>}
            {action.decision_reason && <div><dt>决策 / 执行说明</dt><dd>{action.decision_reason}</dd></div>}
          </dl>
          {action.status === "simulated" && <p className="action-note">演练完成，未执行真实变更。</p>}
          {action.status === "failed" && <p className="action-note">执行失败或结果未知，请查看原因并人工核查。</p>}
          {action.dry_run === null && <p className="action-note">旧版记录，执行上下文未知。</p>}
          {action.verification.status === "passed" && incidentStatus !== "resolved" && (
            <p className="action-note">目标健康检查通过，告警状态尚未同步。不代表所有故障已恢复。</p>
          )}
        </div>
      )}
    </section>
  );
}

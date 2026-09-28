import { Play } from "lucide-react";

import { ApprovalDTO, Receipt } from "../../api";
import { modeLabel, statusLabel, timeLabel, verificationStatusLabel } from "../../labels";
import { statusTone } from "../../tone";
import { Badge, EmptyState, Fact, Facts, Mono, Panel } from "../ui";

// ActionCard 把执行和恢复验证作为两条独立事实展示，状态全部取自服务端，
// 不从诊断成功或告警状态推断「已执行」「已恢复」。
export function ActionCard({ action, incidentStatus }: { action: ApprovalDTO | null; incidentStatus: string }) {
  return (
    <Panel title="最近变更" icon={<Play size={14} />} actions={action ? <span className="font-mono text-[11px] text-fg-faint">#{action.id}</span> : undefined}>
      {!action ? (
        <EmptyState title="没有变更记录" hint="诊断完成不代表已执行变更或故障恢复。" />
      ) : (
        <>
          <div className="mb-3 flex flex-wrap gap-1.5">
            <Badge tone={statusTone(action.status)}>执行 · {statusLabel(action.status)}</Badge>
            <Badge tone={statusTone(action.verification.status)}>验证 · {verificationStatusLabel(action.verification.status)}</Badge>
          </div>
          <Facts>
            <Fact label="动作"><span className="font-mono">{action.tool_name || "未知"}</span></Fact>
            <Fact label="目标">{action.target || "未知"}{action.target_id && <> · <Mono>{action.target_id}</Mono></>}</Fact>
            {action.rule_id && <Fact label="授权规则">{action.rule_id}（{modeLabel(action.mode)}）</Fact>}
            {action.kind === "compensation" && <Fact label="类型">补偿审批 #{action.parent_approval_id} 的动作</Fact>}
            <Fact label="执行状态">{statusLabel(action.status)}</Fact>
            {action.result && <Fact label="执行回执">{receiptLabel(action.result)}</Fact>}
            <Fact label="恢复验证">
              {verificationStatusLabel(action.verification.status)}
              {action.verification.phase === "watch" && action.verification.status === "pending" ? "（恢复后观察期）" : ""}
            </Fact>
            {action.verification.last_checked_at && <Fact label="最近检查">{timeLabel(action.verification.last_checked_at, true)}</Fact>}
            {action.verification.deadline_at && <Fact label="观察窗口截止">{timeLabel(action.verification.deadline_at, true)}</Fact>}
            {action.verification.detail && <Fact label="观测说明">{action.verification.detail}</Fact>}
            {action.decision_reason && <Fact label="决策说明">{action.decision_reason}</Fact>}
          </Facts>
          {action.status === "aborted" && <Note>执行前复验发现对象已变化，没有写入；需要新的决策。</Note>}
          {action.status === "failed" && <Note>执行失败或结果未知，请查看原因并人工核查。</Note>}
          {!action.rule_id && <Note>旧版记录，执行上下文未知。</Note>}
          {(action.verification.status === "passed" || action.verification.status === "stable") && incidentStatus !== "resolved" && (
            <Note>恢复检查通过，告警状态尚未同步。不代表所有故障已恢复。</Note>
          )}
        </>
      )}
    </Panel>
  );
}

function Note({ children }: { children: string }) {
  return <p className="mt-2.5 rounded-md bg-surface-2 px-2.5 py-1.5 text-xs text-fg-muted">{children}</p>;
}

function receiptLabel(receipt: Receipt): string {
  const parts = [receipt.written ? "已写入" : receipt.outcome === "unknown" ? "结果未知" : "未写入"];
  if (receipt.before || receipt.after) parts.push(`${receipt.before || "?"} → ${receipt.after || "?"}`);
  if (receipt.detail) parts.push(receipt.detail);
  if (receipt.error) parts.push(`错误：${receipt.error}`);
  if (receipt.manual_check) parts.push("需要人工核查");
  return parts.join("；");
}

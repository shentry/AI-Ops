import { Check, FlaskConical, ShieldCheck, X } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import { ApprovalDTO } from "../api";
import { statusLabel, timeLabel } from "../labels";

interface ApprovalPanelProps {
	approval: ApprovalDTO | null;
	onDecide: (approvalID: number, approve: boolean, planHash: string, reason: string) => Promise<void>;
	onRequestEvidence?: (approvalID: number) => void;
}

export function ApprovalPanel({ approval, onDecide, onRequestEvidence }: ApprovalPanelProps) {
	const [reason, setReason] = useState("");
	const [busy, setBusy] = useState(false);
	const inFlight = useRef(false);
	const [now, setNow] = useState(() => Date.now());
	const expiresAt = Date.parse(approval?.expires_at ?? "");
	const expired = Number.isFinite(expiresAt) && expiresAt <= now;

	useEffect(() => {
		setReason("");
	}, [approval?.id, approval?.plan_hash]);

	useEffect(() => {
		const timer = window.setInterval(() => setNow(Date.now()), 1000);
		return () => window.clearInterval(timer);
	}, []);

  if (!approval) {
    return (
      <section className="panel approval-panel" aria-labelledby="approval-title">
        <div className="panel-heading">
          <div><span className="eyebrow">变更闸门</span><h2 id="approval-title">人工审批</h2></div>
          <span className="status-pill status-succeeded"><ShieldCheck size={11} />无待批</span>
        </div>
        <div className="empty-state compact-empty"><p>没有待审批的变更</p><small>执行与恢复验证结果见最近变更。</small></div>
      </section>
    );
  }

  const missing = [
    !approval.tool_name.trim() && "动作",
    !approval.target.trim() && "目标",
    approval.scope !== "single_container" && "影响范围",
    !["L2", "L3"].includes(approval.safety_level) && "工具安全等级",
    typeof approval.dry_run !== "boolean" && "演练模式",
    !approval.reason.trim() && "理由",
    !approval.plan_hash.trim() && "计划指纹",
    !Number.isFinite(expiresAt) && "过期时间",
  ].filter(Boolean);
  const blocked = busy || expired || approval.status !== "pending";

  const decide = async (approve: boolean) => {
    if (inFlight.current || blocked || expiresAt <= Date.now() || (approve && missing.length > 0) || !approval.plan_hash.trim() || (!approve && !reason.trim())) return;
    // Keep the clicked snapshot, not a hash read from a subsequent SSE refresh.
    const { id, plan_hash } = approval;
    inFlight.current = true;
    setBusy(true);
    try {
      await onDecide(id, approve, plan_hash, reason.trim());
    } finally {
      inFlight.current = false;
      setBusy(false);
    }
  };

  return (
    <section className="panel approval-panel" aria-labelledby="approval-title">
      <div className="panel-heading">
        <div><span className="eyebrow">变更闸门</span><h2 id="approval-title">等待你审批</h2></div>
        <span className={`status-pill ${expired ? "status-failed" : "status-blocked"}`}>{expired ? "已过期" : statusLabel(approval.status)}</span>
      </div>
      <div className="approval-card">
        <div className="approval-action"><span>请求执行</span><strong>{approval.tool_name || "变更动作"}</strong></div>
        <dl className="detail-list approval-details">
          <div><dt>目标</dt><dd>{approval.target || "未知"}</dd></div>
          <div><dt>影响范围</dt><dd>{approval.scope === "single_container" ? "单个容器" : approval.scope || "未知"}</dd></div>
          <div><dt>工具安全等级</dt><dd>{approval.safety_level || "未知"}</dd></div>
          <div><dt>理由</dt><dd>{approval.reason || "未填写理由"}</dd></div>
          <div><dt>计划指纹</dt><dd><code>{approval.plan_hash || "未知"}</code></dd></div>
          <div><dt>过期时间</dt><dd>{timeLabel(approval.expires_at)}</dd></div>
          <div><dt>演练模式</dt><dd>{approval.dry_run === null ? "未知" : approval.dry_run ? "是（不会真的执行）" : "否（会真的执行）"}</dd></div>
        </dl>
      </div>
      {approval.status === "pending" && missing.length > 0 && (
        <div className="inline-error approval-error" role="alert">审批信息不完整或无效：{missing.join("、")}。无法批准，请重新诊断生成完整计划。</div>
      )}
      <label className="field-label" htmlFor={`approval-reason-${approval.id}`}>决策备注 {approval.status === "pending" && <span>（拒绝时必填）</span>}</label>
      <textarea
        id={`approval-reason-${approval.id}`}
        className="text-input decision-input"
        value={reason}
        onChange={(event) => setReason(event.target.value.slice(0, 2000))}
        placeholder="写下为什么这个决策是安全的…"
        rows={3}
        disabled={blocked}
      />
      <div className="approval-actions">
        <button className="button danger" type="button" disabled={blocked || !approval.plan_hash.trim() || !reason.trim()} onClick={() => void decide(false)}>
          <X size={13} />
          拒绝
        </button>
        <button className="button primary" type="button" disabled={blocked || missing.length > 0} onClick={() => void decide(true)}>
          <Check size={13} />
          {approval.dry_run === true ? "批准演练" : "批准执行"}
        </button>
      </div>
      {onRequestEvidence && approval.status === "pending" && (
        <button className="text-button" type="button" disabled={busy} onClick={() => onRequestEvidence(approval.id)}>
          <FlaskConical size={12} />
          先补充证据再决定
        </button>
      )}
    </section>
  );
}

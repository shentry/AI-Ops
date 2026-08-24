import { Check, FlaskConical, ShieldCheck, X } from "lucide-react";
import { useEffect, useMemo, useState } from "react";

import { ApprovalDTO } from "../api";
import { statusLabel, timeLabel } from "../labels";

interface ApprovalPanelProps {
	approval: ApprovalDTO | null;
	onDecide: (approvalID: number, approve: boolean, reason: string) => Promise<void>;
	onRequestEvidence?: (approvalID: number) => void;
}

export function ApprovalPanel({ approval, onDecide, onRequestEvidence }: ApprovalPanelProps) {
	const [reason, setReason] = useState("");
	const [busy, setBusy] = useState(false);
	const [now, setNow] = useState(() => Date.now());
	const expiresAt = useMemo(() => approval?.expires_at ? new Date(approval.expires_at) : null, [approval?.expires_at]);
	const expired = Boolean(expiresAt && !Number.isNaN(expiresAt.valueOf()) && expiresAt.valueOf() <= now);

	useEffect(() => {
		setReason("");
		setBusy(false);
	}, [approval?.id]);

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
        <div className="empty-state compact-empty"><p>没有待审批的变更</p><small>Policy 尚未拦下任何动作。</small></div>
      </section>
    );
  }

  const decide = async (approve: boolean) => {
    if (expired || approval.status !== "pending" || (!approve && !reason.trim())) return;
    setBusy(true);
    try {
      await onDecide(approval.id, approve, reason.trim());
      if (!approve) setReason("");
    } finally {
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
          {approval.target && <div><dt>目标</dt><dd>{approval.target}</dd></div>}
          {approval.scope && <div><dt>影响范围</dt><dd>{approval.scope}</dd></div>}
          {approval.risk && <div><dt>风险</dt><dd>{approval.risk}</dd></div>}
          <div><dt>理由</dt><dd>{approval.reason || "未填写理由"}</dd></div>
          <div><dt>计划指纹</dt><dd><code>{shortHash(approval.plan_hash)}</code></dd></div>
          <div><dt>过期时间</dt><dd>{timeLabel(approval.expires_at)}</dd></div>
          {approval.dry_run !== undefined && <div><dt>演练模式</dt><dd>{approval.dry_run ? "是（不会真的执行）" : "否（会真的执行）"}</dd></div>}
        </dl>
      </div>
      <label className="field-label" htmlFor={`approval-reason-${approval.id}`}>决策备注 {approval.status === "pending" && <span>（拒绝时必填）</span>}</label>
      <textarea
        id={`approval-reason-${approval.id}`}
        className="text-input decision-input"
        value={reason}
        onChange={(event) => setReason(event.target.value.slice(0, 2000))}
        placeholder="写下为什么这个决策是安全的…"
        rows={3}
        disabled={busy || expired || approval.status !== "pending"}
      />
      <div className="approval-actions">
        <button className="button danger" type="button" disabled={busy || expired || approval.status !== "pending" || !reason.trim()} onClick={() => void decide(false)}>
          <X size={13} />
          拒绝
        </button>
        <button className="button primary" type="button" disabled={busy || expired || approval.status !== "pending"} onClick={() => void decide(true)}>
          <Check size={13} />
          批准执行
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

function shortHash(value: string): string {
  if (!value) return "—";
  return value.length > 18 ? `${value.slice(0, 9)}…${value.slice(-7)}` : value;
}

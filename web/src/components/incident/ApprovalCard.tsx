import { Check, FlaskConical, ShieldCheck, UserCheck, X } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import { ApprovalDTO } from "../../api";
import { checkLabel, modeLabel, relativeTime, statusLabel, timeLabel } from "../../labels";
import { Badge, Button, EmptyState, Fact, Facts, FieldLabel, Mono, Panel, TextArea } from "../ui";

interface ApprovalCardProps {
  approval: ApprovalDTO | null;
  readOnly: boolean;
  onDecide: (approvalID: number, approve: boolean, planHash: string, reason: string) => Promise<void>;
  onRequestEvidence: () => void;
}

// ApprovalCard 是变更闸门。只有完整、有效的 manual 快照能被批准；提交的
// plan_hash 永远来自被点击的那份快照，而不是之后 SSE 刷新回来的新快照。
export function ApprovalCard({ approval, readOnly, onDecide, onRequestEvidence }: ApprovalCardProps) {
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
      <Panel title="人工审批" icon={<UserCheck size={14} />} actions={<Badge tone="ok"><ShieldCheck size={11} aria-hidden="true" />无待批</Badge>}>
        <EmptyState title="没有待审批的变更" hint="执行与恢复验证结果见最近变更。" />
      </Panel>
    );
  }

  const missing = [
    !approval.tool_name.trim() && "动作",
    !approval.target.trim() && "目标",
    !approval.target_id.trim() && "目标身份",
    !approval.rule_id.trim() && "授权规则",
    approval.mode !== "manual" && "人工审批模式",
    approval.checks.length === 0 && "恢复验证",
    !approval.reason.trim() && "理由",
    !approval.plan_hash.trim() && "计划指纹",
    !Number.isFinite(expiresAt) && "过期时间",
  ].filter(Boolean);
  const pending = approval.status === "pending";
  const blocked = readOnly || busy || expired || !pending;

  const decide = async (approve: boolean) => {
    if (inFlight.current || blocked || expiresAt <= Date.now() || (approve && missing.length > 0) || !approval.plan_hash.trim() || (!approve && !reason.trim())) return;
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
    <Panel
      title="等待你审批"
      icon={<UserCheck size={14} />}
      tone={pending && !expired ? "warn" : undefined}
      actions={<Badge tone={expired ? "danger" : "warn"} dot={pending && !expired} pulse={pending && !expired}>{expired ? "已过期" : statusLabel(approval.status)}</Badge>}
    >
      <div className="mb-3 rounded-md border border-line-soft bg-canvas/50 px-3 py-2">
        <p className="text-[11px] text-fg-faint">请求执行</p>
        <p className="font-mono text-[14px] font-medium text-fg">{approval.tool_name || "变更动作"}</p>
      </div>
      <Facts>
        <Fact label="目标">{approval.target || "未知"}</Fact>
        <Fact label="目标身份"><Mono>{approval.target_id || "未知"}</Mono></Fact>
        <Fact label="授权规则">{approval.rule_id ? `${approval.rule_id}（${modeLabel(approval.mode)}）` : "未知"}</Fact>
        <Fact label="执行后验证">{approval.checks.length ? approval.checks.map(checkLabel).join("、") : "未知"}</Fact>
        {approval.compensation && <Fact label="失败时补偿">{approval.compensation}</Fact>}
        <Fact label="理由">{approval.reason || "未填写理由"}</Fact>
        <Fact label="计划指纹"><Mono className="text-[11px]">{approval.plan_hash || "未知"}</Mono></Fact>
        <Fact label="过期时间">
          {timeLabel(approval.expires_at)}
          {Number.isFinite(expiresAt) && !expired && <span className="ml-1.5 text-fg-faint">（{relativeTime(approval.expires_at, now)}）</span>}
        </Fact>
      </Facts>

      {pending && missing.length > 0 && (
        <p role="alert" className="mt-3 rounded-md border border-danger/35 bg-danger/10 px-2.5 py-2 text-xs text-danger">
          审批信息不完整或无效：{missing.join("、")}。无法批准，请重新诊断生成完整计划。
        </p>
      )}

      <div className="mt-3">
        <FieldLabel htmlFor={`approval-reason-${approval.id}`} hint={pending ? "（拒绝时必填）" : undefined}>决策备注 </FieldLabel>
        <TextArea
          id={`approval-reason-${approval.id}`}
          value={reason}
          onChange={(event) => setReason(event.target.value.slice(0, 2000))}
          placeholder="写下为什么这个决策是安全的…"
          rows={2}
          disabled={blocked}
        />
      </div>
      <div className="mt-2.5 flex gap-2">
        <Button variant="danger" className="flex-1" disabled={blocked || !approval.plan_hash.trim() || !reason.trim()} onClick={() => void decide(false)}>
          <X size={13} aria-hidden="true" />
          拒绝
        </Button>
        <Button variant="primary" className="flex-1" disabled={blocked || missing.length > 0} onClick={() => void decide(true)}>
          <Check size={13} aria-hidden="true" />
          批准执行
        </Button>
      </div>
      {readOnly && pending && <p className="mt-2 text-xs text-fg-faint">只读账号不能审批。</p>}
      {pending && (
        <button type="button" disabled={busy || readOnly} onClick={onRequestEvidence} className="mt-2 inline-flex cursor-pointer items-center gap-1 text-xs text-fg-muted hover:text-fg disabled:cursor-not-allowed disabled:opacity-50">
          <FlaskConical size={12} aria-hidden="true" />
          先补充证据再决定
        </button>
      )}
    </Panel>
  );
}

import { BellRing, CheckCheck, Database, LucideIcon, Play, Scale, ShieldCheck, Sparkles, UserCheck } from "lucide-react";

import { FlowNode } from "../../api";
import { durationLabel, statusLabel } from "../../labels";
import { statusTone, toneDot, toneText } from "../../tone";
import { cx } from "../ui";

const stages: { key: string; label: string; aliases: string[]; Icon: LucideIcon }[] = [
  { key: "alert", label: "告警接入", aliases: ["alert", "incident", "ingest"], Icon: BellRing },
  { key: "evidence", label: "证据采集", aliases: ["evidence", "collector", "collectors"], Icon: Database },
  { key: "reasoner", label: "模型诊断", aliases: ["reasoner", "llm", "diagnose"], Icon: Sparkles },
  { key: "guard", label: "Guard 校验", aliases: ["guard"], Icon: ShieldCheck },
  { key: "policy", label: "Policy 判定", aliases: ["policy"], Icon: Scale },
  { key: "approval", label: "人工审批", aliases: ["approval"], Icon: UserCheck },
  { key: "execute", label: "执行动作", aliases: ["execute", "execution"], Icon: Play },
  { key: "verify", label: "恢复验证", aliases: ["verify", "verification"], Icon: CheckCheck },
];

// FlowStrip 按服务端给的阶段状态画处理链路。缺失的阶段显示「排队中」，
// 不根据其他阶段推测它的状态。
export function FlowStrip({ nodes, onSelect }: { nodes: FlowNode[]; onSelect: (node: FlowNode) => void }) {
  return (
    <section aria-labelledby="flow-title" className="rounded-lg border border-line bg-surface">
      <header className="flex items-center gap-2 px-4 pt-2.5">
        <h2 id="flow-title" className="text-[13px] font-semibold">处理流程</h2>
        <span className="text-xs text-fg-faint">点节点查看这一步的诊断轨迹</span>
      </header>
      <ol className="grid grid-cols-2 gap-1.5 px-3 pb-3 pt-2.5 sm:grid-cols-4 2xl:grid-cols-8">
        {stages.map((stage, index) => {
          const node = findNode(nodes, stage.key, stage.aliases) ?? { id: stage.key, key: stage.key, name: stage.label, status: "queued" };
          const tone = statusTone(node.status);
          const running = node.status === "running";
          return (
            <li key={stage.key} className="flex min-w-0 items-stretch">
              <button
                type="button"
                onClick={() => onSelect(node)}
                title={node.error ?? `${stage.label}：${statusLabel(node.status, "排队中")}`}
                className={cx(
                  "group flex w-full cursor-pointer flex-col gap-1 rounded-md border px-2.5 py-2 text-left transition-colors",
                  tone === "neutral" ? "border-line-soft bg-canvas/40 hover:border-line" : "border-line bg-surface-2/50 hover:border-fg-faint/50",
                )}
              >
                <span className="flex items-center gap-1.5">
                  <stage.Icon size={13} aria-hidden="true" className={cx("shrink-0", toneText[tone])} />
                  <span className="tabular text-[10.5px] text-fg-faint">0{index + 1}</span>
                  <span aria-hidden="true" className={cx("ml-auto size-1.5 rounded-full", toneDot[tone], running && "animate-pulse-dot")} />
                </span>
                <span className="text-[12.5px] font-medium text-fg">{stage.label}</span>
                <span className={cx("text-[11.5px]", toneText[tone])}>{statusLabel(node.status, "排队中")}</span>
                <span className="tabular text-[11px] text-fg-faint">{durationLabel(node.duration_ms)}</span>
                {node.error && <span className="line-clamp-2 text-[11px] text-danger">{node.error}</span>}
              </button>
            </li>
          );
        })}
      </ol>
    </section>
  );
}

function findNode(nodes: FlowNode[], key: string, aliases: string[]): FlowNode | undefined {
  return nodes.find((node) => node.key === key || node.id === key)
    ?? nodes.find((node) => {
      const value = `${node.key ?? ""} ${node.id} ${node.name} ${node.phase ?? ""}`.toLowerCase();
      return aliases.some((alias) => value.includes(alias));
    });
}

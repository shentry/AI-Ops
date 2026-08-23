import { FlowNode } from "../api";
import { durationLabel, statusLabel } from "../labels";

interface FlowGraphProps {
  nodes: FlowNode[];
  onSelectNode?: (node: FlowNode) => void;
}

const stages = [
  { key: "alert", label: "告警接入", aliases: ["alert", "incident", "ingest"] },
  { key: "evidence", label: "证据采集", aliases: ["evidence", "collector", "collectors"] },
  { key: "reasoner", label: "模型诊断", aliases: ["reasoner", "llm", "diagnose"] },
  { key: "guard", label: "Guard 校验", aliases: ["guard"] },
  { key: "policy", label: "Policy 判定", aliases: ["policy"] },
  { key: "approval", label: "人工审批", aliases: ["approval"] },
  { key: "execute", label: "执行动作", aliases: ["execute", "execution"] },
  { key: "verify", label: "结果验证", aliases: ["verify", "verification"] },
];

export function FlowGraph({ nodes, onSelectNode }: FlowGraphProps) {
  return (
    <section className="panel flow-panel" aria-labelledby="flow-title">
      <div className="panel-heading">
        <div>
          <span className="eyebrow">实时链路</span>
          <h2 id="flow-title">处理流程</h2>
        </div>
        <span className="panel-caption">点击节点看该步详情</span>
      </div>
      <div className="flow-graph" role="list" aria-label="Incident 处理阶段">
        {stages.map((stage, index) => {
          const node = findNode(nodes, stage.key, stage.aliases) ?? defaultNode(stage.key, stage.label);
          return (
            <div className="flow-slot" key={stage.key} role="listitem">
              <button
                className={`flow-node status-${statusName(node.status)}`}
                type="button"
                onClick={() => onSelectNode?.(node)}
                title={node.error ?? `${stage.label}：${statusLabel(node.status, "排队中")}`}
              >
                <span className="flow-node-topline">
                  <span className="flow-node-index">0{index + 1}</span>
                  <span className="status-dot" />
                </span>
                <strong>{stage.label}</strong>
                <span className="flow-node-status">{statusLabel(node.status, "排队中")}</span>
                <span className="flow-node-duration">{durationLabel(node.duration_ms)}</span>
                {node.owner && <span className="flow-node-owner">{node.owner}</span>}
                {node.error && <span className="flow-node-error">{node.error}</span>}
              </button>
              {index < stages.length - 1 && <span className="flow-arrow" aria-hidden="true">→</span>}
            </div>
          );
        })}
      </div>
      <div className="flow-legend" aria-label="状态图例">
        {(["queued", "running", "succeeded", "degraded", "failed", "blocked", "inconclusive"] as const).map((status) => (
          <span key={status}><i className={`legend-dot status-${status}`} />{statusLabel(status)}</span>
        ))}
      </div>
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

function defaultNode(key: string, label: string): FlowNode {
  return { id: key, key, name: label, status: "queued" };
}

function statusName(value: string): string {
  const allowed = new Set(["queued", "running", "succeeded", "degraded", "failed", "blocked", "inconclusive"]);
  return allowed.has(value) ? value : "queued";
}

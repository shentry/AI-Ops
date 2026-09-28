// Copied from ongridio/ongrid@81e08b5efbe9ccd9a5781574d5f3ba10215eccac,
// web/src/pages/Topology.tsx (GraphTab and NodeDetailDrawer) — Copyright the
// ongrid authors, AGPL-3.0 (see LICENSE and NOTICE).
// Modified 2026-09-28 for oncall-agent: the graph is read-only and comes from
// GET /api/v1/topology (declared in config, states observed live), so node and
// relation editing, type chips, app focus and the orphan and relation-type
// filters are dropped; the side column lists the nodes and, for a selected
// node, shows its state, container facts, firing alerts, relations and a
// Monitor link; ?incident=ID highlights that incident's nodes; the page polls
// every 15 seconds; strings are Chinese-only.
import { Activity, RefreshCw, X } from 'lucide-react';
import { useCallback, useEffect, useMemo, useState } from 'react';
import { Link, useSearchParams } from 'react-router';

import { ApiError, getTopology, type NodeState, type Topology as TopologyData, type TopologyEdge, type TopologyNode } from '../api';
import { PageBody, PageHeader } from '../components/layout/PageHeader';
import { KIND_LABEL, STATE_LABEL, TopologyGraph } from '../components/topology/Graph';
import { Badge, Button, Fact, Facts, Mono, Notice, Skeleton, buttonClass } from '../components/ui';
import { timeLabel } from '../labels';
import { usePoll } from '../lib/usePoll';
import type { Tone } from '../tone';

const STATE_TONE: Record<NodeState, Tone> = { up: 'ok', down: 'danger', missing: 'warn', unknown: 'neutral' };

export function Topology() {
  const [searchParams] = useSearchParams();
  const incident = Math.max(0, Math.trunc(Number(searchParams.get('incident')) || 0));
  const [data, setData] = useState<TopologyData | null>(null);
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [selectedID, setSelectedID] = useState<string | null>(null);

  const fetchAll = useCallback(async () => {
    setLoading(true);
    try {
      setData(await getTopology(incident));
      setErr(null);
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : '拓扑加载失败');
    } finally {
      setLoading(false);
    }
  }, [incident]);

  useEffect(() => {
    void fetchAll();
  }, [fetchAll]);
  // The server re-observes at most every 15 seconds.
  usePoll(fetchAll, 15_000);

  const highlight = useMemo(() => new Set(data?.highlight ?? []), [data]);
  const selected = data?.nodes.find((n) => n.id === selectedID) ?? null;

  return (
    <>
      <PageHeader
        title="拓扑"
        description="sub2api 与依赖的实时状态：容器来自 docker inspect，健康与告警来自 Prometheus。依赖故障时，重启下游会被 Guard 拒绝并转人工。"
        back={incident > 0 ? { to: `/incidents/${incident}`, label: `事件 #${incident}` } : undefined}
        meta={data && (
          <>
            <span>{data.nodes.length} 节点 · {data.edges.length} 关系</span>
            <span>检查于 {timeLabel(data.checked_at, true)}</span>
            <span>实线：依赖，故障会传播 · 虚线：运行于</span>
          </>
        )}
        actions={
          <Button onClick={fetchAll} disabled={loading}>
            <RefreshCw size={13} className={loading ? 'animate-spin' : ''} aria-hidden="true" />
            刷新
          </Button>
        }
      />
      <PageBody wide>
        {err && <Notice tone="danger">{err}</Notice>}
        {data?.alerts_error && <Notice tone="warn">告警读取失败，节点上没有告警不代表没有触发中的告警。</Notice>}
        {incident > 0 && data && (
          <Notice tone="info">
            {highlight.size > 0 ? `已高亮事件 #${incident} 的告警所涉及的节点。` : `事件 #${incident} 的告警没有映射到拓扑节点。`}
          </Notice>
        )}
        {!data ? (
          !err && <Skeleton className="h-[480px]" />
        ) : (
          <div className="flex flex-col gap-3 lg:flex-row">
            <div className="h-[420px] min-w-0 overflow-hidden rounded-lg border border-line lg:h-[calc(100vh-220px)] lg:min-h-[420px] lg:flex-1">
              <TopologyGraph nodes={data.nodes} edges={data.edges} selectedID={selectedID} highlight={highlight} onSelect={setSelectedID} />
            </div>
            {selected ? (
              <NodeDetailDrawer node={selected} nodes={data.nodes} edges={data.edges} onClose={() => setSelectedID(null)} />
            ) : (
              <NodeList nodes={data.nodes} highlight={highlight} onSelect={setSelectedID} />
            )}
          </div>
        )}
      </PageBody>
    </>
  );
}

function NodeList({ nodes, highlight, onSelect }: { nodes: TopologyNode[]; highlight: Set<string>; onSelect(id: string): void }) {
  return (
    <ul aria-label="节点" className="divide-y divide-line-soft overflow-hidden rounded-lg border border-line bg-surface lg:w-80 lg:self-start">
      {nodes.map((node) => (
        <li key={node.id}>
          <button type="button" onClick={() => onSelect(node.id)} className="flex w-full cursor-pointer items-center justify-between gap-2 px-3 py-2 text-left hover:bg-surface-2">
            <span className="min-w-0">
              <span className="block truncate text-[13px] text-fg">{node.id}</span>
              <span className="text-[11px] text-fg-faint">{KIND_LABEL[node.kind] ?? node.kind}</span>
            </span>
            <span className="flex shrink-0 items-center gap-1.5">
              {highlight.has(node.id) && <Badge tone="accent">事件</Badge>}
              {node.alerts.length > 0 && <Badge tone="danger">{node.alerts.length} 告警</Badge>}
              <Badge tone={STATE_TONE[node.state]} dot>{STATE_LABEL[node.state]}</Badge>
            </span>
          </button>
        </li>
      ))}
    </ul>
  );
}

// monitorHref opens the board that charts the node: the service board for the
// service itself, the dependency and host board for everything else.
function monitorHref(node: TopologyNode): string {
  return `/monitor?board=${node.kind === 'service' ? 'sub2api' : 'dependencies-host'}`;
}

function NodeDetailDrawer({ node, nodes, edges, onClose }: {
  node: TopologyNode;
  nodes: TopologyNode[];
  edges: TopologyEdge[];
  onClose(): void;
}) {
  const states = new Map(nodes.map((n) => [n.id, n.state]));
  const relations = edges.filter((edge) => edge.from === node.id || edge.to === node.id);
  return (
    <aside aria-label={`节点 ${node.id}`} className="flex shrink-0 flex-col rounded-lg border border-line bg-surface lg:w-80 lg:self-start">
      <div className="flex items-start justify-between gap-2 border-b border-line px-4 py-3">
        <div className="min-w-0">
          <div className="truncate text-sm font-semibold text-fg">{node.id}</div>
          <div className="mt-1 flex items-center gap-1.5 text-[11px] text-fg-muted">
            <Badge tone={STATE_TONE[node.state]} dot>{STATE_LABEL[node.state]}</Badge>
            {KIND_LABEL[node.kind] ?? node.kind}
          </div>
        </div>
        <Button variant="ghost" size="xs" onClick={onClose} aria-label="关闭">
          <X size={14} aria-hidden="true" />
        </Button>
      </div>
      <div className="space-y-4 px-4 py-3">
        {node.detail && <p className="text-[12.5px] text-fg-muted">{node.detail}</p>}
        {node.container && (
          <Facts>
            <Fact label="容器"><Mono>{node.container}</Mono></Fact>
            {node.container_id && <Fact label="容器 ID"><Mono>{node.container_id.slice(0, 12)}</Mono></Fact>}
            {node.container_id && <Fact label="重启次数">{node.restart_count}</Fact>}
            {node.oom_killed && <Fact label="OOM">上次退出因内存不足被杀</Fact>}
          </Facts>
        )}
        <section>
          <h3 className="mb-1.5 text-[11px] font-medium text-fg-faint">触发中的告警（{node.alerts.length}）</h3>
          {node.alerts.length === 0 ? (
            <p className="text-[12px] text-fg-faint">无</p>
          ) : (
            <ul className="space-y-1">{node.alerts.map((alert) => <li key={alert}><Mono>{alert}</Mono></li>)}</ul>
          )}
        </section>
        <section>
          <h3 className="mb-1.5 text-[11px] font-medium text-fg-faint">关系（{relations.length}）</h3>
          <ul className="space-y-1 text-[12px] text-fg-muted">
            {relations.map((edge) => {
              const other = edge.from === node.id ? edge.to : edge.from;
              const verb = edge.type === 'depends_on' ? '依赖' : '运行于';
              return (
                <li key={`${edge.from}-${edge.type}-${edge.to}`} className="flex items-center justify-between gap-2">
                  <span>{edge.from === node.id ? `${verb} ${other}` : `${other} ${verb}本节点`}</span>
                  <Badge tone={STATE_TONE[states.get(other) ?? 'unknown']}>{STATE_LABEL[states.get(other) ?? 'unknown']}</Badge>
                </li>
              );
            })}
          </ul>
        </section>
        <Link to={monitorHref(node)} className={buttonClass('secondary', 'sm')}>
          <Activity size={13} aria-hidden="true" />
          查看监控
        </Link>
      </div>
    </aside>
  );
}

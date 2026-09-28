// Copied from ongridio/ongrid@81e08b5efbe9ccd9a5781574d5f3ba10215eccac,
// web/src/components/topology/Graph.tsx — Copyright the ongrid authors,
// AGPL-3.0 (see LICENSE and NOTICE).
// Modified 2026-09-28 for oncall-agent: nodes are this console's declared
// topology with live states; node borders, edges and the canvas use the theme
// tokens (so both themes need no color tables or theme hook); a node shows its
// state and firing-alert count and can carry the incident highlight; edges are
// depends_on (solid, red when the dependency is down or missing) and runs_on
// (dashed); nodes are kept in state through applyNodeChanges so they carry
// React Flow's measurements, which @xyflow/react 12.12 needs to keep handle
// bounds (and so edges) across re-renders. The MiniMap, relation labels,
// orphan and relation-type filters and the resource-column alignment are
// dropped.
import { useCallback, useEffect, useMemo, useState } from 'react';
import {
  applyNodeChanges,
  Background,
  BackgroundVariant,
  BaseEdge,
  Controls,
  Edge,
  EdgeProps,
  getSmoothStepPath,
  Handle,
  MarkerType,
  Node,
  NodeChange,
  NodeProps,
  Position,
  ReactFlow,
  useNodes,
} from '@xyflow/react';
import '@xyflow/react/dist/style.css';
import dagre from '@dagrejs/dagre';

import type { NodeState, TopologyEdge, TopologyNode } from '../../api';
import { facingSides, routeAroundNodes } from './route';

const NODE_WIDTH = 160;
const NODE_HEIGHT = 44;
const HIDDEN_HANDLE_STYLE = { visibility: 'hidden' as const };

export const STATE_LABEL: Record<NodeState, string> = { up: '正常', down: '故障', missing: '不存在', unknown: '未知' };
const STATE_COLOR: Record<NodeState, string> = { up: 'var(--ok)', down: 'var(--danger)', missing: 'var(--warn)', unknown: 'var(--fg-faint)' };
export const KIND_LABEL: Record<string, string> = { service: '服务', container: '容器', datastore: '数据存储', upstream: '上游', host: '主机' };

type NodeData = { node: TopologyNode; selected: boolean; highlighted: boolean };

// CustomTopologyNode renders one node tile inside react-flow.
function CustomTopologyNode(props: NodeProps) {
  const { node, selected, highlighted } = props.data as NodeData;
  const color = STATE_COLOR[node.state];
  return (
    <div
      data-testid={`topology-node-${node.id}`}
      style={{
        position: 'relative',
        width: NODE_WIDTH,
        height: NODE_HEIGHT,
        background: 'var(--surface)',
        border: `1.5px solid ${color}`,
        borderRadius: 8,
        padding: '6px 10px',
        boxShadow: selected ? '0 0 0 2px var(--fg)' : highlighted ? '0 0 0 3px var(--ring)' : undefined,
        color: 'var(--fg)',
        fontSize: 11,
        display: 'flex',
        flexDirection: 'column',
        justifyContent: 'center',
        overflow: 'hidden',
      }}
    >
      {Object.values(Position).flatMap((position) => (['source', 'target'] as const).map((type) => (
        <Handle key={`${type}-${position}`} id={`${type}-${position}`} type={type} position={position} style={HIDDEN_HANDLE_STYLE} />
      )))}
      <div
        style={{
          fontWeight: 500,
          fontSize: 12,
          whiteSpace: 'nowrap',
          overflow: 'hidden',
          textOverflow: 'ellipsis',
          paddingRight: node.alerts.length > 0 ? 20 : 0,
        }}
      >
        {node.id}
      </div>
      <div style={{ fontSize: 10, color: 'var(--fg-muted)' }}>
        {KIND_LABEL[node.kind] ?? node.kind} · <span style={{ color }}>{STATE_LABEL[node.state]}</span>
      </div>
      {node.alerts.length > 0 && (
        <span
          title={node.alerts.join('\n')}
          aria-label={`${node.alerts.length} 条告警`}
          style={{
            position: 'absolute',
            top: 5,
            right: 6,
            minWidth: 16,
            height: 16,
            padding: '0 4px',
            borderRadius: 8,
            background: 'var(--danger)',
            color: 'var(--accent-fg)',
            fontSize: 10,
            fontWeight: 600,
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'center',
          }}
        >
          {node.alerts.length}
        </span>
      )}
    </div>
  );
}

const nodeTypes = { topo: CustomTopologyNode };

function AvoidingEdge(props: EdgeProps) {
  const nodes = useNodes();
  const start = { x: props.sourceX, y: props.sourceY };
  const end = { x: props.targetX, y: props.targetY };
  const offset = (point: { x: number; y: number }, side: Position) => ({
    x: point.x + (side === Position.Left ? -24 : side === Position.Right ? 24 : 0),
    y: point.y + (side === Position.Top ? -24 : side === Position.Bottom ? 24 : 0),
  });
  const route = routeAroundNodes(
    offset(start, props.sourcePosition),
    offset(end, props.targetPosition),
    nodes.map((n) => ({ ...n.position, width: n.width ?? NODE_WIDTH, height: n.height ?? NODE_HEIGHT })),
  );
  let [path] = getSmoothStepPath(props);
  if (route) {
    path = [start, ...route, end].map((p, i) => `${i ? 'L' : 'M'} ${p.x} ${p.y}`).join(' ');
  }
  return <BaseEdge id={props.id} path={path} style={props.style} markerEnd={props.markerEnd} />;
}

const edgeTypes = { avoiding: AvoidingEdge };

type Props = {
  nodes: TopologyNode[];
  edges: TopologyEdge[];
  selectedID: string | null;
  highlight: Set<string>;
  onSelect(id: string): void;
};

export function TopologyGraph({ nodes, edges, selectedID, highlight, onSelect }: Props) {
  const { rfNodes: layoutNodes, rfEdges: layoutEdges } = useMemo(
    () => layoutGraph(nodes, edges, selectedID, highlight),
    [nodes, edges, selectedID, highlight],
  );
  // React Flow is controlled here because the graph is rebuilt from the
  // server-side topology data. A rebuilt node keeps its dragged position and
  // the size React Flow measured: without `measured`, a new node object loses
  // its handle bounds and no edge can be drawn.
  const [rfNodes, setRfNodes] = useState<Node[]>(layoutNodes);
  useEffect(() => {
    setRfNodes((current) => {
      const previous = new Map(current.map((node) => [node.id, node]));
      return layoutNodes.map((node) => {
        const kept = previous.get(node.id);
        return kept ? { ...node, position: kept.position, measured: kept.measured } : node;
      });
    });
  }, [layoutNodes]);
  const rfEdges = useMemo(() => {
    const positions = new Map(rfNodes.map((node) => [node.id, node.position]));
    return layoutEdges.map((edge) => {
      const [source, target] = facingSides(positions.get(edge.source)!, positions.get(edge.target)!);
      return { ...edge, sourceHandle: `source-${source}`, targetHandle: `target-${target}` };
    });
  }, [rfNodes, layoutEdges]);
  const onNodesChange = useCallback((changes: NodeChange[]) => setRfNodes((current) => applyNodeChanges(changes, current)), []);

  // Cheap effect: force a window-resize event after first paint so
  // react-flow recomputes its container bounds. Without it the canvas
  // sometimes mounts at 0×0 inside a flex parent that hasn't laid out
  // its children yet.
  useEffect(() => {
    const t = setTimeout(() => window.dispatchEvent(new Event('resize')), 50);
    return () => clearTimeout(t);
  }, []);

  return (
    <div style={{ width: '100%', height: '100%' }}>
      <ReactFlow
        nodes={rfNodes}
        edges={rfEdges}
        nodeTypes={nodeTypes}
        edgeTypes={edgeTypes}
        onNodesChange={onNodesChange}
        nodesDraggable
        nodesConnectable={false}
        elementsSelectable
        proOptions={{ hideAttribution: true }}
        fitView
        fitViewOptions={{ padding: 0.2, maxZoom: 1.2 }}
        minZoom={0.2}
        maxZoom={2.5}
        onNodeClick={(_, n) => onSelect(n.id)}
        style={{ background: 'var(--canvas)' }}
      >
        <Background variant={BackgroundVariant.Dots} color="var(--line)" gap={20} />
        <Controls showInteractive={false} />
      </ReactFlow>
    </div>
  );
}

// layoutGraph runs dagre to assign positions then builds the react-flow
// node + edge arrays. Pure function — no react state or DOM access.
function layoutGraph(
  nodes: TopologyNode[],
  edges: TopologyEdge[],
  selectedID: string | null,
  highlight: Set<string>,
): { rfNodes: Node[]; rfEdges: Edge[] } {
  const g = new dagre.graphlib.Graph();
  g.setDefaultEdgeLabel(() => ({}));
  g.setGraph({
    rankdir: 'LR',
    nodesep: 80,
    ranksep: 110,
    marginx: 40,
    marginy: 40,
  });
  for (const n of nodes) {
    g.setNode(n.id, { width: NODE_WIDTH, height: NODE_HEIGHT });
  }
  for (const e of edges) {
    g.setEdge(e.from, e.to);
  }
  dagre.layout(g);

  const rfNodes: Node[] = nodes.map((n) => {
    const pos = g.node(n.id);
    return {
      id: n.id,
      type: 'topo',
      position: { x: pos.x - NODE_WIDTH / 2, y: pos.y - NODE_HEIGHT / 2 },
      width: NODE_WIDTH,
      height: NODE_HEIGHT,
      data: { node: n, selected: selectedID === n.id, highlighted: highlight.has(n.id) } satisfies NodeData,
    };
  });

  const states = new Map(nodes.map((n) => [n.id, n.state]));
  const rfEdges: Edge[] = edges.map((e) => {
    const dependency = e.type === 'depends_on';
    const broken = dependency && (states.get(e.to) === 'down' || states.get(e.to) === 'missing');
    const stroke = broken ? 'var(--danger)' : dependency ? 'var(--fg-muted)' : 'var(--fg-faint)';
    const isSel = selectedID === e.from || selectedID === e.to;
    return {
      id: `${e.from}-${e.type}-${e.to}`,
      source: e.from,
      target: e.to,
      type: 'avoiding',
      style: {
        stroke,
        strokeWidth: isSel || broken ? 2.2 : 1.4,
        strokeDasharray: dependency ? undefined : '6 3',
        opacity: selectedID && !isSel ? 0.35 : 0.9,
      },
      markerEnd: { type: MarkerType.ArrowClosed, color: stroke, width: 16, height: 16 },
    };
  });

  return { rfNodes, rfEdges };
}

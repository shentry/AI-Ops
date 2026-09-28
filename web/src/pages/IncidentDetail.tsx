import { MessagesSquare, OctagonAlert, RefreshCw, RotateCcw } from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";
import { useParams } from "react-router";

import {
  ApiError,
  ControlRoom,
  ConversationMessage,
  EventDTO,
  FlowNode,
  RunStep,
  canOperate,
  decideApproval,
  getControlRoom,
  getConversation,
  getRunSteps,
  requestEvidence,
  rediagnose,
  subscribeIncident,
} from "../api";
import { useIncidentFeed, useSession } from "../app/context";
import { ActionCard } from "../components/incident/ActionCard";
import { AgentTrace } from "../components/incident/AgentTrace";
import { ApprovalCard } from "../components/incident/ApprovalCard";
import { Conversation } from "../components/incident/Conversation";
import { EventTimeline } from "../components/incident/EventTimeline";
import { FlowStrip } from "../components/incident/FlowStrip";
import { MembersCard } from "../components/incident/MembersCard";
import { ProblemsCard } from "../components/incident/ProblemsCard";
import { ReportCard } from "../components/incident/ReportCard";
import { ReviewCard } from "../components/incident/ReviewCard";
import { PageHeader } from "../components/layout/PageHeader";
import { Badge, Button, Notice, Skeleton, Tabs, cx } from "../components/ui";
import { relativeTime, severityLabel, statusLabel, timeLabel } from "../labels";
import { Tone, severityTone, statusTone, toneText } from "../tone";
import { NotFound } from "./NotFound";

type TabKey = "timeline" | "trace" | "chat";
type StreamState = "connecting" | "live" | "reconnecting";

function mergeByID<T extends { id: number }>(previous: T[], incoming: T[]): T[] {
  const byID = new Map<number, T>();
  for (const item of [...previous, ...incoming]) {
    if (item.id > 0) byID.set(item.id, item);
  }
  return [...byID.values()].sort((a, b) => a.id - b.id);
}

export function IncidentDetailRoute() {
  const { id } = useParams();
  const incidentID = Number(id);
  if (!Number.isSafeInteger(incidentID) || incidentID <= 0) return <NotFound />;
  return <IncidentDetail key={incidentID} incidentId={incidentID} />;
}

function IncidentDetail({ incidentId }: { incidentId: number }) {
  const { session } = useSession();
  const feed = useIncidentFeed();
  // Viewers see everything but cannot decide, ask or rediagnose; the server enforces the same rule.
  const operable = canOperate(session);
  const [room, setRoom] = useState<ControlRoom | null>(null);
  const [events, setEvents] = useState<EventDTO[]>([]);
  const [steps, setSteps] = useState<RunStep[]>([]);
  const [conversation, setConversation] = useState<ConversationMessage[]>([]);
  const [focusStepID, setFocusStepID] = useState<number | null>(null);
  const [tab, setTab] = useState<TabKey>("timeline");
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [streamState, setStreamState] = useState<StreamState>("connecting");
  const composerRef = useRef<HTMLTextAreaElement | null>(null);
  const lastEventID = useRef(0);
  const refreshTimer = useRef<number | null>(null);

  const refresh = async (showSpinner = false) => {
    if (showSpinner) setRefreshing(true);
    try {
      const next = await getControlRoom(incidentId);
      setRoom(next);
      setEvents((previous) => mergeByID(previous, next.recent_events));
      setSteps(next.current_run?.id ? await getRunSteps(next.current_run.id, incidentId) : []);
      setError(null);
    } catch (cause) {
      setError(cause instanceof ApiError ? cause.message : "这个 Incident 加载失败");
    } finally {
      setLoading(false);
      setRefreshing(false);
    }
  };

  // SSE 事件密集到达时合并成一次聚合刷新。
  const scheduleRefresh = () => {
    if (refreshTimer.current !== null) return;
    refreshTimer.current = window.setTimeout(() => {
      refreshTimer.current = null;
      void refresh(false);
    }, 100);
  };

  useEffect(() => {
    let cancelled = false;
    void getControlRoom(incidentId).then(async (next) => {
      if (cancelled) return;
      setRoom(next);
      setEvents((previous) => mergeByID(previous, next.recent_events));
      lastEventID.current = Math.max(lastEventID.current, ...next.recent_events.map((event) => event.id), 0);
      if (next.current_run?.id) setSteps(await getRunSteps(next.current_run.id, incidentId));
      setError(null);
      setLoading(false);
      try {
        const messages = await getConversation(incidentId);
        if (!cancelled) setConversation((previous) => mergeByID(previous, messages));
      } catch (cause) {
        if (!cancelled) setError(cause instanceof ApiError ? `对话加载失败：${cause.message}` : "对话加载失败");
      }
    }).catch((cause) => {
      if (cancelled) return;
      setError(cause instanceof ApiError ? cause.message : "这个 Incident 加载失败");
      setLoading(false);
    });
    return () => {
      cancelled = true;
      if (refreshTimer.current !== null) {
        window.clearTimeout(refreshTimer.current);
        refreshTimer.current = null;
      }
    };
  }, [incidentId]);

  useEffect(() => {
    setStreamState("connecting");
    return subscribeIncident(incidentId, {
      after: lastEventID.current,
      onOpen: () => setStreamState("live"),
      onError: () => setStreamState("reconnecting"),
      onEvent: (event) => {
        setStreamState("live");
        lastEventID.current = Math.max(lastEventID.current, event.id);
        setEvents((previous) => mergeByID(previous, [event]));
        scheduleRefresh();
        if (event.event_type.startsWith("incident.")) feed.refresh();
        if (event.event_type.startsWith("conversation.")) {
          void getConversation(incidentId).then((messages) => setConversation((previous) => mergeByID(previous, messages))).catch((cause) => {
            if (cause instanceof ApiError) setError(`对话刷新失败：${cause.message}`);
          });
        }
      },
    });
    // feed.refresh is stable; the stream is per incident.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [incidentId]);

  const handleAction = async (action: () => Promise<unknown>, success: string) => {
    setNotice(null);
    try {
      await action();
      setNotice(success);
    } catch (cause) {
      setNotice(cause instanceof ApiError ? cause.message : "操作没有完成");
      if (cause instanceof ApiError && cause.status === 409) await refresh(true);
      return;
    }
    try {
      await refresh(true);
    } catch {
      // The accepted mutation remains successful even when the follow-up read fails.
      setError("操作已提交，但状态刷新失败，请稍后重试。");
    }
  };

  const selectFlowNode = (node: FlowNode) => {
    setTab("trace");
    if (node.step_id) setFocusStepID(node.step_id);
  };

  const openChat = () => {
    setTab("chat");
    window.setTimeout(() => composerRef.current?.focus(), 0);
  };

  const tabs = useMemo(() => [
    { key: "timeline" as const, label: "事件时间线", count: events.length },
    { key: "trace" as const, label: "诊断轨迹", count: steps.length },
    { key: "chat" as const, label: "问 Agent", count: conversation.length },
  ], [events.length, steps.length, conversation.length]);

  if (loading && !room) {
    return (
      <div aria-busy="true" aria-label={`正在加载 Incident #${incidentId}`} className="space-y-4 px-7 py-6">
        <Skeleton className="h-5 w-72" />
        <Skeleton className="h-4 w-96" />
        <Skeleton className="h-36 w-full" />
        <Skeleton className="h-24 w-full" />
      </div>
    );
  }

  if (!room) {
    return (
      <div className="grid min-h-[60vh] place-items-center px-6">
        <div className="w-full max-w-md rounded-lg border border-line bg-surface p-6">
          <OctagonAlert size={18} aria-hidden="true" className="text-danger" />
          <h1 className="mt-3 text-[15px] font-semibold">打不开 Incident #{incidentId}</h1>
          <p className="mt-1 text-[13px] text-fg-muted">{error ?? "数据加载失败。"}</p>
          <Button variant="primary" className="mt-4" onClick={() => void refresh(true)}>重试</Button>
        </div>
      </div>
    );
  }

  const incident = room.incident;
  const run = room.current_run;
  const runStatus = run?.status;

  return (
    <>
      <PageHeader
        back={{ to: "/incidents", label: "全部事件" }}
        title={incident.title || `Incident #${incident.id}`}
        badges={
          <>
            <Badge tone={severityTone(incident.severity)}>{severityLabel(incident.severity)}</Badge>
            <Badge tone={statusTone(incident.status)} dot pulse={incident.status === "firing"}>{statusLabel(incident.status)}</Badge>
            <span className="font-mono text-xs text-fg-faint">#{incident.id} · {incident.group_key}</span>
          </>
        }
        meta={
          <>
            <span>{incident.alerts_count} 条告警</span>
            <span title={timeLabel(incident.started_at, true)}>开始 {relativeTime(incident.started_at)}</span>
            <span title={timeLabel(incident.last_seen_at, true)}>最近出现 {relativeTime(incident.last_seen_at)}</span>
            {incident.resolved_at && <span title={timeLabel(incident.resolved_at, true)}>恢复于 {timeLabel(incident.resolved_at)}</span>}
          </>
        }
        actions={
          <>
            <StreamBadge state={streamState} />
            <Button onClick={openChat}>
              <MessagesSquare size={13} aria-hidden="true" />
              问 Agent
            </Button>
            <Button disabled={refreshing} onClick={() => void refresh(true)}>
              <RefreshCw size={13} aria-hidden="true" className={refreshing ? "animate-spin" : undefined} />
              {refreshing ? "刷新中…" : "刷新"}
            </Button>
            <Button variant="primary" disabled={!operable} onClick={() => void handleAction(() => rediagnose(incident.id), "已排队一次新的诊断。")}>
              <RotateCcw size={13} aria-hidden="true" />
              重新诊断
            </Button>
          </>
        }
      >
        <section aria-label="Incident 概览" className="mt-3 flex flex-wrap gap-x-7 gap-y-2 border-t border-line-soft pt-2.5 text-xs">
          <Stat label="当前诊断" value={runStatus === "succeeded" ? "诊断完成" : statusLabel(runStatus, "尚未诊断")} tone={statusTone(runStatus)} />
          <Stat label="待处理问题" value={String(room.open_problems.length)} tone={room.open_problems.length ? "danger" : "ok"} />
          <Stat label="待审批" value={room.pending_approval ? "1 个" : "无"} tone={room.pending_approval ? "warn" : "neutral"} />
          <Stat label="事件数" value={String(events.length)} tone="neutral" />
        </section>
      </PageHeader>

      <div className="space-y-4 px-5 py-5 sm:px-7">
        {error && <Notice tone="danger">{error}</Notice>}
        {notice && <Notice tone="info">{notice}</Notice>}

        <div className="grid gap-4 xl:grid-cols-[minmax(0,1fr)_380px]">
          <div className="min-w-0 space-y-4">
            <ReportCard run={run} onShowTrace={() => setTab("trace")} />
            <FlowStrip nodes={room.flow_nodes} onSelect={selectFlowNode} />
            <div className="overflow-hidden rounded-lg border border-line bg-surface">
              <Tabs idBase="incident" label="Incident 明细" items={tabs} value={tab} onChange={setTab} />
              <div id={`incident-panel-${tab}`} role="tabpanel" aria-labelledby={`incident-tab-${tab}`}>
                {tab === "timeline" && <EventTimeline events={events} />}
                {tab === "trace" && <AgentTrace run={run} steps={steps} focusStepID={focusStepID} />}
                {tab === "chat" && (
                  <Conversation
                    incidentID={incident.id}
                    readOnly={!operable}
                    messages={conversation}
                    composerRef={composerRef}
                    onMessage={(message) => setConversation((previous) => mergeByID(previous, [message]))}
                  />
                )}
              </div>
            </div>
          </div>

          <aside aria-label="决策与处置" className="min-w-0 space-y-4">
            <ApprovalCard
              approval={room.pending_approval}
              readOnly={!operable}
              onDecide={(approvalID, approve, planHash, reason) =>
                handleAction(() => decideApproval(approvalID, approve, planHash, reason), approve ? "已批准，执行器会接管。" : "已拒绝。")}
              onRequestEvidence={() => void handleAction(() => requestEvidence(incident.id), "已请求补充证据。")}
            />
            <ActionCard action={room.latest_action} incidentStatus={incident.status} />
            <ProblemsCard problems={room.open_problems} />
            <MembersCard members={room.members} />
            <ReviewCard incidentID={incident.id} runID={run?.id ?? null} approvalID={room.latest_action?.id ?? null} readOnly={!operable} />
          </aside>
        </div>
      </div>
    </>
  );
}

function Stat({ label, value, tone }: { label: string; value: string; tone: Tone }) {
  return (
    <div className="flex items-baseline gap-2">
      <span className="text-fg-faint">{label}</span>
      <strong className={cx("tabular text-[13px] font-semibold", tone === "neutral" ? "text-fg" : toneText[tone])}>{value}</strong>
    </div>
  );
}

function StreamBadge({ state }: { state: StreamState }) {
  if (state === "live") return <Badge tone="ok" dot pulse>实时</Badge>;
  if (state === "reconnecting") return <Badge tone="warn" dot>重连中</Badge>;
  return <Badge dot>连接中</Badge>;
}

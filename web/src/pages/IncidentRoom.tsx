import { MessagesSquare, OctagonAlert, Radar, RefreshCw, RotateCcw } from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";

import {
	ApiError,
	ControlRoom,
	EventDTO,
	ModelState,
	decideApproval,
	getControlRoom,
	getConversation,
	getRunSteps,
	requestEvidence,
	rediagnose,
	subscribeIncident,
} from "../api";
import { ActionPanel } from "../components/ActionPanel";
import { ApprovalPanel } from "../components/ApprovalPanel";
import { ConversationPanel } from "../components/ConversationPanel";
import { EventTimeline } from "../components/EventTimeline";
import { FlowGraph } from "../components/FlowGraph";
import { ModelSwitcher } from "../components/ModelSwitcher";
import { ProblemPanel } from "../components/ProblemPanel";
import { StepInspector } from "../components/StepInspector";
import { severityLabel, statusLabel, timeLabel } from "../labels";

interface IncidentRoomProps {
  incidentId: number;
  actorName?: string;
  modelState: ModelState | null;
  onModelChanged: (state: ModelState) => void;
}

function mergeEvents(previous: EventDTO[], incoming: EventDTO[]): EventDTO[] {
  const byID = new Map<number, EventDTO>();
  for (const event of [...previous, ...incoming]) {
    if (event.id > 0) byID.set(event.id, event);
  }
  return [...byID.values()].sort((a, b) => a.id - b.id);
}
function mergeMessages<T extends { id: number }>(previous: T[], incoming: T[]): T[] {
	const byID = new Map<number, T>();
	for (const message of [...previous, ...incoming]) {
		if (message.id > 0) byID.set(message.id, message);
	}
	return [...byID.values()].sort((a, b) => a.id - b.id);
}

export function IncidentRoom({ incidentId, actorName, modelState, onModelChanged }: IncidentRoomProps) {
  const [room, setRoom] = useState<ControlRoom | null>(null);
  const [events, setEvents] = useState<EventDTO[]>([]);
  const [steps, setSteps] = useState<Awaited<ReturnType<typeof getRunSteps>>>([]);
  const [conversation, setConversation] = useState<Awaited<ReturnType<typeof getConversation>>>([]);
  const [selectedStepID, setSelectedStepID] = useState<number | null>(null);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [streamState, setStreamState] = useState<"connecting" | "live" | "reconnecting">("connecting");
  const [actionMessage, setActionMessage] = useState<string | null>(null);
  const conversationRef = useRef<HTMLDivElement | null>(null);
  const lastEventID = useRef(0);
  const refreshTimer = useRef<number | null>(null);

  const refresh = async (showSpinner = false) => {
	    if (showSpinner) setRefreshing(true);
	    try {
	      const nextRoom = await getControlRoom(incidentId);
	      setRoom(nextRoom);
	      setEvents((previous) => mergeEvents(previous, nextRoom.recent_events));
	      if (nextRoom.current_run?.id) {
	        const nextSteps = await getRunSteps(nextRoom.current_run.id, incidentId);
	        setSteps(nextSteps);
	      } else {
	        setSteps([]);
	      }
	      setError(null);
	    } catch (cause) {
	      setError(cause instanceof ApiError ? cause.message : "这个 Incident 加载失败");
	    } finally {
	      setLoading(false);
	      setRefreshing(false);
	    }
  };

  const scheduleRefresh = () => {
	    if (refreshTimer.current !== null) return;
	    refreshTimer.current = window.setTimeout(() => {
	      refreshTimer.current = null;
	      void refresh(false);
	    }, 100);
  };

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setRoom(null);
    setEvents([]);
    setSteps([]);
    setConversation([]);
    lastEventID.current = 0;
    void getControlRoom(incidentId).then(async (nextRoom) => {
      if (cancelled) return;
      setRoom(nextRoom);
      setEvents((previous) => mergeEvents(previous, nextRoom.recent_events));
      lastEventID.current = Math.max(lastEventID.current, ...nextRoom.recent_events.map((event) => event.id), 0);
      if (nextRoom.current_run?.id) setSteps(await getRunSteps(nextRoom.current_run.id, incidentId));
      setError(null);
      setLoading(false);
      try {
        const messages = await getConversation(incidentId);
        if (!cancelled) setConversation((previous) => mergeMessages(previous, messages));
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
    const close = subscribeIncident(incidentId, {
      after: lastEventID.current,
      onOpen: () => setStreamState("live"),
      onError: () => setStreamState("reconnecting"),
      onEvent: (event) => {
        setStreamState("live");
        lastEventID.current = Math.max(lastEventID.current, event.id);
        setEvents((previous) => mergeEvents(previous, [event]));
        scheduleRefresh();
        if (event.event_type.startsWith("conversation.")) {
          void getConversation(incidentId).then((messages) => setConversation((previous) => mergeMessages(previous, messages))).catch((cause) => {
            if (cause instanceof ApiError) setError(`对话刷新失败：${cause.message}`);
          });
        }
      },
    });
    return close;
  }, [incidentId]);

  const selectedStep = useMemo(
    () => steps.find((step) => step.id === selectedStepID) ?? steps[steps.length - 1] ?? null,
    [selectedStepID, steps],
  );

  const handleAction = async (action: () => Promise<unknown>, success: string) => {
    setActionMessage(null);
    try {
      await action();
      setActionMessage(success);
    } catch (cause) {
      setActionMessage(cause instanceof ApiError ? cause.message : "操作没有完成");
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

  if (loading && !room) {
    return (
      <main className="shell center-stage" aria-busy="true">
        <div className="loading-card">
          <span className="card-icon"><Radar size={17} strokeWidth={2} /></span>
          <span className="eyebrow">作战台</span>
          <h1>正在加载 Incident #{incidentId}</h1>
          <div className="loading-line" />
        </div>
      </main>
    );
  }

  if (!room) {
    return (
      <main className="shell center-stage">
        <div className="error-card" role="alert">
          <span className="card-icon card-icon-danger"><OctagonAlert size={17} strokeWidth={2} /></span>
          <span className="eyebrow">作战台</span>
          <h1>打不开这个 Incident</h1>
          <p>{error ?? "数据加载失败。"}</p>
          <button className="button primary" type="button" onClick={() => void refresh(true)}>
            重试
          </button>
        </div>
      </main>
    );
  }

  const incident = room.incident;
  const latestStatus = room.current_run?.status;

  return (
    <main className="shell room-shell">
      <header className="topbar">
        <a className="brand" href="/" aria-label="返回 Incident 列表">
          <span className="brand-mark"><Radar size={15} strokeWidth={2} /></span>
          <span>值班 <b>控制台</b></span>
        </a>
        <div className="topbar-actions">
          <span className={`stream-indicator ${streamState}`}>
            <span className="status-dot" />
            {streamState === "live" ? "实时" : streamState === "reconnecting" ? "重连中" : "连接中"}
          </span>
          <ModelSwitcher state={modelState} onChanged={onModelChanged} />
          {actorName && <span className="operator">{actorName}</span>}
        </div>
      </header>

      <section className="incident-heading">
        <div>
          <div className="breadcrumb"><a href="/">全部 Incident</a> / #{incident.id}</div>
          <h1>{incident.title || `Incident #${incident.id}`}</h1>
          <div className="incident-meta">
            <span className={`severity severity-${String(incident.severity).toLowerCase()}`}>
              {severityLabel(incident.severity)}
            </span>
            <span className="status-pill">{statusLabel(incident.status)}</span>
            <span>{incident.alerts_count} 条告警</span>
            <span>最近出现 {timeLabel(incident.last_seen_at)}</span>
          </div>
        </div>
        <div className="heading-actions">
          <button
            className="button secondary"
            type="button"
            onClick={() => conversationRef.current?.scrollIntoView({ behavior: "smooth", block: "start" })}
          >
            <MessagesSquare size={14} />
            问 Agent
          </button>
          <button
            className="button secondary"
            type="button"
            disabled={refreshing}
            onClick={() => void refresh(true)}
          >
            {refreshing ? "刷新中…" : "刷新"}
            <RefreshCw size={13} className={refreshing ? "spin" : undefined} />
          </button>
          <button
            className="button primary"
            type="button"
            onClick={() => void handleAction(() => rediagnose(incident.id), "已排队一次新的诊断。")}
          >
            <RotateCcw size={13} />
            重新诊断
          </button>
        </div>
      </section>

      {error && <div className="inline-error" role="status">{error}</div>}
      {actionMessage && <div className="inline-notice" role="status">{actionMessage}</div>}

      <section className="summary-strip" aria-label="Incident 概览">
        <SummaryMetric label="当前诊断" value={latestStatus === "succeeded" ? "诊断完成" : statusLabel(latestStatus, "尚未诊断")} tone={latestStatus} />
        <SummaryMetric label="待处理问题" value={String(room.open_problems.length)} tone={room.open_problems.length ? "failed" : "succeeded"} />
        <SummaryMetric label="待审批" value={room.pending_approval ? "1 个" : "无"} tone={room.pending_approval ? "blocked" : "succeeded"} />
        <SummaryMetric label="事件数" value={String(events.length)} tone="neutral" />
      </section>

      <section className="control-grid">
        <div className="control-main">
          <FlowGraph nodes={room.flow_nodes} onSelectNode={(node) => node.step_id && setSelectedStepID(node.step_id)} />
          <EventTimeline events={events} onSelectEvent={() => undefined} />
        </div>
        <aside className="control-side">
          <ProblemPanel problems={room.open_problems} />
          <ApprovalPanel
            approval={room.pending_approval}
            onDecide={(approvalID, approve, planHash, reason) =>
              handleAction(
                () => decideApproval(approvalID, approve, planHash, reason),
                approve ? "已批准，执行器会接管。" : "已拒绝。",
              )
            }
            onRequestEvidence={() => void handleAction(() => requestEvidence(incident.id), "已请求补充证据。")}
          />
          <ActionPanel action={room.latest_action} incidentStatus={incident.status} />
          <StepInspector step={selectedStep} />
        </aside>
      </section>

      <div ref={conversationRef}>
        <ConversationPanel
          incidentID={incident.id}
          messages={conversation}
          onMessage={(message) => setConversation((previous) => mergeMessages(previous, [message]))}
        />
      </div>
    </main>
  );
}

function SummaryMetric({ label, value, tone }: { label: string; value: string; tone?: string }) {
  return (
    <div className="summary-metric">
      <span>{label}</span>
      <strong className={tone ? `tone-${tone}` : undefined}>{value}</strong>
    </div>
  );
}

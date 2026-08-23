import { EventDTO } from "../api";
import { eventTypeLabel, phaseLabel, statusLabel, timeLabel } from "../labels";

interface EventTimelineProps {
  events: EventDTO[];
  onSelectEvent?: (event: EventDTO) => void;
}

export function EventTimeline({ events, onSelectEvent }: EventTimelineProps) {
  const visibleEvents = [...events].sort((a, b) => b.id - a.id);
  return (
    <section className="panel timeline-panel" aria-labelledby="timeline-title">
      <div className="panel-heading">
        <div>
          <span className="eyebrow">审计流水</span>
          <h2 id="timeline-title">事件时间线</h2>
        </div>
        <span className="panel-caption">已落库 {events.length} 条</span>
      </div>
      {visibleEvents.length === 0 ? (
        <div className="empty-state compact-empty"><p>还没有事件，等待第一条落库…</p></div>
      ) : (
        <ol className="timeline-list">
          {visibleEvents.map((event) => (
            <li key={event.id} className="timeline-item">
              <span className={`timeline-marker status-${statusName(event.status)}`} />
              <button className="timeline-content" type="button" onClick={() => onSelectEvent?.(event)}>
                <span className="timeline-head">
                  <strong>{eventTypeLabel(event.event_type)}</strong>
                  <time dateTime={event.created_at}>{timeLabel(event.created_at, true)}</time>
                </span>
                <span className="timeline-summary">{event.summary || "无摘要"}</span>
                <span className="timeline-meta">
                  <span>{phaseLabel(event.phase)}</span>
                  <span className={`event-status event-${statusName(event.status)}`}>{statusLabel(event.status, "已记录")}</span>
                  <span>#{event.id}</span>
                </span>
              </button>
            </li>
          ))}
        </ol>
      )}
    </section>
  );
}

function statusName(value: string): string {
  if (value === "succeeded" || value === "completed" || value === "passed" || value === "sent") return "succeeded";
  if (value === "running" || value === "started") return "running";
  if (value === "failed" || value === "error" || value === "denied") return "failed";
  if (value === "blocked" || value === "pending") return "blocked";
  if (value === "inconclusive") return "inconclusive";
  return "queued";
}

import { BellRing, Bot, CheckCheck, Database, LucideIcon, MessagesSquare, Play, RotateCcw, Scale, Send, ShieldCheck, Sparkles, UserCheck } from "lucide-react";

import { EventDTO } from "../../api";
import { eventTypeLabel, phaseLabel, relativeTime, statusLabel, timeLabel } from "../../labels";
import { statusTone } from "../../tone";
import { Badge, EmptyState } from "../ui";

const phaseIcons: Record<string, LucideIcon> = {
  ingest: BellRing, incident: BellRing, evidence: Database, collector: Database, llm: Sparkles, reasoner: Sparkles,
  guard: ShieldCheck, policy: Scale, approval: UserCheck, execution: Play, execute: Play, verify: CheckCheck,
  verification: CheckCheck, retry: RotateCcw, notification: Send, notify: Send, conversation: MessagesSquare,
};

// EventTimeline 是落库事件的审计流水，最新在上。
export function EventTimeline({ events }: { events: EventDTO[] }) {
  const ordered = [...events].sort((a, b) => b.id - a.id);
  if (ordered.length === 0) return <EmptyState className="px-4 py-4" title="还没有事件" hint="第一条事件落库后会实时出现在这里。" />;
  return (
    <ol className="divide-y divide-line-soft">
      {ordered.map((event) => {
        const Icon = phaseIcons[event.phase] ?? Bot;
        return (
          <li key={event.id} className="flex gap-3 px-4 py-3">
            <span aria-hidden="true" className="mt-0.5 grid size-7 shrink-0 place-items-center rounded-full border border-line-soft bg-surface-2 text-fg-muted">
              <Icon size={13} />
            </span>
            <div className="min-w-0 flex-1">
              <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                <span className="text-[13px] font-medium text-fg">{eventTypeLabel(event.event_type)}</span>
                <Badge tone={statusTone(event.status)}>{statusLabel(event.status, "已记录")}</Badge>
                <span className="text-xs text-fg-faint">{phaseLabel(event.phase)}</span>
                <time dateTime={event.created_at} title={timeLabel(event.created_at, true)} className="ml-auto text-xs text-fg-faint">
                  {relativeTime(event.created_at)}
                </time>
              </div>
              <p className="mt-1 break-words text-[13px] text-fg-muted">{event.summary || "无摘要"}</p>
              <p className="mt-0.5 font-mono text-[11px] text-fg-faint">
                #{event.id}{event.run_id ? ` · run ${event.run_id}` : ""}{event.approval_id ? ` · approval ${event.approval_id}` : ""}
              </p>
            </div>
          </li>
        );
      })}
    </ol>
  );
}

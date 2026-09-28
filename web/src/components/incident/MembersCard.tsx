import { BellRing } from "lucide-react";

import { IncidentMember } from "../../api";
import { relativeTime, severityLabel, statusLabel, timeLabel } from "../../labels";
import { severityTone, statusTone } from "../../tone";
import { Badge, EmptyState, Panel, StatusDot } from "../ui";

// MembersCard 列出归并进这个 Incident 的告警（按 fingerprint 去重后的成员）。
export function MembersCard({ members }: { members: IncidentMember[] }) {
  return (
    <Panel title="告警成员" icon={<BellRing size={14} />} meta={<span className="tabular">{members.length}</span>} bodyClassName={members.length ? "p-0" : undefined}>
      {members.length === 0 ? (
        <EmptyState title="没有成员明细" />
      ) : (
        <ul className="divide-y divide-line-soft">
          {members.slice(0, 20).map((member) => (
            <li key={member.fingerprint || member.name} className="flex items-start gap-2.5 px-4 py-2">
              <StatusDot tone={statusTone(member.status)} className="mt-1.5 size-1.5" />
              <div className="min-w-0 flex-1">
                <div className="flex items-center gap-1.5">
                  <span className="min-w-0 flex-1 truncate text-[12.5px] font-medium text-fg">{member.name || "未命名告警"}</span>
                  <Badge tone={severityTone(member.severity)}>{severityLabel(member.severity)}</Badge>
                </div>
                <p className="mt-0.5 truncate text-[11px] text-fg-faint">
                  {statusLabel(member.status)} · <span title={timeLabel(member.linked_at, true)}>{relativeTime(member.linked_at)}</span> · <code title={member.fingerprint}>{member.fingerprint.slice(0, 12)}</code>
                </p>
              </div>
            </li>
          ))}
        </ul>
      )}
      {members.length > 20 && <p className="border-t border-line-soft px-4 py-2 text-xs text-fg-faint">另有 {members.length - 20} 条未展示</p>}
    </Panel>
  );
}

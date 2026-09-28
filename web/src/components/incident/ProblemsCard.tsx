import { Check, TriangleAlert } from "lucide-react";

import { ProblemDTO } from "../../api";
import { problemCodeLabel, problemSeverityLabel, statusLabel } from "../../labels";
import { severityTone } from "../../tone";
import { Badge, EmptyState, Panel, cx } from "../ui";

export function ProblemsCard({ problems }: { problems: ProblemDTO[] }) {
  return (
    <Panel
      title="当前问题"
      icon={<TriangleAlert size={14} />}
      actions={<span className={cx("tabular rounded-full px-1.5 text-[11px] font-semibold", problems.length ? "bg-danger/15 text-danger" : "bg-surface-2 text-fg-faint")}>{problems.length}</span>}
      bodyClassName={problems.length ? "p-0" : undefined}
    >
      {problems.length === 0 ? (
        <EmptyState icon={<Check size={15} className="text-ok" />} title="没有待处理问题" hint="各阶段信号都在预期范围内。" />
      ) : (
        <ul className="divide-y divide-line-soft">
          {problems.map((problem) => (
            <li key={problem.id || `${problem.code}-${problem.summary}`} className="px-4 py-2.5">
              <div className="flex items-center gap-2">
                <Badge tone={severityTone(problem.severity)}>{problemSeverityLabel(problem.severity)}</Badge>
                <span className="min-w-0 flex-1 truncate text-[13px] font-medium text-fg">{problemCodeLabel(problem.code)}</span>
              </div>
              <p className="mt-1 text-xs text-fg-muted">{problem.summary || "没有更多说明"}</p>
              {problem.detail !== undefined && problem.detail !== null && (
                <p className="mt-1 line-clamp-4 whitespace-pre-wrap break-words font-mono text-[11px] text-fg-faint">{detailText(problem.detail)}</p>
              )}
              <p className="mt-1 text-[11px] text-fg-faint">
                <code>{problem.code}</code> · {statusLabel(problem.status)} · {problem.run_id ? `诊断 #${problem.run_id}` : "Incident 级"}
              </p>
            </li>
          ))}
        </ul>
      )}
    </Panel>
  );
}

function detailText(detail: unknown): string {
  if (typeof detail === "string") return detail;
  try {
    return JSON.stringify(detail) ?? "";
  } catch {
    return "详情不可用";
  }
}

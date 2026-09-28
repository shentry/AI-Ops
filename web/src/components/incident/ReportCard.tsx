import { ListTree, Sparkles, TriangleAlert } from "lucide-react";

import { Run } from "../../api";
import { relativeTime, statusLabel, timeLabel } from "../../labels";
import { statusTone } from "../../tone";
import { Badge, Button, EmptyState } from "../ui";
import { MarkdownText } from "./Markdown";

// ReportCard 是当前诊断的结论。只陈述模型给出的根因，不推断执行或恢复：
// 那些事实由右侧「最近变更」按服务端状态展示。
export function ReportCard({ run, onShowTrace }: { run: Run | null; onShowTrace: () => void }) {
  const status = run?.status ?? "";
  const tone = statusTone(status);
  const running = status === "running" || status === "queued";
  return (
    <section aria-labelledby="report-title" className="rounded-lg border border-line bg-surface">
      <header className="flex flex-wrap items-center gap-x-2.5 gap-y-1.5 border-b border-line-soft px-4 py-2.5">
        <span aria-hidden="true" className="grid size-6 place-items-center rounded-md bg-accent/14 text-accent">
          <Sparkles size={13} />
        </span>
        <h2 id="report-title" className="text-[13.5px] font-semibold">诊断报告</h2>
        {run && <Badge tone={tone} dot pulse={running}>{statusLabel(status, "未知")}</Badge>}
        {run && (
          <div className="ml-auto flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-fg-faint">
            <span>诊断 #{run.id}</span>
            {run.mode && <span>模式 {run.mode}</span>}
            {run.retry_of ? <span>重试自 #{run.retry_of}</span> : null}
            {(run.tokens_in || run.tokens_out) ? (
              <span className="tabular">tokens {(run.tokens_in ?? 0).toLocaleString()} / {(run.tokens_out ?? 0).toLocaleString()}</span>
            ) : null}
            <span title={timeLabel(run.started_at, true)}>{relativeTime(run.started_at)}</span>
            <Button size="xs" variant="ghost" onClick={onShowTrace}>
              <ListTree size={12} aria-hidden="true" />
              诊断轨迹
            </Button>
          </div>
        )}
      </header>
      <div className="px-4 py-3.5">
        {!run ? (
          <EmptyState title="还没有诊断" hint="critical / high 告警促发后会自动诊断；也可以手动重新诊断。" />
        ) : (
          <>
            <p className="caps mb-1.5 !text-accent/90">根因分析</p>
            {run.rca_text?.trim() ? (
              <MarkdownText>{run.rca_text}</MarkdownText>
            ) : (
              <p className="text-[13px] text-fg-muted">
                {running ? "诊断进行中，证据采集和模型推理的每一步见诊断轨迹。" : status === "failed" ? "这次诊断没有产出结论，原因见当前问题和诊断轨迹。" : "这次诊断没有给出根因文本。"}
              </p>
            )}
          </>
        )}
      </div>
      {run && (
        <footer className="flex items-center gap-1.5 border-t border-line-soft px-4 py-2 text-[11.5px] text-fg-faint">
          <TriangleAlert size={12} aria-hidden="true" className="text-warn" />
          模型生成的结论，需结合证据核对；诊断完成不代表变更已执行或故障已恢复。
        </footer>
      )}
    </section>
  );
}

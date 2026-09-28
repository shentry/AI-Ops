import { ChevronRight, CircleCheck, CircleDashed, CircleX, LoaderCircle } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import { Run, RunStep } from "../../api";
import { durationLabel, phaseLabel, statusLabel, timeLabel } from "../../labels";
import { EmptyState, Mono, cx } from "../ui";
import { isEmptyValue, safeJSON } from "./redact";

// AgentTrace 列出当前诊断的每一步（证据采集、模型调用、工具调用……），
// 每步可展开看脱敏后的输入输出。流程图点中的步骤会自动展开并滚动到视野内。
export function AgentTrace({ run, steps, focusStepID }: { run: Run | null; steps: RunStep[]; focusStepID: number | null }) {
  const [open, setOpen] = useState<Set<number>>(() => new Set());
  const list = useRef<HTMLOListElement | null>(null);

  useEffect(() => {
    if (focusStepID === null) return;
    setOpen((previous) => new Set(previous).add(focusStepID));
    window.setTimeout(() => {
      list.current?.querySelector(`[data-step="${focusStepID}"]`)?.scrollIntoView({ block: "nearest", behavior: "smooth" });
    }, 0);
  }, [focusStepID]);

  if (!run) return <EmptyState className="px-4 py-4" title="还没有诊断" hint="诊断开始后，每一步都会记录在这里。" />;
  if (steps.length === 0) return <EmptyState className="px-4 py-4" title={`诊断 #${run.id} 还没有记录步骤`} hint="证据采集和模型调用开始后会出现在这里。" />;

  const toggle = (id: number) => setOpen((previous) => {
    const next = new Set(previous);
    if (next.has(id)) next.delete(id);
    else next.add(id);
    return next;
  });

  return (
    <div>
      <p className="px-4 pt-3 text-xs text-fg-faint">诊断 #{run.id} · {steps.length} 步 · 输入输出已在服务端和浏览器两侧脱敏</p>
      <ol ref={list} className="space-y-1.5 p-3">
        {steps.map((step) => {
          const expanded = open.has(step.id);
          const failed = Boolean(step.error) || step.status === "failed";
          return (
            <li key={step.id} data-step={step.id} className={cx("overflow-hidden rounded-md border", focusStepID === step.id ? "border-accent/50" : "border-line-soft")}>
              <button
                type="button"
                aria-expanded={expanded}
                onClick={() => toggle(step.id)}
                className="flex w-full cursor-pointer items-center gap-2.5 bg-canvas/30 px-3 py-2 text-left hover:bg-surface-2/60"
              >
                <StepIcon status={step.status} failed={failed} />
                <span className="rounded bg-surface-2 px-1.5 text-[11px] text-fg-muted">{phaseLabel(step.kind)}</span>
                <span className="min-w-0 flex-1 truncate font-mono text-[12.5px] text-fg">{step.name || phaseLabel(step.kind)}</span>
                <span className="tabular shrink-0 text-[11px] text-fg-faint">#{step.seq} · {durationLabel(step.duration_ms)}</span>
                <ChevronRight size={14} aria-hidden="true" className={cx("shrink-0 text-fg-faint transition-transform", expanded && "rotate-90")} />
              </button>
              {expanded && (
                <div className="space-y-2.5 border-t border-line-soft px-3 py-2.5">
                  <p className="text-xs text-fg-faint">
                    {statusLabel(step.status, failed ? "失败" : "已记录")} · {timeLabel(step.started_at, true)} → {timeLabel(step.finished_at, true)}
                    {step.owner ? ` · ${step.owner}` : ""}
                  </p>
                  {step.error && <p className="rounded-md border border-danger/35 bg-danger/10 px-2.5 py-1.5 text-[12.5px] text-danger">{step.error}</p>}
                  <CodeBlock label="输入" value={step.input} />
                  <CodeBlock label="输出" value={step.output} />
                  {step.truncated && <p className="text-xs text-warn">输出在服务端已被截断，这里不是全文。</p>}
                  {isEmptyValue(step.input) && isEmptyValue(step.output) && !step.error && <p className="text-xs text-fg-faint">这一步没有记录输入输出。</p>}
                </div>
              )}
            </li>
          );
        })}
      </ol>
    </div>
  );
}

function StepIcon({ status, failed }: { status?: string; failed: boolean }) {
  if (failed) return <CircleX size={14} aria-hidden="true" className="shrink-0 text-danger" />;
  if (status === "running" || status === "started") return <LoaderCircle size={14} aria-hidden="true" className="shrink-0 animate-spin text-info" />;
  if (status === "queued") return <CircleDashed size={14} aria-hidden="true" className="shrink-0 text-fg-faint" />;
  return <CircleCheck size={14} aria-hidden="true" className="shrink-0 text-ok" />;
}

function CodeBlock({ label, value }: { label: string; value: unknown }) {
  if (isEmptyValue(value)) return null;
  return (
    <div>
      <p className="caps mb-1">{label}</p>
      <pre className="max-h-72 overflow-auto rounded-md border border-line-soft bg-canvas p-2.5 text-[11.5px] leading-5 text-fg-muted"><Mono className="!text-[11.5px] !text-fg-muted">{safeJSON(value)}</Mono></pre>
    </div>
  );
}

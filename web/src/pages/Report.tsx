import { ClipboardList, Info } from "lucide-react";
import { useEffect, useState } from "react";
import { Link } from "react-router";

import { ApiError, Report as ReportState, getReport } from "../api";
import { PageBody, PageHeader } from "../components/layout/PageHeader";
import { Badge, EmptyState, Notice, Panel, Skeleton, cx } from "../components/ui";
import { minutes, percent } from "../format";
import { Tone, toneDot } from "../tone";

const ranges = [7, 30, 90];

const queueLabels: Record<string, string> = {
  failed: "执行失败",
  compensated: "已补偿",
  unknown: "结果未知",
  recurred: "恢复后复发",
  verification_failed: "验证失败",
  sampled: "自动成功抽样",
};

const queueTone: Record<string, Tone> = {
  failed: "danger", compensated: "warn", unknown: "warn", recurred: "danger", verification_failed: "danger", sampled: "neutral",
};

// Report 是处置效果：每个比例旁都给原始计数，分母为 0 时显示「样本不足」。
export function Report() {
  const [days, setDays] = useState(30);
  const [report, setReport] = useState<ReportState | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let active = true;
    setReport(null);
    void getReport(days).then((next) => {
      if (!active) return;
      setReport(next);
      setError(null);
    }).catch((cause) => {
      if (active) setError(cause instanceof ApiError ? cause.message : "效果报表加载失败");
    });
    return () => {
      active = false;
    };
  }, [days]);

  return (
    <>
      <PageHeader
        title="效果评估"
        description={report ? `${report.incidents} 个真实事件；比例旁均为原始计数。` : "处置效果与待复盘队列。"}
        actions={
          <div role="group" aria-label="统计窗口" className="inline-flex rounded-md border border-line bg-surface p-0.5">
            {ranges.map((value) => (
              <button key={value} type="button" aria-pressed={days === value} onClick={() => setDays(value)}
                className={cx("h-7 cursor-pointer rounded px-3 text-xs", days === value ? "bg-surface-2 font-medium text-fg" : "text-fg-muted hover:text-fg")}>
                {value} 天
              </button>
            ))}
          </div>
        }
      />
      <PageBody>
        {error && <Notice tone="danger">{error}</Notice>}
        {!report ? (
          !error && <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">{[0, 1, 2, 3, 4, 5, 6, 7].map((index) => <Skeleton key={index} className="h-24" />)}</div>
        ) : (
          <>
            <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
              <Metric label="整体无人介入恢复率" value={percent(report.unattended.rate)} rate={report.unattended.rate} tone="ok"
                detail={`${report.unattended.recovered} / ${report.incidents}；抽查确认 ${report.unattended.confirmed}`} />
              <Metric label="支持范围内自动恢复率" value={percent(report.unattended.in_scope_rate)} rate={report.unattended.in_scope_rate} tone="ok"
                detail={`${report.unattended.recovered} / ${report.unattended.in_scope} 个已授权故障`} />
              <Metric label="错误执行率" value={percent(report.executions.error_rate)} rate={report.executions.error_rate} tone="danger"
                detail={`错误 ${report.executions.wrong} / 已审阅 ${report.executions.reviewed}；审阅覆盖 ${percent(report.executions.coverage)}（共 ${report.executions.total} 次真实执行）`} />
              <Metric label="复发率" value={percent(report.recurrence.rate)} rate={report.recurrence.rate} tone="danger"
                detail={`${report.recurrence.recurred} / ${report.recurrence.watched} 次观察期`} />
              <Metric label="恢复耗时中位数" value={minutes(report.recovery_minutes.median)} detail={`最长 ${minutes(report.recovery_minutes.max)}`} />
              <Metric label="人工介入占比" value={percent(report.manual.share)} rate={report.manual.share} tone="warn"
                detail={`${report.manual.incidents} 个事件；记录 ${report.manual.minutes} 分钟`} />
              <Metric label="根因准确率（抽样）" value={percent(report.root_cause.accuracy)} rate={report.root_cause.accuracy} tone="ok"
                detail={`样本 ${report.root_cause.reviewed}：正确 ${report.root_cause.correct} / 部分 ${report.root_cause.partial} / 错误 ${report.root_cause.wrong}；未知 ${report.root_cause.unknown}`} />
              <Metric label="单事件模型 token" value={report.cost.tokens_per_incident === null ? "样本不足" : Math.round(report.cost.tokens_per_incident).toLocaleString()}
                detail={`输入 ${report.cost.tokens_in.toLocaleString()} / 输出 ${report.cost.tokens_out.toLocaleString()}`} />
            </div>
            <p className="flex items-start gap-1.5 text-xs text-fg-faint">
              <Info size={13} aria-hidden="true" className="mt-px shrink-0" />
              「无人介入」指平台内没有记录人工介入；平台外操作无法完全观测，抽查确认值单列。
            </p>

            <Panel title="待复盘" icon={<ClipboardList size={14} />} meta={<span>失败、补偿、未知、复发全部列出；自动成功抽样 1/5</span>} bodyClassName="p-0">
              {report.review_queue.length === 0 ? (
                <EmptyState className="px-4 py-3" title="没有待复盘的动作" />
              ) : (
                <ul className="divide-y divide-line-soft">
                  {report.review_queue.map((item) => (
                    <li key={item.approval_id} className="flex items-center gap-3 px-4 py-2.5">
                      <Link to={`/incidents/${item.incident_id}`} className="text-[13px] text-fg hover:underline">Incident #{item.incident_id} · 动作 #{item.approval_id}</Link>
                      <Badge tone={queueTone[item.reason] ?? "neutral"} className="ml-auto">{queueLabels[item.reason] ?? item.reason}</Badge>
                    </li>
                  ))}
                </ul>
              )}
            </Panel>
          </>
        )}
      </PageBody>
    </>
  );
}

// Metric 用 aria-label 成为具名区域，读屏和测试都能按指标名定位。
function Metric({ label, value, detail, rate, tone = "neutral" }: { label: string; value: string; detail: string; rate?: number | null; tone?: Tone }) {
  const insufficient = value === "样本不足";
  return (
    <section aria-label={label} className="rounded-lg border border-line bg-surface px-4 py-3.5">
      <p className="text-xs text-fg-faint">{label}</p>
      <p className={cx("tabular mt-1.5 text-[21px] font-semibold leading-none tracking-tight", insufficient ? "text-[15px] font-medium text-fg-faint" : "text-fg")}>{value}</p>
      {rate !== undefined && (
        <span aria-hidden="true" className="mt-2.5 block h-1 overflow-hidden rounded-full bg-surface-2">
          {rate !== null && <span className={cx("block h-full rounded-full", toneDot[tone])} style={{ width: `${Math.max(2, Math.min(100, rate * 100))}%` }} />}
        </span>
      )}
      <p className="mt-2 text-[11.5px] leading-snug text-fg-faint">{detail}</p>
    </section>
  );
}

import { Search } from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import { Link, useSearchParams } from "react-router";

import { ApiError, Incident, listIncidents } from "../api";
import { PageBody, PageHeader } from "../components/layout/PageHeader";
import { Badge, EmptyState, Notice, Skeleton, TextInput, cx } from "../components/ui";
import { relativeTime, severityLabel, statusLabel, timeLabel } from "../labels";
import { severityTone, statusTone } from "../tone";

const filters = [
  { value: "firing", label: "触发中" },
  { value: "candidate", label: "候选" },
  { value: "resolved", label: "已恢复" },
  { value: "", label: "全部" },
];

// Incidents 按服务端状态筛选，标题 / group key 在本页内过滤。
export function Incidents() {
  const [params, setParams] = useSearchParams();
  const status = filters.some((filter) => filter.value === params.get("status")) ? params.get("status") ?? "" : "";
  const [rows, setRows] = useState<Incident[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [query, setQuery] = useState("");

  useEffect(() => {
    let active = true;
    setRows(null);
    void listIncidents(status, 100)
      .then((next) => {
        if (!active) return;
        setRows(next);
        setError(null);
      })
      .catch((cause) => {
        if (!active) return;
        setRows([]);
        setError(cause instanceof ApiError ? cause.message : "事件列表加载失败");
      });
    return () => {
      active = false;
    };
  }, [status]);

  const visible = useMemo(() => {
    const needle = query.trim().toLowerCase();
    return (rows ?? []).filter((row) => !needle || `${row.id} ${row.title} ${row.group_key}`.toLowerCase().includes(needle));
  }, [rows, query]);

  return (
    <>
      <PageHeader
        title="事件"
        description="Alertmanager 告警去重、归并后的 Incident。点进去看诊断、审批与处置。"
      >
        <div className="mt-3 flex flex-wrap items-center gap-3">
          <div role="group" aria-label="按状态筛选" className="inline-flex rounded-md border border-line bg-surface p-0.5">
            {filters.map((filter) => (
              <button
                key={filter.value || "all"}
                type="button"
                aria-pressed={status === filter.value}
                onClick={() => setParams(filter.value ? { status: filter.value } : {})}
                className={cx(
                  "h-7 cursor-pointer rounded px-3 text-xs transition-colors",
                  status === filter.value ? "bg-surface-2 font-medium text-fg" : "text-fg-muted hover:text-fg",
                )}
              >
                {filter.label}
              </button>
            ))}
          </div>
          <div className="relative w-full max-w-xs">
            <Search size={13} aria-hidden="true" className="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-fg-faint" />
            <TextInput aria-label="过滤事件" value={query} onChange={(event) => setQuery(event.target.value)} placeholder="标题、group key 或编号" className="pl-8" />
          </div>
          <span className="text-xs text-fg-faint">{rows ? `${visible.length} / ${rows.length} 条` : "加载中…"}</span>
        </div>
      </PageHeader>
      <PageBody wide>
        {error && <Notice tone="danger">{error}</Notice>}
        <div className="overflow-x-auto rounded-lg border border-line bg-surface">
          {rows === null ? (
            <div className="space-y-2 p-4">{[0, 1, 2, 3].map((index) => <Skeleton key={index} className="h-8 w-full" />)}</div>
          ) : visible.length === 0 ? (
            <EmptyState className="px-4 py-5" title={rows.length ? "没有匹配的事件" : "这个状态下没有事件"} hint={rows.length ? "换个关键词试试。" : "Alertmanager 还没有推送过告警，或者都被去重了。"} />
          ) : (
            <table className="w-full min-w-[760px] text-[13px]">
              <thead>
                <tr className="border-b border-line text-left text-xs text-fg-faint">
                  <th className="w-20 px-4 py-2.5 font-medium">严重度</th>
                  <th className="px-2 py-2.5 font-medium">事件</th>
                  <th className="w-24 px-2 py-2.5 font-medium">状态</th>
                  <th className="w-16 px-2 py-2.5 text-right font-medium">告警</th>
                  <th className="w-28 px-2 py-2.5 font-medium">开始</th>
                  <th className="w-28 px-4 py-2.5 font-medium">最近出现</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-line-soft">
                {visible.map((row) => (
                  <tr key={row.id} className="group hover:bg-surface-2/40">
                    <td className="px-4 py-2.5"><Badge tone={severityTone(row.severity)}>{severityLabel(row.severity)}</Badge></td>
                    <td className="max-w-0 px-2 py-2.5">
                      <Link to={`/incidents/${row.id}`} className="block truncate font-medium text-fg group-hover:underline">{row.title || `Incident #${row.id}`}</Link>
                      <span className="block truncate font-mono text-[11px] text-fg-faint">#{row.id} · {row.group_key}</span>
                    </td>
                    <td className="px-2 py-2.5"><Badge tone={statusTone(row.status)} dot pulse={row.status === "firing"}>{statusLabel(row.status)}</Badge></td>
                    <td className="tabular px-2 py-2.5 text-right text-fg-muted">{row.alerts_count}</td>
                    <td className="px-2 py-2.5 text-xs text-fg-muted" title={timeLabel(row.started_at, true)}>{relativeTime(row.started_at)}</td>
                    <td className="px-4 py-2.5 text-xs text-fg-muted" title={timeLabel(row.last_seen_at, true)}>{relativeTime(row.last_seen_at)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      </PageBody>
    </>
  );
}

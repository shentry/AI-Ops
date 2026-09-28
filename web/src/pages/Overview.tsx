import { ArrowUpRight, RefreshCw, ShieldCheck, Siren, UserCheck } from "lucide-react";
import { ReactNode, useEffect, useState } from "react";
import { Link } from "react-router";

import { ApiError, ApprovalDTO, Remediation, Report, getRemediation, getReport, listApprovals } from "../api";
import { useIncidentFeed } from "../app/context";
import { PageBody, PageHeader } from "../components/layout/PageHeader";
import { Badge, Button, EmptyState, Panel, StatusDot, cx } from "../components/ui";
import { percent } from "../format";
import { modeLabel, relativeTime, severityLabel, statusLabel, timeLabel } from "../labels";
import { Tone, severityTone, statusTone, toneText } from "../tone";

interface Section<T> {
  value: T | null;
  error: string | null;
}

const empty = { value: null, error: null };

function settle<T>(result: PromiseSettledResult<T>, fallback: string): Section<T> {
  if (result.status === "fulfilled") return { value: result.value, error: null };
  return { value: null, error: result.reason instanceof ApiError ? result.reason.message : fallback };
}

// Overview 是登录后的首页：正在发生的事件、等你决策的变更、自动处置的状态。
// 每一块独立加载，一块失败不影响其他块。
export function Overview() {
  const feed = useIncidentFeed();
  const [approvals, setApprovals] = useState<Section<ApprovalDTO[]>>(empty);
  const [remediation, setRemediation] = useState<Section<Remediation>>(empty);
  const [report, setReport] = useState<Section<Report>>(empty);
  const [loading, setLoading] = useState(false);

  const load = async () => {
    setLoading(true);
    const [nextApprovals, nextRemediation, nextReport] = await Promise.allSettled([listApprovals("pending"), getRemediation(), getReport(30)]);
    setApprovals(settle(nextApprovals, "待审批列表加载失败"));
    setRemediation(settle(nextRemediation, "自动处置状态加载失败"));
    setReport(settle(nextReport, "效果报表加载失败"));
    setLoading(false);
  };

  useEffect(() => {
    void load();
  }, []);

  const firing = feed.incidents.filter((incident) => incident.status === "firing");
  const blocked = remediation.value?.rules.filter((rule) => rule.blocked) ?? [];
  const pending = approvals.value ?? [];

  return (
    <>
      <PageHeader
        title="概览"
        description="正在发生的事件、等你决策的变更，以及自动处置的运行状态。"
        actions={
          <Button disabled={loading} onClick={() => { feed.refresh(); void load(); }}>
            <RefreshCw size={13} aria-hidden="true" className={loading ? "animate-spin" : undefined} />
            刷新
          </Button>
        }
      />
      <PageBody>
        <section aria-label="值班概况" className="grid gap-px overflow-hidden rounded-lg border border-line bg-line-soft sm:grid-cols-2 lg:grid-cols-4">
          <Tile label="触发中事件" value={feed.loaded ? String(firing.length) : "—"} tone={firing.length ? "danger" : "ok"} hint={firing.length ? "最近 50 个事件内" : "没有正在触发的事件"} to="/incidents?status=firing" />
          <Tile label="待审批" value={approvals.value ? String(pending.length) : "—"} tone={pending.length ? "warn" : "neutral"} hint={approvals.error ?? (pending.length ? "manual 规则等待人工决定" : "没有等待决定的变更")} />
          <Tile
            label="自动处置"
            value={remediation.value ? (remediation.value.emergency_stop ? "急停中" : "运行中") : "—"}
            tone={remediation.value?.emergency_stop ? "danger" : remediation.value ? "ok" : "neutral"}
            hint={remediation.error ?? (blocked.length ? `${blocked.length} 条规则被阻断` : remediation.value ? `规则版本 ${remediation.value.rules_version}` : "")}
            to="/remediation"
          />
          <Tile
            label="无人介入恢复率 · 30 天"
            value={report.value ? percent(report.value.unattended.rate) : "—"}
            tone="neutral"
            hint={report.error ?? (report.value ? `${report.value.unattended.recovered} / ${report.value.incidents} 个事件` : "")}
            to="/report"
          />
        </section>

        <div className="grid gap-4 lg:grid-cols-[minmax(0,1.45fr)_minmax(0,1fr)]">
          <div className="min-w-0 space-y-4">
            <Panel title="正在发生" icon={<Siren size={14} />} meta={<span className="tabular">{firing.length}</span>} bodyClassName="p-0"
              actions={<Link to="/incidents" className="text-xs text-fg-muted hover:text-fg">全部事件</Link>}>
              {firing.length === 0 ? (
                <EmptyState className="px-4 py-3" title={feed.loaded ? "没有触发中的事件" : "加载中…"} hint={feed.loaded ? "新的告警促发后会出现在这里。" : undefined} />
              ) : (
                <ul className="divide-y divide-line-soft">
                  {firing.slice(0, 8).map((incident) => (
                    <li key={incident.id}>
                      <Link to={`/incidents/${incident.id}`} className="group flex items-center gap-3 px-4 py-2.5 hover:bg-surface-2/50">
                        <Badge tone={severityTone(incident.severity)}>{severityLabel(incident.severity)}</Badge>
                        <span className="min-w-0 flex-1 truncate text-[13px] text-fg">{incident.title || `Incident #${incident.id}`}</span>
                        <span className="shrink-0 text-xs text-fg-faint">{incident.alerts_count} 条告警</span>
                        <span className="w-16 shrink-0 text-right text-xs text-fg-faint" title={timeLabel(incident.last_seen_at, true)}>{relativeTime(incident.last_seen_at)}</span>
                        <ArrowUpRight size={14} aria-hidden="true" className="shrink-0 text-fg-faint opacity-0 group-hover:opacity-100" />
                      </Link>
                    </li>
                  ))}
                </ul>
              )}
            </Panel>

            <Panel title="最近事件" bodyClassName="p-0">
              {feed.incidents.length === 0 ? (
                <EmptyState className="px-4 py-3" title={feed.loaded ? "还没有任何事件" : "加载中…"} hint={feed.loaded ? "Alertmanager 还没有推送过告警，或者都被去重了。" : undefined} />
              ) : (
                <table className="w-full table-fixed text-[13px]">
                  <thead>
                    <tr className="border-b border-line-soft text-left text-xs text-fg-faint">
                      <th className="px-4 py-2 font-medium">事件</th>
                      <th className="w-24 px-2 py-2 font-medium">状态</th>
                      <th className="hidden w-14 px-2 py-2 font-medium sm:table-cell">告警</th>
                      <th className="w-24 px-4 py-2 text-right font-medium">最近出现</th>
                    </tr>
                  </thead>
                  <tbody className="divide-y divide-line-soft">
                    {feed.incidents.slice(0, 10).map((incident) => (
                      <tr key={incident.id} className="hover:bg-surface-2/40">
                        <td className="max-w-0 px-4 py-2">
                          <Link to={`/incidents/${incident.id}`} className="flex items-center gap-2 text-fg hover:underline">
                            <span className="tabular shrink-0 font-mono text-xs text-fg-faint">#{incident.id}</span>
                            <span className="truncate">{incident.title || incident.group_key}</span>
                          </Link>
                        </td>
                        <td className="px-2 py-2"><Badge tone={statusTone(incident.status)}>{statusLabel(incident.status)}</Badge></td>
                        <td className="tabular hidden px-2 py-2 text-fg-muted sm:table-cell">{incident.alerts_count}</td>
                        <td className="px-4 py-2 text-right text-xs text-fg-faint" title={timeLabel(incident.last_seen_at, true)}>{relativeTime(incident.last_seen_at)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </Panel>
          </div>

          <div className="min-w-0 space-y-4">
            <Panel title="等你决策" icon={<UserCheck size={14} />} tone={pending.length ? "warn" : undefined} meta={<span className="tabular">{pending.length}</span>} bodyClassName={pending.length ? "p-0" : undefined}>
              {approvals.error ? (
                <p className="text-xs text-danger">{approvals.error}</p>
              ) : pending.length === 0 ? (
                <EmptyState title={approvals.value ? "没有待审批的变更" : "加载中…"} hint={approvals.value ? "auto 规则自动执行，manual 规则会在这里等你。" : undefined} />
              ) : (
                <ul className="divide-y divide-line-soft">
                  {pending.map((approval) => (
                    <li key={approval.id}>
                      <Link to={`/incidents/${approval.incident_id}`} className="block px-4 py-2.5 hover:bg-surface-2/50">
                        <div className="flex items-center gap-2">
                          <span className="font-mono text-[13px] font-medium text-fg">{approval.tool_name || "变更动作"}</span>
                          <span className="min-w-0 flex-1 truncate text-xs text-fg-muted">{approval.target || "目标未知"}</span>
                          <span className="text-[11px] text-fg-faint">Incident #{approval.incident_id}</span>
                        </div>
                        <p className="mt-0.5 text-[11.5px] text-fg-faint">
                          {approval.rule_id ? `${approval.rule_id} · ${modeLabel(approval.mode)}` : "快照不完整"} · 过期 {relativeTime(approval.expires_at)}
                        </p>
                      </Link>
                    </li>
                  ))}
                </ul>
              )}
            </Panel>

            <Panel title="处置规则" icon={<ShieldCheck size={14} />} bodyClassName="p-0"
              actions={<Link to="/remediation" className="text-xs text-fg-muted hover:text-fg">管理</Link>}>
              {remediation.error ? (
                <p className="px-4 py-3 text-xs text-danger">{remediation.error}</p>
              ) : !remediation.value ? (
                <EmptyState className="px-4 py-3" title="加载中…" />
              ) : remediation.value.rules.length === 0 ? (
                <EmptyState className="px-4 py-3" title="没有配置处置规则" hint="没有规则时只诊断，不产生任何写操作。" />
              ) : (
                <ul className="divide-y divide-line-soft">
                  {remediation.value.rules.map((rule) => (
                    <li key={rule.id} className="flex items-center gap-2.5 px-4 py-2">
                      <StatusDot tone={rule.blocked ? "danger" : "ok"} />
                      <span className="min-w-0 flex-1 truncate font-mono text-[12.5px] text-fg" title={rule.blocked || undefined}>{rule.id}</span>
                      <span className="text-[11px] text-fg-faint">{modeLabel(rule.mode)}</span>
                      <span className="tabular text-[11px] text-fg-faint">{rule.executions}/{rule.max_executions}</span>
                    </li>
                  ))}
                </ul>
              )}
            </Panel>
          </div>
        </div>
      </PageBody>
    </>
  );
}

function Tile({ label, value, tone, hint, to }: { label: string; value: string; tone: Tone; hint?: ReactNode; to?: string }) {
  const body = (
    <>
      <p className="text-xs text-fg-faint">{label}</p>
      <p className={cx("tabular mt-1 text-[22px] font-semibold leading-none tracking-tight", tone === "neutral" ? "text-fg" : toneText[tone])}>{value}</p>
      {hint && <p className="mt-1.5 truncate text-[11.5px] text-fg-faint">{hint}</p>}
    </>
  );
  const frame = "block bg-surface px-4 py-3.5";
  return to ? <Link to={to} className={cx(frame, "hover:bg-surface-2")}>{body}</Link> : <div className={frame}>{body}</div>;
}

import { OctagonPause, Play, RotateCcw, ScrollText, ShieldCheck } from "lucide-react";
import { useEffect, useState } from "react";

import { ApiError, Remediation as RemediationState, controlRemediation, getRemediation } from "../api";
import { useSession } from "../app/context";
import { PageBody, PageHeader } from "../components/layout/PageHeader";
import { Badge, Button, EmptyState, FieldLabel, Mono, Notice, Panel, TextInput } from "../components/ui";
import { modeLabel, relativeTime, timeLabel } from "../labels";

const controlLabels: Record<string, string> = {
  emergency_stop: "急停",
  emergency_resume: "解除急停",
  rule_reset: "规则复位",
  rules_loaded: "规则发布",
};

const modeTone = { auto: "accent", manual: "warn", observe: "neutral" } as const;

// Remediation 展示生效中的规则发布及其状态。规则只能通过版本化配置发布修改；
// 这里只有管理员的急停 / 解除 / 规则复位，且必须写明原因进入审计。
export function Remediation() {
  const { session } = useSession();
  const admin = session.role === "admin";
  const [state, setState] = useState<RemediationState | null>(null);
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState<string | null>(null);

  const load = async () => {
    try {
      setState(await getRemediation());
    } catch (cause) {
      setMessage(cause instanceof ApiError ? cause.message : "自动处置状态加载失败");
    }
  };

  useEffect(() => {
    void load();
  }, []);

  const control = async (kind: "stop" | "resume" | "reset", ruleID = "") => {
    if (busy) return;
    if (!reason.trim()) {
      setMessage("请先填写原因");
      return;
    }
    setBusy(true);
    setMessage(null);
    try {
      await controlRemediation(kind, reason.trim(), ruleID);
      setReason("");
      setMessage({ stop: "已急停：新的写操作会在领取时被拒绝，已开始的外部操作无法瞬时撤回。", resume: "已解除急停。", reset: `规则 ${ruleID} 已复位。` }[kind]);
      await load();
    } catch (cause) {
      setMessage(cause instanceof ApiError ? cause.message : "操作没有完成");
    } finally {
      setBusy(false);
    }
  };

  return (
    <>
      <PageHeader
        title="自动处置"
        description="规则只能通过版本化配置发布修改；这里查看状态、急停和复位。"
        badges={state && (
          <>
            <Badge tone={state.emergency_stop ? "danger" : "ok"} dot pulse={!state.emergency_stop}>{state.emergency_stop ? `急停中：${state.stop_reason}` : "运行中"}</Badge>
            {state.maintenance && <Badge tone="warn">维护窗口：{state.maintenance}</Badge>}
            {state.busy_with > 0 && <Badge tone="info">正在处置审批 #{state.busy_with}</Badge>}
          </>
        )}
        meta={state && (
          <>
            <span>{state.service}（{state.env}）</span>
            <span>规则版本 <Mono className="text-[11.5px] !text-fg-muted">{state.rules_version}</Mono></span>
          </>
        )}
      />
      <PageBody>
        {message && <Notice tone="info">{message}</Notice>}

        {admin && state && (
          <Panel title="急停与复位" icon={<OctagonPause size={14} />} tone={state.emergency_stop ? "danger" : undefined} meta={<span>管理员</span>}>
            <FieldLabel htmlFor="control-reason">原因（写入审计）</FieldLabel>
            <div className="flex flex-wrap gap-2">
              <TextInput id="control-reason" className="min-w-60 flex-1" value={reason} onChange={(event) => setReason(event.target.value.slice(0, 500))} placeholder="为什么急停、解除或复位" />
              {state.emergency_stop ? (
                <Button variant="primary" disabled={busy} onClick={() => void control("resume")}><Play size={13} aria-hidden="true" />解除急停</Button>
              ) : (
                <Button variant="danger" disabled={busy} onClick={() => void control("stop")}><OctagonPause size={13} aria-hidden="true" />急停</Button>
              )}
            </div>
            <p className="mt-2 text-xs text-fg-faint">急停后新的写操作在领取时被拒绝，已排队的任务同样受影响；已开始的外部操作无法瞬时撤回。规则复位使用同一个原因。</p>
          </Panel>
        )}

        <Panel title="处置规则" icon={<ShieldCheck size={14} />} meta={state && <span className="tabular">{state.rules.length}</span>} bodyClassName="p-0 overflow-x-auto">
          {!state ? (
            <EmptyState className="px-4 py-3" title="加载中…" />
          ) : state.rules.length === 0 ? (
            <EmptyState className="px-4 py-3" title="没有配置处置规则" hint="没有规则覆盖时只保留诊断并转人工，不产生写操作。" />
          ) : (
            <table className="w-full min-w-[760px] text-[13px]">
              <thead>
                <tr className="border-b border-line-soft text-left text-xs text-fg-faint">
                  <th className="px-4 py-2 font-medium">规则</th>
                  <th className="px-2 py-2 font-medium">动作</th>
                  <th className="px-2 py-2 font-medium">模式</th>
                  <th className="px-2 py-2 font-medium">覆盖告警</th>
                  <th className="px-2 py-2 font-medium">窗口内执行</th>
                  <th className="px-4 py-2 font-medium">状态</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-line-soft">
                {state.rules.map((rule) => {
                  const ratio = rule.max_executions > 0 ? Math.min(1, rule.executions / rule.max_executions) : 0;
                  return (
                    <tr key={rule.id} className="align-top">
                      <td className="px-4 py-2.5"><code className="whitespace-nowrap font-mono text-[12.5px] text-fg">{rule.id}</code></td>
                      <td className="px-2 py-2.5 font-mono text-[12.5px] text-fg-muted">{rule.action}</td>
                      <td className="px-2 py-2.5"><Badge tone={modeTone[rule.mode as keyof typeof modeTone] ?? "neutral"}>{modeLabel(rule.mode)}</Badge></td>
                      <td className="px-2 py-2.5 text-xs text-fg-muted">{rule.alerts.join("、")}</td>
                      <td className="px-2 py-2.5">
                        <span className="tabular text-[12.5px]">{rule.executions} / {rule.max_executions}</span>
                        <span className="ml-1 text-xs text-fg-faint">（{rule.window_minutes} 分钟）</span>
                        <span aria-hidden="true" className="mt-1 block h-1 w-24 overflow-hidden rounded-full bg-surface-2">
                          <span className={ratio >= 1 ? "block h-full bg-warn" : "block h-full bg-accent"} style={{ width: `${ratio * 100}%` }} />
                        </span>
                      </td>
                      <td className="px-4 py-2.5">
                        <div className="flex items-center gap-2">
                          {rule.blocked ? <Badge tone="danger" title={rule.blocked}>已阻断</Badge> : <Badge tone="ok">正常</Badge>}
                          {rule.blocked && admin && (
                            <Button size="xs" variant="ghost" disabled={busy} onClick={() => void control("reset", rule.id)}><RotateCcw size={12} aria-hidden="true" />复位</Button>
                          )}
                        </div>
                        {rule.blocked && <p className="mt-1 max-w-xs break-words text-[11.5px] text-fg-faint">{rule.blocked}</p>}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          )}
        </Panel>

        <Panel title="控制记录" icon={<ScrollText size={14} />} bodyClassName="p-0">
          {(state?.events ?? []).length === 0 ? (
            <EmptyState className="px-4 py-3" title="没有控制记录" />
          ) : (
            <ol className="divide-y divide-line-soft">
              {state!.events.map((event) => (
                <li key={event.id} className="flex items-start gap-3 px-4 py-2.5">
                  <Badge tone={event.kind === "emergency_stop" ? "danger" : event.kind === "rule_reset" ? "warn" : "neutral"}>{controlLabels[event.kind] ?? event.kind}</Badge>
                  <div className="min-w-0 flex-1">
                    <p className="break-words text-[13px] text-fg">{event.reason}</p>
                    <p className="mt-0.5 text-[11.5px] text-fg-faint">
                      {event.actor}{event.rule_id ? ` · ${event.rule_id}` : ""} · <span title={timeLabel(event.created_at, true)}>{relativeTime(event.created_at)}</span>
                    </p>
                  </div>
                </li>
              ))}
            </ol>
          )}
        </Panel>
      </PageBody>
    </>
  );
}

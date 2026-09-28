import { CircleCheck, Rocket } from "lucide-react";
import { useEffect, useState } from "react";

import { ApiError, Change, canOperate, getChanges, verifyRelease } from "../api";
import { useSession } from "../app/context";
import { PageBody, PageHeader } from "../components/layout/PageHeader";
import { Badge, Button, EmptyState, Mono, Notice, Panel } from "../components/ui";
import { relativeTime, timeLabel } from "../labels";

// Changes 是发布记录。只有人工确认健康的发布能作为 deployment_rollback 的回退目标；
// CI 用机器令牌登记发布，值班人在这里标记健康。
export function Changes() {
  const { session } = useSession();
  const operable = canOperate(session);
  const [changes, setChanges] = useState<Change[] | null>(null);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState<string | null>(null);

  const load = async () => {
    try {
      setChanges(await getChanges());
    } catch (cause) {
      setChanges([]);
      setMessage(cause instanceof ApiError ? cause.message : "发布记录加载失败");
    }
  };

  useEffect(() => {
    void load();
  }, []);

  const verify = async (change: Change) => {
    if (busy) return;
    setBusy(true);
    setMessage(null);
    try {
      await verifyRelease(change.id);
      setMessage(`发布 ${change.release_id} 已标记为健康。`);
      await load();
    } catch (cause) {
      setMessage(cause instanceof ApiError ? cause.message : "操作没有完成");
    } finally {
      setBusy(false);
    }
  };

  return (
    <>
      <PageHeader title="发布记录" description="版本与回退目标：只有人工确认健康的发布，才能作为回退目标。" />
      <PageBody>
        {message && <Notice tone="info">{message}</Notice>}
        <Panel title="发布与迁移" icon={<Rocket size={14} />} meta={changes && <span className="tabular">{changes.length}</span>} bodyClassName="p-0 overflow-x-auto">
          {changes === null ? (
            <EmptyState className="px-4 py-3" title="加载中…" />
          ) : changes.length === 0 ? (
            <EmptyState className="px-4 py-3" title="还没有发布记录" hint="CI 发布后用机器令牌调用 POST /api/v1/changes 登记。" />
          ) : (
            <table className="w-full min-w-[860px] text-[13px]">
              <thead>
                <tr className="border-b border-line-soft text-left text-xs text-fg-faint">
                  <th className="px-4 py-2 font-medium">时间</th>
                  <th className="px-2 py-2 font-medium">类型</th>
                  <th className="px-2 py-2 font-medium">发布</th>
                  <th className="px-2 py-2 font-medium">镜像</th>
                  <th className="px-2 py-2 font-medium">迁移</th>
                  <th className="px-2 py-2 font-medium">来源</th>
                  <th className="px-4 py-2 font-medium">健康确认</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-line-soft">
                {changes.map((change) => (
                  <tr key={change.id} className="align-top">
                    <td className="px-4 py-2.5 text-xs text-fg-muted" title={timeLabel(change.occurred_at, true)}>{relativeTime(change.occurred_at)}</td>
                    <td className="px-2 py-2.5"><Badge tone={change.change_type === "release" ? "accent" : "neutral"}>{change.change_type}</Badge></td>
                    <td className="px-2 py-2.5"><Mono>{change.release_id || "—"}</Mono></td>
                    <td className="max-w-72 px-2 py-2.5"><Mono className="text-[11px] !text-fg-muted">{change.image_ref || "—"}</Mono></td>
                    <td className="px-2 py-2.5 text-xs text-fg-muted">{change.db_migration}</td>
                    <td className="px-2 py-2.5 text-xs text-fg-muted">{change.source} · {change.actor}</td>
                    <td className="px-4 py-2.5">
                      {change.verified_at ? (
                        <span className="inline-flex items-center gap-1 text-xs text-ok" title={timeLabel(change.verified_at, true)}>
                          <CircleCheck size={13} aria-hidden="true" />{timeLabel(change.verified_at)}
                        </span>
                      ) : change.change_type === "release" && operable ? (
                        <Button size="xs" disabled={busy} onClick={() => void verify(change)}>标记健康</Button>
                      ) : (
                        <span className="text-xs text-fg-faint">否</span>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </Panel>
      </PageBody>
    </>
  );
}

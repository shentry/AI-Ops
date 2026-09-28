import { ArrowLeft, BookOpen, FileText, Search, Trash2, Wrench } from "lucide-react";
import { FormEvent, useEffect, useState } from "react";
import { Link, useSearchParams } from "react-router";

import { ApiError, KnowledgeEntry, Skill, deleteKnowledgeEntry, getKnowledgeEntry, getSkills, searchKnowledge } from "../api";
import { useSession } from "../app/context";
import { MarkdownText } from "../components/incident/Markdown";
import { PageBody, PageHeader } from "../components/layout/PageHeader";
import { Badge, Button, EmptyState, Mono, Notice, SelectInput, Tabs, TextInput } from "../components/ui";
import { relativeTime, timeLabel } from "../labels";

type Tab = "skills" | "docs";

const sourceLabels: Record<KnowledgeEntry["source"], string> = { repo: "仓库手册", incident: "事件复盘" };

// Knowledge 是给模型看的知识：排查技能按告警名匹配后放在证据之前；参考文档由模型
// 用 knowledge_search 按需检索。技能与仓库手册只能通过仓库修改，事件复盘由操作人
// 从 Incident 复盘卡片加入、管理员删除。
export function Knowledge() {
  const [params, setParams] = useSearchParams();
  const tab: Tab = params.get("tab") === "docs" ? "docs" : "skills";
  const [skills, setSkills] = useState<Skill[] | null>(null);
  const [entries, setEntries] = useState<KnowledgeEntry[] | null>(null);
  const [message, setMessage] = useState<string | null>(null);

  useEffect(() => {
    let active = true;
    void getSkills().then((value) => {
      if (active) setSkills(value);
    }).catch((cause) => {
      if (!active) return;
      setSkills([]);
      setMessage(cause instanceof ApiError ? cause.message : "排查技能加载失败");
    });
    return () => {
      active = false;
    };
  }, []);

  return (
    <>
      <PageHeader title="知识库" description="诊断时交给模型的参考：排查技能按告警名匹配、放在证据之前；参考文档由模型按需检索。它们只说明怎么查，不是证据，也不构成执行授权。" />
      <PageBody>
        {message && <Notice tone="danger">{message}</Notice>}
        <div className="overflow-hidden rounded-lg border border-line bg-surface">
          <Tabs idBase="knowledge" label="知识库内容" value={tab} onChange={(key) => setParams(key === "docs" ? { tab: "docs" } : {})}
            items={[{ key: "skills", label: "排查技能", count: skills?.length }, { key: "docs", label: "参考文档", count: entries?.length }]} />
          <div id={`knowledge-panel-${tab}`} role="tabpanel" aria-labelledby={`knowledge-tab-${tab}`}>
            {tab === "skills" ? (
              <div className="divide-y divide-line-soft">
                {skills === null ? (
                  <EmptyState className="px-4 py-3" title="加载中…" />
                ) : skills.length === 0 ? (
                  <EmptyState className="px-4 py-3" title="没有排查技能" />
                ) : skills.map((skill) => <SkillRow key={skill.name} skill={skill} />)}
              </div>
            ) : (
              <Documents entries={entries} setEntries={setEntries} />
            )}
          </div>
        </div>
      </PageBody>
    </>
  );
}

function SkillRow({ skill }: { skill: Skill }) {
  return (
    <article aria-label={`技能 ${skill.name}`} className="space-y-2 px-4 py-3">
      <div className="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1">
        <h2 className="flex items-center gap-1.5 text-[13px] font-medium text-fg">
          <BookOpen size={13} aria-hidden="true" className="text-fg-faint" />
          <Mono>{skill.name}</Mono>
        </h2>
        <span className="text-xs text-fg-faint">
          近 30 天激活 <span className="tabular text-fg-muted">{skill.activations_30d}</span> 次 · SHA <Mono className="!text-fg-faint">{skill.sha256.slice(0, 12)}</Mono>
        </span>
      </div>
      <p className="text-[13px] text-fg-muted">{skill.description}</p>
      <div className="flex flex-wrap items-center gap-1.5 text-xs">
        <span className="text-fg-faint">匹配告警</span>
        {skill.alerts.map((alert) => <Badge key={alert} tone="accent">{alert}</Badge>)}
      </div>
      <div className="flex flex-wrap items-center gap-1.5 text-xs">
        <span className="inline-flex items-center gap-1 text-fg-faint"><Wrench size={12} aria-hidden="true" />工具</span>
        {skill.tools.map((tool) => <Badge key={tool}>{tool}</Badge>)}
      </div>
      <details>
        <summary className="cursor-pointer text-xs text-fg-muted hover:text-fg">查看排查步骤</summary>
        <MarkdownText className="mt-2 text-[13px]">{skill.body}</MarkdownText>
      </details>
    </article>
  );
}

function Documents({ entries, setEntries }: { entries: KnowledgeEntry[] | null; setEntries: (value: KnowledgeEntry[] | null) => void }) {
  const { session } = useSession();
  const [query, setQuery] = useState("");
  const [source, setSource] = useState("");
  const [open, setOpen] = useState<KnowledgeEntry | null>(null);
  const [confirming, setConfirming] = useState(false);
  const [message, setMessage] = useState<string | null>(null);

  const load = async (q: string, s: string) => {
    setMessage(null);
    try {
      setEntries(await searchKnowledge(q.trim(), s));
    } catch (cause) {
      setEntries([]);
      setMessage(cause instanceof ApiError ? cause.message : "参考文档加载失败");
    }
  };

  useEffect(() => {
    // Loads once; later loads come from the search form.
    void load("", "");
  }, []);

  const submit = (event: FormEvent) => {
    event.preventDefault();
    void load(query, source);
  };

  const show = async (entry: KnowledgeEntry) => {
    setConfirming(false);
    try {
      setOpen(await getKnowledgeEntry(entry.id));
    } catch (cause) {
      setMessage(cause instanceof ApiError ? cause.message : "条目加载失败");
    }
  };

  const remove = async (entry: KnowledgeEntry) => {
    try {
      await deleteKnowledgeEntry(entry.id);
      setOpen(null);
      await load(query, source);
      setMessage(`已删除：${entry.title}`);
    } catch (cause) {
      setMessage(cause instanceof ApiError ? cause.message : "没有删除");
    }
  };

  if (open) {
    const incidentID = open.source === "incident" ? Number(open.ref.replace("incident/", "")) : 0;
    return (
      <article aria-label={open.title} className="space-y-3 px-4 py-3">
        <button type="button" onClick={() => setOpen(null)} className="inline-flex items-center gap-1 text-xs text-fg-muted hover:text-fg">
          <ArrowLeft size={12} aria-hidden="true" />返回列表
        </button>
        <div className="flex flex-wrap items-start justify-between gap-2">
          <div>
            <h2 className="text-[14px] font-medium text-fg">{open.title}</h2>
            <p className="mt-0.5 text-xs text-fg-faint">
              <Badge tone={open.source === "incident" ? "accent" : "neutral"}>{sourceLabels[open.source]}</Badge>{" "}
              <Mono className="!text-fg-faint">{open.ref}</Mono> · {open.created_by} · <span title={timeLabel(open.updated_at, true)}>{relativeTime(open.updated_at)}</span>
              {incidentID > 0 && <> · <Link className="text-accent hover:underline" to={`/incidents/${incidentID}`}>打开 Incident</Link></>}
            </p>
          </div>
          {open.source === "incident" && session.role === "admin" && (
            confirming ? (
              <div className="flex items-center gap-1.5">
                <Button size="xs" variant="danger" onClick={() => void remove(open)}>确认删除</Button>
                <Button size="xs" onClick={() => setConfirming(false)}>取消</Button>
              </div>
            ) : (
              <Button size="xs" onClick={() => setConfirming(true)}><Trash2 size={13} aria-hidden="true" />删除</Button>
            )
          )}
        </div>
        <MarkdownText className="text-[13px]">{open.body}</MarkdownText>
      </article>
    );
  }

  return (
    <div>
      <form role="search" onSubmit={submit} className="flex flex-wrap items-center gap-2 border-b border-line-soft px-4 py-3">
        <label htmlFor="knowledge-query" className="sr-only">搜索参考文档</label>
        <div className="min-w-0 flex-1 basis-56">
          <TextInput id="knowledge-query" placeholder="例如：数据库密码错误、上游 429" value={query} onChange={(event) => setQuery(event.target.value.slice(0, 200))} />
        </div>
        <label htmlFor="knowledge-source" className="sr-only">来源</label>
        <div className="w-32 shrink-0">
          <SelectInput id="knowledge-source" value={source} onChange={(event) => setSource(event.target.value)}>
            <option value="">全部来源</option>
            <option value="repo">仓库手册</option>
            <option value="incident">事件复盘</option>
          </SelectInput>
        </div>
        <Button type="submit"><Search size={13} aria-hidden="true" />搜索</Button>
      </form>
      {message && <div className="px-4 pt-3"><Notice tone="info">{message}</Notice></div>}
      {entries === null ? (
        <EmptyState className="px-4 py-3" title="加载中…" />
      ) : entries.length === 0 ? (
        <EmptyState className="px-4 py-3" title="没有匹配的条目" hint="事件复盘在 Incident 的复盘卡片上用「加入知识库」添加。" />
      ) : (
        <ul className="divide-y divide-line-soft">
          {entries.map((entry) => (
            <li key={entry.id}>
              <button type="button" onClick={() => void show(entry)} className="block w-full px-4 py-2.5 text-left hover:bg-surface-2">
                <span className="flex items-start gap-1.5">
                  <FileText size={13} aria-hidden="true" className="mt-0.5 shrink-0 text-fg-faint" />
                  <span className="min-w-0 flex-1 text-[13px] font-medium text-fg">{entry.title}</span>
                  <span className="shrink-0 text-[11px] text-fg-faint">{relativeTime(entry.updated_at)}</span>
                </span>
                <span className="mt-1 flex items-start gap-1.5 pl-[19px] text-xs text-fg-muted">
                  <Badge tone={entry.source === "incident" ? "accent" : "neutral"}>{sourceLabels[entry.source]}</Badge>
                  {entry.snippet && <span className="min-w-0">{entry.snippet}</span>}
                </span>
              </button>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

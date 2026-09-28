import { BookOpen, Wrench } from "lucide-react";
import { useEffect, useState } from "react";

import { ApiError, Skill, getSkills } from "../api";
import { MarkdownText } from "../components/incident/Markdown";
import { PageBody, PageHeader } from "../components/layout/PageHeader";
import { Badge, EmptyState, Mono, Notice, Tabs } from "../components/ui";

type Tab = "skills";

// Knowledge 是给模型看的知识：排查技能按告警名匹配后放在证据之前。
// 技能只能通过仓库修改，这里只读。
export function Knowledge() {
  const [tab, setTab] = useState<Tab>("skills");
  const [skills, setSkills] = useState<Skill[] | null>(null);
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
      <PageHeader title="知识库" description="诊断时交给模型的参考：排查技能按告警名匹配，放在证据之前；它们只说明怎么查，不构成执行授权。" />
      <PageBody>
        {message && <Notice tone="danger">{message}</Notice>}
        <div className="overflow-hidden rounded-lg border border-line bg-surface">
          <Tabs idBase="knowledge" label="知识库内容" value={tab} onChange={setTab}
            items={[{ key: "skills", label: "排查技能", count: skills?.length }]} />
          <div id="knowledge-panel-skills" role="tabpanel" aria-labelledby="knowledge-tab-skills" className="divide-y divide-line-soft">
            {skills === null ? (
              <EmptyState className="px-4 py-3" title="加载中…" />
            ) : skills.length === 0 ? (
              <EmptyState className="px-4 py-3" title="没有排查技能" />
            ) : skills.map((skill) => <SkillRow key={skill.name} skill={skill} />)}
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
      <details className="group">
        <summary className="cursor-pointer text-xs text-fg-muted hover:text-fg">查看排查步骤</summary>
        <MarkdownText className="mt-2 text-[13px]">{skill.body}</MarkdownText>
      </details>
    </article>
  );
}

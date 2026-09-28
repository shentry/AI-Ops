import { Bot, ChevronRight, Cpu, SendHorizontal, ShieldCheck, Wrench } from "lucide-react";
import { FormEvent, KeyboardEvent, RefObject, useEffect, useMemo, useRef, useState } from "react";

import { ApiError, ConversationCitation, ConversationMessage, SuggestedAction, askQuestion } from "../../api";
import { useSession } from "../../app/context";
import { relativeTime, roleLabel, statusLabel, timeLabel } from "../../labels";
import { statusTone } from "../../tone";
import { Badge, Button, cx } from "../ui";
import { MarkdownText } from "./Markdown";

interface ConversationProps {
  incidentID: number;
  readOnly: boolean;
  messages: ConversationMessage[];
  onMessage: (message: ConversationMessage) => void;
  composerRef: RefObject<HTMLTextAreaElement | null>;
}

const examples = ["现在到底卡在哪一步？", "Redis 那条证据为什么采集失败？", "为什么 Policy 降级为人工审批？"];

// Conversation：围绕这个 Incident 问排障 Agent。回答只引用已落库的步骤和事件，
// 只能调用只读工具，不会触发任何变更。
export function Conversation({ incidentID, readOnly, messages, onMessage, composerRef }: ConversationProps) {
  const { model } = useSession();
  const [question, setQuestion] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const end = useRef<HTMLDivElement | null>(null);
  const ordered = useMemo(() => [...messages].sort((a, b) => a.id - b.id), [messages]);

  useEffect(() => {
    end.current?.scrollIntoView({ block: "nearest" });
  }, [ordered.length]);

  const submit = async (event?: FormEvent) => {
    event?.preventDefault();
    const value = question.trim();
    if (!value || busy || readOnly) return;
    setBusy(true);
    setError(null);
    try {
      onMessage(await askQuestion(incidentID, value));
      setQuestion("");
    } catch (cause) {
      setError(cause instanceof ApiError ? cause.message : "提问没有提交成功");
    } finally {
      setBusy(false);
    }
  };

  // Enter 发送、Shift+Enter 换行；中文输入法选词时的 Enter 不算发送。
  const onKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) {
      event.preventDefault();
      void submit();
    }
  };

  return (
    <div className="flex flex-col">
      <div aria-live="polite" className="space-y-4 px-4 py-4">
        {ordered.length === 0 ? (
          <div className="rounded-lg border border-dashed border-line px-4 py-5">
            <p className="text-[13px] text-fg">可以直接问排障 Agent</p>
            <p className="mt-1 text-xs text-fg-faint">回答会引用已落库的步骤和事件；提问只查证据，不会触发任何变更动作。</p>
            {!readOnly && (
              <div className="mt-3 flex flex-wrap gap-1.5">
                {examples.map((example) => (
                  <button key={example} type="button" onClick={() => { setQuestion(example); composerRef.current?.focus(); }} className="cursor-pointer rounded-full border border-line px-2.5 py-1 text-xs text-fg-muted hover:border-fg-faint/50 hover:text-fg">
                    {example}
                  </button>
                ))}
              </div>
            )}
          </div>
        ) : ordered.map((message) => <Message key={message.id || `${message.created_at}-${message.role}`} message={message} />)}
        <div ref={end} />
      </div>

      <form onSubmit={submit} className="sticky bottom-0 border-t border-line-soft bg-surface px-4 pb-3 pt-3">
        {error && <p role="alert" className="mb-2 text-xs text-danger">{error}</p>}
        <div className="rounded-lg border border-line bg-canvas focus-within:border-accent/60 focus-within:ring-2 focus-within:ring-ring">
          <label className="sr-only" htmlFor="incident-question">问题</label>
          <textarea
            ref={composerRef}
            id="incident-question"
            value={question}
            onChange={(event) => setQuestion(event.target.value.slice(0, 4000))}
            onKeyDown={onKeyDown}
            placeholder={readOnly ? "只读账号不能提问" : "问排障 Agent… Enter 发送，Shift+Enter 换行"}
            rows={2}
            maxLength={4000}
            disabled={busy || readOnly}
            className="block max-h-48 min-h-[52px] w-full resize-y bg-transparent px-3 pt-2.5 text-[13px] leading-relaxed text-fg placeholder:text-fg-faint focus:outline-none disabled:cursor-not-allowed"
          />
          <div className="flex items-center gap-2 px-2 pb-2">
            {model?.current_model && (
              <span className="inline-flex items-center gap-1 rounded-md border border-line-soft px-1.5 py-0.5 font-mono text-[11px] text-fg-faint">
                <Cpu size={11} aria-hidden="true" />{model.current_model}
              </span>
            )}
            <span className="inline-flex items-center gap-1 text-[11px] text-fg-faint">
              <ShieldCheck size={11} aria-hidden="true" />只读工具
            </span>
            <span className="tabular ml-auto text-[11px] text-fg-faint">{question.length}/4000</span>
            <Button type="submit" variant="primary" size="sm" disabled={busy || readOnly || !question.trim()}>
              {busy ? "提交中…" : "提问"}
              <SendHorizontal size={12} aria-hidden="true" />
            </Button>
          </div>
        </div>
      </form>
    </div>
  );
}

function Message({ message }: { message: ConversationMessage }) {
  if (message.role === "tool") return <ToolMessage message={message} />;
  const citations = message.citations ?? [];
  const uncertainties = message.uncertainties ?? [];
  const suggestions = message.suggested_actions ?? [];
  const settled = message.status === "answered" || message.status === "completed" || message.status === "succeeded";

  if (message.role === "user") {
    return (
      <article className="flex flex-col items-end">
        <div className="max-w-[85%] rounded-2xl rounded-br-md border border-line-soft bg-surface-2 px-3.5 py-2 text-[13px] text-fg">
          <p className="whitespace-pre-wrap break-words">{message.content}</p>
        </div>
        <p className="mt-1 text-[11px] text-fg-faint">
          {message.actor_name || roleLabel(message.role)} · <time dateTime={message.created_at} title={timeLabel(message.created_at, true)}>{relativeTime(message.created_at)}</time>
        </p>
      </article>
    );
  }

  return (
    <article className="flex gap-3">
      <span aria-hidden="true" className="mt-0.5 grid size-7 shrink-0 place-items-center rounded-full bg-accent/14 text-accent">
        <Bot size={14} />
      </span>
      <div className="min-w-0 flex-1">
        <div className="mb-1 flex flex-wrap items-center gap-2 text-xs">
          <span className="font-medium text-fg">{message.role === "assistant" ? "排障 Agent" : roleLabel(message.role)}</span>
          {!settled && <Badge tone={statusTone(message.status)} dot pulse={message.status === "queued" || message.status === "running"}>{statusLabel(message.status)}</Badge>}
          <time dateTime={message.created_at} title={timeLabel(message.created_at, true)} className="text-fg-faint">{relativeTime(message.created_at)}</time>
        </div>
        {message.content
          ? <MarkdownText>{message.content}</MarkdownText>
          : <p className="text-[13px] text-fg-faint">{message.status === "queued" ? "问题已排队，等待 Agent 回答…" : "没有回答内容"}</p>}
        {message.tool_name && (
          <span className="mt-2 inline-flex items-center gap-1 rounded border border-line-soft px-1.5 py-0.5 font-mono text-[11px] text-fg-faint"><Wrench size={10} aria-hidden="true" />{message.tool_name}</span>
        )}
        {citations.length > 0 && (
          <div className="mt-2.5 flex flex-wrap items-center gap-1.5">
            <span className="text-[11px] text-fg-faint">依据</span>
            {citations.slice(0, 12).map((citation, index) => (
              <cite key={`${citationID(citation)}-${index}`} title={citation.quote} className="rounded border border-line-soft bg-surface-2 px-1.5 text-[11px] not-italic text-fg-muted">
                {citationLabel(citation)}
              </cite>
            ))}
          </div>
        )}
        {uncertainties.length > 0 && (
          <p className="mt-2 rounded-md border border-warn/30 bg-warn/8 px-2.5 py-1.5 text-xs text-fg-muted"><b className="font-medium text-warn">还不确定：</b>{uncertainties.slice(0, 3).join(" · ")}</p>
        )}
        {suggestions.length > 0 && (
          <p className="mt-2 text-xs text-fg-muted"><b className="font-medium text-fg">建议：</b>{suggestions.slice(0, 3).map(suggestionLabel).join(" · ")}</p>
        )}
      </div>
    </article>
  );
}

function ToolMessage({ message }: { message: ConversationMessage }) {
  const [open, setOpen] = useState(false);
  return (
    <div className="ml-10 overflow-hidden rounded-md border border-line-soft">
      <button type="button" aria-expanded={open} onClick={() => setOpen((value) => !value)} className="flex w-full cursor-pointer items-center gap-2 px-3 py-1.5 text-left hover:bg-surface-2/60">
        <Wrench size={12} aria-hidden="true" className="text-fg-faint" />
        <span className="min-w-0 flex-1 truncate font-mono text-xs text-fg-muted">{message.tool_name || "工具调用"}</span>
        <span className="text-[11px] text-fg-faint">{statusLabel(message.status)}</span>
        <ChevronRight size={13} aria-hidden="true" className={cx("text-fg-faint transition-transform", open && "rotate-90")} />
      </button>
      {open && <pre className="max-h-64 overflow-auto border-t border-line-soft bg-canvas px-3 py-2 font-mono text-[11.5px] leading-5 whitespace-pre-wrap break-words text-fg-muted">{message.content || "（无输出）"}</pre>}
    </div>
  );
}

const citationKinds: Record<string, string> = {
  run_step: "步骤", step: "步骤", event: "事件", evidence: "证据", approval: "审批", problem: "问题", incident: "Incident",
};

const suggestionLabels: Record<string, string> = {
  collect_evidence: "补采证据", rediagnose: "重新诊断", manual_review: "人工复核",
};

function citationID(value: ConversationCitation): string {
  return String(value.id ?? value.event_id ?? value.step_id ?? value.reference ?? "citation");
}

function citationLabel(value: ConversationCitation): string {
  const raw = value.reference || value.label || value.kind || value.type || "";
  const reference = citationKinds[raw] || raw || "依据";
  const id = value.id ?? value.event_id ?? value.step_id;
  return `${reference}${id ? ` #${id}` : ""}`;
}

function suggestionLabel(value: SuggestedAction): string {
  const type = value.type ?? "manual_review";
  return [suggestionLabels[type] ?? type.replace(/_/g, " "), value.target, value.reason || value.description].filter(Boolean).join(" · ");
}

import { Bot, SendHorizontal, Wrench } from "lucide-react";
import { FormEvent, useMemo, useState } from "react";

import { ApiError, ConversationMessage, askQuestion } from "../api";
import { roleLabel, statusLabel, timeLabel } from "../labels";

interface ConversationPanelProps {
  incidentID: number;
  messages: ConversationMessage[];
  onMessage: (message: ConversationMessage) => void;
}

export function ConversationPanel({ incidentID, messages, onMessage }: ConversationPanelProps) {
  const [question, setQuestion] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const orderedMessages = useMemo(() => [...messages].sort((a, b) => a.id - b.id), [messages]);

  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const value = question.trim();
    if (!value || busy) return;
    setBusy(true);
    setError(null);
    try {
      const message = await askQuestion(incidentID, value);
      onMessage(message);
      setQuestion("");
    } catch (cause) {
      setError(cause instanceof ApiError ? cause.message : "提问没有提交成功");
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="panel conversation-panel" aria-labelledby="conversation-title">
      <div className="panel-heading">
        <div>
          <span className="eyebrow">排障 Agent</span>
          <h2 id="conversation-title">问 Agent</h2>
        </div>
        <span className="panel-caption">只读证据，不会执行变更</span>
      </div>
      <div className="conversation-log" aria-live="polite">
        {orderedMessages.length === 0 ? (
          <div className="empty-state conversation-empty">
            <p>可以直接问：现在到底卡在哪一步？</p>
            <small>回答会引用已落库的步骤和事件；提问只查证据，不会触发任何变更动作。</small>
          </div>
        ) : orderedMessages.map((message) => <MessageBubble key={message.id || `${message.created_at}-${message.role}`} message={message} />)}
      </div>
      {error && <div className="inline-error" role="alert">{error}</div>}
      <form className="question-form" onSubmit={submit}>
        <label className="sr-only" htmlFor="incident-question">问题</label>
        <textarea
          id="incident-question"
          className="text-input question-input"
          value={question}
          onChange={(event) => setQuestion(event.target.value.slice(0, 4000))}
          placeholder="例如：Redis 那条证据为什么采集失败？"
          rows={3}
          disabled={busy}
          maxLength={4000}
        />
        <div className="question-footer">
          <small>{question.length}/4000 · 只允许只读工具</small>
          <button className="button primary" type="submit" disabled={busy || !question.trim()}>
            {busy ? "提交中…" : "提问"}
            <SendHorizontal size={13} />
          </button>
        </div>
      </form>
    </section>
  );
}

function MessageBubble({ message }: { message: ConversationMessage }) {
  const citations = message.citations ?? [];
  const uncertainties = message.uncertainties ?? [];
  const suggestedActions = message.suggested_actions ?? [];
  return (
    <article className={`message-bubble role-${message.role}`}>
      <div className="message-meta">
        <strong>{message.role === "assistant" ? <Bot size={13} /> : null}{message.role === "assistant" ? "排障 Agent" : message.actor_name || roleLabel(message.role)}</strong>
        <time dateTime={message.created_at}>{timeLabel(message.created_at)}</time>
        <span className={`message-status status-${message.status}`}>{statusLabel(message.status)}</span>
      </div>
      <p className="message-content">{message.content || (message.status === "queued" ? "问题已排队，等待 Agent 回答…" : "")}</p>
      {message.tool_name && <span className="tool-chip"><Wrench size={10} />工具 · {message.tool_name}</span>}
      {citations.length > 0 && (
        <div className="citation-list"><span>依据</span>{citations.slice(0, 12).map((citation, index) => <cite key={`${citationID(citation)}-${index}`}>{citationLabel(citation)}</cite>)}</div>
      )}
      {uncertainties.length > 0 && <p className="uncertainty"><b>还不确定：</b>{uncertainties.slice(0, 3).join(" · ")}</p>}
      {suggestedActions.length > 0 && <p className="suggested-actions"><b>建议：</b>{suggestedActions.slice(0, 3).map(actionLabel).join(" · ")}</p>}
    </article>
  );
}

function citationID(value: NonNullable<ConversationMessage["citations"]>[number]): string {
  return String(value.id ?? value.event_id ?? value.step_id ?? value.reference ?? "citation");
}

const citationKinds: Record<string, string> = {
  run_step: "步骤",
  step: "步骤",
  event: "事件",
  evidence: "证据",
  approval: "审批",
  problem: "问题",
  incident: "Incident",
};

const suggestedActionLabels: Record<string, string> = {
  collect_evidence: "补采证据",
  rediagnose: "重新诊断",
  manual_review: "人工复核",
};

function citationLabel(value: NonNullable<ConversationMessage["citations"]>[number]): string {
  const raw = value.reference || value.label || value.kind || value.type || "";
  const reference = citationKinds[raw] || raw || "依据";
  const id = value.id ?? value.event_id ?? value.step_id;
  return `${reference}${id ? ` #${id}` : ""}`;
}

function actionLabel(value: NonNullable<ConversationMessage["suggested_actions"]>[number]): string {
  const type = value.type ?? "manual_review";
  const label = suggestedActionLabels[type] ?? type.replace(/_/g, " ");
  return [label, value.target, value.reason || value.description].filter(Boolean).join(" · ");
}

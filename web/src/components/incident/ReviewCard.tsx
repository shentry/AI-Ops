import { BookPlus, ClipboardCheck } from "lucide-react";
import { useEffect, useState } from "react";

import { ApiError, Review, ReviewInput, addIncidentKnowledge, addReview, getReviews } from "../../api";
import { relativeTime, timeLabel } from "../../labels";
import { Badge, Button, FieldLabel, Panel, SelectInput, TextArea, TextInput } from "../ui";

const verdictLabels: Record<string, string> = { correct: "正确", partial: "部分正确", wrong: "错误", unknown: "无法判断" };
const verdictTone = { correct: "ok", partial: "warn", wrong: "danger", unknown: "neutral" } as const;

interface ReviewCardProps {
  incidentID: number;
  runID: number | null;
  approvalID: number | null;
  readOnly: boolean;
}

// ReviewCard 记录真实根因和处理，并评价诊断或动作是否正确。动作被标为错误
// 会阻断该规则的自动执行，直到管理员复位；「无法判断」不计为正确。
// 评审人由服务端按登录身份记录，页面从不发送。
export function ReviewCard({ incidentID, runID, approvalID, readOnly }: ReviewCardProps) {
  const [reviews, setReviews] = useState<Review[]>([]);
  const [subject, setSubject] = useState<ReviewInput["subject"]>(approvalID ? "action" : "diagnosis");
  const [verdict, setVerdict] = useState<ReviewInput["verdict"]>("correct");
  const [rootCause, setRootCause] = useState("");
  const [actualFix, setActualFix] = useState("");
  const [minutes, setMinutes] = useState(0);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState<string | null>(null);

  useEffect(() => {
    let active = true;
    void getReviews(incidentID).then((rows) => {
      if (active) setReviews(rows);
    }).catch(() => {
      if (active) setMessage("复盘记录加载失败");
    });
    return () => {
      active = false;
    };
  }, [incidentID]);

  const target = subject === "action" ? approvalID : runID;
  // Same rule as the server: only a confirmed root cause becomes knowledge.
  const confirmed = reviews.some((review) => review.verdict === "correct" || (review.verdict !== "unknown" && review.root_cause.trim() !== ""));
  const addToKnowledge = async () => {
    if (busy || readOnly) return;
    setBusy(true);
    setMessage(null);
    try {
      await addIncidentKnowledge(incidentID);
      setMessage("已加入知识库：诊断时模型可以用 knowledge_search 检索到这次复盘。");
    } catch (cause) {
      setMessage(cause instanceof ApiError ? cause.message : "没有加入知识库");
    } finally {
      setBusy(false);
    }
  };
  const submit = async () => {
    if (busy || readOnly || !target) return;
    setBusy(true);
    setMessage(null);
    try {
      const input: ReviewInput = { subject, verdict, root_cause: rootCause.trim(), actual_fix: actualFix.trim(), manual_minutes: Math.max(0, minutes) };
      if (subject === "action") input.approval_id = target;
      else input.run_id = target;
      const saved = await addReview(incidentID, input);
      setReviews((previous) => [saved, ...previous]);
      setRootCause("");
      setActualFix("");
      setMinutes(0);
      setMessage(subject === "action" && verdict === "wrong" ? "已记录。该规则的自动执行已阻断，需要管理员复位。" : "已记录复盘。");
    } catch (cause) {
      setMessage(cause instanceof ApiError ? cause.message : "复盘没有保存");
    } finally {
      setBusy(false);
    }
  };

  return (
    <Panel title="复盘标注" icon={<ClipboardCheck size={14} />} meta={<span className="tabular">{reviews.length} 条</span>}
      actions={!readOnly && (
        <Button size="xs" disabled={busy || !confirmed} title={confirmed ? "把最近一次确认过根因的复盘写入知识库" : "需要先有确认了根因的复盘"} onClick={() => void addToKnowledge()}>
          <BookPlus size={13} aria-hidden="true" />加入知识库
        </Button>
      )}>
      {!readOnly && (
        <div className="space-y-2.5">
          <div className="grid grid-cols-2 gap-2">
            <div>
              <FieldLabel htmlFor="review-subject">评价对象</FieldLabel>
              <SelectInput id="review-subject" value={subject} onChange={(event) => setSubject(event.target.value as ReviewInput["subject"])}>
                <option value="action" disabled={!approvalID}>动作{approvalID ? ` #${approvalID}` : "（无）"}</option>
                <option value="diagnosis" disabled={!runID}>诊断{runID ? ` #${runID}` : "（无）"}</option>
              </SelectInput>
            </div>
            <div>
              <FieldLabel htmlFor="review-verdict">结论</FieldLabel>
              <SelectInput id="review-verdict" value={verdict} onChange={(event) => setVerdict(event.target.value as ReviewInput["verdict"])}>
                {Object.entries(verdictLabels).map(([value, label]) => <option key={value} value={value}>{label}</option>)}
              </SelectInput>
            </div>
          </div>
          <div>
            <FieldLabel htmlFor="review-root-cause">真实根因</FieldLabel>
            <TextArea id="review-root-cause" rows={2} value={rootCause} onChange={(event) => setRootCause(event.target.value.slice(0, 1000))} />
          </div>
          <div>
            <FieldLabel htmlFor="review-fix">实际处理</FieldLabel>
            <TextArea id="review-fix" rows={2} value={actualFix} onChange={(event) => setActualFix(event.target.value.slice(0, 1000))} />
          </div>
          <div className="flex items-end gap-2">
            <div className="w-32">
              <FieldLabel htmlFor="review-minutes">人工处理分钟数</FieldLabel>
              <TextInput id="review-minutes" type="number" min={0} value={minutes} onChange={(event) => setMinutes(Number(event.target.value) || 0)} />
            </div>
            <Button variant="primary" className="ml-auto" disabled={busy || !target} onClick={() => void submit()}>保存复盘</Button>
          </div>
        </div>
      )}
      {message && <p role="status" className="mt-2.5 text-xs text-fg-muted">{message}</p>}
      {reviews.length > 0 ? (
        <ul className="mt-3 space-y-2">
          {reviews.map((review) => (
            <li key={review.id} className="rounded-md border border-line-soft px-3 py-2">
              <div className="flex flex-wrap items-center gap-1.5">
                <span className="text-[12.5px] font-medium text-fg">{review.subject === "action" ? `动作 #${review.approval_id}` : `诊断 #${review.run_id}`}</span>
                <Badge tone={verdictTone[review.verdict as keyof typeof verdictTone] ?? "neutral"}>{verdictLabels[review.verdict] ?? review.verdict}</Badge>
                <span className="ml-auto text-[11px] text-fg-faint" title={timeLabel(review.created_at, true)}>
                  {review.reviewer} · {relativeTime(review.created_at)}{review.manual_minutes > 0 ? ` · 人工 ${review.manual_minutes} 分钟` : ""}
                </span>
              </div>
              {review.root_cause && <p className="mt-1 text-xs text-fg-muted">根因：{review.root_cause}</p>}
              {review.actual_fix && <p className="mt-0.5 text-xs text-fg-muted">处理：{review.actual_fix}</p>}
            </li>
          ))}
        </ul>
      ) : readOnly ? (
        <p className="text-xs text-fg-faint">还没有复盘记录。</p>
      ) : null}
    </Panel>
  );
}

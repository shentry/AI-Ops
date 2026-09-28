package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"oncall-agent/internal/store"
	"oncall-agent/internal/tools"
)

// ErrNotReviewed: only a human-confirmed root cause becomes knowledge.
var ErrNotReviewed = errors.New("knowledge: the incident has no review with a confirmed root cause")

// IncidentStore is what adding a reviewed incident reads and writes.
type IncidentStore interface {
	GetIncident(ctx context.Context, id uint64) (store.Incident, error)
	ListIncidentAlerts(ctx context.Context, incidentID uint64) ([]store.Alert, error)
	ListReviews(ctx context.Context, incidentID uint64) ([]store.Review, error)
	GetAgentRun(ctx context.Context, id uint64) (store.AgentRun, error)
	GetApproval(ctx context.Context, id uint64) (store.Approval, error)
	SaveIncidentKnowledge(ctx context.Context, incidentID uint64, entry store.KnowledgeEntry) (store.KnowledgeEntry, error)
}

// AddIncident writes the incident's latest confirmed review as a knowledge
// entry: its alerts, the confirmed root cause, what was done and how the
// action verified. Adding it again rewrites the same entry.
func AddIncident(ctx context.Context, db IncidentStore, incidentID uint64, actor string, now time.Time) (store.KnowledgeEntry, error) {
	incident, err := db.GetIncident(ctx, incidentID)
	if err != nil {
		return store.KnowledgeEntry{}, err
	}
	reviews, err := db.ListReviews(ctx, incidentID)
	if err != nil {
		return store.KnowledgeEntry{}, err
	}
	alerts, err := db.ListIncidentAlerts(ctx, incidentID)
	if err != nil {
		return store.KnowledgeEntry{}, err
	}
	var review *store.Review
	var run *store.AgentRun
	for i := range reviews { // newest first
		candidate := reviews[i]
		if candidate.Verdict == "unknown" || (candidate.Verdict != "correct" && strings.TrimSpace(candidate.RootCause) == "") {
			continue
		}
		if candidate.RunID != nil {
			value, err := db.GetAgentRun(ctx, *candidate.RunID)
			if err != nil {
				return store.KnowledgeEntry{}, err
			}
			run = &value
		}
		review = &candidate
		break
	}
	if review == nil || (strings.TrimSpace(review.RootCause) == "" && (run == nil || run.RCAText == nil)) {
		return store.KnowledgeEntry{}, ErrNotReviewed
	}
	var approval *store.Approval
	if review.ApprovalID != nil {
		value, err := db.GetApproval(ctx, *review.ApprovalID)
		if err != nil {
			return store.KnowledgeEntry{}, err
		}
		approval = &value
	}
	entry := incidentEntry(incident, alerts, *review, run, approval)
	entry.CreatedBy, entry.UpdatedAt = actor, now
	return db.SaveIncidentKnowledge(ctx, incidentID, entry)
}

var verdictLabels = map[string]string{"correct": "诊断正确", "partial": "诊断部分正确，已人工修正", "wrong": "诊断错误，已人工修正"}

func incidentEntry(incident store.Incident, alerts []store.Alert, review store.Review, run *store.AgentRun, approval *store.Approval) store.KnowledgeEntry {
	var body strings.Builder
	names := make([]string, 0, len(alerts))
	for _, alert := range alerts {
		names = append(names, alert.Name)
	}
	fmt.Fprintf(&body, "- Incident：/incidents/%d（%s）\n", incident.ID, incident.GroupKey)
	fmt.Fprintf(&body, "- 告警：%s\n", strings.Join(names, "、"))
	fmt.Fprintf(&body, "- 开始：%s", incident.StartedAt.UTC().Format(time.RFC3339))
	if incident.ResolvedAt != nil {
		fmt.Fprintf(&body, "，恢复：%s", incident.ResolvedAt.UTC().Format(time.RFC3339))
	}
	fmt.Fprintf(&body, "\n- 复盘：%s（%s）\n\n", verdictLabels[review.Verdict], review.Reviewer)
	rootCause := strings.TrimSpace(review.RootCause)
	if rootCause == "" && run != nil && run.RCAText != nil {
		rootCause = *run.RCAText
	}
	fmt.Fprintf(&body, "## 根因\n%s\n\n", rootCause)
	if fix := strings.TrimSpace(review.ActualFix); fix != "" {
		fmt.Fprintf(&body, "## 实际处置\n%s\n\n", fix)
	}
	if run != nil && run.RCAText != nil && strings.TrimSpace(review.RootCause) != "" {
		fmt.Fprintf(&body, "## 当时的诊断\n%s\n", *run.RCAText)
		var plan struct {
			Action string `json:"action"`
			Target struct{ Kind, Name string }
		}
		if run.PlanJSON != nil && json.Unmarshal(*run.PlanJSON, &plan) == nil && plan.Action != "" {
			fmt.Fprintf(&body, "建议动作：%s %s/%s\n", plan.Action, plan.Target.Kind, plan.Target.Name)
		}
		body.WriteString("\n")
	}
	if approval != nil {
		fmt.Fprintf(&body, "## 执行与验证\n动作 %s，状态 %s", approval.ToolName, approval.Status)
		if approval.Verification != nil {
			fmt.Fprintf(&body, "，恢复验证 %s", approval.Verification.Status)
		}
		body.WriteString("\n")
	}
	text := tools.Sanitize(strings.TrimSpace(body.String()))
	if runes := []rune(text); len(text) > maxEntryBody {
		text = string(runes[:maxEntryBody/4]) + "…[truncated]"
	}
	title := fmt.Sprintf("Incident #%d 复盘：%s", incident.ID, incident.Title)
	return store.KnowledgeEntry{Title: tools.Truncate(title, 200), Body: text, SHA256: digest(title, text)}
}

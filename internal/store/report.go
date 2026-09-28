package store

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"
	"strings"
	"time"

	"oncall-agent/internal/eventlog"
)

// maxReportIncidents bounds one report; a longer history is read in windows.
const maxReportIncidents = 5000

// Report is the remediation effect over incidents started in a window. Every
// figure is computed from persisted facts, reports its raw counts, and is nil
// when its denominator is zero: a small sample is never shown as "zero risk".
type Report struct {
	Since     time.Time `json:"since"`
	Until     time.Time `json:"until"`
	Incidents int       `json:"incidents"`
	// Executions are real primary actions: the write was attempted
	// (executed or failed), not refused before writing.
	Executions struct {
		Total    int      `json:"total"`
		Reviewed int      `json:"reviewed"`
		Wrong    int      `json:"wrong"`
		Coverage *float64 `json:"coverage"`
		// ErrorRate is confirmed wrong / reviewed executions.
		ErrorRate *float64 `json:"error_rate"`
	} `json:"executions"`
	// Unattended counts incidents recovered by an automatic action with no
	// recorded human intervention: verification stable (or passed without a
	// watch window) and the original alert resolved. Confirmed ones also have
	// an action review marked correct. "No recorded intervention" is not
	// "confirmed no intervention": people can act outside the platform.
	Unattended struct {
		Recovered     int      `json:"recovered"`
		Confirmed     int      `json:"confirmed"`
		Rate          *float64 `json:"rate"`
		ConfirmedRate *float64 `json:"confirmed_rate"`
		// InScope are incidents opened by an alert an auto rule covers; the
		// in-scope rate is shown beside the overall rate, never instead of it.
		InScope     int      `json:"in_scope"`
		InScopeRate *float64 `json:"in_scope_rate"`
	} `json:"unattended"`
	// RecoveryMinutes is from incident start to the passed verification of
	// unattended recoveries.
	RecoveryMinutes struct {
		Median *float64 `json:"median"`
		Max    *float64 `json:"max"`
	} `json:"recovery_minutes"`
	Recurrence struct {
		Watched  int      `json:"watched"`
		Recurred int      `json:"recurred"`
		Rate     *float64 `json:"rate"`
	} `json:"recurrence"`
	Manual struct {
		Incidents int      `json:"incidents"`
		Share     *float64 `json:"share"`
		Minutes   int      `json:"minutes"`
	} `json:"manual"`
	// Cost is model usage. Business probe cost is bounded by configuration
	// (exporter interval, verification window), not attributed per incident.
	Cost struct {
		TokensIn          int      `json:"tokens_in"`
		TokensOut         int      `json:"tokens_out"`
		TokensPerIncident *float64 `json:"tokens_per_incident"`
	} `json:"cost"`
	// RootCause is estimated only from human-reviewed diagnoses; unknown is
	// counted separately and never as correct.
	RootCause struct {
		Reviewed int      `json:"reviewed"`
		Correct  int      `json:"correct"`
		Partial  int      `json:"partial"`
		Wrong    int      `json:"wrong"`
		Unknown  int      `json:"unknown"`
		Accuracy *float64 `json:"accuracy"`
	} `json:"root_cause"`
	// ReviewQueue lists incidents awaiting an action review: every failed,
	// compensated, unknown or recurred action, plus a stable one-in-five sample
	// of automatic successes.
	ReviewQueue []ReviewItem `json:"review_queue"`
}

type ReviewItem struct {
	IncidentID uint64 `json:"incident_id"`
	ApprovalID uint64 `json:"approval_id"`
	Reason     string `json:"reason"`
}

// RemediationReport loads the window's facts and computes the report.
// autoAlerts are the alerts covered by auto rules of the current release.
func (db *DB) RemediationReport(ctx context.Context, since, until time.Time, autoAlerts []string) (Report, error) {
	if !since.Before(until) {
		return Report{}, errors.New("store: report window must end after it starts")
	}
	var f reportFacts
	q := db.WithContext(ctx)
	if err := q.Where("started_at >= ? AND started_at < ?", since, until).Order("id ASC").Limit(maxReportIncidents + 1).Find(&f.incidents).Error; err != nil {
		return Report{}, fmt.Errorf("store: report incidents: %w", err)
	}
	if len(f.incidents) > maxReportIncidents {
		return Report{}, fmt.Errorf("store: report window holds more than %d incidents; choose a shorter window", maxReportIncidents)
	}
	ids := make([]uint64, 0, len(f.incidents))
	for _, incident := range f.incidents {
		ids = append(ids, incident.ID)
	}
	if len(ids) > 0 {
		if err := q.Preload("Verification").Where("incident_id IN ?", ids).Order("id ASC").Find(&f.approvals).Error; err != nil {
			return Report{}, fmt.Errorf("store: report approvals: %w", err)
		}
		if err := q.Where("incident_id IN ?", ids).Order("id ASC").Find(&f.reviews).Error; err != nil {
			return Report{}, fmt.Errorf("store: report reviews: %w", err)
		}
		if err := q.Model(&AgentRun{}).Select("incident_id, SUM(tokens_in) AS tokens_in, SUM(tokens_out) AS tokens_out").
			Where("incident_id IN ?", ids).Group("incident_id").Scan(&f.tokens).Error; err != nil {
			return Report{}, fmt.Errorf("store: report tokens: %w", err)
		}
		if err := q.Table("incident_alert").Select("incident_alert.incident_id, alert.name").
			Joins("JOIN last_alert ON last_alert.fingerprint = incident_alert.fingerprint").
			Joins("JOIN alert ON alert.id = last_alert.alert_id").
			Where("incident_alert.incident_id IN ?", ids).Scan(&f.alerts).Error; err != nil {
			return Report{}, fmt.Errorf("store: report alerts: %w", err)
		}
		if err := q.Model(&IncidentEvent{}).Select("approval_id, MIN(created_at) AS created_at").
			Where("incident_id IN ? AND event_type = ?", ids, string(eventlog.EventVerifyPassed)).Group("approval_id").Scan(&f.passed).Error; err != nil {
			return Report{}, fmt.Errorf("store: report recoveries: %w", err)
		}
	}
	return buildReport(f, since, until, autoAlerts), nil
}

type reportFacts struct {
	incidents []Incident
	approvals []Approval
	reviews   []Review
	tokens    []struct {
		IncidentID uint64
		TokensIn   int
		TokensOut  int
	}
	alerts []struct {
		IncidentID uint64
		Name       string
	}
	passed []struct {
		ApprovalID uint64
		CreatedAt  time.Time
	}
}

func ratio(n, d int) *float64 {
	if d == 0 {
		return nil
	}
	value := float64(n) / float64(d)
	return &value
}

// personDecided is a decision made by a person rather than a rule.
func personDecided(a Approval) bool {
	return a.DecisionSource != nil && *a.DecisionSource != "rule"
}

func automatic(a Approval) bool {
	return a.DecidedBy != nil && strings.HasPrefix(*a.DecidedBy, "system:rule:")
}

// recovered is a primary action whose recovery was confirmed and, when a
// watch window was frozen, did not recur.
func recovered(a Approval) bool {
	task := a.Verification
	return task != nil && (task.Status == "stable" || (task.Status == "passed" && task.Phase == "verify"))
}

func sampled(incidentID uint64) bool {
	h := fnv.New32a()
	fmt.Fprint(h, incidentID)
	return h.Sum32()%5 == 0
}

func buildReport(f reportFacts, since, until time.Time, autoAlerts []string) Report {
	var r Report
	r.Since, r.Until, r.Incidents = since.UTC(), until.UTC(), len(f.incidents)
	r.ReviewQueue = []ReviewItem{}
	approvals := map[uint64][]Approval{}
	for _, a := range f.approvals {
		approvals[a.IncidentID] = append(approvals[a.IncidentID], a)
	}
	passedAt := map[uint64]time.Time{}
	for _, p := range f.passed {
		passedAt[p.ApprovalID] = p.CreatedAt
	}
	names := map[uint64][]string{}
	for _, alert := range f.alerts {
		names[alert.IncidentID] = append(names[alert.IncidentID], alert.Name)
	}
	// The latest review per subject and target counts.
	actionReview, diagnosisReview, manualMinutes := map[uint64]Review{}, map[uint64]Review{}, map[uint64]int{}
	for _, review := range f.reviews {
		manualMinutes[review.IncidentID] += review.ManualMinutes
		switch {
		case review.Subject == "action" && review.ApprovalID != nil:
			actionReview[*review.ApprovalID] = review
		case review.Subject == "diagnosis" && review.RunID != nil:
			diagnosisReview[*review.RunID] = review
		}
	}
	for _, review := range diagnosisReview {
		switch review.Verdict {
		case "correct":
			r.RootCause.Correct++
		case "partial":
			r.RootCause.Partial++
		case "wrong":
			r.RootCause.Wrong++
		default:
			r.RootCause.Unknown++
		}
	}
	r.RootCause.Reviewed = r.RootCause.Correct + r.RootCause.Partial + r.RootCause.Wrong
	r.RootCause.Accuracy = ratio(r.RootCause.Correct, r.RootCause.Reviewed)

	var recoveryMinutes []float64
	for _, incident := range f.incidents {
		intervened := manualMinutes[incident.ID] > 0
		unattended, confirmed := false, false
		for _, a := range approvals[incident.ID] {
			intervened = intervened || personDecided(a)
			task := a.Verification
			if task != nil && (task.Status == "stable" || task.Status == "recurred") {
				r.Recurrence.Watched++
				if task.Status == "recurred" {
					r.Recurrence.Recurred++
				}
			}
			if a.ParentApprovalID != nil {
				continue
			}
			review, reviewed := actionReview[a.ID]
			if a.Status == "executed" || a.Status == "failed" {
				r.Executions.Total++
				if reviewed && review.Verdict != "unknown" {
					r.Executions.Reviewed++
					if review.Verdict == "wrong" {
						r.Executions.Wrong++
					}
				}
			}
			if automatic(a) && recovered(a) && incident.Status == "resolved" {
				unattended = true
				confirmed = confirmed || (reviewed && review.Verdict == "correct")
				if at, ok := passedAt[a.ID]; ok {
					recoveryMinutes = append(recoveryMinutes, at.Sub(incident.StartedAt).Minutes())
				}
			}
			if reason := reviewReason(a, approvals[incident.ID]); reason != "" && !reviewed {
				r.ReviewQueue = append(r.ReviewQueue, ReviewItem{IncidentID: incident.ID, ApprovalID: a.ID, Reason: reason})
			}
		}
		if intervened {
			r.Manual.Incidents++
			unattended = false
		}
		if unattended {
			r.Unattended.Recovered++
			if confirmed {
				r.Unattended.Confirmed++
			}
		}
		for _, name := range names[incident.ID] {
			if slices.Contains(autoAlerts, name) {
				r.Unattended.InScope++
				break
			}
		}
		r.Manual.Minutes += manualMinutes[incident.ID]
	}
	r.Executions.Coverage = ratio(r.Executions.Reviewed, r.Executions.Total)
	r.Executions.ErrorRate = ratio(r.Executions.Wrong, r.Executions.Reviewed)
	r.Unattended.Rate = ratio(r.Unattended.Recovered, r.Incidents)
	r.Unattended.ConfirmedRate = ratio(r.Unattended.Confirmed, r.Incidents)
	r.Unattended.InScopeRate = ratio(r.Unattended.Recovered, r.Unattended.InScope)
	r.Recurrence.Rate = ratio(r.Recurrence.Recurred, r.Recurrence.Watched)
	r.Manual.Share = ratio(r.Manual.Incidents, r.Incidents)
	if len(recoveryMinutes) > 0 {
		slices.Sort(recoveryMinutes)
		median := recoveryMinutes[len(recoveryMinutes)/2]
		if len(recoveryMinutes)%2 == 0 {
			median = (recoveryMinutes[len(recoveryMinutes)/2-1] + median) / 2
		}
		r.RecoveryMinutes.Median, r.RecoveryMinutes.Max = &median, &recoveryMinutes[len(recoveryMinutes)-1]
	}
	for _, t := range f.tokens {
		r.Cost.TokensIn += t.TokensIn
		r.Cost.TokensOut += t.TokensOut
	}
	if r.Incidents > 0 {
		perIncident := float64(r.Cost.TokensIn+r.Cost.TokensOut) / float64(r.Incidents)
		r.Cost.TokensPerIncident = &perIncident
	}
	return r
}

// reviewReason says why an executed primary action needs a person to review
// it; empty means it is not in the review queue.
func reviewReason(a Approval, family []Approval) string {
	for _, other := range family {
		if other.ParentApprovalID != nil && *other.ParentApprovalID == a.ID {
			return "compensated"
		}
	}
	task := a.Verification
	switch {
	case a.Status == "failed":
		return "failed"
	case a.Status != "executed":
		return ""
	case task == nil:
		return "unknown"
	case task.Status == "recurred":
		return "recurred"
	case task.Status == "failed":
		return "verification_failed"
	case task.Status == "inconclusive":
		return "unknown"
	case recovered(a) && automatic(a) && sampled(a.IncidentID):
		return "sampled"
	}
	return ""
}

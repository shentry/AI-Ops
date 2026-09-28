package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gogf/gf/v2/net/ghttp"

	"oncall-agent/internal/config"
	"oncall-agent/internal/incident"
	"oncall-agent/internal/store"
)

// remediationAuthority is the current rules release the process runs with.
type remediationAuthority interface {
	Service() string
	Env() string
	Release() string
	Rules() []config.RuleConfig
	Policy(now time.Time) store.RemediationPolicy
}

type remediationStore interface {
	RemediationState(ctx context.Context, q store.RemediationQuery) (store.RemediationState, error)
	AppendControlEvent(ctx context.Context, event store.ControlEvent) (store.ControlEvent, error)
	ListControlEvents(ctx context.Context, limit int) ([]store.ControlEvent, error)
	RemediationReport(ctx context.Context, since, until time.Time, autoAlerts []string) (store.Report, error)
	AddReview(ctx context.Context, review store.Review) (store.Review, error)
	ListReviews(ctx context.Context, incidentID uint64) ([]store.Review, error)
	RecordChange(ctx context.Context, change store.ChangeEvent) (store.ChangeEvent, bool, error)
	ListChanges(ctx context.Context, service string, since time.Time, limit int) ([]store.ChangeEvent, error)
	MarkReleaseVerified(ctx context.Context, id uint64, at time.Time) (store.ChangeEvent, error)
}

// RemediationAPI is the operator surface of automatic handling: the rules in
// effect and their state, emergency stop, rule reset, reviews, deployment
// change records and the effect report. Rules themselves change only through
// a versioned configuration release, never through this API.
type RemediationAPI struct {
	db        remediationStore
	authority remediationAuthority
	auth      *Auth
	now       func() time.Time
}

func NewRemediationAPI(db remediationStore, authority remediationAuthority, auth *Auth) *RemediationAPI {
	return &RemediationAPI{db: db, authority: authority, auth: auth, now: func() time.Time { return time.Now().UTC() }}
}

func (h *RemediationAPI) Handle(r *ghttp.Request) {
	h.ServeHTTP(r.Response.BufferWriter, r.Request)
}

func (h *RemediationAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.db == nil || h.authority == nil {
		writeError(w, http.StatusServiceUnavailable, "remediation unavailable")
		return
	}
	path := strings.Trim(r.URL.Path, "/")
	parts := strings.Split(path, "/")
	switch {
	case path == "api/v1/remediation" && r.Method == http.MethodGet:
		h.state(w, r)
	case path == "api/v1/remediation/report" && r.Method == http.MethodGet:
		h.report(w, r)
	case (path == "api/v1/remediation/stop" || path == "api/v1/remediation/resume") && r.Method == http.MethodPost:
		kind := store.ControlEmergencyStop
		if strings.HasSuffix(path, "resume") {
			kind = store.ControlEmergencyResume
		}
		h.control(w, r, kind, nil)
	case len(parts) == 6 && parts[3] == "rules" && parts[5] == "reset" && r.Method == http.MethodPost:
		h.control(w, r, store.ControlRuleReset, &parts[4])
	case len(parts) == 5 && parts[2] == "incidents" && parts[4] == "reviews":
		h.reviews(w, r, parts[3])
	case path == "api/v1/changes":
		h.changes(w, r)
	case len(parts) == 5 && parts[2] == "changes" && parts[4] == "verify" && r.Method == http.MethodPost:
		h.verifyRelease(w, r, parts[3])
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

type RemediationRuleDTO struct {
	ID            string   `json:"id"`
	Action        string   `json:"action"`
	Mode          string   `json:"mode"`
	Alerts        []string `json:"alerts"`
	MaxExecutions int      `json:"max_executions"`
	WindowMinutes int      `json:"window_minutes"`
	Executions    int      `json:"executions"`
	// Blocked explains why the rule's automatic actions are stopped until a reset.
	Blocked string `json:"blocked,omitempty"`
}

type ControlEventDTO struct {
	ID        uint64    `json:"id"`
	Kind      string    `json:"kind"`
	RuleID    string    `json:"rule_id,omitempty"`
	Actor     string    `json:"actor"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}

type RemediationDTO struct {
	Service       string               `json:"service"`
	Env           string               `json:"env"`
	RulesVersion  string               `json:"rules_version"`
	EmergencyStop bool                 `json:"emergency_stop"`
	StopReason    string               `json:"stop_reason,omitempty"`
	Maintenance   string               `json:"maintenance,omitempty"`
	BusyWith      uint64               `json:"busy_with,omitempty"`
	Rules         []RemediationRuleDTO `json:"rules"`
	Events        []ControlEventDTO    `json:"events"`
}

func (h *RemediationAPI) state(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.auth.Require(w, r, RoleViewer, true); !ok {
		return
	}
	now := h.now()
	service := h.authority.Service()
	overall, err := h.db.RemediationState(r.Context(), store.RemediationQuery{Service: service})
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "remediation state unavailable")
		return
	}
	body := RemediationDTO{Service: service, Env: h.authority.Env(), RulesVersion: h.authority.Release(), EmergencyStop: overall.Stopped,
		StopReason: safeText(overall.StopReason, 512), Maintenance: safeText(h.authority.Policy(now).Maintenance, 512), BusyWith: overall.BusyWith,
		Rules: []RemediationRuleDTO{}, Events: []ControlEventDTO{}}
	for _, rule := range h.authority.Rules() {
		state, err := h.db.RemediationState(r.Context(), store.RemediationQuery{RuleID: rule.ID, Since: now.Add(-time.Duration(rule.WindowMinutes) * time.Minute)})
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "remediation state unavailable")
			return
		}
		body.Rules = append(body.Rules, RemediationRuleDTO{ID: rule.ID, Action: rule.Action, Mode: rule.Mode, Alerts: rule.Alerts,
			MaxExecutions: rule.MaxExecutions, WindowMinutes: rule.WindowMinutes, Executions: state.Executions, Blocked: safeText(state.Blocked, 512)})
	}
	events, err := h.db.ListControlEvents(r.Context(), 50)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "control events unavailable")
		return
	}
	for _, event := range events {
		ruleID := ""
		if event.RuleID != nil {
			ruleID = *event.RuleID
		}
		body.Events = append(body.Events, ControlEventDTO{ID: event.ID, Kind: event.Kind, RuleID: safeText(ruleID, 64), Actor: safeText(event.Actor, 64),
			Reason: safeText(event.Reason, 512), CreatedAt: event.CreatedAt.UTC()})
	}
	writeJSON(w, http.StatusOK, body)
}

// control records an emergency stop/resume or a rule reset. Each is an admin
// decision with a reason; a stop prevents new writes at the next claim and
// cannot recall an external operation already in progress.
func (h *RemediationAPI) control(w http.ResponseWriter, r *http.Request, kind string, ruleID *string) {
	actor, ok := h.auth.Require(w, r, RoleAdmin, false)
	if !ok {
		return
	}
	var input struct {
		Reason string `json:"reason"`
	}
	if err := decodeBoundedJSON(r, &input, 16<<10); err != nil || strings.TrimSpace(input.Reason) == "" {
		writeError(w, http.StatusBadRequest, "reason is required")
		return
	}
	if ruleID != nil {
		known := false
		for _, rule := range h.authority.Rules() {
			known = known || rule.ID == *ruleID
		}
		if !known {
			writeError(w, http.StatusNotFound, "rule is not in the current release")
			return
		}
	}
	event, err := h.db.AppendControlEvent(r.Context(), store.ControlEvent{Kind: kind, RuleID: ruleID, Actor: actor.ID, Reason: input.Reason, CreatedAt: h.now()})
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "control event not recorded")
		return
	}
	writeJSON(w, http.StatusOK, ControlEventDTO{ID: event.ID, Kind: event.Kind, RuleID: safeText(derefText(event.RuleID), 64), Actor: event.Actor, Reason: safeText(event.Reason, 512), CreatedAt: event.CreatedAt})
}

func derefText(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func (h *RemediationAPI) report(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.auth.Require(w, r, RoleViewer, true); !ok {
		return
	}
	days := 30
	if raw := r.URL.Query().Get("days"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 90 {
			writeError(w, http.StatusBadRequest, "days must be 1-90")
			return
		}
		days = value
	}
	var autoAlerts []string
	for _, rule := range h.authority.Rules() {
		if rule.Mode == incident.ModeAuto {
			autoAlerts = append(autoAlerts, rule.Alerts...)
		}
	}
	until := h.now()
	report, err := h.db.RemediationReport(r.Context(), until.AddDate(0, 0, -days), until, autoAlerts)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, safeText(err.Error(), 256))
		return
	}
	writeJSON(w, http.StatusOK, report)
}

type ReviewDTO struct {
	ID            uint64    `json:"id"`
	IncidentID    uint64    `json:"incident_id"`
	RunID         *uint64   `json:"run_id,omitempty"`
	ApprovalID    *uint64   `json:"approval_id,omitempty"`
	Subject       string    `json:"subject"`
	Verdict       string    `json:"verdict"`
	RootCause     string    `json:"root_cause"`
	ActualFix     string    `json:"actual_fix"`
	ManualMinutes int       `json:"manual_minutes"`
	Reviewer      string    `json:"reviewer"`
	CreatedAt     time.Time `json:"created_at"`
}

func reviewDTO(row store.Review) ReviewDTO {
	return ReviewDTO{ID: row.ID, IncidentID: row.IncidentID, RunID: row.RunID, ApprovalID: row.ApprovalID, Subject: row.Subject, Verdict: row.Verdict,
		RootCause: safeText(row.RootCause, 1024), ActualFix: safeText(row.ActualFix, 1024), ManualMinutes: row.ManualMinutes,
		Reviewer: safeText(row.Reviewer, 64), CreatedAt: row.CreatedAt.UTC()}
}

// reviews records a person's verdict on a diagnosis or an action. An action
// reviewed wrong blocks its rule's automatic actions until an admin reset.
func (h *RemediationAPI) reviews(w http.ResponseWriter, r *http.Request, rawID string) {
	incidentID, err := parseIncidentID(rawID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid incident id")
		return
	}
	switch r.Method {
	case http.MethodGet:
		if _, ok := h.auth.Require(w, r, RoleViewer, true); !ok {
			return
		}
		rows, err := h.db.ListReviews(r.Context(), incidentID)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "reviews unavailable")
			return
		}
		values := make([]ReviewDTO, 0, len(rows))
		for _, row := range rows {
			values = append(values, reviewDTO(row))
		}
		writeJSON(w, http.StatusOK, map[string]any{"reviews": values})
	case http.MethodPost:
		actor, ok := h.auth.Require(w, r, RoleOperator, false)
		if !ok {
			return
		}
		var input struct {
			Subject       string  `json:"subject"`
			Verdict       string  `json:"verdict"`
			RunID         *uint64 `json:"run_id"`
			ApprovalID    *uint64 `json:"approval_id"`
			RootCause     string  `json:"root_cause"`
			ActualFix     string  `json:"actual_fix"`
			ManualMinutes int     `json:"manual_minutes"`
		}
		if err := decodeBoundedJSON(r, &input, 16<<10); err != nil {
			writeError(w, http.StatusBadRequest, "invalid review")
			return
		}
		row, err := h.db.AddReview(r.Context(), store.Review{IncidentID: incidentID, RunID: input.RunID, ApprovalID: input.ApprovalID, Subject: input.Subject,
			Verdict: input.Verdict, RootCause: input.RootCause, ActualFix: input.ActualFix, ManualMinutes: input.ManualMinutes, Reviewer: actor.ID, CreatedAt: h.now()})
		if err != nil {
			writeError(w, http.StatusBadRequest, safeText(err.Error(), 256))
			return
		}
		writeJSON(w, http.StatusCreated, reviewDTO(row))
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

type ChangeDTO struct {
	ID            uint64     `json:"id"`
	Env           string     `json:"env"`
	Service       string     `json:"service"`
	ChangeType    string     `json:"change_type"`
	ReleaseID     string     `json:"release_id,omitempty"`
	ImageRef      string     `json:"image_ref,omitempty"`
	BeforeRef     string     `json:"before_ref,omitempty"`
	ConfigVersion string     `json:"config_version,omitempty"`
	DBMigration   string     `json:"db_migration"`
	VerifiedAt    *time.Time `json:"verified_at,omitempty"`
	OccurredAt    time.Time  `json:"occurred_at"`
	Source        string     `json:"source"`
	Actor         string     `json:"actor"`
	ApprovalID    *uint64    `json:"approval_id,omitempty"`
}

func changeDTO(row store.ChangeEvent) ChangeDTO {
	return ChangeDTO{ID: row.ID, Env: row.Env, Service: row.Service, ChangeType: row.ChangeType, ReleaseID: safeText(derefText(row.ReleaseID), 128),
		ImageRef: safeText(derefText(row.ImageRef), 512), BeforeRef: safeText(derefText(row.BeforeRef), 512), ConfigVersion: safeText(derefText(row.ConfigVersion), 128),
		DBMigration: row.DBMigration, VerifiedAt: utcTimePtr(row.VerifiedAt), OccurredAt: row.OccurredAt.UTC(), Source: row.Source, Actor: safeText(row.Actor, 64), ApprovalID: row.ApprovalID}
}

// changes lists and records deployment changes of the configured service.
// CI may report a release with the automation token; only a person may mark
// a release verified, because verified releases are rollback targets.
func (h *RemediationAPI) changes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if _, ok := h.auth.Require(w, r, RoleViewer, true); !ok {
			return
		}
		rows, err := h.db.ListChanges(r.Context(), h.authority.Service(), time.Time{}, 100)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "changes unavailable")
			return
		}
		values := make([]ChangeDTO, 0, len(rows))
		for _, row := range rows {
			values = append(values, changeDTO(row))
		}
		writeJSON(w, http.StatusOK, map[string]any{"changes": values})
	case http.MethodPost:
		actor, ok := h.auth.Require(w, r, RoleOperator, true)
		if !ok {
			return
		}
		var input struct {
			ChangeType     string    `json:"change_type"`
			ReleaseID      *string   `json:"release_id"`
			ImageRef       *string   `json:"image_ref"`
			BeforeRef      *string   `json:"before_ref"`
			ConfigVersion  *string   `json:"config_version"`
			DBMigration    string    `json:"db_migration"`
			OccurredAt     time.Time `json:"occurred_at"`
			IdempotencyKey string    `json:"idempotency_key"`
		}
		if err := decodeBoundedJSON(r, &input, 16<<10); err != nil {
			writeError(w, http.StatusBadRequest, "invalid change")
			return
		}
		if input.ChangeType == "rollback" {
			writeError(w, http.StatusBadRequest, "rollbacks are recorded by the action that made them")
			return
		}
		source := "operator"
		if actor.Machine {
			source = "ci"
		}
		row, created, err := h.db.RecordChange(r.Context(), store.ChangeEvent{Env: h.authority.Env(), Service: h.authority.Service(), ChangeType: input.ChangeType,
			ReleaseID: input.ReleaseID, ImageRef: input.ImageRef, BeforeRef: input.BeforeRef, ConfigVersion: input.ConfigVersion, DBMigration: input.DBMigration,
			OccurredAt: input.OccurredAt, Source: source, Actor: actor.ID, IdempotencyKey: input.IdempotencyKey, CreatedAt: h.now()})
		if err != nil {
			writeError(w, http.StatusBadRequest, safeText(err.Error(), 256))
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeJSON(w, status, changeDTO(row))
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (h *RemediationAPI) verifyRelease(w http.ResponseWriter, r *http.Request, rawID string) {
	if _, ok := h.auth.Require(w, r, RoleOperator, false); !ok {
		return
	}
	id, err := parseIncidentID(rawID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid change id")
		return
	}
	row, err := h.db.MarkReleaseVerified(r.Context(), id, h.now())
	switch {
	case errors.Is(err, store.ErrChangeNotFound):
		writeError(w, http.StatusNotFound, "change not found")
	case err != nil:
		writeError(w, http.StatusConflict, safeText(err.Error(), 256))
	default:
		writeJSON(w, http.StatusOK, changeDTO(row))
	}
}

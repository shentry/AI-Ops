package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"gorm.io/datatypes"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"oncall-agent/internal/eventlog"
	"strings"
	"time"
)

// ErrInvalidRawEvent 是"报文不是合法 JSON"的哨兵错误。HTTP 层用 errors.Is
// 区分输入问题和存储故障：前者回 400 怪调用方，后者回 503 怪自己。
var ErrInvalidRawEvent = errors.New("store: raw event payload must be valid JSON")

// ErrIncidentNotFound 是"incident 不存在"的哨兵错误。查询 API 用 errors.Is
// 把它映射成 404，和真正的存储故障（503）区分开。
var ErrIncidentNotFound = errors.New("store: incident not found")

// ErrAgentRunNotFound 是"诊断 run 不存在"的哨兵错误。Run/Step API 用它区分
// 不存在或不属于目标 Incident 的 run，避免把任意 run ID 当成授权凭据。
var ErrAgentRunNotFound = errors.New("store: agent run not found")

// ErrConversationMessageNotFound 是对话消息不存在的哨兵错误。
var ErrConversationMessageNotFound = errors.New("store: conversation message not found")

// ErrWebOAuthStateNotFound means the one-time OAuth state does not exist.
var ErrWebOAuthStateNotFound = errors.New("store: web oauth state not found")

// ErrWebOAuthStateExpired means the one-time OAuth state existed but was expired.
var ErrWebOAuthStateExpired = errors.New("store: web oauth state expired")

// ErrWebSessionNotFound means no active browser session matched the supplied token.
var ErrWebSessionNotFound = errors.New("store: web session not found")

// ErrLLMModelSelectionNotFound means the singleton model-selection row has
// not been initialized. Startup creates it before any model is built.
var ErrLLMModelSelectionNotFound = errors.New("store: llm model selection not found")

// ErrIMBindingNotFound 是飞书消息绑定未命中的哨兵错误。
var ErrIMBindingNotFound = errors.New("store: im binding not found")

const integrationReceiptLease = 5 * time.Minute

// DedupResult 是新到告警与同指纹 last_alert 比对后的三种结论，
// 下游（worker 的 hook）按它决定建 incident 还是只续心跳。
type DedupResult string

const (
	// DedupNew：指纹第一次出现，alert 和 last_alert 都是新插的。
	DedupNew DedupResult = "new"
	// DedupFull：AlertHash 完全一致 —— 内容一模一样的重复推送
	//（多半来自 Alertmanager 的 repeat_interval 重发）。不插 alert 行。
	DedupFull DedupResult = "full"
	// DedupPartial：指纹相同但内容变了（severity 升级、annotation 更新……）。
	// 追加一条新版本 alert，last_alert 改指到它。
	DedupPartial DedupResult = "partial"
)

// AlertInput 是 ingest.NormalizedAlert 的 store 侧镜像。依赖方向只能是
// ingest → store，store 反过来 import ingest 会成环，所以持久化层
// 自己声明一份入参结构，字段一一对应。
type AlertInput struct {
	Fingerprint  string
	AlertHash    string
	Source       string
	Name         string
	Severity     int
	Status       string
	Labels       map[string]string
	Annotations  map[string]string
	GeneratorURL string
	StartsAt     time.Time
	ReceivedAt   time.Time
}

// IncidentInput 是关联器（D04 Correlator）视角的一条 firing 告警：
// 只带分组要用的字段，不带 annotations 这类大块内容。
type IncidentInput struct {
	GroupKey    string
	Fingerprint string
	Name        string
	Severity    int
	ObservedAt  time.Time
}

// IncidentAssignment 是 AssignIncident 的结论。Promoted 专门标记
// "这次调用恰好把 incident 从 candidate 升成 firing" —— worker 靠它
// 只在升级瞬间做一次促发动作（D05 落诊断队列），后续普通挂载不重复。
// Severity 是促发后 incident 的当前级别（成员最大值），D05 按它分流诊断模式。
type IncidentAssignment struct {
	IncidentID uint64
	Status     string
	Severity   int
	Created    bool
	Promoted   bool
}

// EventProblemWriter 是事务内写入 Incident 事实事件和当前问题读模型的窄接口。
// IncidentTx 嵌入该接口，保证事件/问题和告警、Incident、run 同一事务提交。
type EventProblemWriter interface {
	AppendIncidentEvent(context.Context, IncidentEvent) (IncidentEvent, error)
	OpenIncidentProblem(context.Context, IncidentProblem) (IncidentProblem, error)
	ResolveIncidentProblem(context.Context, uint64, string, *uint64, time.Time) (bool, error)
}

// ProblemMutationKind 是 AppendRunStepRecord 可接受的有限问题操作。
// 禁止通过批次接口传入任意 SQL 操作，问题只能 open 或 resolve。
type ProblemMutationKind string

const (
	ProblemOpen            ProblemMutationKind = "open"
	ProblemResolve         ProblemMutationKind = "resolve"
	ProblemMutationOpen    ProblemMutationKind = ProblemOpen
	ProblemMutationResolve ProblemMutationKind = ProblemResolve
)

// ProblemMutation 描述一次固定语义的问题读模型变更。
// open 使用 Problem；resolve 使用 IncidentID、Code、RunID、ResolvedAt。
type ProblemMutation struct {
	Kind       ProblemMutationKind
	Problem    IncidentProblem
	IncidentID uint64
	Code       string
	RunID      *uint64
	ResolvedAt time.Time
}

// RunStepRecord 是一步审计、事实事件和问题变更的一次性批次。
// AppendRunStepRecord 会在同一短事务中写入全部内容。
type RunStepRecord struct {
	Step     AgentRunStep
	Events   []IncidentEvent
	Problems []ProblemMutation
}

// RunCompletion 原子写入一次 run 的终态和最后一批审计记录。
// Steps、Events、Problems 会与 agent_run 终态在同一短事务提交。
type RunCompletion struct {
	RunID      uint64
	RCA        string
	PlanJSON   []byte
	TokensIn   int
	TokensOut  int
	Status     string
	FinishedAt time.Time
	Steps      []AgentRunStep
	Events     []IncidentEvent
	Problems   []ProblemMutation
}

type IncidentTx interface {
	AssignIncident(context.Context, IncidentInput, time.Duration, int) (IncidentAssignment, error)
	TouchIncident(context.Context, uint64, time.Time, int) error
	// D05：resolved 传播。全部成员 resolved 时把 incident 关单，返回是否本次关闭。
	ResolveIncident(context.Context, uint64, time.Time) (bool, error)
	// D05：促发分流。在促发同一事务里落 agent_run 队列行，保证跨表原子性。
	EnqueueAgentRun(context.Context, AgentRun) error
	EventProblemWriter
}

// AlertApplyResult 是单条告警 apply 之后回给 hook 的结果：
// 输入本身 + 去重结论 + apply 之后（不是之前）的 last_alert 快照，
// hook 拿 Last.IncidentID 就知道这条告警当前挂在哪个 incident 上。
type AlertApplyResult struct {
	Input AlertInput
	Dedup DedupResult
	Last  LastAlert
}

// RawEventApplyHook 在每条告警 apply 完、事务提交前被调用。
// 返回 error 会连累整条 raw_event 回滚，所以这里只该放
// "必须和 alert 落库同生共死"的逻辑 —— D04 的 incident 关联正是。
type RawEventApplyHook func(context.Context, IncidentTx, AlertApplyResult) error

// transactionIncidentTx 把 ApplyRawEvent 里的事务句柄适配成 IncidentTx。
type transactionIncidentTx struct {
	db *gorm.DB
}

// AssignIncident 把一条 firing 告警并进"还活着"的 incident，没有就新建：
// 挂成员 → 只升不降地刷新 severity / last_seen_at → 成员数攒够 minAlerts
// 时把 candidate 升为 firing。
func (t *transactionIncidentTx) AssignIncident(ctx context.Context, input IncidentInput, window time.Duration, minAlerts int) (IncidentAssignment, error) {
	// 身份三要素和窗口参数是 D04 关联的硬前提，缺了宁可报错也不猜 ——
	// 猜错一个 group_key 就会把不相干的告警并进同一个 incident。
	if input.GroupKey == "" || input.Fingerprint == "" || input.Name == "" {
		return IncidentAssignment{}, errors.New("store: incident identity is required")
	}
	if input.ObservedAt.IsZero() {
		return IncidentAssignment{}, errors.New("store: incident observed time is required")
	}
	if window <= 0 {
		return IncidentAssignment{}, errors.New("store: incident window must be positive")
	}
	if minAlerts < 1 {
		return IncidentAssignment{}, errors.New("store: incident minimum alerts must be at least 1")
	}

	observedAt := input.ObservedAt.UTC()
	// cutoff 之后仍有活动的 incident 才算"活着"。过了时间窗的不再吸收
	// 新告警 —— 上次故障和这次复发是两个 incident，不能缝在一起。
	cutoff := observedAt.Add(-window)
	var incident Incident
	// FOR UPDATE 行锁：并发处理同一 group 时，后到的事务会等先到的提交，
	// 不会两边都读到"没有"然后各建一个 incident。
	// BINARY 是大小写敏感比较：表排序规则是 *_ci，不绕过它的话
	// "DB" 和 "db" 会被判成同一个 group_key。
	query := t.db.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("BINARY group_key = BINARY ? AND status IN ? AND last_seen_at >= ?", input.GroupKey, []string{"candidate", "firing"}, cutoff).
		// 窗口内可能同时活着好几个 incident，挑最近活跃的那个；
		// id DESC 兜底，last_seen_at 相同时结果也是确定的。
		Order("last_seen_at DESC, id DESC").
		First(&incident)
	created := false
	if errors.Is(query.Error, gorm.ErrRecordNotFound) {
		// 新建的是 candidate（候选）：单条告警不足以下结论，攒够
		// minAlerts 才升 firing，避免一条孤立告警就制造一次故障。
		incident = Incident{
			GroupKey:    input.GroupKey,
			Status:      "candidate",
			Severity:    uint8(input.Severity),
			AlertsCount: 0,
			Title:       fmt.Sprintf("%s: %s", input.GroupKey, input.Name),
			StartedAt:   observedAt,
			LastSeenAt:  observedAt,
		}
		if err := t.db.WithContext(ctx).Create(&incident).Error; err != nil {
			return IncidentAssignment{}, fmt.Errorf("store: create incident: %w", err)
		}
		created = true
	} else if query.Error != nil {
		return IncidentAssignment{}, fmt.Errorf("store: find open incident: %w", query.Error)
	}
	// "进来时是不是 candidate"要在更新前存下，等会儿升级后就看不出来了。
	wasCandidate := incident.Status == "candidate"

	// 挂成员关系。ON CONFLICT DO NOTHING：同一指纹反复 firing 不能把
	// alerts_count 刷高，只有真正的新成员（RowsAffected>0）才计数。
	member := IncidentAlert{IncidentID: incident.ID, Fingerprint: input.Fingerprint, LinkedAt: observedAt}
	insert := t.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&member)
	if insert.Error != nil {
		return IncidentAssignment{}, fmt.Errorf("store: link incident alert: %w", insert.Error)
	}
	if insert.RowsAffected > 0 {
		incident.AlertsCount++
	}
	// severity 只升不降：incident 保留吸收过的最高级别，
	// 否则后到的一条 info 会把 critical 的故障"降级"。
	if input.Severity > int(incident.Severity) {
		incident.Severity = uint8(input.Severity)
	}
	// 重放积压时告警可能乱序到达，last_seen_at 只前进不后退。
	if observedAt.After(incident.LastSeenAt) {
		incident.LastSeenAt = observedAt
	}

	updates := map[string]any{
		"severity":     incident.Severity,
		"alerts_count": incident.AlertsCount,
		"last_seen_at": incident.LastSeenAt,
	}
	// candidate → firing 只在这里发生：成员数够 minAlerts 的那一刻升级。
	if incident.Status == "candidate" && incident.AlertsCount >= minAlerts {
		incident.Status = "firing"
		updates["status"] = incident.Status
	}
	if err := t.db.WithContext(ctx).Model(&Incident{}).Where("id = ?", incident.ID).Updates(updates).Error; err != nil {
		return IncidentAssignment{}, fmt.Errorf("store: update incident: %w", err)
	}
	// 反向指针：last_alert 记住自己挂在哪个 incident 上，
	// 之后 DedupFull 的心跳（TouchIncident）就顺着它找到续命对象。
	if err := t.db.WithContext(ctx).Model(&LastAlert{}).Where("fingerprint = ?", input.Fingerprint).Update("incident_id", incident.ID).Error; err != nil {
		return IncidentAssignment{}, fmt.Errorf("store: link last alert to incident: %w", err)
	}
	return IncidentAssignment{
		IncidentID: incident.ID,
		Status:     incident.Status,
		Severity:   int(incident.Severity),
		Created:    created,
		Promoted:   wasCandidate && incident.Status == "firing",
	}, nil
}

// TouchIncident 是 DedupFull 的心跳：内容没变的重复推送只需把 incident 的
// last_seen_at 续上（时间窗在告警持续 firing 期间不该过期），severity 顺手只升不降。
// 整体 best-effort：入参不完整、incident 不存在或已关单，都静默返回 nil ——
// 心跳没续上不该让整条 raw_event 事务回滚重试。
func (t *transactionIncidentTx) TouchIncident(ctx context.Context, id uint64, observedAt time.Time, severity int) error {
	if id == 0 || observedAt.IsZero() {
		return nil
	}
	var incident Incident
	query := t.db.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).First(&incident, id)
	if errors.Is(query.Error, gorm.ErrRecordNotFound) {
		return nil
	}
	if query.Error != nil {
		return fmt.Errorf("store: find incident for heartbeat: %w", query.Error)
	}
	// 只有 open 状态（candidate/firing）才值得续命，其余状态不动。
	if incident.Status != "candidate" && incident.Status != "firing" {
		return nil
	}
	observedAt = observedAt.UTC()
	if observedAt.After(incident.LastSeenAt) {
		incident.LastSeenAt = observedAt
	}
	if severity > int(incident.Severity) {
		incident.Severity = uint8(severity)
	}
	if err := t.db.WithContext(ctx).Model(&Incident{}).Where("id = ?", id).Updates(map[string]any{
		"last_seen_at": incident.LastSeenAt,
		"severity":     incident.Severity,
	}).Error; err != nil {
		return fmt.Errorf("store: refresh incident heartbeat: %w", err)
	}
	return nil
}

// ResolveIncident 是 D05 的 resolved 传播：一条成员告警 resolved 后调用，
// 只有当 incident 的全部成员都 resolved 时才把 incident 关单（resolve_on=ALL）。
// 幂等：incident 不存在、已关单或仍有成员 firing 时静默返回 false ——
// 一条无关的 resolved 不该让整条 raw_event 事务回滚重试。
func (t *transactionIncidentTx) ResolveIncident(ctx context.Context, id uint64, observedAt time.Time) (bool, error) {
	if id == 0 || observedAt.IsZero() {
		return false, nil
	}
	var incident Incident
	query := t.db.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).First(&incident, id)
	if errors.Is(query.Error, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if query.Error != nil {
		return false, fmt.Errorf("store: find incident for resolve: %w", query.Error)
	}
	// 已关单（resolved）或被人工接管（acknowledged）的不重复处理：
	// 重放 resolved 事件不能把 resolved_at 改来改去。
	if incident.Status != "candidate" && incident.Status != "firing" {
		return false, nil
	}
	// 成员状态以 last_alert 快照为准：它是每个 fingerprint 的当前态。
	// 还有任何一个成员没 resolved，incident 就保持开放。
	var firingMembers int64
	count := t.db.WithContext(ctx).
		Table("incident_alert").
		Joins("JOIN last_alert ON last_alert.fingerprint = incident_alert.fingerprint").
		Where("incident_alert.incident_id = ? AND last_alert.status != ?", id, "resolved").
		Count(&firingMembers)
	if count.Error != nil {
		return false, fmt.Errorf("store: count unresolved incident members: %w", count.Error)
	}
	if firingMembers > 0 {
		return false, nil
	}
	resolvedAt := observedAt.UTC()
	if err := t.db.WithContext(ctx).Model(&Incident{}).Where("id = ? AND status IN ?", id, []string{"candidate", "firing"}).Updates(map[string]any{
		"status":      "resolved",
		"resolved_at": resolvedAt,
	}).Error; err != nil {
		return false, fmt.Errorf("store: resolve incident: %w", err)
	}
	return true, nil
}

// EnqueueAgentRun 在促发事务里落一行 agent_run —— 这张表就是 D05 的诊断队列。
// 身份字段缺失直接报错，让事务回滚，不落一条永远没人消费的脏队列行。
func (t *transactionIncidentTx) EnqueueAgentRun(ctx context.Context, run AgentRun) error {
	return t.enqueueAgentRunWithID(ctx, &run)
}

// EnqueueAgentRunWithID is an optional concrete transaction method for callers
// that need the auto-incremented run ID to append same-transaction events.
// IncidentTx intentionally keeps the legacy value-based method unchanged.
func (t *transactionIncidentTx) EnqueueAgentRunWithID(ctx context.Context, run *AgentRun) error {
	return t.enqueueAgentRunWithID(ctx, run)
}

func (t *transactionIncidentTx) enqueueAgentRunWithID(ctx context.Context, run *AgentRun) error {
	if run == nil {
		return errors.New("store: agent run is required")
	}
	if run.IncidentID == 0 {
		return errors.New("store: agent run incident is required")
	}
	if run.Mode == "" || run.Status == "" {
		return errors.New("store: agent run mode and status are required")
	}
	if run.StartedAt.IsZero() {
		return errors.New("store: agent run start time is required")
	}
	if err := t.db.WithContext(ctx).Create(run).Error; err != nil {
		return fmt.Errorf("store: enqueue agent run: %w", err)
	}
	return nil
}

// appendIncidentEvent 是 DB 与 transactionIncidentTx 共用的事件写入实现。
// 调用方负责在进入这里前完成 Sanitization；store 只拒绝明显无效的 JSON。
func appendIncidentEvent(ctx context.Context, q *gorm.DB, event IncidentEvent) (IncidentEvent, error) {
	if event.IncidentID == 0 {
		return IncidentEvent{}, errors.New("store: incident event incident is required")
	}
	if strings.TrimSpace(event.EventType) == "" || strings.TrimSpace(event.Phase) == "" || strings.TrimSpace(event.Status) == "" {
		return IncidentEvent{}, errors.New("store: incident event type, phase and status are required")
	}
	event.Summary = truncateStoreText(event.Summary, 512)
	if strings.TrimSpace(event.Summary) == "" {
		return IncidentEvent{}, errors.New("store: incident event summary is required")
	}
	if event.CreatedAt.IsZero() {
		return IncidentEvent{}, errors.New("store: incident event created_at is required")
	}
	if event.PayloadJSON != nil && len(*event.PayloadJSON) > 0 && !json.Valid(*event.PayloadJSON) {
		return IncidentEvent{}, errors.New("store: incident event payload must be valid JSON")
	}
	event.CreatedAt = event.CreatedAt.UTC()
	if err := q.WithContext(ctx).Create(&event).Error; err != nil {
		return IncidentEvent{}, fmt.Errorf("store: append incident event: %w", err)
	}
	return event, nil
}

func truncateStoreText(value string, max int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + "…"
}

func openIncidentProblem(ctx context.Context, q *gorm.DB, problem IncidentProblem) (IncidentProblem, error) {
	if problem.IncidentID == 0 || strings.TrimSpace(problem.Code) == "" {
		return IncidentProblem{}, errors.New("store: incident problem incident and code are required")
	}
	problem.Summary = truncateStoreText(problem.Summary, 512)
	if strings.TrimSpace(problem.Summary) == "" {
		return IncidentProblem{}, errors.New("store: incident problem summary is required")
	}
	if strings.TrimSpace(problem.Severity) == "" {
		problem.Severity = "warning"
	}
	if problem.DetailJSON != nil && len(*problem.DetailJSON) > 0 && !json.Valid(*problem.DetailJSON) {
		return IncidentProblem{}, errors.New("store: incident problem detail must be valid JSON")
	}
	if problem.LastSeenAt.IsZero() {
		problem.LastSeenAt = problem.FirstSeenAt
	}
	if problem.LastSeenAt.IsZero() {
		problem.LastSeenAt = time.Now().UTC()
	}
	if problem.FirstSeenAt.IsZero() {
		problem.FirstSeenAt = problem.LastSeenAt
	}
	problem.FirstSeenAt = problem.FirstSeenAt.UTC()
	problem.LastSeenAt = problem.LastSeenAt.UTC()
	problem.Status = "open"
	problem.ResolvedAt = nil

	var current IncidentProblem
	query := q.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("incident_id = ? AND code = ?", problem.IncidentID, problem.Code).First(&current)
	if errors.Is(query.Error, gorm.ErrRecordNotFound) {
		if err := q.WithContext(ctx).Create(&problem).Error; err == nil {
			return problem, nil
		} else {
			query = q.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("incident_id = ? AND code = ?", problem.IncidentID, problem.Code).First(&current)
			if query.Error != nil {
				return IncidentProblem{}, fmt.Errorf("store: open incident problem: %w", err)
			}
		}
	}
	if query.Error != nil {
		return IncidentProblem{}, fmt.Errorf("store: find incident problem: %w", query.Error)
	}
	updates := map[string]any{"run_id": problem.RunID, "severity": problem.Severity, "status": "open", "summary": problem.Summary, "detail_json": problem.DetailJSON, "last_seen_at": problem.LastSeenAt, "resolved_at": nil}
	if err := q.WithContext(ctx).Model(&IncidentProblem{}).Where("id = ?", current.ID).Updates(updates).Error; err != nil {
		return IncidentProblem{}, fmt.Errorf("store: reopen incident problem: %w", err)
	}
	current.RunID = problem.RunID
	current.Severity = problem.Severity
	current.Status = "open"
	current.Summary = problem.Summary
	current.DetailJSON = problem.DetailJSON
	current.LastSeenAt = problem.LastSeenAt
	current.ResolvedAt = nil
	return current, nil
}

// resolveIncidentProblem 是问题 resolve 的幂等实现；不存在或已 resolved 返回 false。
func resolveIncidentProblem(ctx context.Context, q *gorm.DB, incidentID uint64, code string, runID *uint64, resolvedAt time.Time) (bool, error) {
	if incidentID == 0 || strings.TrimSpace(code) == "" {
		return false, errors.New("store: incident problem incident and code are required")
	}
	if resolvedAt.IsZero() {
		return false, errors.New("store: incident problem resolved_at is required")
	}
	resolvedAt = resolvedAt.UTC()
	var current IncidentProblem
	query := q.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("incident_id = ? AND code = ?", incidentID, code).First(&current)
	if errors.Is(query.Error, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if query.Error != nil {
		return false, fmt.Errorf("store: find incident problem for resolve: %w", query.Error)
	}
	if current.Status == "resolved" {
		return false, nil
	}
	updates := map[string]any{"status": "resolved", "resolved_at": resolvedAt, "last_seen_at": resolvedAt}
	if runID != nil {
		updates["run_id"] = runID
	}
	if err := q.WithContext(ctx).Model(&IncidentProblem{}).Where("id = ? AND status <> ?", current.ID, "resolved").Updates(updates).Error; err != nil {
		return false, fmt.Errorf("store: resolve incident problem: %w", err)
	}
	return true, nil
}

// AppendIncidentEvent 写入非事务路径的 Incident 事件。
func (db *DB) AppendIncidentEvent(ctx context.Context, event IncidentEvent) (IncidentEvent, error) {
	return appendIncidentEvent(ctx, db.DB, event)
}

// OpenIncidentProblem 打开或重开一个同 incident/code 的问题。
func (db *DB) OpenIncidentProblem(ctx context.Context, problem IncidentProblem) (IncidentProblem, error) {
	return openIncidentProblem(ctx, db.DB, problem)
}

// ResolveIncidentProblem 幂等关闭一个 Incident 问题。
func (db *DB) ResolveIncidentProblem(ctx context.Context, incidentID uint64, code string, runID *uint64, resolvedAt time.Time) (bool, error) {
	return resolveIncidentProblem(ctx, db.DB, incidentID, code, runID, resolvedAt)
}

// AppendIncidentEvent 在 ApplyRawEvent 事务内追加事实事件。
func (t *transactionIncidentTx) AppendIncidentEvent(ctx context.Context, event IncidentEvent) (IncidentEvent, error) {
	return appendIncidentEvent(ctx, t.db, event)
}

// OpenIncidentProblem 在 ApplyRawEvent 事务内打开或重开问题。
func (t *transactionIncidentTx) OpenIncidentProblem(ctx context.Context, problem IncidentProblem) (IncidentProblem, error) {
	return openIncidentProblem(ctx, t.db, problem)
}

// ResolveIncidentProblem 在 ApplyRawEvent 事务内幂等关闭问题。
func (t *transactionIncidentTx) ResolveIncidentProblem(ctx context.Context, incidentID uint64, code string, runID *uint64, resolvedAt time.Time) (bool, error) {
	return resolveIncidentProblem(ctx, t.db, incidentID, code, runID, resolvedAt)
}

// ClaimIntegrationEventReceipt atomically claims a third-party event ID.
// Completed receipts are immutable; a processing receipt can be reclaimed
// only after its lease expires, recovering callbacks interrupted by a crash.
func (db *DB) ClaimIntegrationEventReceipt(ctx context.Context, receipt IntegrationEventReceipt) (bool, error) {
	if db == nil || db.DB == nil {
		return false, errors.New("store: database is required")
	}
	receipt.EventID = strings.TrimSpace(receipt.EventID)
	receipt.Provider = strings.TrimSpace(receipt.Provider)
	receipt.EventType = strings.TrimSpace(receipt.EventType)
	if receipt.EventID == "" {
		return false, errors.New("store: integration receipt event_id is required")
	}
	if receipt.Provider == "" || receipt.EventType == "" {
		return false, errors.New("store: integration receipt provider and event_type are required")
	}
	if len([]rune(receipt.EventID)) > 128 || len([]rune(receipt.Provider)) > 16 || len([]rune(receipt.EventType)) > 64 {
		return false, errors.New("store: integration receipt field is too long")
	}
	if receipt.ProcessedAt.IsZero() {
		receipt.ProcessedAt = time.Now().UTC()
	} else {
		receipt.ProcessedAt = receipt.ProcessedAt.UTC()
	}
	if strings.TrimSpace(receipt.Result) == "" {
		receipt.Result = "processing"
	}
	if receipt.Result != "processing" || len([]rune(receipt.Result)) > 32 {
		return false, errors.New("store: integration receipt claim must use processing result")
	}
	created := db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&receipt)
	if created.Error != nil {
		return false, fmt.Errorf("store: claim integration receipt: %w", created.Error)
	}
	if created.RowsAffected == 1 {
		return true, nil
	}
	staleBefore := receipt.ProcessedAt.Add(-integrationReceiptLease)
	reclaimed := db.WithContext(ctx).Model(&IntegrationEventReceipt{}).
		Where("event_id = ? AND result = ? AND processed_at < ?", receipt.EventID, "processing", staleBefore).
		Updates(map[string]any{"provider": receipt.Provider, "event_type": receipt.EventType, "processed_at": receipt.ProcessedAt})
	if reclaimed.Error != nil {
		return false, fmt.Errorf("store: reclaim integration receipt: %w", reclaimed.Error)
	}
	return reclaimed.RowsAffected == 1, nil
}

// CompleteIntegrationEventReceipt records the terminal handling result. It
// intentionally does not require the row to be in a particular intermediate
// state so recovery/replay tooling can safely finalize an older receipt.
func (db *DB) CompleteIntegrationEventReceipt(ctx context.Context, eventID, result string, processedAt time.Time) error {
	eventID = strings.TrimSpace(eventID)
	result = strings.TrimSpace(result)
	if eventID == "" {
		return errors.New("store: integration receipt event_id is required")
	}
	if result == "" {
		return errors.New("store: integration receipt result is required")
	}
	if len([]rune(result)) > 32 {
		return errors.New("store: integration receipt result is too long")
	}
	if processedAt.IsZero() {
		processedAt = time.Now().UTC()
	}
	updates := map[string]any{"result": result, "processed_at": processedAt.UTC()}
	query := db.WithContext(ctx).Model(&IntegrationEventReceipt{}).Where("event_id = ?", eventID).Updates(updates)
	if query.Error != nil {
		return fmt.Errorf("store: complete integration receipt: %w", query.Error)
	}
	if query.RowsAffected == 0 {
		var existing IntegrationEventReceipt
		lookupErr := db.WithContext(ctx).Where("event_id = ?", eventID).First(&existing).Error
		if errors.Is(lookupErr, gorm.ErrRecordNotFound) {
			return gorm.ErrRecordNotFound
		}
		if lookupErr != nil {
			return fmt.Errorf("store: find integration receipt: %w", lookupErr)
		}
	}
	return nil
}

// MarkIntegrationEventReceipt is a concise alias for callers that prefer a
// state-transition name.
func (db *DB) MarkIntegrationEventReceipt(ctx context.Context, eventID, result string, processedAt time.Time) error {
	return db.CompleteIntegrationEventReceipt(ctx, eventID, result, processedAt)
}

// GetIntegrationEventReceipt reads one receipt by its globally unique event ID.
func (db *DB) GetIntegrationEventReceipt(ctx context.Context, eventID string) (IntegrationEventReceipt, error) {
	eventID = strings.TrimSpace(eventID)
	if eventID == "" {
		return IntegrationEventReceipt{}, errors.New("store: integration receipt event_id is required")
	}
	var receipt IntegrationEventReceipt
	err := db.WithContext(ctx).First(&receipt, "event_id = ?", eventID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return IntegrationEventReceipt{}, gorm.ErrRecordNotFound
	}
	if err != nil {
		return IntegrationEventReceipt{}, fmt.Errorf("store: get integration receipt: %w", err)
	}
	return receipt, nil
}

// DeleteIntegrationEventReceipt removes a receipt only when a caller is
// deliberately abandoning a failed claim. Normal handlers should finalize it
// instead, keeping duplicate callbacks idempotent.
func (db *DB) DeleteIntegrationEventReceipt(ctx context.Context, eventID string) error {
	eventID = strings.TrimSpace(eventID)
	if eventID == "" {
		return errors.New("store: integration receipt event_id is required")
	}
	if err := db.WithContext(ctx).Where("event_id = ?", eventID).Delete(&IntegrationEventReceipt{}).Error; err != nil {
		return fmt.Errorf("store: delete integration receipt: %w", err)
	}
	return nil
}

// CreateIMBinding records a Feishu message/thread binding. Insert is
// idempotent on (provider,message_id), allowing a notification retry to reuse
// the existing binding rather than creating ambiguous Incident associations.
func (db *DB) CreateIMBinding(ctx context.Context, binding IMBinding) (IMBinding, error) {
	binding.Provider = strings.TrimSpace(binding.Provider)
	binding.ChatID = strings.TrimSpace(binding.ChatID)
	binding.MessageID = strings.TrimSpace(binding.MessageID)
	binding.MessageKind = strings.TrimSpace(binding.MessageKind)
	if binding.Provider == "" || binding.ChatID == "" || binding.MessageID == "" || binding.MessageKind == "" {
		return IMBinding{}, errors.New("store: im binding provider, chat_id, message_id and message_kind are required")
	}
	if binding.IncidentID == 0 {
		return IMBinding{}, errors.New("store: im binding incident is required")
	}
	if len([]rune(binding.Provider)) > 16 || len([]rune(binding.ChatID)) > 128 || len([]rune(binding.MessageID)) > 128 || len([]rune(binding.MessageKind)) > 32 {
		return IMBinding{}, errors.New("store: im binding field is too long")
	}
	if binding.CreatedAt.IsZero() {
		binding.CreatedAt = time.Now().UTC()
	} else {
		binding.CreatedAt = binding.CreatedAt.UTC()
	}
	result := db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&binding)
	if result.Error != nil {
		return IMBinding{}, fmt.Errorf("store: create im binding: %w", result.Error)
	}
	if result.RowsAffected == 1 {
		return binding, nil
	}
	var existing IMBinding
	if err := db.WithContext(ctx).Where("provider = ? AND message_id = ?", binding.Provider, binding.MessageID).First(&existing).Error; err != nil {
		return IMBinding{}, fmt.Errorf("store: find existing im binding: %w", err)
	}
	if existing.ChatID != binding.ChatID || existing.IncidentID != binding.IncidentID || existing.MessageKind != binding.MessageKind {
		return IMBinding{}, errors.New("store: im binding conflicts with existing message")
	}
	return existing, nil
}

// GetIMBinding reads the unique provider/message binding.
func (db *DB) GetIMBinding(ctx context.Context, provider, messageID string) (IMBinding, error) {
	provider = strings.TrimSpace(provider)
	messageID = strings.TrimSpace(messageID)
	if provider == "" || messageID == "" {
		return IMBinding{}, errors.New("store: im binding provider and message_id are required")
	}
	var binding IMBinding
	err := db.WithContext(ctx).Where("provider = ? AND message_id = ?", provider, messageID).First(&binding).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return IMBinding{}, gorm.ErrRecordNotFound
	}
	if err != nil {
		return IMBinding{}, fmt.Errorf("store: get im binding: %w", err)
	}
	return binding, nil
}

// FindIMBinding resolves a message to its Incident by exact chat and any
// message/thread identity. Message ID is preferred over root/thread matches,
// and newest binding wins when several messages share a thread.
func (db *DB) FindIMBinding(ctx context.Context, provider, chatID, messageID, rootMessageID, threadID string) (IMBinding, error) {
	provider = strings.TrimSpace(provider)
	chatID = strings.TrimSpace(chatID)
	messageID = strings.TrimSpace(messageID)
	rootMessageID = strings.TrimSpace(rootMessageID)
	threadID = strings.TrimSpace(threadID)
	if provider == "" || chatID == "" {
		return IMBinding{}, errors.New("store: im binding provider and chat_id are required")
	}
	if messageID == "" && rootMessageID == "" && threadID == "" {
		return IMBinding{}, errors.New("store: im binding message identity is required")
	}
	query := db.WithContext(ctx).Where("provider = ? AND chat_id = ?", provider, chatID)
	identities := make([]string, 0, 3)
	args := make([]any, 0, 3)
	if messageID != "" {
		identities = append(identities, "message_id = ?")
		args = append(args, messageID)
	}
	if rootMessageID != "" {
		identities = append(identities, "root_message_id = ?")
		args = append(args, rootMessageID)
	}
	if threadID != "" {
		identities = append(identities, "thread_id = ?")
		args = append(args, threadID)
	}
	query = query.Where("("+strings.Join(identities, " OR ")+")", args...)
	var binding IMBinding
	err := query.Order(gorm.Expr("CASE WHEN message_id = ? THEN 0 WHEN root_message_id = ? THEN 1 ELSE 2 END", messageID, rootMessageID)).Order("created_at DESC, id DESC").First(&binding).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return IMBinding{}, gorm.ErrRecordNotFound
	}
	if err != nil {
		return IMBinding{}, fmt.Errorf("store: find im binding: %w", err)
	}
	return binding, nil
}

// LatestIMBinding 返回该 Incident 在指定 provider 下最新一条绑定。
func (db *DB) LatestIMBinding(ctx context.Context, provider string, incidentID uint64, messageKind string) (IMBinding, error) {
	provider = strings.TrimSpace(provider)
	messageKind = strings.TrimSpace(messageKind)
	if provider == "" || incidentID == 0 {
		return IMBinding{}, errors.New("store: im binding lookup is incomplete")
	}
	query := db.WithContext(ctx).Where("provider = ? AND incident_id = ?", provider, incidentID)
	if messageKind != "" {
		query = query.Where("message_kind = ?", messageKind)
	}
	var binding IMBinding
	err := query.Order("id DESC").First(&binding).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return IMBinding{}, ErrIMBindingNotFound
	}
	if err != nil {
		return IMBinding{}, fmt.Errorf("store: latest im binding: %w", err)
	}
	return binding, nil
}

// DB 持有进程级 GORM 连接。D01 复用这一条；后续包不要自己再开连接池。
type DB struct {
	*gorm.DB
}

// Open 只建连。表结构走 migrations/001_init.sql，绝不 AutoMigrate。
func Open(dsn string) (*DB, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("store: mysql DSN is required")
	}

	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("store: open MySQL: %w", err)
	}
	return &DB{DB: db}, nil
}

// Close 关闭底层连接池。
func (db *DB) Close() error {
	if db == nil || db.DB == nil {
		return nil
	}

	sqlDB, err := db.DB.DB()
	if err != nil {
		return fmt.Errorf("store: access SQL database: %w", err)
	}
	return sqlDB.Close()
}

// CreateRawEvent 是 webhook handler 的直接落库点，只做最基本的把关：
// 必须是合法 JSON 才能进队列 —— 坏报文在这里就拦下（handler 回 400），
// 根本不占用 worker 的重试环。payload 拷贝一份再存，和调用方的切片脱钩，
// 避免 buffer 复用把已入库内容改掉。落库即 pending，等 worker 来消费。
func (db *DB) CreateRawEvent(ctx context.Context, source string, payload []byte, createdAt time.Time) (RawEvent, error) {
	if len(bytes.TrimSpace(payload)) == 0 || !json.Valid(payload) {
		return RawEvent{}, ErrInvalidRawEvent
	}
	event := RawEvent{Source: source, Payload: datatypes.JSON(append([]byte(nil), payload...)), Status: "pending", CreatedAt: createdAt}
	if err := db.WithContext(ctx).Create(&event).Error; err != nil {
		return RawEvent{}, fmt.Errorf("store: create raw event: %w", err)
	}
	return event, nil
}

// NextPendingRawEvent 取队首（id 最小的 pending 行）。队列空是正常状态，
// 返回 found=false 而不是 error —— worker 的 drain 靠它判断"排空了，收手"。
func (db *DB) NextPendingRawEvent(ctx context.Context) (RawEvent, bool, error) {
	var event RawEvent
	err := db.WithContext(ctx).Where("status = ?", "pending").Order("id ASC").First(&event).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return RawEvent{}, false, nil
	}
	if err != nil {
		return RawEvent{}, false, fmt.Errorf("store: get next pending raw event: %w", err)
	}
	return event, true, nil
}

// MarkRawEventFailed 给报文本身没救的 raw_event 判死刑并落原因。
// WHERE 带 status='pending'：只允许 pending → failed 这一条路，
// 终态（processed/failed）不会被并发的另一轮处理覆盖。
func (db *DB) MarkRawEventFailed(ctx context.Context, id uint64, message string, processedAt time.Time) error {
	updates := map[string]any{"status": "failed", "processed_at": processedAt, "error": message}
	if err := db.WithContext(ctx).Model(&RawEvent{}).Where("id = ? AND status = ?", id, "pending").Updates(updates).Error; err != nil {
		return fmt.Errorf("store: mark raw event failed: %w", err)
	}
	return nil
}

// ApplyRawEvent 在单个事务里完成：锁住 pending 的 raw_event → 逐条
// applyAlert（每条之后跑一次关联 hook）→ 最后标 processed。
// 任何一步失败整条回滚，raw_event 保持 pending 等 worker 重试 ——
// 它和 MarkRawEventFailed 正是 worker 那套二分法的落库侧：
// 报文没救走 failed，库出问题走这里的回滚重试。
func (db *DB) ApplyRawEvent(ctx context.Context, rawEventID uint64, inputs []AlertInput, processedAt time.Time, hook RawEventApplyHook) ([]AlertApplyResult, error) {
	results := make([]AlertApplyResult, 0, len(inputs))
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var event RawEvent
		// FOR UPDATE + status=pending 双保险：锁住行并重验状态，
		// 同一条 raw_event 不可能被应用两次 —— 等到锁时状态已不是
		// pending 的话，这里查不到行，直接报错回滚。
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND status = ?", rawEventID, "pending").First(&event).Error; err != nil {
			return fmt.Errorf("lock pending raw event: %w", err)
		}
		incidentTx := &transactionIncidentTx{db: tx}
		for _, input := range inputs {
			result, err := applyAlert(tx, input)
			if err != nil {
				return err
			}
			if hook != nil {
				if err := hook(ctx, incidentTx, result); err != nil {
					return fmt.Errorf("apply raw event hook: %w", err)
				}
			}
			results = append(results, result)
		}
		updates := map[string]any{"status": "processed", "processed_at": processedAt, "error": nil}
		if err := tx.Model(&RawEvent{}).Where("id = ? AND status = ?", rawEventID, "pending").Updates(updates).Error; err != nil {
			return fmt.Errorf("mark raw event processed: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("store: apply raw event: %w", err)
	}
	return results, nil
}

// applyAlert 是去重核心：按指纹取 last_alert，和本次 AlertHash 比对，
// 三种结果分别对应 DedupNew / DedupFull / DedupPartial。必须在事务里跑：
// last_alert 的"读-比-写"不是原子的，靠 FOR UPDATE 串行化。
func applyAlert(tx *gorm.DB, input AlertInput) (AlertApplyResult, error) {
	// 身份字段缺失直接报错，不落一条永远查不回来的脏数据。
	if input.Fingerprint == "" || input.AlertHash == "" || input.Name == "" {
		return AlertApplyResult{}, errors.New("store: alert identity is required")
	}
	labels, err := json.Marshal(input.Labels)
	if err != nil {
		return AlertApplyResult{}, fmt.Errorf("store: encode alert labels: %w", err)
	}
	annotations, err := json.Marshal(input.Annotations)
	if err != nil {
		return AlertApplyResult{}, fmt.Errorf("store: encode alert annotations: %w", err)
	}
	alert := Alert{
		Fingerprint: input.Fingerprint, AlertHash: input.AlertHash, Source: input.Source,
		Name: input.Name, Severity: uint8(input.Severity), Status: input.Status,
		Labels: datatypes.JSON(labels), Annotations: datatypes.JSON(annotations),
		GeneratorURL: input.GeneratorURL, StartsAt: input.StartsAt, ReceivedAt: input.ReceivedAt,
	}

	// FOR UPDATE 锁 last_alert 行：同一指纹并发到达时后到的等先到的提交，
	// 不会都读到"不存在"然后各插一份。
	var last LastAlert
	queryErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("fingerprint = ?", input.Fingerprint).First(&last).Error
	if errors.Is(queryErr, gorm.ErrRecordNotFound) {
		// 第一次见到这个指纹：alert 与 last_alert 各插一条，firing_count 从 1 起步。
		if err := tx.Create(&alert).Error; err != nil {
			return AlertApplyResult{}, fmt.Errorf("insert alert: %w", err)
		}
		last = LastAlert{Fingerprint: input.Fingerprint, AlertID: alert.ID, AlertHash: input.AlertHash, Status: input.Status, Severity: uint8(input.Severity), FirstSeen: input.ReceivedAt, LastSeen: input.ReceivedAt, FiringCount: 1}
		if err := tx.Create(&last).Error; err != nil {
			return AlertApplyResult{}, fmt.Errorf("insert last alert: %w", err)
		}
		return AlertApplyResult{Input: input, Dedup: DedupNew, Last: last}, nil
	}
	if queryErr != nil {
		return AlertApplyResult{}, fmt.Errorf("read last alert: %w", queryErr)
	}
	if last.AlertHash == input.AlertHash {
		// 内容一模一样（AlertHash 不含时间字段，repeat_interval 重发的就是这种）：
		// 不插 alert 行 —— 全量去重的意义就在这，告警持续期间 alert 表不膨胀。
		// 只续 last_seen，worker 拿最新时间去 TouchIncident 续 incident 的时间窗。
		if err := tx.Model(&LastAlert{}).Where("fingerprint = ?", input.Fingerprint).Update("last_seen", input.ReceivedAt).Error; err != nil {
			return AlertApplyResult{}, fmt.Errorf("refresh full duplicate: %w", err)
		}
		last.LastSeen = input.ReceivedAt
		return AlertApplyResult{Input: input, Dedup: DedupFull, Last: last}, nil
	}
	// 同指纹不同内容：追加一条新版本 alert，last_alert 改指到它。
	// firing_count 用 SQL 原子自增（gorm.Expr）而不是读-改-写，并发下不丢计数。
	if err := tx.Create(&alert).Error; err != nil {
		return AlertApplyResult{}, fmt.Errorf("insert alert: %w", err)
	}
	updates := map[string]any{
		"alert_id": alert.ID, "alert_hash": input.AlertHash, "status": input.Status,
		"severity": input.Severity, "last_seen": input.ReceivedAt,
		"firing_count": gorm.Expr("firing_count + ?", 1),
	}
	if err := tx.Model(&LastAlert{}).Where("fingerprint = ?", input.Fingerprint).Updates(updates).Error; err != nil {
		return AlertApplyResult{}, fmt.Errorf("update partial duplicate: %w", err)
	}
	last.AlertID = alert.ID
	last.AlertHash = input.AlertHash
	last.Status = input.Status
	last.Severity = uint8(input.Severity)
	last.LastSeen = input.ReceivedAt
	last.FiringCount++
	return AlertApplyResult{Input: input, Dedup: DedupPartial, Last: last}, nil
}

// IncidentMember 是 incident 详情的成员视图：成员关系 + 该 fingerprint 的
// 当前快照状态。 incident_alert 只记"谁进来过"，现状要 join last_alert 和
// 当前版本 alert 才能回答"这个成员现在还在 firing 吗"。
type IncidentMember struct {
	Fingerprint string    `gorm:"column:fingerprint"`
	Name        string    `gorm:"column:name"`
	Status      string    `gorm:"column:status"`
	Severity    uint8     `gorm:"column:severity"`
	LinkedAt    time.Time `gorm:"column:linked_at"`
}

// ListIncidents 按 id 倒序（最新优先）列 incident，status 非空时精确过滤。
// 倒序 + 主键唯一，排序是稳定的 —— 翻页或重查不会出现同一行漂移。
func (db *DB) ListIncidents(ctx context.Context, status string) ([]Incident, error) {
	query := db.WithContext(ctx).Model(&Incident{})
	if status != "" {
		query = query.Where("status = ?", status)
	}
	incidents := make([]Incident, 0)
	if err := query.Order("id DESC").Find(&incidents).Error; err != nil {
		return nil, fmt.Errorf("store: list incidents: %w", err)
	}
	return incidents, nil
}

// ListLatestIncidents 是控制室列表读模型：id DESC（最新优先）并在 SQL 侧限量，
// 不把全表读进内存再截断。status 非空时精确过滤。
func (db *DB) ListLatestIncidents(ctx context.Context, status string, limit int) ([]Incident, error) {
	query := db.WithContext(ctx).Model(&Incident{})
	if strings.TrimSpace(status) != "" {
		query = query.Where("status = ?", status)
	}
	incidents := make([]Incident, 0)
	if err := query.Order("id DESC").Limit(normalizePageLimit(limit)).Find(&incidents).Error; err != nil {
		return nil, fmt.Errorf("store: list latest incidents: %w", err)
	}
	return incidents, nil
}

// GetIncident 按 id 取单条 incident，不存在返回 ErrIncidentNotFound。
func (db *DB) GetIncident(ctx context.Context, id uint64) (Incident, error) {
	var incident Incident
	err := db.WithContext(ctx).First(&incident, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Incident{}, ErrIncidentNotFound
	}
	if err != nil {
		return Incident{}, fmt.Errorf("store: get incident: %w", err)
	}
	return incident, nil
}

// ListIncidentMembers 取 incident 的成员视图，按挂载时间正序（先来的先看）。
func (db *DB) ListIncidentMembers(ctx context.Context, incidentID uint64) ([]IncidentMember, error) {
	members := make([]IncidentMember, 0)
	err := db.WithContext(ctx).
		Table("incident_alert").
		Select("incident_alert.fingerprint, alert.name, last_alert.status, last_alert.severity, incident_alert.linked_at").
		Joins("JOIN last_alert ON last_alert.fingerprint = incident_alert.fingerprint").
		Joins("JOIN alert ON alert.id = last_alert.alert_id").
		Where("incident_alert.incident_id = ?", incidentID).
		Order("incident_alert.linked_at ASC, incident_alert.fingerprint ASC").
		Scan(&members).Error
	if err != nil {
		return nil, fmt.Errorf("store: list incident members: %w", err)
	}
	return members, nil
}

const (
	defaultPageLimit = 20
	maxPageLimit     = 100
)

func normalizePageLimit(limit int) int {
	if limit < 1 {
		return defaultPageLimit
	}
	if limit > maxPageLimit {
		return maxPageLimit
	}
	return limit
}

// ListIncidentEvents 按严格 id > afterID、id ASC 读取 Incident 事实事件。
func (db *DB) ListIncidentEvents(ctx context.Context, incidentID, afterID uint64, limit int) ([]IncidentEvent, error) {
	if incidentID == 0 {
		return nil, errors.New("store: incident event incident is required")
	}
	rows := make([]IncidentEvent, 0)
	err := db.WithContext(ctx).Where("incident_id = ? AND id > ?", incidentID, afterID).
		Order("id ASC").Limit(normalizePageLimit(limit)).Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("store: list incident events: %w", err)
	}
	return rows, nil
}

// ListLatestIncidentEvents 取 Incident 最新 N 条事件，仍按 id ASC 返回，
// 供控制室首屏使用。SSE 回放继续走 ListIncidentEvents 的 afterID 游标。
func (db *DB) ListLatestIncidentEvents(ctx context.Context, incidentID uint64, limit int) ([]IncidentEvent, error) {
	if incidentID == 0 {
		return nil, errors.New("store: incident event incident is required")
	}
	limit = normalizePageLimit(limit)
	desc := make([]IncidentEvent, 0, limit)
	err := db.WithContext(ctx).Where("incident_id = ?", incidentID).
		Order("id DESC").Limit(limit).Find(&desc).Error
	if err != nil {
		return nil, fmt.Errorf("store: list latest incident events: %w", err)
	}
	for i, j := 0, len(desc)-1; i < j; i, j = i+1, j-1 {
		desc[i], desc[j] = desc[j], desc[i]
	}
	return desc, nil
}

// ListIncidentProblems 按 Incident 和可选状态读取问题，默认 id ASC。
func (db *DB) ListIncidentProblems(ctx context.Context, incidentID uint64, status string, limit int) ([]IncidentProblem, error) {
	if incidentID == 0 {
		return nil, errors.New("store: incident problem incident is required")
	}
	query := db.WithContext(ctx).Where("incident_id = ?", incidentID)
	if strings.TrimSpace(status) != "" {
		query = query.Where("status = ?", status)
	}
	rows := make([]IncidentProblem, 0)
	if err := query.Order("id ASC").Limit(normalizePageLimit(limit)).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("store: list incident problems: %w", err)
	}
	return rows, nil
}

// ListAgentRuns 按严格 id > afterID、id ASC 读取目标 Incident 的诊断 run。
func (db *DB) ListAgentRuns(ctx context.Context, incidentID, afterID uint64, limit int) ([]AgentRun, error) {
	if incidentID == 0 {
		return nil, errors.New("store: agent run incident is required")
	}
	rows := make([]AgentRun, 0)
	err := db.WithContext(ctx).Where("incident_id = ? AND id > ?", incidentID, afterID).
		Order("id ASC").Limit(normalizePageLimit(limit)).Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("store: list agent runs: %w", err)
	}
	return rows, nil
}

// ListIncidentApprovals 读取目标 Incident 的审批，最新记录优先。
func (db *DB) ListIncidentApprovals(ctx context.Context, incidentID uint64, status string, limit int) ([]Approval, error) {
	if incidentID == 0 {
		return nil, errors.New("store: approval incident is required")
	}
	query := db.WithContext(ctx).Where("incident_id = ?", incidentID)
	if strings.TrimSpace(status) != "" {
		query = query.Where("status = ?", status)
	}
	rows := make([]Approval, 0)
	if err := query.Order("id DESC").Limit(normalizePageLimit(limit)).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("store: list incident approvals: %w", err)
	}
	return rows, nil
}

// CreateAgentRun 是独立 API 入口的落库点。pending run 与 run.queued 事件
// 必须同一事务提交，SSE 才能把 202 入队结果作为持久化事实回放。
func (db *DB) CreateAgentRun(ctx context.Context, run AgentRun) (AgentRun, error) {
	if run.IncidentID == 0 {
		return AgentRun{}, errors.New("store: agent run incident is required")
	}
	if run.Mode == "" || run.Status == "" {
		return AgentRun{}, errors.New("store: agent run mode and status are required")
	}
	if run.StartedAt.IsZero() {
		return AgentRun{}, errors.New("store: agent run start time is required")
	}
	run.StartedAt = run.StartedAt.UTC()
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var incident Incident
		if err := tx.WithContext(ctx).Select("id").First(&incident, run.IncidentID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrIncidentNotFound
			}
			return fmt.Errorf("store: find incident for agent run: %w", err)
		}
		if err := tx.WithContext(ctx).Create(&run).Error; err != nil {
			return fmt.Errorf("store: create agent run: %w", err)
		}
		runID := run.ID
		_, err := appendIncidentEvent(ctx, tx, IncidentEvent{IncidentID: run.IncidentID, RunID: &runID, EventType: string(eventlog.EventRunQueued), Phase: "run", Status: "pending", Summary: "diagnostic run queued", CreatedAt: run.StartedAt})
		return err
	})
	if err != nil {
		return AgentRun{}, err
	}
	return run, nil
}

// CreateRetryAgentRun 原子创建自动重诊 run。事务先锁住 Incident 行，
// 再锁定同一 Incident 的 pending/running run；已有活跃 run 时不写任何行，
// 返回 created=false。新 run、run.queued 和 retry.scheduled 必须同批提交。
// reason 由调用方先完成脱敏；这里再限制事件摘要和 payload 的长度。
func (db *DB) CreateRetryAgentRun(ctx context.Context, run AgentRun, reason string) (createdRun AgentRun, created bool, err error) {
	if run.IncidentID == 0 {
		return AgentRun{}, false, errors.New("store: retry agent run incident is required")
	}
	if run.Mode == "" {
		return AgentRun{}, false, errors.New("store: retry agent run mode is required")
	}
	if run.Status != "pending" {
		return AgentRun{}, false, errors.New("store: retry agent run status must be pending")
	}
	if run.RetryOf == nil || *run.RetryOf == 0 {
		return AgentRun{}, false, errors.New("store: retry agent run retry_of is required")
	}
	if run.StartedAt.IsZero() {
		return AgentRun{}, false, errors.New("store: retry agent run start time is required")
	}

	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "verification failed"
	}
	reason = truncateRetryEventText(reason, 1024)
	payloadBytes, marshalErr := json.Marshal(struct {
		Reason  string `json:"reason"`
		RetryOf uint64 `json:"retry_of"`
	}{Reason: reason, RetryOf: *run.RetryOf})
	if marshalErr != nil {
		return AgentRun{}, false, fmt.Errorf("store: marshal retry event payload: %w", marshalErr)
	}

	run.StartedAt = run.StartedAt.UTC()
	eventTime := run.StartedAt
	queuedSummary := truncateRetryEventText("retry run queued: "+reason, 512)
	scheduledSummary := truncateRetryEventText("retry scheduled: "+reason, 512)
	payload := datatypes.JSON(payloadBytes)

	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Locking the parent Incident closes the no-active-row gap: two concurrent
		// retries for an Incident with no active run cannot both pass the check.
		var incident Incident
		query := tx.WithContext(ctx).
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id").First(&incident, run.IncidentID)
		if errors.Is(query.Error, gorm.ErrRecordNotFound) {
			return ErrIncidentNotFound
		}
		if query.Error != nil {
			return fmt.Errorf("lock retry incident: %w", query.Error)
		}

		var active AgentRun
		query = tx.WithContext(ctx).
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("incident_id = ? AND status IN ?", run.IncidentID, []string{"pending", "running"}).
			Order("id ASC").First(&active)
		if query.Error == nil {
			return nil
		}
		if !errors.Is(query.Error, gorm.ErrRecordNotFound) {
			return fmt.Errorf("lock active retry runs: %w", query.Error)
		}

		if err := tx.WithContext(ctx).Create(&run).Error; err != nil {
			return fmt.Errorf("insert retry agent run: %w", err)
		}
		runID := run.ID
		if _, err := appendIncidentEvent(ctx, tx, IncidentEvent{
			IncidentID:  run.IncidentID,
			RunID:       &runID,
			EventType:   string(eventlog.EventRunQueued),
			Phase:       "run",
			Status:      "pending",
			Summary:     queuedSummary,
			PayloadJSON: &payload,
			CreatedAt:   eventTime,
		}); err != nil {
			return err
		}
		if _, err := appendIncidentEvent(ctx, tx, IncidentEvent{
			IncidentID:  run.IncidentID,
			RunID:       &runID,
			EventType:   string(eventlog.EventRetryScheduled),
			Phase:       "retry",
			Status:      "scheduled",
			Summary:     scheduledSummary,
			PayloadJSON: &payload,
			CreatedAt:   eventTime,
		}); err != nil {
			return err
		}
		createdRun = run
		created = true
		return nil
	})
	if err != nil {
		return AgentRun{}, false, fmt.Errorf("store: create retry agent run: %w", err)
	}
	if !created {
		return AgentRun{}, false, nil
	}
	return createdRun, true, nil
}

func truncateRetryEventText(text string, maxRunes int) string {
	if maxRunes < 1 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	const suffix = "…[truncated]"
	suffixRunes := []rune(suffix)
	if len(suffixRunes) >= maxRunes {
		return string(runes[:maxRunes])
	}
	return string(runes[:maxRunes-len(suffixRunes)]) + suffix
}

// ListIncidentAlerts 取 incident 每个成员的当前版本 alert（含 labels、
// annotations、generatorURL），D07 证据采集用。按挂载时间正序。
func (db *DB) ListIncidentAlerts(ctx context.Context, incidentID uint64) ([]Alert, error) {
	alerts := make([]Alert, 0)
	err := db.WithContext(ctx).
		Table("incident_alert").
		Select("alert.*").
		Joins("JOIN last_alert ON last_alert.fingerprint = incident_alert.fingerprint").
		Joins("JOIN alert ON alert.id = last_alert.alert_id").
		Where("incident_alert.incident_id = ?", incidentID).
		Order("incident_alert.linked_at ASC, incident_alert.fingerprint ASC").
		Scan(&alerts).Error
	if err != nil {
		return nil, fmt.Errorf("store: list incident alerts: %w", err)
	}
	return alerts, nil
}

// CompleteAgentRun 写入一次诊断的结论：RCA、Plan、token 用量和终态。
// WHERE 限定 pending/running：终态 run 不可被覆盖（审计不可改写）。
func (db *DB) CompleteAgentRun(ctx context.Context, id uint64, rca string, planJSON []byte, tokensIn, tokensOut int, status string, finishedAt time.Time) error {
	if id == 0 {
		return errors.New("store: agent run id is required")
	}
	if status != "succeeded" && status != "failed" {
		return errors.New("store: agent run final status must be succeeded or failed")
	}
	updates := map[string]any{
		"status":      status,
		"tokens_in":   tokensIn,
		"tokens_out":  tokensOut,
		"finished_at": finishedAt,
	}
	if rca != "" {
		updates["rca_text"] = rca
	}
	if len(planJSON) > 0 {
		updates["plan_json"] = datatypes.JSON(planJSON)
	}
	result := db.WithContext(ctx).Model(&AgentRun{}).
		Where("id = ? AND status IN ?", id, []string{"pending", "running"}).
		Updates(updates)
	if result.Error != nil {
		return fmt.Errorf("store: complete agent run: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("store: agent run %d is not pending/running", id)
	}
	return nil
}

// NextPendingAgentRun 取诊断队列队首（id 最小的 pending run）。
// 队列空返回 found=false，与 NextPendingRawEvent 同语义。
func (db *DB) NextPendingAgentRun(ctx context.Context) (AgentRun, bool, error) {
	var run AgentRun
	err := db.WithContext(ctx).Where("status = ?", "pending").Order("id ASC").First(&run).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return AgentRun{}, false, nil
	}
	if err != nil {
		return AgentRun{}, false, fmt.Errorf("store: get next pending agent run: %w", err)
	}
	return run, true, nil
}

// ClaimAgentRun 在短事务内锁定 pending run，读取其 Incident，完成
// pending → running 的 CAS，并把 run.started 与状态变更一起提交。
// 并发消费者或重启补账不会重复认领同一行；RowsAffected=0 表示已经被抢先。
func (db *DB) ClaimAgentRun(ctx context.Context, id uint64, startedAt time.Time) (claimed bool, err error) {
	if id == 0 {
		return false, errors.New("store: agent run id is required")
	}
	if startedAt.IsZero() {
		return false, errors.New("store: agent run start time is required")
	}
	startedAt = startedAt.UTC()

	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var run AgentRun
		query := tx.WithContext(ctx).
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND status = ?", id, "pending").
			First(&run)
		if errors.Is(query.Error, gorm.ErrRecordNotFound) {
			return nil
		}
		if query.Error != nil {
			return fmt.Errorf("lock pending agent run: %w", query.Error)
		}

		// The Incident lookup is intentionally inside the same transaction: event
		// rows must never be emitted for a run whose parent cannot be resolved.
		var incident Incident
		query = tx.WithContext(ctx).Select("id").First(&incident, run.IncidentID)
		if errors.Is(query.Error, gorm.ErrRecordNotFound) {
			return fmt.Errorf("agent run %d incident %d not found", run.ID, run.IncidentID)
		}
		if query.Error != nil {
			return fmt.Errorf("read incident for agent run %d: %w", run.ID, query.Error)
		}

		result := tx.WithContext(ctx).Model(&AgentRun{}).
			Where("id = ? AND status = ?", id, "pending").
			Updates(map[string]any{"status": "running", "started_at": startedAt})
		if result.Error != nil {
			return fmt.Errorf("update claimed agent run: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return nil
		}

		runID := run.ID
		if _, err := appendIncidentEvent(ctx, tx, IncidentEvent{
			IncidentID: incident.ID,
			RunID:      &runID,
			EventType:  string(eventlog.EventRunStarted),
			Phase:      "run",
			Status:     "running",
			Summary:    "diagnostic run started",
			CreatedAt:  startedAt,
		}); err != nil {
			return err
		}
		claimed = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("store: claim agent run: %w", err)
	}
	return claimed, nil
}

// RequeueStaleAgentRuns 在一个短事务内锁定超时 running 行，逐行执行
// running → pending，并为每行写 run.stalled 与幂等的 run_stalled 问题。
// 事务失败时状态、事件和问题全部回滚，下一轮仍可完整补账。
func (db *DB) RequeueStaleAgentRuns(ctx context.Context, staleBefore time.Time) (requeued int64, err error) {
	if staleBefore.IsZero() {
		return 0, errors.New("store: stale-before time is required")
	}
	staleBefore = staleBefore.UTC()

	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var runs []AgentRun
		if err := tx.WithContext(ctx).
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("status = ? AND started_at < ?", "running", staleBefore).
			Order("id ASC").
			Find(&runs).Error; err != nil {
			return fmt.Errorf("lock stale agent runs: %w", err)
		}
		if len(runs) == 0 {
			return nil
		}
		requeuedAt := time.Now().UTC()
		for _, run := range runs {
			var incident Incident
			query := tx.WithContext(ctx).Select("id").First(&incident, run.IncidentID)
			if errors.Is(query.Error, gorm.ErrRecordNotFound) {
				return fmt.Errorf("agent run %d incident %d not found", run.ID, run.IncidentID)
			}
			if query.Error != nil {
				return fmt.Errorf("read incident for stale agent run %d: %w", run.ID, query.Error)
			}

			result := tx.WithContext(ctx).Model(&AgentRun{}).
				Where("id = ? AND status = ?", run.ID, "running").
				Update("status", "pending")
			if result.Error != nil {
				return fmt.Errorf("requeue stale agent run %d: %w", run.ID, result.Error)
			}
			if result.RowsAffected == 0 {
				continue
			}

			runID := run.ID
			if _, err := appendIncidentEvent(ctx, tx, IncidentEvent{
				IncidentID: incident.ID,
				RunID:      &runID,
				EventType:  string(eventlog.EventRunStalled),
				Phase:      "run",
				Status:     "pending",
				Summary:    "diagnostic run stalled and was requeued",
				CreatedAt:  requeuedAt,
			}); err != nil {
				return err
			}
			if _, err := openIncidentProblem(ctx, tx, IncidentProblem{
				IncidentID:  incident.ID,
				RunID:       &runID,
				Code:        "run_stalled",
				Severity:    "warning",
				Status:      "open",
				Summary:     "diagnostic run stalled and was requeued",
				FirstSeenAt: requeuedAt,
				LastSeenAt:  requeuedAt,
			}); err != nil {
				return err
			}
			requeued++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("store: requeue stale agent runs: %w", err)
	}
	return requeued, nil
}

// AppendRunStep 落一条流水线审计步。input/output 应在调用方截断，
// 这里只兜底校验 run 关联和序号。
func (db *DB) AppendRunStep(ctx context.Context, step AgentRunStep) error {
	if step.RunID == 0 || step.Name == "" || step.Kind == "" {
		return errors.New("store: run step run_id, kind and name are required")
	}
	if step.StartedAt.IsZero() {
		return errors.New("store: run step start time is required")
	}
	if err := db.WithContext(ctx).Create(&step).Error; err != nil {
		return fmt.Errorf("store: append run step: %w", err)
	}
	return nil
}

// AppendRunStepRecord 在同一短事务中写入 step、事件和有限问题变更。
// 任一写入失败都会回滚整批，旧 AppendRunStep 保持单步兼容。
func (db *DB) AppendRunStepRecord(ctx context.Context, record RunStepRecord) error {
	step := record.Step
	if step.RunID == 0 || step.Name == "" || step.Kind == "" {
		return errors.New("store: run step run_id, kind and name are required")
	}
	if step.StartedAt.IsZero() {
		return errors.New("store: run step start time is required")
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.WithContext(ctx).Create(&step).Error; err != nil {
			return fmt.Errorf("store: append run step: %w", err)
		}
		for _, event := range record.Events {
			if _, err := appendIncidentEvent(ctx, tx, event); err != nil {
				return err
			}
		}
		for _, mutation := range record.Problems {
			if err := applyProblemMutation(ctx, tx, mutation); err != nil {
				return err
			}
		}
		return nil
	})
}

// AppendRunStepBatch 是 AppendRunStepRecord 的语义同义入口，便于调用方迁移。
func (db *DB) AppendRunStepBatch(ctx context.Context, record RunStepRecord) error {
	return db.AppendRunStepRecord(ctx, record)
}

// CompleteRun 原子更新 run 终态，并写入最后一批 Step/Event/Problem。
// 事务锁住 run 后执行 CAS；任一审计记录失败都会回滚终态更新。
func (db *DB) CompleteRun(ctx context.Context, completion RunCompletion) error {
	if completion.RunID == 0 {
		return errors.New("store: run completion run_id is required")
	}
	if completion.Status != "succeeded" && completion.Status != "failed" {
		return errors.New("store: run completion status must be succeeded or failed")
	}
	if completion.FinishedAt.IsZero() {
		return errors.New("store: run completion finished_at is required")
	}
	if len(completion.PlanJSON) > 0 && !json.Valid(completion.PlanJSON) {
		return errors.New("store: run completion plan must be valid JSON")
	}
	completion.FinishedAt = completion.FinishedAt.UTC()
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var run AgentRun
		query := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).First(&run, completion.RunID)
		if errors.Is(query.Error, gorm.ErrRecordNotFound) {
			return ErrAgentRunNotFound
		}
		if query.Error != nil {
			return fmt.Errorf("lock agent run for completion: %w", query.Error)
		}
		if run.Status != "pending" && run.Status != "running" {
			return fmt.Errorf("store: agent run %d is not pending/running", completion.RunID)
		}

		updates := map[string]any{
			"status":      completion.Status,
			"tokens_in":   completion.TokensIn,
			"tokens_out":  completion.TokensOut,
			"finished_at": completion.FinishedAt,
		}
		if completion.RCA != "" {
			updates["rca_text"] = completion.RCA
		}
		if len(completion.PlanJSON) > 0 {
			updates["plan_json"] = datatypes.JSON(append([]byte(nil), completion.PlanJSON...))
		}
		result := tx.WithContext(ctx).Model(&AgentRun{}).
			Where("id = ? AND status IN ?", completion.RunID, []string{"pending", "running"}).
			Updates(updates)
		if result.Error != nil {
			return fmt.Errorf("complete agent run: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return fmt.Errorf("store: agent run %d is not pending/running", completion.RunID)
		}

		for _, step := range completion.Steps {
			if step.RunID == 0 {
				step.RunID = run.ID
			}
			if step.RunID != run.ID || step.Name == "" || step.Kind == "" {
				return errors.New("store: run completion step has invalid run_id, kind or name")
			}
			if step.StartedAt.IsZero() {
				return errors.New("store: run completion step start time is required")
			}
			if err := tx.WithContext(ctx).Create(&step).Error; err != nil {
				return fmt.Errorf("append completion run step: %w", err)
			}
		}
		for _, event := range completion.Events {
			if event.IncidentID == 0 {
				event.IncidentID = run.IncidentID
			}
			if event.IncidentID != run.IncidentID {
				return errors.New("store: run completion event incident does not match run")
			}
			if event.RunID == nil {
				runID := run.ID
				event.RunID = &runID
			} else if *event.RunID != run.ID {
				return errors.New("store: run completion event run does not match run")
			}
			if _, err := appendIncidentEvent(ctx, tx, event); err != nil {
				return err
			}
		}
		for _, mutation := range completion.Problems {
			if mutation.Kind == ProblemOpen {
				if mutation.Problem.IncidentID == 0 {
					mutation.Problem.IncidentID = run.IncidentID
				}
				if mutation.Problem.IncidentID != run.IncidentID {
					return errors.New("store: run completion problem incident does not match run")
				}
			} else if mutation.Kind == ProblemResolve {
				if mutation.IncidentID == 0 {
					mutation.IncidentID = run.IncidentID
				}
				if mutation.IncidentID != run.IncidentID {
					return errors.New("store: run completion problem incident does not match run")
				}
			}
			if err := applyProblemMutation(ctx, tx, mutation); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("store: complete run: %w", err)
	}
	return nil
}
func applyProblemMutation(ctx context.Context, tx *gorm.DB, mutation ProblemMutation) error {
	switch mutation.Kind {
	case ProblemOpen:
		problem := mutation.Problem
		if problem.IncidentID == 0 {
			problem.IncidentID = mutation.IncidentID
		}
		if _, err := openIncidentProblem(ctx, tx, problem); err != nil {
			return err
		}
		return nil
	case ProblemResolve:
		incidentID := mutation.IncidentID
		if incidentID == 0 {
			incidentID = mutation.Problem.IncidentID
		}
		code := mutation.Code
		if code == "" {
			code = mutation.Problem.Code
		}
		if _, err := resolveIncidentProblem(ctx, tx, incidentID, code, mutation.RunID, mutation.ResolvedAt); err != nil {
			return err
		}
		return nil
	default:
		return fmt.Errorf("store: unsupported problem mutation kind %q", mutation.Kind)
	}
}

// CreateConversationMessage 插入一条对话消息，供 Ask/Feishu worker 共享。
func (db *DB) CreateConversationMessage(ctx context.Context, message ConversationMessage) (ConversationMessage, error) {
	if message.IncidentID == 0 {
		return ConversationMessage{}, errors.New("store: conversation message incident is required")
	}
	if strings.TrimSpace(message.Channel) == "" || strings.TrimSpace(message.Role) == "" || strings.TrimSpace(message.Status) == "" {
		return ConversationMessage{}, errors.New("store: conversation message channel, role and status are required")
	}
	if message.Content == "" {
		return ConversationMessage{}, errors.New("store: conversation message content is required")
	}
	if message.CreatedAt.IsZero() {
		return ConversationMessage{}, errors.New("store: conversation message created_at is required")
	}
	if message.MetadataJSON != nil && len(*message.MetadataJSON) > 0 && !json.Valid(*message.MetadataJSON) {
		return ConversationMessage{}, errors.New("store: conversation message metadata must be valid JSON")
	}
	if err := db.WithContext(ctx).Create(&message).Error; err != nil {
		return ConversationMessage{}, fmt.Errorf("store: create conversation message: %w", err)
	}
	return message, nil
}

// ListConversationMessages 按严格 id > afterID、id ASC 回放对话消息。
func (db *DB) ListConversationMessages(ctx context.Context, incidentID, afterID uint64, limit int) ([]ConversationMessage, error) {
	if incidentID == 0 {
		return nil, errors.New("store: conversation message incident is required")
	}
	rows := make([]ConversationMessage, 0)
	err := db.WithContext(ctx).Where("incident_id = ? AND id > ?", incidentID, afterID).
		Order("id ASC").Limit(normalizePageLimit(limit)).Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("store: list conversation messages: %w", err)
	}
	return rows, nil
}

// ClaimConversationMessage CAS queued -> running and records the lease start.
func (db *DB) ClaimConversationMessage(ctx context.Context, id uint64, now time.Time) (bool, error) {
	if id == 0 {
		return false, errors.New("store: conversation message id is required")
	}
	if now.IsZero() {
		return false, errors.New("store: conversation message claim time is required")
	}
	result := db.WithContext(ctx).Model(&ConversationMessage{}).
		Where("id = ? AND status = ?", id, "queued").
		Updates(map[string]any{"status": "running", "claimed_at": now.UTC()})
	if result.Error != nil {
		return false, fmt.Errorf("store: claim conversation message: %w", result.Error)
	}
	return result.RowsAffected > 0, nil
}

// CompleteConversationMessage CAS running -> completed/failed and clears the lease.
func (db *DB) CompleteConversationMessage(ctx context.Context, id uint64, status string, finishedAt time.Time) error {
	if id == 0 {
		return errors.New("store: conversation message id is required")
	}
	if status != "completed" && status != "failed" {
		return errors.New("store: conversation message final status must be completed or failed")
	}
	if finishedAt.IsZero() {
		return errors.New("store: conversation message finished_at is required")
	}
	result := db.WithContext(ctx).Model(&ConversationMessage{}).
		Where("id = ? AND status = ?", id, "running").
		Updates(map[string]any{"status": status, "finished_at": finishedAt.UTC(), "claimed_at": nil})
	if result.Error != nil {
		return fmt.Errorf("store: complete conversation message: %w", result.Error)
	}
	if result.RowsAffected > 0 {
		return nil
	}
	var existing ConversationMessage
	findErr := db.WithContext(ctx).First(&existing, id).Error
	if errors.Is(findErr, gorm.ErrRecordNotFound) {
		return ErrConversationMessageNotFound
	}
	if findErr != nil {
		return fmt.Errorf("store: find conversation message: %w", findErr)
	}
	return fmt.Errorf("store: conversation message %d is not running", id)
}

// RequeueStaleConversationMessages recovers a question claimed by a process
// that died before writing its terminal state. A running worker has five
// minutes to complete the LLM call before another poller may reclaim it.
func (db *DB) RequeueStaleConversationMessages(ctx context.Context, staleBefore time.Time) (int64, error) {
	if staleBefore.IsZero() {
		return 0, errors.New("store: stale conversation threshold is required")
	}
	result := db.WithContext(ctx).Model(&ConversationMessage{}).
		Where("role = ? AND status = ? AND claimed_at IS NOT NULL AND claimed_at < ?", "user", "running", staleBefore.UTC()).
		Updates(map[string]any{"status": "queued", "claimed_at": nil})
	if result.Error != nil {
		return 0, fmt.Errorf("store: requeue stale conversation messages: %w", result.Error)
	}
	return result.RowsAffected, nil
}

// NextQueuedConversationMessage returns the oldest queued message for worker polling.
// ClaimConversationMessage performs the concurrent CAS after this best-effort read.
func (db *DB) NextQueuedConversationMessage(ctx context.Context) (ConversationMessage, bool, error) {
	var message ConversationMessage
	err := db.WithContext(ctx).Where("status = ?", "queued").Order("id ASC").First(&message).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ConversationMessage{}, false, nil
	}
	if err != nil {
		return ConversationMessage{}, false, fmt.Errorf("store: get next queued conversation message: %w", err)
	}
	return message, true, nil
}

// CreateWebOAuthState persists a one-time OAuth state. The caller passes only
// the hash of the browser-visible state and an encrypted PKCE verifier.
func (db *DB) CreateWebOAuthState(ctx context.Context, state WebOAuthState) (WebOAuthState, error) {
	if strings.TrimSpace(state.StateHash) == "" {
		return WebOAuthState{}, errors.New("store: oauth state hash is required")
	}
	if strings.TrimSpace(state.CodeVerifierCiphertext) == "" {
		return WebOAuthState{}, errors.New("store: oauth verifier ciphertext is required")
	}
	if strings.TrimSpace(state.RedirectURI) == "" {
		return WebOAuthState{}, errors.New("store: oauth redirect URI is required")
	}
	if state.ExpiresAt.IsZero() {
		return WebOAuthState{}, errors.New("store: oauth state expiry is required")
	}
	if state.CreatedAt.IsZero() {
		state.CreatedAt = time.Now().UTC()
	}
	state.ExpiresAt = state.ExpiresAt.UTC()
	state.CreatedAt = state.CreatedAt.UTC()
	if err := db.WithContext(ctx).Create(&state).Error; err != nil {
		return WebOAuthState{}, fmt.Errorf("store: create oauth state: %w", err)
	}
	return state, nil
}

// ConsumeWebOAuthState atomically reads and deletes an OAuth state. A state is
// never reusable, including after a successful token exchange. Expired rows are
// deleted while being observed so an abandoned login cannot accumulate rows.
func (db *DB) ConsumeWebOAuthState(ctx context.Context, stateHash string, now time.Time) (WebOAuthState, error) {
	stateHash = strings.TrimSpace(stateHash)
	if stateHash == "" {
		return WebOAuthState{}, ErrWebOAuthStateNotFound
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var state WebOAuthState
	expired := false
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("state_hash = ?", stateHash).First(&state)
		if errors.Is(query.Error, gorm.ErrRecordNotFound) {
			return ErrWebOAuthStateNotFound
		}
		if query.Error != nil {
			return fmt.Errorf("store: get oauth state: %w", query.Error)
		}
		expired = !state.ExpiresAt.After(now)
		if err := tx.Delete(&WebOAuthState{}, "state_hash = ?", stateHash).Error; err != nil {
			if expired {
				return fmt.Errorf("store: delete expired oauth state: %w", err)
			}
			return fmt.Errorf("store: consume oauth state: %w", err)
		}
		return nil
	})
	if err != nil {
		return WebOAuthState{}, err
	}
	if expired {
		return WebOAuthState{}, ErrWebOAuthStateExpired
	}
	return state, nil
}

// DeleteWebOAuthState removes an OAuth state without attempting to consume it.
// It is useful for cleanup paths and is intentionally idempotent.
func (db *DB) DeleteWebOAuthState(ctx context.Context, stateHash string) error {
	stateHash = strings.TrimSpace(stateHash)
	if stateHash == "" {
		return nil
	}
	if err := db.WithContext(ctx).Delete(&WebOAuthState{}, "state_hash = ?", stateHash).Error; err != nil {
		return fmt.Errorf("store: delete oauth state: %w", err)
	}
	return nil
}

// CreateWebSession stores only hashes of the session and CSRF secrets.
func (db *DB) CreateWebSession(ctx context.Context, session WebSession) (WebSession, error) {
	if strings.TrimSpace(session.ID) == "" {
		return WebSession{}, errors.New("store: web session id is required")
	}
	if strings.TrimSpace(session.TokenHash) == "" || strings.TrimSpace(session.CSRFTokenHash) == "" {
		return WebSession{}, errors.New("store: web session secret hashes are required")
	}
	if strings.TrimSpace(session.ActorID) == "" || strings.TrimSpace(session.ActorName) == "" {
		return WebSession{}, errors.New("store: web session actor is required")
	}
	if session.ExpiresAt.IsZero() {
		return WebSession{}, errors.New("store: web session expiry is required")
	}
	if session.CreatedAt.IsZero() {
		session.CreatedAt = time.Now().UTC()
	}
	if session.LastSeenAt.IsZero() {
		session.LastSeenAt = session.CreatedAt
	}
	if err := db.WithContext(ctx).Create(&session).Error; err != nil {
		return WebSession{}, fmt.Errorf("store: create web session: %w", err)
	}
	return session, nil
}

// GetWebSessionByTokenHash returns only a currently valid, non-revoked session.
func (db *DB) GetWebSessionByTokenHash(ctx context.Context, tokenHash string, now time.Time) (WebSession, error) {
	tokenHash = strings.TrimSpace(tokenHash)
	if tokenHash == "" {
		return WebSession{}, ErrWebSessionNotFound
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var session WebSession
	query := db.WithContext(ctx).Where("token_hash = ? AND revoked_at IS NULL AND expires_at > ?", tokenHash, now.UTC()).First(&session)
	if errors.Is(query.Error, gorm.ErrRecordNotFound) {
		return WebSession{}, ErrWebSessionNotFound
	}
	if query.Error != nil {
		return WebSession{}, fmt.Errorf("store: get web session: %w", query.Error)
	}
	return session, nil
}

// TouchWebSession updates activity only while the session remains valid.
func (db *DB) TouchWebSession(ctx context.Context, tokenHash string, now time.Time) error {
	tokenHash = strings.TrimSpace(tokenHash)
	if tokenHash == "" {
		return ErrWebSessionNotFound
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	result := db.WithContext(ctx).Model(&WebSession{}).
		Where("token_hash = ? AND revoked_at IS NULL AND expires_at > ?", tokenHash, now.UTC()).
		Update("last_seen_at", now.UTC())
	if result.Error != nil {
		return fmt.Errorf("store: touch web session: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return ErrWebSessionNotFound
	}
	return nil
}

// RevokeWebSession invalidates a session. Repeated logout is deliberately
// idempotent, so a missing or already-revoked token is not an error.
func (db *DB) RevokeWebSession(ctx context.Context, tokenHash string, now time.Time) error {
	tokenHash = strings.TrimSpace(tokenHash)
	if tokenHash == "" {
		return nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	result := db.WithContext(ctx).Model(&WebSession{}).
		Where("token_hash = ? AND revoked_at IS NULL", tokenHash).
		Update("revoked_at", now.UTC())
	if result.Error != nil {
		return fmt.Errorf("store: revoke web session: %w", result.Error)
	}
	return nil
}

// GetWebSession reads a session by its opaque database id. It is not used for
// authentication; token-hash lookup above is the only browser auth path.
func (db *DB) GetWebSession(ctx context.Context, id string) (WebSession, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return WebSession{}, ErrWebSessionNotFound
	}
	var session WebSession
	query := db.WithContext(ctx).Where("id = ?", id).First(&session)
	if errors.Is(query.Error, gorm.ErrRecordNotFound) {
		return WebSession{}, ErrWebSessionNotFound
	}
	if query.Error != nil {
		return WebSession{}, fmt.Errorf("store: get web session: %w", query.Error)
	}
	return session, nil
}

// GetLLMModelSelection reads the globally selected model.
func (db *DB) GetLLMModelSelection(ctx context.Context) (LLMModelSelection, error) {
	var selection LLMModelSelection
	query := db.WithContext(ctx).Where("singleton_id = ?", 1).First(&selection)
	if errors.Is(query.Error, gorm.ErrRecordNotFound) {
		return LLMModelSelection{}, ErrLLMModelSelectionNotFound
	}
	if query.Error != nil {
		return LLMModelSelection{}, fmt.Errorf("store: get llm model selection: %w", query.Error)
	}
	return selection, nil
}

// GetOrInitializeLLMModelSelection creates the singleton only when absent.
// The configured default never overwrites a previously selected model.
func (db *DB) GetOrInitializeLLMModelSelection(ctx context.Context, defaultModel string, now time.Time) (LLMModelSelection, error) {
	defaultModel, err := normalizeLLMModelName(defaultModel)
	if err != nil {
		return LLMModelSelection{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	candidate := LLMModelSelection{SingletonID: 1, CurrentModel: defaultModel, UpdatedAt: now.UTC()}
	if err := db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&candidate).Error; err != nil {
		return LLMModelSelection{}, fmt.Errorf("store: initialize llm model selection: %w", err)
	}
	return db.GetLLMModelSelection(ctx)
}

// SetLLMModelSelection serializes global model changes with a row lock. The
// caller must validate the model against its configuration allowlist first.
func (db *DB) SetLLMModelSelection(ctx context.Context, model string, now time.Time) (LLMModelSelection, error) {
	model, err := normalizeLLMModelName(model)
	if err != nil {
		return LLMModelSelection{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var selection LLMModelSelection
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("singleton_id = ?", 1).First(&selection)
		if errors.Is(query.Error, gorm.ErrRecordNotFound) {
			return ErrLLMModelSelectionNotFound
		}
		if query.Error != nil {
			return fmt.Errorf("store: lock llm model selection: %w", query.Error)
		}
		selection.CurrentModel = model
		selection.UpdatedAt = now.UTC()
		if err := tx.Model(&selection).Updates(map[string]any{
			"current_model": selection.CurrentModel,
			"updated_at":    selection.UpdatedAt,
		}).Error; err != nil {
			return fmt.Errorf("store: set llm model selection: %w", err)
		}
		return nil
	})
	if err != nil {
		return LLMModelSelection{}, err
	}
	return selection, nil
}

func normalizeLLMModelName(model string) (string, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", errors.New("store: llm model is required")
	}
	if len([]rune(model)) > 128 {
		return "", errors.New("store: llm model is too long")
	}
	return model, nil
}

// ErrApprovalNotFound / ErrApprovalConflict 是审批决策的哨兵错误：
// 404 与 409 的 HTTP 映射靠它们。
var ErrApprovalNotFound = errors.New("store: approval not found")
var ErrApprovalConflict = errors.New("store: approval already decided")

const approvalNearExpiryWindow = 5 * time.Minute

func validateApprovalCreate(approval Approval) error {
	if approval.IncidentID == 0 || approval.RunID == 0 {
		return errors.New("store: approval incident and run are required")
	}
	if approval.ToolName == "" || len(approval.ArgsJSON) == 0 || approval.PlanHash == "" {
		return errors.New("store: approval tool, args and plan_hash are required")
	}
	if approval.ExpiresAt.IsZero() || approval.CreatedAt.IsZero() {
		return errors.New("store: approval times are required")
	}
	if !json.Valid(approval.ArgsJSON) {
		return errors.New("store: approval args must be valid JSON")
	}
	return nil
}

func validateApprovalDecision(status, decidedBy, decisionSource string, now time.Time) error {
	if status != "approved" && status != "denied" {
		return errors.New("store: approval decision must be approved or denied")
	}
	if strings.TrimSpace(decidedBy) == "" {
		return errors.New("store: approval decider is required")
	}
	if len([]rune(decidedBy)) > 64 {
		return errors.New("store: approval decider is too long")
	}
	if strings.TrimSpace(decisionSource) == "" {
		return errors.New("store: approval decision source is required")
	}
	if len([]rune(decisionSource)) > 16 {
		return errors.New("store: approval decision source is too long")
	}
	if now.IsZero() {
		return errors.New("store: approval decision time is required")
	}
	return nil
}

func appendApprovalEvent(ctx context.Context, tx *gorm.DB, approval Approval, eventType eventlog.EventType, status, summary string, createdAt time.Time) error {
	approvalID := approval.ID
	runID := approval.RunID
	_, err := appendIncidentEvent(ctx, tx, IncidentEvent{
		IncidentID: approval.IncidentID,
		RunID:      &runID,
		ApprovalID: &approvalID,
		EventType:  string(eventType),
		Phase:      "approval",
		Status:     status,
		Summary:    summary,
		CreatedAt:  createdAt,
	})
	return err
}

func appendApprovalCreatedEvent(ctx context.Context, tx *gorm.DB, approval Approval) error {
	return appendApprovalEvent(ctx, tx, approval, eventlog.EventApprovalCreated, "pending", "approval created", approval.CreatedAt)
}

// decideApprovalInTx 执行带行锁的 pending + expires_at CAS，并追加决定事件。
// 调用方必须已经校验参数；所有修改和事件写入都在 tx 中完成。
func decideApprovalInTx(ctx context.Context, tx *gorm.DB, id uint64, status, decidedBy, decisionReason, decisionSource string, now time.Time) (Approval, error) {
	var approval Approval
	query := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		First(&approval, id)
	if errors.Is(query.Error, gorm.ErrRecordNotFound) {
		return Approval{}, ErrApprovalNotFound
	}
	if query.Error != nil {
		return Approval{}, fmt.Errorf("store: lock approval: %w", query.Error)
	}
	if approval.Status != "pending" || !approval.ExpiresAt.After(now) {
		return Approval{}, ErrApprovalConflict
	}

	decidedAt := now.UTC()
	var reason any
	if decisionReason != "" {
		reason = decisionReason
	}
	result := tx.WithContext(ctx).Model(&Approval{}).
		Where("id = ? AND status = ? AND expires_at > ?", id, "pending", now).
		Updates(map[string]any{
			"status":          status,
			"decided_by":      decidedBy,
			"decided_at":      decidedAt,
			"decision_reason": reason,
			"decision_source": decisionSource,
		})
	if result.Error != nil {
		return Approval{}, fmt.Errorf("store: decide approval: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return Approval{}, ErrApprovalConflict
	}

	decidedByCopy := decidedBy
	sourceCopy := decisionSource
	approval.Status = status
	approval.DecidedBy = &decidedByCopy
	approval.DecidedAt = &decidedAt
	approval.DecisionSource = &sourceCopy
	if decisionReason != "" {
		reasonCopy := decisionReason
		approval.DecisionReason = &reasonCopy
	} else {
		approval.DecisionReason = nil
	}
	eventType := eventlog.EventApprovalDenied
	if status == "approved" {
		eventType = eventlog.EventApprovalApproved
	}
	if err := appendApprovalEvent(ctx, tx, approval, eventType, status, "approval "+status, decidedAt); err != nil {
		return Approval{}, err
	}
	return approval, nil
}

// CreateApproval 落一条 pending 审批单，并在同一事务写 approval.created。
// 绑定 incident/run/tool/args/plan_hash 与过期时间 —— 审批通过的是"这份计划"，
// 不是某个模糊意图（GC-13）。
func (db *DB) CreateApproval(ctx context.Context, approval Approval) (Approval, error) {
	if err := validateApprovalCreate(approval); err != nil {
		return Approval{}, err
	}
	approval.Status = "pending"
	approval.DecidedBy = nil
	approval.DecidedAt = nil
	approval.DecisionReason = nil
	approval.DecisionSource = nil
	approval.ResultJSON = nil
	approval.CreatedAt = approval.CreatedAt.UTC()
	approval.ExpiresAt = approval.ExpiresAt.UTC()
	if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.WithContext(ctx).Create(&approval).Error; err != nil {
			return fmt.Errorf("store: create approval: %w", err)
		}
		if err := appendApprovalCreatedEvent(ctx, tx, approval); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return Approval{}, err
	}
	return approval, nil
}

// CreateSystemApprovedApproval 原子创建并系统批准一张审批单。
// approval.created、approval.approved 与两次状态写入必须同一事务提交。
func (db *DB) CreateSystemApprovedApproval(ctx context.Context, approval Approval, decidedBy, decisionReason, decisionSource string, now time.Time) (Approval, error) {
	if err := validateApprovalCreate(approval); err != nil {
		return Approval{}, err
	}
	now = now.UTC()
	if err := validateApprovalDecision("approved", decidedBy, decisionSource, now); err != nil {
		return Approval{}, err
	}
	approval.Status = "pending"
	approval.DecidedBy = nil
	approval.DecidedAt = nil
	approval.DecisionReason = nil
	approval.DecisionSource = nil
	approval.ResultJSON = nil
	approval.CreatedAt = approval.CreatedAt.UTC()
	approval.ExpiresAt = approval.ExpiresAt.UTC()
	var decided Approval
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.WithContext(ctx).Create(&approval).Error; err != nil {
			return fmt.Errorf("store: create system approval: %w", err)
		}
		if err := appendApprovalCreatedEvent(ctx, tx, approval); err != nil {
			return err
		}
		var err error
		decided, err = decideApprovalInTx(ctx, tx, approval.ID, "approved", decidedBy, decisionReason, decisionSource, now)
		return err
	})
	if err != nil {
		if errors.Is(err, ErrApprovalNotFound) || errors.Is(err, ErrApprovalConflict) {
			return Approval{}, err
		}
		return Approval{}, fmt.Errorf("store: create system approval: %w", err)
	}
	return decided, nil
}

// GetApproval 按 id 取审批单。
func (db *DB) GetApproval(ctx context.Context, id uint64) (Approval, error) {
	var approval Approval
	err := db.WithContext(ctx).First(&approval, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Approval{}, ErrApprovalNotFound
	}
	if err != nil {
		return Approval{}, fmt.Errorf("store: get approval: %w", err)
	}
	return approval, nil
}

// DecideApproval 以 pending + expires_at 为 CAS 条件决定审批，并在同一事务
// 写入 decided_at/reason/source 与 approval.approved/denied 事件。冲突不会覆盖
// 原操作者、时间或原因。
func (db *DB) DecideApproval(ctx context.Context, id uint64, status, decidedBy, decisionReason, decisionSource string, now time.Time) (Approval, error) {
	if err := validateApprovalDecision(status, decidedBy, decisionSource, now); err != nil {
		return Approval{}, err
	}
	now = now.UTC()
	var decided Approval
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		decided, err = decideApprovalInTx(ctx, tx, id, status, decidedBy, decisionReason, decisionSource, now)
		return err
	})
	if err != nil {
		if errors.Is(err, ErrApprovalNotFound) || errors.Is(err, ErrApprovalConflict) {
			return Approval{}, err
		}
		return Approval{}, fmt.Errorf("store: decide approval: %w", err)
	}
	return decided, nil
}

// ExpireApprovals 在短事务内逐行锁定过期 pending/approved 审批，转为
// expired，并为每行写 approval.expired；near-expiry 问题同步 resolve/open。
// executing 与其它终态不会被触碰。
func (db *DB) ExpireApprovals(ctx context.Context, now time.Time) (int64, error) {
	if now.IsZero() {
		return 0, errors.New("store: approval expiry time is required")
	}
	now = now.UTC()
	var expired int64
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var approvals []Approval
		if err := tx.WithContext(ctx).
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("status IN ? AND expires_at <= ?", []string{"pending", "approved"}, now).
			Order("id ASC").Find(&approvals).Error; err != nil {
			return fmt.Errorf("store: lock expired approvals: %w", err)
		}
		for _, approval := range approvals {
			result := tx.WithContext(ctx).Model(&Approval{}).
				Where("id = ? AND status IN ? AND expires_at <= ?", approval.ID, []string{"pending", "approved"}, now).
				Update("status", "expired")
			if result.Error != nil {
				return fmt.Errorf("store: expire approval %d: %w", approval.ID, result.Error)
			}
			if result.RowsAffected != 1 {
				continue
			}
			if err := appendApprovalEvent(ctx, tx, approval, eventlog.EventApprovalExpired, "expired", "approval expired", now); err != nil {
				return err
			}
			runID := approval.RunID
			if _, err := resolveIncidentProblem(ctx, tx, approval.IncidentID, "approval_near_expiry", &runID, now); err != nil {
				return err
			}
			expired++
		}

		var nearExpiry []Approval
		if err := tx.WithContext(ctx).
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("status IN ? AND expires_at > ? AND expires_at <= ?", []string{"pending", "approved"}, now, now.Add(approvalNearExpiryWindow)).
			Order("id ASC").Find(&nearExpiry).Error; err != nil {
			return fmt.Errorf("store: lock near-expiry approvals: %w", err)
		}
		for _, approval := range nearExpiry {
			runID := approval.RunID
			if _, err := openIncidentProblem(ctx, tx, IncidentProblem{
				IncidentID:  approval.IncidentID,
				RunID:       &runID,
				Code:        "approval_near_expiry",
				Severity:    "warning",
				Summary:     "approval is nearing expiration",
				FirstSeenAt: now,
				LastSeenAt:  now,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("store: expire approvals: %w", err)
	}
	return expired, nil
}

// ListApprovals 按状态过滤列出审批单，id 倒序稳定排序。
func (db *DB) ListApprovals(ctx context.Context, status string) ([]Approval, error) {
	query := db.WithContext(ctx).Model(&Approval{})
	if status != "" {
		query = query.Where("status = ?", status)
	}
	approvals := make([]Approval, 0)
	if err := query.Order("id DESC").Find(&approvals).Error; err != nil {
		return nil, fmt.Errorf("store: list approvals: %w", err)
	}
	return approvals, nil
}

// ListApprovalsPage 是审批列表的分页入口，缺省 20，最大 100。
func (db *DB) ListApprovalsPage(ctx context.Context, status string, limit int) ([]Approval, error) {
	query := db.WithContext(ctx).Model(&Approval{})
	if status != "" {
		query = query.Where("status = ?", status)
	}
	approvals := make([]Approval, 0)
	if err := query.Order("id DESC").Limit(normalizePageLimit(limit)).Find(&approvals).Error; err != nil {
		return nil, fmt.Errorf("store: list approvals page: %w", err)
	}
	return approvals, nil
}

// NextApprovedApproval 取执行队列队首（id 最小且未过期的 approved 单）。
// 过期 approved 会被 ExpireApprovals 扫走；扫走前也不能堵死队列头。
func (db *DB) NextApprovedApproval(ctx context.Context, now time.Time) (Approval, bool, error) {
	var approval Approval
	err := db.WithContext(ctx).Where("status = ? AND expires_at > ?", "approved", now).Order("id ASC").First(&approval).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Approval{}, false, nil
	}
	if err != nil {
		return Approval{}, false, fmt.Errorf("store: get next approved approval: %w", err)
	}
	return approval, true, nil
}

// ClaimApprovalExecution 把 approved 原子置为 executing，并与 execution.started
// 事实事件同一短事务提交。重复领取或过期审批返回 claimed=false。
func (db *DB) ClaimApprovalExecution(ctx context.Context, id uint64, now time.Time) (claimed bool, err error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var approval Approval
		query := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", id).First(&approval)
		if errors.Is(query.Error, gorm.ErrRecordNotFound) {
			return nil
		}
		if query.Error != nil {
			return fmt.Errorf("store: lock approval for execution: %w", query.Error)
		}
		if approval.Status != "approved" || !approval.ExpiresAt.After(now) {
			return nil
		}
		result := tx.WithContext(ctx).Model(&Approval{}).
			Where("id = ? AND status = ? AND expires_at > ?", id, "approved", now).
			Update("status", "executing")
		if result.Error != nil {
			return fmt.Errorf("store: claim approval execution: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return nil
		}
		runID, approvalID := approval.RunID, approval.ID
		if _, eventErr := appendIncidentEvent(ctx, tx, IncidentEvent{
			IncidentID:  approval.IncidentID,
			RunID:       &runID,
			ApprovalID:  &approvalID,
			EventType:   string(eventlog.EventExecutionStarted),
			Phase:       "execution",
			Status:      "executing",
			Summary:     "approval execution started",
			PayloadJSON: executionStatusPayload("executing"),
			CreatedAt:   now,
		}); eventErr != nil {
			return eventErr
		}
		claimed = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("store: claim approval execution transaction: %w", err)
	}
	return claimed, nil
}

// FinishApprovalExecution 在一笔短事务中完成 executing→executed/failed 的 CAS，
// 保存有界结果，并追加 execution 事件及 execution_failed 问题变更。
func (db *DB) FinishApprovalExecution(ctx context.Context, id uint64, status string, resultJSON []byte) error {
	if status != "executed" && status != "failed" {
		return errors.New("store: approval execution status must be executed or failed")
	}
	now := time.Now().UTC()
	boundedResult := boundedExecutionResult(resultJSON)
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var approval Approval
		query := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", id).First(&approval)
		if errors.Is(query.Error, gorm.ErrRecordNotFound) {
			return fmt.Errorf("store: approval %d not found", id)
		}
		if query.Error != nil {
			return fmt.Errorf("store: lock approval for finish: %w", query.Error)
		}
		if approval.Status != "executing" {
			return fmt.Errorf("store: approval %d is not executing", id)
		}
		updates := map[string]any{"status": status}
		if boundedResult != nil {
			updates["result_json"] = boundedResult
		}
		result := tx.WithContext(ctx).Model(&Approval{}).
			Where("id = ? AND status = ?", id, "executing").Updates(updates)
		if result.Error != nil {
			return fmt.Errorf("store: finish approval execution: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("store: approval %d is not executing", id)
		}
		runID, approvalID := approval.RunID, approval.ID
		eventType := eventlog.EventExecutionCompleted
		summary := "approval execution completed"
		if status == "failed" {
			eventType = eventlog.EventExecutionFailed
			summary = "approval execution failed"
		}
		if _, eventErr := appendIncidentEvent(ctx, tx, IncidentEvent{
			IncidentID:  approval.IncidentID,
			RunID:       &runID,
			ApprovalID:  &approvalID,
			EventType:   string(eventType),
			Phase:       "execution",
			Status:      status,
			Summary:     summary,
			PayloadJSON: executionStatusPayload(status),
			CreatedAt:   now,
		}); eventErr != nil {
			return eventErr
		}
		if status == "failed" {
			if _, problemErr := openIncidentProblem(ctx, tx, IncidentProblem{
				IncidentID:  approval.IncidentID,
				RunID:       &runID,
				Code:        "execution_failed",
				Severity:    "error",
				Status:      "open",
				Summary:     "approval execution failed",
				FirstSeenAt: now,
				LastSeenAt:  now,
			}); problemErr != nil {
				return problemErr
			}
		} else if _, problemErr := resolveIncidentProblem(ctx, tx, approval.IncidentID, "execution_failed", &runID, now); problemErr != nil {
			return problemErr
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("store: finish approval execution transaction: %w", err)
	}
	return nil
}

// RecoverExecutingApprovals 逐行锁定 executing 审批并标记 failed。
// executing 可能已经触达外部系统，恢复阶段绝不自动重放，只写人工核查问题。
func (db *DB) RecoverExecutingApprovals(ctx context.Context, now time.Time) (recovered int64, err error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	interrupted := []byte(`{"error":"executor interrupted; manual verification required","manual_check":true}`)
	b := boundedExecutionResult(interrupted)
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var approvals []Approval
		query := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("status = ?", "executing").Order("id ASC").Find(&approvals)
		if query.Error != nil {
			return fmt.Errorf("store: lock executing approvals: %w", query.Error)
		}
		for _, approval := range approvals {
			result := tx.WithContext(ctx).Model(&Approval{}).
				Where("id = ? AND status = ?", approval.ID, "executing").
				Updates(map[string]any{"status": "failed", "result_json": b})
			if result.Error != nil {
				return fmt.Errorf("store: recover approval %d: %w", approval.ID, result.Error)
			}
			if result.RowsAffected != 1 {
				continue
			}
			runID, approvalID := approval.RunID, approval.ID
			if _, eventErr := appendIncidentEvent(ctx, tx, IncidentEvent{
				IncidentID:  approval.IncidentID,
				RunID:       &runID,
				ApprovalID:  &approvalID,
				EventType:   string(eventlog.EventExecutionFailed),
				Phase:       "execution",
				Status:      "failed",
				Summary:     "approval execution interrupted; manual verification required",
				PayloadJSON: executionManualPayload(),
				CreatedAt:   now,
			}); eventErr != nil {
				return eventErr
			}
			for _, problem := range []IncidentProblem{
				{IncidentID: approval.IncidentID, RunID: &runID, Code: "manual_check", Severity: "critical", Status: "open", Summary: "manual verification required after interrupted execution", FirstSeenAt: now, LastSeenAt: now},
				{IncidentID: approval.IncidentID, RunID: &runID, Code: "execution_failed", Severity: "error", Status: "open", Summary: "approval execution interrupted", FirstSeenAt: now, LastSeenAt: now},
			} {
				if _, problemErr := openIncidentProblem(ctx, tx, problem); problemErr != nil {
					return problemErr
				}
			}
			recovered++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("store: recover executing approvals transaction: %w", err)
	}
	return recovered, nil
}

const maxExecutionResultBytes = 8192

// boundedExecutionResult keeps result_json valid JSON and below the audit budget.
// Oversized or malformed external output is represented by a fixed safe marker.
func boundedExecutionResult(result []byte) datatypes.JSON {
	if len(result) == 0 {
		return nil
	}
	if len(result) <= maxExecutionResultBytes && json.Valid(result) {
		return datatypes.JSON(append([]byte(nil), result...))
	}
	return datatypes.JSON([]byte(`{"truncated":true,"reason":"execution result omitted"}`))
}

func executionStatusPayload(status string) *datatypes.JSON {
	payload := datatypes.JSON([]byte(fmt.Sprintf(`{"status":%q}`, status)))
	return &payload
}

func executionManualPayload() *datatypes.JSON {
	payload := datatypes.JSON([]byte(`{"manual_check":true}`))
	return &payload
}

// CountRecentExecutions 数同一 plan_hash（同 tool + 同 target）在 since 之后
// 进入过执行的审批单。L2 限频护栏用它判断"这个动作最近做过几次"。
// 统计包含 failed：反复失败的重复动作正是限频要拦的（配置错/凭据错重启无救）。
func (db *DB) CountRecentExecutions(ctx context.Context, planHash string, since time.Time) (int, error) {
	if strings.TrimSpace(planHash) == "" {
		return 0, errors.New("store: plan hash is required")
	}
	var count int64
	err := db.WithContext(ctx).Model(&Approval{}).
		Where("plan_hash = ? AND status IN ? AND created_at >= ?",
			planHash, []string{"executing", "executed", "failed"}, since).
		Count(&count).Error
	if err != nil {
		return 0, fmt.Errorf("store: count recent executions: %w", err)
	}
	return int(count), nil
}

// InsertFaultCmdHistory 记录一次已审批动作的执行结果（D13 的诊断注入源）。
func (db *DB) InsertFaultCmdHistory(ctx context.Context, row FaultCmdHistory) error {
	if row.Fingerprint == "" || row.ToolName == "" {
		return errors.New("store: cmd history fingerprint and tool are required")
	}
	if row.CreatedAt.IsZero() {
		return errors.New("store: cmd history time is required")
	}
	if err := db.WithContext(ctx).Create(&row).Error; err != nil {
		return fmt.Errorf("store: insert cmd history: %w", err)
	}
	return nil
}

// GetAgentRun 按 id 取 run。重诊注入上一轮失败上下文用。
func (db *DB) GetAgentRun(ctx context.Context, id uint64) (AgentRun, error) {
	var run AgentRun
	err := db.WithContext(ctx).First(&run, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return AgentRun{}, ErrAgentRunNotFound
	}
	if err != nil {
		return AgentRun{}, fmt.Errorf("store: get agent run: %w", err)
	}
	return run, nil
}

// CountIncidentRuns 数 incident 的全部 run，重诊链长度判定用。
func (db *DB) CountIncidentRuns(ctx context.Context, incidentID uint64) (int64, error) {
	var count int64
	if err := db.WithContext(ctx).Model(&AgentRun{}).Where("incident_id = ?", incidentID).Count(&count).Error; err != nil {
		return 0, fmt.Errorf("store: count incident runs: %w", err)
	}
	return count, nil
}

// ListIncidentRunIDs 按 id 升序列出 incident 的全部 run id（升级通知的 run 链）。
func (db *DB) ListIncidentRunIDs(ctx context.Context, incidentID uint64) ([]uint64, error) {
	var ids []uint64
	err := db.WithContext(ctx).Model(&AgentRun{}).Where("incident_id = ?", incidentID).Order("id ASC").Pluck("id", &ids).Error
	if err != nil {
		return nil, fmt.Errorf("store: list incident run ids: %w", err)
	}
	return ids, nil
}

// ListRunSteps 按 seq 升序取 run 的全部审计步（重诊注入上一轮 verify 结论用）。
func (db *DB) ListRunSteps(ctx context.Context, runID uint64) ([]AgentRunStep, error) {
	steps := make([]AgentRunStep, 0)
	err := db.WithContext(ctx).Where("run_id = ?", runID).Order("seq ASC, id ASC").Find(&steps).Error
	if err != nil {
		return nil, fmt.Errorf("store: list run steps: %w", err)
	}
	return steps, nil
}
func (db *DB) ListRunStepsForIncident(ctx context.Context, incidentID, runID, afterID uint64, limit int) ([]AgentRunStep, error) {
	if incidentID == 0 || runID == 0 {
		return nil, ErrAgentRunNotFound
	}
	run, err := db.GetAgentRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if run.IncidentID != incidentID {
		return nil, ErrAgentRunNotFound
	}
	steps := make([]AgentRunStep, 0)
	err = db.WithContext(ctx).Where("run_id = ? AND id > ?", runID, afterID).Order("id ASC").Limit(normalizePageLimit(limit)).Find(&steps).Error
	if err != nil {
		return nil, fmt.Errorf("store: list incident run steps: %w", err)
	}
	return steps, nil
}

// ListIncidentRunSteps is a descriptive alias for ListRunStepsForIncident.
func (db *DB) ListIncidentRunSteps(ctx context.Context, incidentID, runID, afterID uint64, limit int) ([]AgentRunStep, error) {
	return db.ListRunStepsForIncident(ctx, incidentID, runID, afterID, limit)
}

// ErrMemoryNotFound 是故障记忆未命中的哨兵错误。
var ErrMemoryNotFound = errors.New("store: fault memory not found")

// GetFaultMemory 按键取记忆条目。
func (db *DB) GetFaultMemory(ctx context.Context, fingerprint string) (FaultMemory, error) {
	var entry FaultMemory
	err := db.WithContext(ctx).First(&entry, "fingerprint = ?", fingerprint).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return FaultMemory{}, ErrMemoryNotFound
	}
	if err != nil {
		return FaultMemory{}, fmt.Errorf("store: get fault memory: %w", err)
	}
	return entry, nil
}

// UpsertFaultMemory 写入或覆盖记忆条目（同指纹复诊成功刷新内容）。
func (db *DB) UpsertFaultMemory(ctx context.Context, entry FaultMemory) error {
	if entry.Fingerprint == "" {
		return errors.New("store: fault memory fingerprint is required")
	}
	err := db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "fingerprint"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"group_key", "alert_name", "rca_text", "plan_json", "confidence", "last_success", "ttl_sec",
		}),
	}).Create(&entry).Error
	if err != nil {
		return fmt.Errorf("store: upsert fault memory: %w", err)
	}
	return nil
}

// TouchFaultMemory 命中计数：hits+1、刷新 last_used。安全等级不变。
func (db *DB) TouchFaultMemory(ctx context.Context, fingerprint string, now time.Time) error {
	result := db.WithContext(ctx).Model(&FaultMemory{}).Where("fingerprint = ?", fingerprint).
		Updates(map[string]any{"hits": gorm.Expr("hits + 1"), "last_used": now})
	if result.Error != nil {
		return fmt.Errorf("store: touch fault memory: %w", result.Error)
	}
	return nil
}

// DemoteFaultMemory 把置信度降为 low（验证失败拉黑）。last_used 同时刷新，
// 让"这条记忆坑过人"在审计里可见。
func (db *DB) DemoteFaultMemory(ctx context.Context, fingerprint string, now time.Time) error {
	result := db.WithContext(ctx).Model(&FaultMemory{}).Where("fingerprint = ?", fingerprint).
		Updates(map[string]any{"confidence": "low", "last_used": now})
	if result.Error != nil {
		return fmt.Errorf("store: demote fault memory: %w", result.Error)
	}
	return nil
}

// ListCmdHistory 按时间倒序取同指纹的最近命令历史。
func (db *DB) ListCmdHistory(ctx context.Context, fingerprint string, limit int) ([]FaultCmdHistory, error) {
	if limit <= 0 {
		limit = 5
	}
	rows := make([]FaultCmdHistory, 0)
	err := db.WithContext(ctx).Where("fingerprint = ?", fingerprint).
		Order("id DESC").Limit(limit).Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("store: list cmd history: %w", err)
	}
	return rows, nil
}

// UpdateAgentRunMode 在运行中改 mode（记忆命中后 full/light → memory_hit）。
// 只允许 running 状态改：终态行的 mode 是审计记录，不可改写。
func (db *DB) UpdateAgentRunMode(ctx context.Context, id uint64, mode string) error {
	result := db.WithContext(ctx).Model(&AgentRun{}).
		Where("id = ? AND status IN ?", id, []string{"pending", "running"}).
		Update("mode", mode)
	if result.Error != nil {
		return fmt.Errorf("store: update agent run mode: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("store: agent run %d is not pending/running", id)
	}
	return nil
}

// CountRawEventsByStatus 统计 raw_event 状态行数（/metrics 队列深度用）。
func (db *DB) CountRawEventsByStatus(ctx context.Context, status string) (int64, error) {
	var count int64
	if err := db.WithContext(ctx).Model(&RawEvent{}).Where("status = ?", status).Count(&count).Error; err != nil {
		return 0, fmt.Errorf("store: count raw events: %w", err)
	}
	return count, nil
}

// CountAgentRunsByStatus 统计 agent_run 状态行数。
func (db *DB) CountAgentRunsByStatus(ctx context.Context, status string) (int64, error) {
	var count int64
	if err := db.WithContext(ctx).Model(&AgentRun{}).Where("status = ?", status).Count(&count).Error; err != nil {
		return 0, fmt.Errorf("store: count agent runs: %w", err)
	}
	return count, nil
}

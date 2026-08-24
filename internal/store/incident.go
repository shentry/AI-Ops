package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// incident 归并事务与只读查询。
// transactionIncidentTx 是 ApplyRawEvent 交给 ingest hook 的窄事务面：
// hook 只能碰归并相关的写，碰不到审批和执行。

// ErrIncidentNotFound 是"incident 不存在"的哨兵错误。查询 API 用 errors.Is
// 把它映射成 404，和真正的存储故障（503）区分开。
var ErrIncidentNotFound = errors.New("store: incident not found")

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

type IncidentTx interface {
	AssignIncident(context.Context, IncidentInput, time.Duration, int) (IncidentAssignment, error)
	TouchIncident(context.Context, uint64, time.Time, int) error
	// D05：resolved 传播。全部成员 resolved 时把 incident 关单，返回是否本次关闭。
	ResolveIncident(context.Context, uint64, time.Time) (bool, error)
	// D05：促发分流。在促发同一事务里落 agent_run 队列行，保证跨表原子性。
	// EnqueueAgentRun 落一行 agent_run 并回填自增 ID，调用方据此在同一事务
	// 里追加 run.queued 事件。
	EnqueueAgentRun(context.Context, *AgentRun) error
	EventProblemWriter
}

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
// 成功后 run.ID 被回填，调用方可用它挂同事务的 run.queued 事件。
func (t *transactionIncidentTx) EnqueueAgentRun(ctx context.Context, run *AgentRun) error {
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

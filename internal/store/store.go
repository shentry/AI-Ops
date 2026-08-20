package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrInvalidRawEvent 是"报文不是合法 JSON"的哨兵错误。HTTP 层用 errors.Is
// 区分输入问题和存储故障：前者回 400 怪调用方，后者回 503 怪自己。
var ErrInvalidRawEvent = errors.New("store: raw event payload must be valid JSON")

// ErrIncidentNotFound 是"incident 不存在"的哨兵错误。查询 API 用 errors.Is
// 把它映射成 404，和真正的存储故障（503）区分开。
var ErrIncidentNotFound = errors.New("store: incident not found")

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

// IncidentTx 是事务内对 incident 的全部操作面：
// 关联器和生命周期 hook 在 ApplyRawEvent 事务里能做的就这么多，
// 做不了任意的库访问。和 ingest.pendingEventStore 同一手法 ——
// 接口收窄，单测换假实现即可。
type IncidentTx interface {
	AssignIncident(context.Context, IncidentInput, time.Duration, int) (IncidentAssignment, error)
	TouchIncident(context.Context, uint64, time.Time, int) error
	// D05：resolved 传播。全部成员 resolved 时把 incident 关单，返回是否本次关闭。
	ResolveIncident(context.Context, uint64, time.Time) (bool, error)
	// D05：促发分流。在促发同一事务里落 agent_run 队列行，保证跨表原子性。
	EnqueueAgentRun(context.Context, AgentRun) error
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
	if run.IncidentID == 0 {
		return errors.New("store: agent run incident is required")
	}
	if run.Mode == "" || run.Status == "" {
		return errors.New("store: agent run mode and status are required")
	}
	if run.StartedAt.IsZero() {
		return errors.New("store: agent run start time is required")
	}
	if err := t.db.WithContext(ctx).Create(&run).Error; err != nil {
		return fmt.Errorf("store: enqueue agent run: %w", err)
	}
	return nil
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

// CreateAgentRun 是手动重诊的落库点：非事务路径，直接插一行 pending run。
// 与 EnqueueAgentRun 的区别只在调用场景 —— 那里挂在促发事务里，
// 这里是 API 入口的独立写入。
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
	if err := db.WithContext(ctx).Create(&run).Error; err != nil {
		return AgentRun{}, fmt.Errorf("store: create agent run: %w", err)
	}
	return run, nil
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

// ClaimAgentRun 把 pending run 原子置为 running。WHERE 带状态条件：
// 并发消费者或重启补账不会重复认领同一行。RowsAffected=0 表示被别人抢先。
func (db *DB) ClaimAgentRun(ctx context.Context, id uint64, startedAt time.Time) (bool, error) {
	result := db.WithContext(ctx).Model(&AgentRun{}).
		Where("id = ? AND status = ?", id, "pending").
		Updates(map[string]any{"status": "running", "started_at": startedAt})
	if result.Error != nil {
		return false, fmt.Errorf("store: claim agent run: %w", result.Error)
	}
	return result.RowsAffected > 0, nil
}

// RequeueStaleAgentRuns 把超时的 running run 放回 pending（进程死在中途的
// 补账语义，与 raw_event 的 pending 重扫对齐）。返回重置行数。
func (db *DB) RequeueStaleAgentRuns(ctx context.Context, staleBefore time.Time) (int64, error) {
	result := db.WithContext(ctx).Model(&AgentRun{}).
		Where("status = ? AND started_at < ?", "running", staleBefore).
		Update("status", "pending")
	if result.Error != nil {
		return 0, fmt.Errorf("store: requeue stale agent runs: %w", result.Error)
	}
	return result.RowsAffected, nil
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

// ErrApprovalNotFound / ErrApprovalConflict 是审批决策的哨兵错误：
// 404 与 409 的 HTTP 映射靠它们。
var ErrApprovalNotFound = errors.New("store: approval not found")
var ErrApprovalConflict = errors.New("store: approval already decided")

// CreateApproval 落一条 pending 审批单。绑定 incident/run/tool/args/plan_hash
// 与过期时间 —— 审批通过的是"这份计划"，不是某个模糊意图（GC-13）。
func (db *DB) CreateApproval(ctx context.Context, approval Approval) (Approval, error) {
	if approval.IncidentID == 0 || approval.RunID == 0 {
		return Approval{}, errors.New("store: approval incident and run are required")
	}
	if approval.ToolName == "" || len(approval.ArgsJSON) == 0 || approval.PlanHash == "" {
		return Approval{}, errors.New("store: approval tool, args and plan_hash are required")
	}
	if approval.ExpiresAt.IsZero() || approval.CreatedAt.IsZero() {
		return Approval{}, errors.New("store: approval times are required")
	}
	approval.Status = "pending"
	if err := db.WithContext(ctx).Create(&approval).Error; err != nil {
		return Approval{}, fmt.Errorf("store: create approval: %w", err)
	}
	return approval, nil
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

// DecideApproval 把 pending 审批单置为终态（approved/denied）。
// WHERE 同时带状态和过期时间：重复决策返回 ErrApprovalConflict，
// 过期单不能再被批准。
func (db *DB) DecideApproval(ctx context.Context, id uint64, status, decidedBy string, now time.Time) error {
	if status != "approved" && status != "denied" {
		return errors.New("store: approval decision must be approved or denied")
	}
	if decidedBy == "" {
		return errors.New("store: approval decider is required")
	}
	result := db.WithContext(ctx).Model(&Approval{}).
		Where("id = ? AND status = ? AND expires_at > ?", id, "pending", now).
		Updates(map[string]any{"status": status, "decided_by": decidedBy})
	if result.Error != nil {
		return fmt.Errorf("store: decide approval: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		// 区分"不存在"和"已决/已过期"：前者 404，后者 409。
		var existing Approval
		findErr := db.WithContext(ctx).First(&existing, id).Error
		if errors.Is(findErr, gorm.ErrRecordNotFound) {
			return ErrApprovalNotFound
		}
		return ErrApprovalConflict
	}
	return nil
}

// ExpireApprovals 把过期的 pending/approved 单批量置为 expired。
// approved 也会过期：批准了但一直没执行的动作不该永久有效（GC-13）。
// executing/终态不动。返回过期行数。
func (db *DB) ExpireApprovals(ctx context.Context, now time.Time) (int64, error) {
	result := db.WithContext(ctx).Model(&Approval{}).
		Where("status IN ? AND expires_at <= ?", []string{"pending", "approved"}, now).
		Update("status", "expired")
	if result.Error != nil {
		return 0, fmt.Errorf("store: expire approvals: %w", result.Error)
	}
	return result.RowsAffected, nil
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

// ClaimApprovalExecution 把 approved 原子置为 executing。
// 重复领取返回 claimed=false —— 同一审批单不会产生两次执行（验收清单）。
// 过期单不许进入执行：approved 的 TTL 语义和 pending 一样硬（GC-13）。
func (db *DB) ClaimApprovalExecution(ctx context.Context, id uint64, now time.Time) (bool, error) {
	result := db.WithContext(ctx).Model(&Approval{}).
		Where("id = ? AND status = ? AND expires_at > ?", id, "approved", now).
		Update("status", "executing")
	if result.Error != nil {
		return false, fmt.Errorf("store: claim approval execution: %w", result.Error)
	}
	return result.RowsAffected > 0, nil
}

// FinishApprovalExecution 写回执行结果：executed/failed + result_json。
// 只允许 executing → 终态，终态不可覆盖。
func (db *DB) FinishApprovalExecution(ctx context.Context, id uint64, status string, resultJSON []byte) error {
	if status != "executed" && status != "failed" {
		return errors.New("store: approval execution status must be executed or failed")
	}
	updates := map[string]any{"status": status}
	if len(resultJSON) > 0 {
		updates["result_json"] = datatypes.JSON(resultJSON)
	}
	result := db.WithContext(ctx).Model(&Approval{}).
		Where("id = ? AND status = ?", id, "executing").
		Updates(updates)
	if result.Error != nil {
		return fmt.Errorf("store: finish approval execution: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("store: approval %d is not executing", id)
	}
	return nil
}

// RecoverExecutingApprovals 将进程重启前遗留的 executing 审批标记为 failed。
// executing 表示动作可能已经触达外部系统，不能自动重放；失败原因要求人工核查。
func (db *DB) RecoverExecutingApprovals(ctx context.Context, now time.Time) (int64, error) {
	result := db.WithContext(ctx).Model(&Approval{}).
		Where("status = ?", "executing").
		Updates(map[string]any{"status": "failed", "result_json": datatypes.JSON([]byte(`{"error":"executor interrupted; manual verification required"}`))})
	if result.Error != nil {
		return 0, fmt.Errorf("store: recover executing approvals: %w", result.Error)
	}
	return result.RowsAffected, nil
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
		return AgentRun{}, fmt.Errorf("store: agent run %d not found", id)
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

// HasActiveRun 判断 incident 是否已有 pending/running 的 run。
// 同一 incident 的重诊不能并发（D12）——创建前查这个。
func (db *DB) HasActiveRun(ctx context.Context, incidentID uint64) (bool, error) {
	var count int64
	if err := db.WithContext(ctx).Model(&AgentRun{}).
		Where("incident_id = ? AND status IN ?", incidentID, []string{"pending", "running"}).
		Count(&count).Error; err != nil {
		return false, fmt.Errorf("store: count active runs: %w", err)
	}
	return count > 0, nil
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

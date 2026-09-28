package incident

import (
	"errors"
	"fmt"
	"time"
)

// 归并规则（原 store.AssignIncident 的判定部分）。
//
// AssignIncident 的写库动作夹在行锁之间，搬不走；能搬走的是「锁到什么状态 +
// 输入是什么 → 该写成什么」这段判断。拆出来之后它是纯函数，
// 不需要真 MySQL 就能覆盖时间窗、severity 只升不降、promote 时机这些规则。

// MergeInput 是关联器视角的一条 firing 告警：只带分组要用的字段。
type MergeInput struct {
	GroupKey    string
	Fingerprint string
	Name        string
	Severity    int
	ObservedAt  time.Time
}

// Validate 检查归并的硬前提。身份三要素缺了宁可报错也不猜 ——
// 猜错一个 group_key 就会把不相干的告警并进同一个 incident。
func (in MergeInput) Validate(window time.Duration, minAlerts int) error {
	if in.GroupKey == "" || in.Fingerprint == "" || in.Name == "" {
		return errors.New("incident: identity is required")
	}
	if in.ObservedAt.IsZero() {
		return errors.New("incident: observed time is required")
	}
	if window <= 0 {
		return errors.New("incident: window must be positive")
	}
	if minAlerts < 1 {
		return errors.New("incident: minimum alerts must be at least 1")
	}
	return nil
}

// Title 是新建 incident 的标题。
func (in MergeInput) Title() string {
	return fmt.Sprintf("%s: %s", in.GroupKey, in.Name)
}

// Cutoff 是时间窗下界：只有此刻之后仍有活动的 incident 才算「活着」。
// 过了时间窗的不再吸收新告警 —— 上次故障和这次复发是两个 incident，
// 不能缝在一起。
func (in MergeInput) Cutoff(window time.Duration) time.Time {
	return in.ObservedAt.UTC().Add(-window)
}

// State 是行锁读到的 incident 当前状态，归并规则的输入之一。
type State struct {
	Status      string
	Severity    int
	AlertsCount int
	LastSeenAt  time.Time
}

// NewCandidate 是新建 incident 的初始状态：单条告警不足以下结论，
// 先落 candidate，攒够 minAlerts 才升 firing，避免一条孤立告警
// 就制造一次故障。AlertsCount 从 0 起 —— 成员挂载成功后才计数。
func NewCandidate(in MergeInput) State {
	return State{
		Status:      StatusCandidate,
		Severity:    in.Severity,
		AlertsCount: 0,
		LastSeenAt:  in.ObservedAt.UTC(),
	}
}

// Merge 把一条告警并进已锁定的 incident，算出该写回的新状态。
//
// isNewMember 来自成员表的 ON CONFLICT DO NOTHING 结果：同一指纹反复
// firing 不能把 alerts_count 刷高，只有真正的新成员才计数。
//
// 三条规则：
//   - severity 只升不降 —— 否则后到的一条 info 会把 critical 的故障「降级」；
//   - last_seen_at 只前进不后退 —— 重放积压时告警可能乱序到达；
//   - candidate → firing 只在成员数够 minAlerts 的那一刻发生。
func Merge(current State, in MergeInput, isNewMember bool, minAlerts int) State {
	next := current
	// AssignIncident 入口已经通过 Validate；纯函数本身仍拒绝非法阈值，
	// 避免未来绕过入口时把 candidate 意外升级成 firing。
	if minAlerts < 1 {
		return next
	}
	if isNewMember {
		next.AlertsCount++
	}
	if in.Severity > next.Severity {
		next.Severity = in.Severity
	}
	observedAt := in.ObservedAt.UTC()
	if observedAt.After(next.LastSeenAt) {
		next.LastSeenAt = observedAt
	}
	if next.Status == StatusCandidate && next.AlertsCount >= minAlerts {
		next.Status = StatusFiring
	}
	return next
}

// Promoted 判断本次归并是否恰好把 incident 从 candidate 升成 firing。
// worker 靠它只在升级瞬间做一次促发动作（落诊断队列），后续普通挂载不重复 ——
// 所以判据必须是「进来时是 candidate 且现在是 firing」这个状态跃迁，
// 而不是「现在是 firing」。
func Promoted(before, after State) bool {
	return before.Status == StatusCandidate && after.Status == StatusFiring
}

// Heartbeat 是内容没变的重复推送要做的最小推进：last_seen_at 续上
// （时间窗在告警持续 firing 期间不该过期），severity 顺手只升不降。
// 非 open 状态不动 —— 返回 changed=false 让调用方跳过写库。
func Heartbeat(current State, observedAt time.Time, severity int) (next State, changed bool) {
	if !IsOpen(current.Status) {
		return current, false
	}
	next = current
	observedAt = observedAt.UTC()
	if observedAt.After(next.LastSeenAt) {
		next.LastSeenAt = observedAt
	}
	if severity > next.Severity {
		next.Severity = severity
	}
	return next, true
}

// CanResolve 判断一条 resolved 告警是否该让 incident 关单。
// resolve_on=ALL：还有任何一个成员没 resolved，incident 就保持开放。
// 已关单或被人工接管的不重复处理 —— 重放 resolved 事件不能把
// resolved_at 改来改去。
func CanResolve(status string, unresolvedMembers int64) bool {
	return IsOpen(status) && unresolvedMembers == 0
}

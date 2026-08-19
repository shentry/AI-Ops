package diagnose

import (
	"context"

	"oncall-agent/internal/store"
)

// incidentReader 是 EvidenceBuilder 对存储层的收窄接口。
type incidentReader interface {
	GetIncident(ctx context.Context, id uint64) (store.Incident, error)
	ListIncidentMembers(ctx context.Context, incidentID uint64) ([]store.IncidentMember, error)
	ListIncidentAlerts(ctx context.Context, incidentID uint64) ([]store.Alert, error)
}

// EvidenceBuilder 装配 Target 并按固定顺序执行所有 collector。
// collector 顺序在构造时固定 —— Render 的稳定性从这儿开始。
type EvidenceBuilder struct {
	db         incidentReader
	collectors []Collector
}

func NewEvidenceBuilder(db incidentReader, collectors []Collector) *EvidenceBuilder {
	return &EvidenceBuilder{db: db, collectors: collectors}
}

// LoadTarget 只装配 incident 上下文，不跑 collector。
// D13 的记忆查找只需要 group_key 和成员告警名，不该为查记忆先采全量证据。
func (b *EvidenceBuilder) LoadTarget(ctx context.Context, incidentID uint64) (Target, error) {
	incident, err := b.db.GetIncident(ctx, incidentID)
	if err != nil {
		return Target{}, err
	}
	members, err := b.db.ListIncidentMembers(ctx, incidentID)
	if err != nil {
		return Target{}, err
	}
	alerts, err := b.db.ListIncidentAlerts(ctx, incidentID)
	if err != nil {
		return Target{}, err
	}
	return Target{Incident: incident, Members: members, Alerts: alerts}, nil
}

// BuildForIncident 从库装配 incident 上下文，然后采集全部证据。
// incident 不存在是唯一会返回 error 的情况 —— 那是调用方的问题（404），
// 单个 collector 的失败只体现在 EvidenceItem 里。
func (b *EvidenceBuilder) BuildForIncident(ctx context.Context, incidentID uint64) (Evidence, error) {
	target, err := b.LoadTarget(ctx, incidentID)
	if err != nil {
		return Evidence{}, err
	}
	return BuildEvidence(ctx, b.collectors, target), nil
}

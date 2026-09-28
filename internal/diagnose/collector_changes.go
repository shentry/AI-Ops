package diagnose

import (
	"context"
	"fmt"
	"strings"
	"time"

	"oncall-agent/internal/config"
	"oncall-agent/internal/store"
)

// changeLookback 是变更回看窗口：告警前 60 分钟内的发布才可能是版本回归的起点。
const changeLookback = 60 * time.Minute

type changeReader interface {
	ListChanges(ctx context.Context, service string, since time.Time, limit int) ([]store.ChangeEvent, error)
	ListReleases(ctx context.Context, service string, limit int) ([]store.ChangeEvent, error)
}

// recentChangesCollector 采集发布记录：告警前后的变更，以及可作为回退目标的
// 历史发布（带发布 ID、镜像摘要、迁移声明和是否验证过）。当前发布是最新的
// 发布或回退记录，它的 ID 就是回退动作核对的对象身份。
type recentChangesCollector struct {
	db      changeReader
	service string
}

func NewRecentChangesCollector(db changeReader, service config.ServiceConfig) Collector {
	return &recentChangesCollector{db: db, service: service.Name}
}

func (*recentChangesCollector) Name() string { return "recent_changes" }

func (c *recentChangesCollector) Collect(ctx context.Context, target Target) EvidenceItem {
	const source = "store:change_event"
	start := target.Incident.StartedAt.UTC()
	changes, err := c.db.ListChanges(ctx, c.service, start.Add(-changeLookback), 20)
	if err != nil {
		return finishItem(c.Name(), source, "", err)
	}
	releases, err := c.db.ListReleases(ctx, c.service, 10)
	if err != nil {
		return finishItem(c.Name(), source, "", err)
	}
	var body strings.Builder
	facts := &ReleaseFacts{}
	fmt.Fprintf(&body, "changes since %s (newest first):\n", start.Add(-changeLookback).Format(time.RFC3339))
	for _, change := range changes {
		if change.OccurredAt.Before(start) {
			facts.Changes++
		}
		fmt.Fprintf(&body, "- %s %s release=%s image=%s migration=%s source=%s actor=%s\n", change.OccurredAt.UTC().Format(time.RFC3339),
			change.ChangeType, deref(change.ReleaseID), deref(change.ImageRef), change.DBMigration, change.Source, change.Actor)
	}
	body.WriteString("releases that ran (newest first; rollback targets must be verified):\n")
	for _, release := range releases {
		verified := "no"
		if release.VerifiedAt != nil {
			verified = release.VerifiedAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(&body, "- release_id=%s %s at %s image=%s migration=%s verified=%s\n", deref(release.ReleaseID), release.ChangeType,
			release.OccurredAt.UTC().Format(time.RFC3339), deref(release.ImageRef), release.DBMigration, verified)
	}
	item := finishItem(c.Name(), source, body.String(), nil)
	if len(releases) > 0 {
		current := releases[0]
		facts.CurrentID, facts.CurrentAt, facts.CurrentMigration = deref(current.ReleaseID), current.OccurredAt.UTC(), current.DBMigration
		facts.DeployedBeforeFault = !current.OccurredAt.After(start) && !current.OccurredAt.Before(start.Add(-changeLookback))
		item.Object = &ObjectRef{Kind: "service", Name: c.service, ID: facts.CurrentID}
	}
	item.Release = facts
	return item
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

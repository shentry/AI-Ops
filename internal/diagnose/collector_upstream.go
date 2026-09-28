package diagnose

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"oncall-agent/internal/sub2api"
)

type upstreamReader interface {
	Availability(ctx context.Context) (sub2api.Availability, error)
	UpstreamErrors(ctx context.Context, window string, pageSize int) (sub2api.UpstreamErrors, error)
}

// upstreamAccountsCollector 读取 sub2api 的账号调度状态和最近 5 分钟按账号
// 归属的上游错误。只调用只读 ops 接口；两项都读到才是完整事实。
type upstreamAccountsCollector struct {
	ops upstreamReader
	now func() time.Time
}

// NewUpstreamAccountsCollector takes nil when no admin key is configured.
func NewUpstreamAccountsCollector(ops upstreamReader) Collector {
	return &upstreamAccountsCollector{ops: ops, now: time.Now}
}

func (*upstreamAccountsCollector) Name() string { return "upstream_accounts" }

func (c *upstreamAccountsCollector) Collect(ctx context.Context, _ Target) EvidenceItem {
	const source = "sub2api:ops"
	if c.ops == nil {
		return missingItem(c.Name(), source, "service admin_api_key is not configured")
	}
	availability, err := c.ops.Availability(ctx)
	if err != nil {
		return finishItem(c.Name(), source, "", err)
	}
	errs, err := c.ops.UpstreamErrors(ctx, "5m", 500)
	if err != nil {
		return finishItem(c.Name(), source, "", err)
	}
	counts := map[int64]int{}
	for _, item := range errs.Items {
		if item.AccountID != nil {
			counts[*item.AccountID]++
		}
	}
	facts := &UpstreamFacts{RealtimeEnabled: availability.Enabled, Sampled: errs.Total > int64(len(errs.Items)), TotalErrors: int(errs.Total)}
	now := c.now()
	for _, account := range availability.Accounts {
		facts.Accounts = append(facts.Accounts, UpstreamAccountFacts{ID: account.AccountID, GroupID: account.GroupID, Available: account.IsAvailable,
			TempUnschedulable: account.TempUnschedulableUntil != nil && now.Before(*account.TempUnschedulableUntil), Errors: counts[account.AccountID]})
	}
	sort.Slice(facts.Accounts, func(i, j int) bool {
		a, b := facts.Accounts[i], facts.Accounts[j]
		return a.Errors > b.Errors || (a.Errors == b.Errors && a.ID < b.ID)
	})
	var body strings.Builder
	fmt.Fprintf(&body, "upstream errors in the last 5m: %d (sampled=%v); realtime availability enabled=%v\n", errs.Total, facts.Sampled, availability.Enabled)
	groups := make([]sub2api.GroupAvailability, 0, len(availability.Groups))
	for _, group := range availability.Groups {
		groups = append(groups, group)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].GroupID < groups[j].GroupID })
	for _, group := range groups {
		fmt.Fprintf(&body, "group %d %q: total=%d available=%d rate_limited=%d error=%d\n", group.GroupID, group.GroupName, group.TotalAccounts, group.AvailableCount, group.RateLimitCount, group.ErrorCount)
	}
	item := finishItem(c.Name(), source, body.String(), nil)
	item.Upstream = facts
	return item
}

package diagnose

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"oncall-agent/internal/config"
)

// 固定只读查询，不拼接任何运行时输入（命令白名单原则）。
const (
	pgProbeQuery   = `SELECT 1`
	pgActivityStat = `SELECT count(*) AS total, count(*) FILTER (WHERE wait_event IS NOT NULL) AS waiting FROM pg_stat_activity`
)

// pgPools 进程级复用连接池：同一 DSN 只建一次。
var pgPools sync.Map // dsn -> *pgxpool.Pool

func pgPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	if pooled, ok := pgPools.Load(dsn); ok {
		return pooled.(*pgxpool.Pool), nil
	}
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse postgres dsn: %w", err)
	}
	poolCfg.MaxConns = 2 // 只读证据采集，两条连接足够，别压被监控库
	poolCfg.ConnConfig.ConnectTimeout = 5 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	actual, _ := pgPools.LoadOrStore(dsn, pool)
	return actual.(*pgxpool.Pool), nil
}

// postgresCollector 采集 Sub2API 的 PostgreSQL 依赖证据：
// 连通性、连接数、等待中的会话数。DSN 未配置时记 missing。
// DSN 里的密码只用于建连，正文只落统计数字，错误经 finishItem 脱敏。
type postgresCollector struct {
	dsn     string
	timeout time.Duration
}

func NewPostgresCollector(service config.ServiceConfig, evidence config.EvidenceConfig) Collector {
	return &postgresCollector{dsn: service.PostgresDSN, timeout: time.Duration(evidence.TimeoutSeconds) * time.Second}
}

func (*postgresCollector) Name() string { return "postgres" }

func (c *postgresCollector) Collect(ctx context.Context, _ Target) EvidenceItem {
	dsn := strings.TrimSpace(c.dsn)
	if dsn == "" {
		return missingItem(c.Name(), "postgres:pg_stat_activity", "postgres dsn is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	pool, err := pgPool(ctx, dsn)
	if err != nil {
		return finishItem(c.Name(), "postgres:pg_stat_activity", "", err)
	}
	var probe int
	if err := pool.QueryRow(ctx, pgProbeQuery).Scan(&probe); err != nil {
		return finishItem(c.Name(), "postgres:pg_stat_activity", "", fmt.Errorf("probe failed: %w", err))
	}
	var total, waiting int
	if err := pool.QueryRow(ctx, pgActivityStat).Scan(&total, &waiting); err != nil {
		return finishItem(c.Name(), "postgres:pg_stat_activity", "", fmt.Errorf("activity stat failed: %w", err))
	}
	body := fmt.Sprintf("connectivity: ok\ntotal_connections: %d\nwaiting_connections: %d\n", total, waiting)
	return finishItem(c.Name(), "postgres:pg_stat_activity", body, nil)
}

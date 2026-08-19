package diagnose

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"oncall-agent/internal/config"
)

// redisClients 进程级复用：同一地址只建一次客户端。
var redisClients sync.Map // addr -> *redis.Client

func redisClient(addr string) *redis.Client {
	if existing, ok := redisClients.Load(addr); ok {
		return existing.(*redis.Client)
	}
	client := redis.NewClient(&redis.Options{
		Addr:         addr,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	})
	actual, _ := redisClients.LoadOrStore(addr, client)
	return actual.(*redis.Client)
}

// redisCollector 采集 Sub2API 的 Redis 依赖证据：PING 延迟、内存占用、
// 连接数。只用 PING/INFO 只读命令（命令白名单），addr 未配置时记 missing。
type redisCollector struct {
	cfg config.EvidenceConfig
}

func NewRedisCollector(cfg config.EvidenceConfig) Collector {
	return &redisCollector{cfg: cfg}
}

func (*redisCollector) Name() string { return "redis" }

func (c *redisCollector) Collect(ctx context.Context, _ Target) EvidenceItem {
	addr := strings.TrimSpace(c.cfg.RedisAddr)
	if addr == "" {
		return missingItem(c.Name(), "redis:PING/INFO", "redis addr is not configured")
	}
	timeout := time.Duration(c.cfg.TimeoutSeconds) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := redisClient(addr)

	started := time.Now()
	if err := client.Ping(ctx).Err(); err != nil {
		return finishItem(c.Name(), "redis:PING/INFO", "", fmt.Errorf("ping failed: %w", err))
	}
	latency := time.Since(started)

	memory, err := client.Info(ctx, "memory").Result()
	if err != nil {
		return finishItem(c.Name(), "redis:PING/INFO", "", fmt.Errorf("info memory failed: %w", err))
	}
	clients, err := client.Info(ctx, "clients").Result()
	if err != nil {
		return finishItem(c.Name(), "redis:PING/INFO", "", fmt.Errorf("info clients failed: %w", err))
	}
	body := fmt.Sprintf("ping: ok (latency_ms=%d)\nused_memory: %s\nused_memory_human: %s\nconnected_clients: %s\nblocked_clients: %s\n",
		latency.Milliseconds(),
		redisInfoValue(memory, "used_memory"),
		redisInfoValue(memory, "used_memory_human"),
		redisInfoValue(clients, "connected_clients"),
		redisInfoValue(clients, "blocked_clients"),
	)
	return finishItem(c.Name(), "redis:PING/INFO", body, nil)
}

// redisInfoValue 从 INFO 文本取单个字段，缺失返回 "unknown"。
func redisInfoValue(info, key string) string {
	for line := range strings.Lines(info) {
		if value, ok := strings.CutPrefix(line, key+":"); ok {
			return strings.TrimSpace(value)
		}
	}
	return "unknown"
}

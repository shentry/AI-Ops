// server starts the D03 webhook ingestion service.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gogf/gf/v2/frame/g"

	"oncall-agent/internal/api"
	"oncall-agent/internal/approval"
	"oncall-agent/internal/config"
	"oncall-agent/internal/diagnose"
	"oncall-agent/internal/ingest"
	"oncall-agent/internal/llm"
	"oncall-agent/internal/memory"
	"oncall-agent/internal/metrics"
	"oncall-agent/internal/notify"
	"oncall-agent/internal/store"
	"oncall-agent/internal/tools"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "oncall-agent server: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := os.Getenv("CONFIG_FILE")
	if configPath == "" {
		configPath = "config.yaml"
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Server.AuthToken) == "" {
		return errors.New("config: server.auth_token is required for webhook server")
	}

	db, err := store.Open(cfg.MySQL.DSN)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	worker := ingest.NewWorker(db, cfg.Ingest, cfg.Correlate, cfg.Diagnose.SeverityRoute, log.Default())
	if err := worker.Start(ctx); err != nil {
		return err
	}

	// D06/D07：工具注册表 + 证据采集。Prometheus 必须可用才注册；
	// Docker socket 缺席时只跳过 Docker 工具，不影响其余证据段。
	registry := tools.NewRegistry()
	promClient, err := tools.NewPrometheusClient(cfg.Tools.Prometheus)
	if err != nil {
		return fmt.Errorf("server: init prometheus client: %w", err)
	}
	if err := promClient.RegisterTools(registry); err != nil {
		return err
	}
	if dockerClient, err := tools.NewDockerClient(cfg.Diagnose.Evidence.DockerSocket); err != nil {
		log.Printf("server: docker socket unavailable, docker evidence disabled: %v", err)
	} else {
		if err := dockerClient.RegisterTools(registry, cfg.Diagnose.Evidence.LogMaxLines); err != nil {
			return err
		}
		if err := dockerClient.RegisterRestartTool(registry, cfg.Tools.Docker.AllowedContainers); err != nil {
			return err
		}
	}
	collectors := []diagnose.Collector{
		diagnose.NewSnapshotCollector(),
		diagnose.NewPromReplayCollector(registry, cfg.Tools.Prometheus.RangeMinutes),
		diagnose.NewGoldenMetricsCollector(registry),
		diagnose.NewSub2APICollector(cfg.Diagnose.Evidence, registry),
		diagnose.NewPostgresCollector(cfg.Diagnose.Evidence),
		diagnose.NewRedisCollector(cfg.Diagnose.Evidence),
		diagnose.NewDockerCollector(cfg.Diagnose.Evidence, registry),
	}
	evidenceBuilder := diagnose.NewEvidenceBuilder(db, collectors)

	// D10：Policy 与审批生命周期。审批不依赖 LLM 配置 —— 手动重诊之外的
	// 自动诊断停了，人工仍要能通过 API 审批存量审批单。
	policy := approval.NewPolicy(registry, approval.PolicyConfig{
		AutoExecuteL2: cfg.Approval.AutoExecuteL2,
		DryRun:        cfg.Approval.DryRun,
	})
	approvalSvc := approval.NewService(db, cfg.Approval.TTLMinutes)
	expiryWorker := approval.NewExpiryWorker(db, log.Default())
	expiryWorker.Start(ctx)

	// D11：执行器消费 approved 审批单。
	verifier := diagnose.NewVerifier(db)
	// 通知器先于诊断链构建：重诊升级通知不依赖 LLM 是否可用。
	var notifier notify.Notifier = notify.NoopNotifier{Logf: log.Printf}
	if webhookNotifier, err := notify.NewWebhookNotifier(cfg.Notify.IM); err != nil {
		log.Printf("server: IM webhook not configured, notifications disabled: %v", err)
	} else {
		notifier = webhookNotifier
	}
	// D12：Verify 失败的重诊调度（最多两次重试，超限升级人工）。
	retryScheduler := diagnose.NewRetryScheduler(db, notifier, 2)
	// D13：精确故障记忆。命中不绕过 Guard/审批/Verify（GC-17）。
	faultMemory := memory.NewStore(db, cfg.Memory.TTLSeconds, cfg.Memory.OnlyHighConfidence)
	executor := approval.NewExecutor(db, registry, cfg.Approval.DryRun,
		time.Duration(cfg.Approval.VerifyDelaySeconds)*time.Second, verifyAdapter{v: verifier}, retryScheduler, faultMemory, log.Default())

	// D14：pending 队列深度 gauge，抓取时现算。
	metrics.RegisterGauge(metrics.PendingRawEvents, func() int64 {
		count, err := db.CountRawEventsByStatus(context.Background(), "pending")
		if err != nil {
			return -1
		}
		return count
	})
	metrics.RegisterGauge(metrics.PendingAgentRuns, func() int64 {
		count, err := db.CountAgentRunsByStatus(context.Background(), "pending")
		if err != nil {
			return -1
		}
		return count
	})
	executor.Start(ctx)
	defer func() { stop(); executor.Wait() }()
	// D09：诊断流水线与独立 worker。LLM 配置缺失时诊断链不启动，
	// 摄入与查询照常 —— 告警摄入绝不能被诊断能力卡住（GC-07）。
	var diagnoseWorker *diagnose.Worker
	llmFactory := llm.NewFactory(cfg.LLM)
	if err := llmFactory.Validate(); err != nil {
		log.Printf("server: LLM not configured, diagnosis worker disabled: %v", err)
	} else {
		reasoner := llm.NewReasoner(llmFactory, registry, cfg.Diagnose.Budget)
		pipeline := diagnose.NewPipeline(db, evidenceBuilder, reasoner, policy, approvalSvc, diagnose.NewNotifyReporter(notifier, fmt.Sprintf("http://127.0.0.1:%d", cfg.Server.Port)), faultMemory, cfg.Memory.CmdHistoryInject)
		diagnoseWorker = diagnose.NewWorker(db, pipeline, log.Default())
		if err := diagnoseWorker.Start(ctx); err != nil {
			return fmt.Errorf("server: start diagnose worker: %w", err)
		}
	}

	server := g.Server()
	server.SetPort(cfg.Server.Port)
	server.SetGraceful(true)
	server.BindHandler("/webhook/alertmanager", api.NewAlertmanagerWebhook(db, worker, cfg.Server.AuthToken).Handle)
	incidentAPI := api.NewIncidentAPI(db, cfg.Server.AuthToken, cfg.Diagnose.SeverityRoute)
	server.BindHandler("/api/v1/incidents", incidentAPI.Handle)
	server.BindHandler("/metrics", api.MetricsHandler)
	server.BindHandler("/api/v1/incidents/:id", incidentAPI.Handle)
	server.BindHandler("/api/v1/incidents/:id/diagnose", incidentAPI.Handle)
	server.BindHandler("/debug/evidence/:id", api.NewEvidenceDebugAPI(evidenceBuilder, cfg.Server.AuthToken).Handle)
	approvalAPI := api.NewApprovalAPI(approvalSvc, cfg.Server.AuthToken)
	server.BindHandler("/api/v1/approvals", approvalAPI.Handle)
	server.BindHandler("/api/v1/approvals/:id/approve", approvalAPI.Handle)
	server.BindHandler("/api/v1/approvals/:id/deny", approvalAPI.Handle)
	if err := server.Start(); err != nil {
		stop()
		worker.Wait()
		expiryWorker.Wait()
		return fmt.Errorf("server: start HTTP listener: %w", err)
	}
	fmt.Fprintln(os.Stdout, "oncall-agent: server ready")

	<-ctx.Done()
	shutdownErr := server.Shutdown()
	stop()
	worker.Wait()
	expiryWorker.Wait()
	if diagnoseWorker != nil {
		diagnoseWorker.Wait()
	}
	return shutdownErr
}

// verifyAdapter 把 diagnose.Verifier 适配成 approval 包的验证接口
// （两包互不依赖，转换只在组装层发生）。
type verifyAdapter struct {
	v *diagnose.Verifier
}

func (a verifyAdapter) VerifyAfterExecution(ctx context.Context, runID, incidentID uint64, delay time.Duration) approval.VerifyOutcome {
	result := a.v.VerifyAfterExecution(ctx, runID, incidentID, delay)
	return approval.VerifyOutcome{Passed: result.Passed, Detail: result.Detail}
}

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
	"oncall-agent/internal/conversation"
	"oncall-agent/internal/diagnose"
	"oncall-agent/internal/incident"
	"oncall-agent/internal/ingest"
	"oncall-agent/internal/llm"
	"oncall-agent/internal/memory"
	"oncall-agent/internal/metrics"
	"oncall-agent/internal/notify"
	"oncall-agent/internal/notify/feishu"
	"oncall-agent/internal/store"
	"oncall-agent/internal/tools"
	"oncall-agent/web"
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
	var expiryWorker *approval.ExpiryWorker
	var executor *approval.Executor
	var diagnoseWorker *diagnose.Worker
	var conversationWorker *conversation.Worker
	// Every worker is started only after its dependency has been assembled. A
	// single cleanup path makes startup failures as safe as normal shutdown.
	defer func() {
		stop()
		if conversationWorker != nil {
			conversationWorker.Wait()
		}
		if diagnoseWorker != nil {
			diagnoseWorker.Wait()
		}
		if executor != nil {
			executor.Wait()
		}
		if expiryWorker != nil {
			expiryWorker.Wait()
		}
		worker.Wait()
	}()

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
		if err := dockerClient.RegisterRestartTool(registry, cfg.Tools.Docker.AllowedContainers, tools.RestartLimits{
			MinInterval: time.Duration(cfg.Tools.Docker.RestartMinIntervalSeconds) * time.Second,
			MaxPerHour:  cfg.Tools.Docker.RestartMaxPerHour,
		}); err != nil {
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
		AutoExecuteL2:  cfg.Approval.AutoExecuteL2,
		DryRun:         cfg.Approval.DryRun,
		AllowedTargets: cfg.Tools.Docker.AllowedContainers,
		RateWindow:     time.Duration(cfg.Approval.L2RateWindowMinutes) * time.Minute,
		MaxPerWindow:   cfg.Approval.L2MaxPerWindow,
	}, db)
	approvalSvc := approval.NewService(db, cfg.Approval.TTLMinutes)
	expiryWorker = approval.NewExpiryWorker(db, log.Default())
	expiryWorker.Start(ctx)

	// D11：执行器消费 approved 审批单。Provider selection is explicit: the
	// enterprise Feishu app never falls back to a legacy webhook or Noop.
	verifier := diagnose.NewVerifier(db, log.Default())
	var notifier notify.Notifier = notify.NoopNotifier{Logf: log.Printf}
	provider := strings.ToLower(strings.TrimSpace(cfg.Notify.IM.Provider))
	var feishuClient *feishu.Client
	var feishuCallback *api.FeishuCallbackAPI
	if provider == "feishu_app" {
		feishuNotifier, feishuErr := feishu.NewFromIMConfig(cfg.Notify.IM)
		if feishuErr != nil {
			return fmt.Errorf("server: init feishu app: %w", feishuErr)
		}
		feishuClient = feishuNotifier
		notifier = feishu.NewBindingNotifier(feishuNotifier, db, "feishu_app", cfg.Notify.IM.Feishu.ChatID)
	} else if webhookNotifier, webhookErr := notify.NewWebhookNotifier(cfg.Notify.IM); webhookErr != nil {
		log.Printf("server: IM webhook not configured, notifications disabled: %v", webhookErr)
	} else {
		notifier = webhookNotifier
	}
	retryScheduler := diagnose.NewRetryScheduler(db, notifier, 2)
	faultMemory := memory.NewStore(db, cfg.Memory.TTLSeconds, cfg.Memory.OnlyHighConfidence)
	executor = approval.NewExecutor(db, registry, cfg.Approval.DryRun,
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

	// The Web surface is intentionally public: console requests carry a stable
	// anonymous actor and require no login or CSRF token. Assembly stays opt-in
	// so webhook-only deployments do not expose the console merely by upgrading
	// the binary; a nil console keeps every browser route closed.
	webEnabled := strings.TrimSpace(cfg.Web.BaseURL) != "" || provider == "feishu_app"
	var console *api.Console
	var conversationService conversation.Service
	if webEnabled {
		console = api.NewConsole()
		conversationService = conversation.NewService(db)
	}
	if feishuClient != nil {
		business := feishu.NewCallbackBusiness(feishu.BusinessDependencies{
			Store:             db,
			Approval:          approvalSvc,
			Conversation:      conversationService,
			Client:            feishuClient,
			ChatID:            cfg.Notify.IM.Feishu.ChatID,
			Provider:          "feishu_app",
			OperatorAllowlist: cfg.Web.OperatorAllowlist,
		})
		dispatcher := feishu.NewDispatcher(
			cfg.Notify.IM.Feishu.VerificationToken,
			cfg.Notify.IM.Feishu.EncryptKey,
			feishu.CallbackHandlers{CardAction: business.CardAction, MessageReceive: business.MessageReceive},
		)
		feishuCallback = api.NewFeishuCallbackAPI(dispatcher)
	}

	// D09：诊断流水线与独立 workers。LLM 配置缺失时诊断 worker 不启动，
	// 但 Web 对话 worker 仍消费队列并记录明确的失败，而不是留下永久 queued。
	llmFactory := llm.NewFactory(cfg.LLM)
	llmReady := llmFactory.Validate() == nil
	var modelSwitcher *llm.ModelSwitcher
	if !llmReady {
		log.Printf("server: LLM not configured, diagnosis worker disabled")
	} else {
		modelSwitcher, err = llm.NewModelSwitcher(db, llmFactory, cfg.LLM)
		if err != nil {
			return fmt.Errorf("server: init model switcher: %w", err)
		}
		if _, err := modelSwitcher.Initialize(ctx); err != nil {
			return fmt.Errorf("server: restore selected model: %w", err)
		}
		reasoner := llm.NewReasoner(llmFactory, registry, cfg.Diagnose.Budget)
		pipeline := diagnose.NewPipeline(db, evidenceBuilder, reasoner, policy, approvalSvc,
			diagnose.NewNotifyReporter(notifier, notificationWebURL(cfg)), faultMemory, cfg.Memory.CmdHistoryInject)
		diagnoseWorker = diagnose.NewWorker(db, pipeline, log.Default())
		if err := diagnoseWorker.Start(ctx); err != nil {
			return fmt.Errorf("server: start diagnose worker: %w", err)
		}
	}
	if conversationService != nil {
		questioner := llm.NewQuestioner(llmFactory, registry, cfg.Diagnose.Budget)
		conversationWorker = conversation.NewWorker(db, questioner, log.Default())
		if feishuClient != nil {
			conversationWorker.SetThreadReply(feishuThreadReply{client: feishuClient}, "feishu_app")
		}
		if err := conversationWorker.Start(ctx); err != nil {
			return fmt.Errorf("server: start conversation worker: %w", err)
		}
	}

	server := g.Server()
	server.SetPort(cfg.Server.Port)
	server.SetGraceful(true)
	server.BindHandler("/webhook/alertmanager", api.NewAlertmanagerWebhook(db, worker, cfg.Server.AuthToken).Handle)
	incidentAPI := api.NewIncidentAPI(db, cfg.Server.AuthToken, cfg.Diagnose.SeverityRoute)
	server.BindHandler("/api/v1/incidents", incidentAPI.Handle)
	server.BindHandler("/api/v1/incidents/:id", incidentAPI.Handle)
	server.BindHandler("/api/v1/incidents/:id/diagnose", incidentAPI.Handle)
	server.BindHandler("/metrics", api.MetricsHandler)
	server.BindHandler("/debug/evidence/:id", api.NewEvidenceDebugAPI(evidenceBuilder, cfg.Server.AuthToken).Handle)
	approvalAPI := api.NewApprovalAPI(approvalSvc, cfg.Server.AuthToken, console)
	server.BindHandler("/api/v1/approvals", approvalAPI.Handle)
	server.BindHandler("/api/v1/approvals/:id", approvalAPI.Handle)
	server.BindHandler("/api/v1/approvals/:id/approve", approvalAPI.Handle)
	server.BindHandler("/api/v1/approvals/:id/deny", approvalAPI.Handle)
	modelAPI := api.NewModelAPI(modelSwitcher, cfg.Server.AuthToken, console)
	server.BindHandler("/api/v1/admin/model", modelAPI.Handle)

	if webEnabled {
		controlRoomAPI := api.NewControlRoomAPI(db, console)
		server.BindHandler("/api/v1/incidents/:id/control-room", controlRoomAPI.Handle)
		server.BindHandler("/api/v1/control-room/incidents", controlRoomAPI.Handle)
		server.BindHandler("/api/v1/control-room/model", modelAPI.Handle)
		server.BindHandler("/api/v1/incidents/:id/events", controlRoomAPI.Handle)
		server.BindHandler("/api/v1/incidents/:id/problems", controlRoomAPI.Handle)
		runAPI := api.NewRunAPI(db, console)
		server.BindHandler("/api/v1/incidents/:id/runs", runAPI.Handle)
		server.BindHandler("/api/v1/runs/:id/steps", runAPI.Handle)
		server.BindHandler("/api/v1/incidents/:id/runs/:run_id/steps", runAPI.Handle)
		streamAPI := api.NewStreamAPI(db, console)
		server.BindHandler("/api/v1/incidents/:id/stream", streamAPI.Handle)
		actionService := &incidentActionService{db: db, severityRoute: cfg.Diagnose.SeverityRoute, questions: conversationService}
		conversationAPI := api.NewConversationAPI(conversationService, console, actionService)
		server.BindHandler("/api/v1/incidents/:id/conversation", conversationAPI.Handle)
		server.BindHandler("/api/v1/incidents/:id/questions", conversationAPI.Handle)
		server.BindHandler("/api/v1/incidents/:id/rediagnose", conversationAPI.Handle)
		server.BindHandler("/api/v1/incidents/:id/request-evidence", conversationAPI.Handle)
	}
	if feishuCallback != nil {
		server.BindHandler("/integrations/feishu/events", feishuCallback.Handle)
	}
	if webEnabled {
		staticAPI := api.NewStaticAPI(web.DistFS)
		server.BindHandler("/", staticAPI.Handle)
		server.BindHandler("/*path", staticAPI.Handle)
	}

	if err := server.Start(); err != nil {
		return fmt.Errorf("server: start HTTP listener: %w", err)
	}
	fmt.Fprintln(os.Stdout, "oncall-agent: server ready")
	<-ctx.Done()
	return server.Shutdown()
}

// notificationWebURL is the single source for links rendered into all
// provider notifications. It intentionally has no localhost fallback.
func notificationWebURL(cfg config.Config) string {
	if base := strings.TrimRight(strings.TrimSpace(cfg.Web.BaseURL), "/"); base != "" {
		return base
	}
	return strings.TrimRight(strings.TrimSpace(cfg.Notify.IM.Feishu.WebBaseURL), "/")
}

// incidentActionService keeps Web action routes asynchronous and Incident
// scoped. Rediagnose only queues a run; evidence requests share the queued
// conversation path so they never execute LLM/Docker work in an HTTP handler.
type incidentActionService struct {
	db            *store.DB
	severityRoute map[string]string
	questions     conversation.Service
}

func (s *incidentActionService) Rediagnose(ctx context.Context, incidentID uint64, _ api.Actor) (store.AgentRun, error) {
	if s == nil || s.db == nil {
		return store.AgentRun{}, errors.New("server: incident action service unavailable")
	}
	found, err := s.db.GetIncident(ctx, incidentID)
	if err != nil {
		return store.AgentRun{}, err
	}
	mode := incident.RouteMode(int(found.Severity), s.severityRoute)
	if mode == incident.ModeSkip {
		mode = incident.ModeLight
	}
	return s.db.CreateAgentRun(ctx, store.AgentRun{
		IncidentID: incidentID,
		Mode:       mode,
		Status:     "pending",
		StartedAt:  time.Now().UTC(),
	})
}

func (s *incidentActionService) RequestEvidence(ctx context.Context, incidentID uint64, actor api.Actor, request string) error {
	if s == nil || s.questions == nil {
		return errors.New("server: conversation service unavailable")
	}
	request = strings.TrimSpace(request)
	if request == "" {
		return errors.New("server: evidence request is required")
	}
	_, err := s.questions.Ask(ctx, incidentID, conversation.Actor{
		ID: actor.ID, Name: actor.Name, Source: "web",
	}, "请补充采集证据："+request, "web")
	return err
}

// （两包互不依赖，转换只在组装层发生）。
type verifyAdapter struct {
	v *diagnose.Verifier
}

type feishuThreadReply struct {
	client *feishu.Client
}

func (r feishuThreadReply) Reply(ctx context.Context, parentMessageID, content string) error {
	if r.client == nil {
		return errors.New("feishu: client is nil")
	}
	_, err := r.client.Reply(ctx, parentMessageID, content)
	return err
}

func (a verifyAdapter) VerifyAfterExecution(ctx context.Context, runID, incidentID uint64, delay time.Duration) approval.VerifyOutcome {
	result := a.v.VerifyAfterExecution(ctx, runID, incidentID, delay)
	return approval.VerifyOutcome{Passed: result.Passed, Inconclusive: result.Inconclusive, Detail: result.Detail}
}

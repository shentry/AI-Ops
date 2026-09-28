// server starts the D03 webhook ingestion service.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gogf/gf/v2/frame/g"

	"oncall-agent/internal/api"
	"oncall-agent/internal/approval"
	"oncall-agent/internal/config"
	"oncall-agent/internal/conversation"
	"oncall-agent/internal/diagnose"
	"oncall-agent/internal/grafana"
	"oncall-agent/internal/incident"
	"oncall-agent/internal/ingest"
	"oncall-agent/internal/llm"
	"oncall-agent/internal/memory"
	"oncall-agent/internal/metrics"
	"oncall-agent/internal/notify"
	"oncall-agent/internal/notify/feishu"
	"oncall-agent/internal/store"
	"oncall-agent/internal/sub2api"
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

	if cfg.AutomaticRemediation() {
		if err := llm.NewFactory(cfg.LLM).Validate(); err != nil {
			return fmt.Errorf("server: auto remediation requires a configured diagnostic model: %w", err)
		}
	}

	db, err := store.Open(cfg.MySQL.DSN)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := db.CheckExecutionReady(ctx); err != nil {
		return fmt.Errorf("server: execution schema/upgrade check: %w", err)
	}
	// Acquire ownership before any worker; release it only after worker cleanup.
	releaseExecutorLock, err := db.AcquireExecutorLock(ctx, "oncall-agent-executor")
	if err != nil {
		return fmt.Errorf("server: %w", err)
	}
	defer releaseExecutorLock()
	leaseFailure := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := db.CheckExecutionLease(ctx); err != nil && ctx.Err() == nil {
					leaseFailure <- err
					stop()
					return
				}
			}
		}
	}()
	worker := ingest.NewWorker(db, cfg.Ingest, cfg.Correlate, cfg.Diagnose.SeverityRoute, log.Default())
	if err := worker.Start(ctx); err != nil {
		return err
	}
	var expiryWorker *approval.ExpiryWorker
	var executor *approval.Executor
	var verificationWorker *diagnose.VerificationWorker
	var diagnoseWorker *diagnose.Worker
	var conversationWorker *conversation.Worker
	var notificationWorker *notify.Worker
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
		if verificationWorker != nil {
			verificationWorker.Wait()
		}
		if executor != nil {
			executor.Wait()
		}
		if expiryWorker != nil {
			expiryWorker.Wait()
		}
		if notificationWorker != nil {
			notificationWorker.Wait()
		}
		worker.Wait()
	}()

	// 工具目录：只读工具与写动作。Prometheus 必须可用；Docker socket 缺席时
	// 只跳过 Docker 工具和依赖它的动作，不影响其余证据段。
	registry := tools.NewRegistry()
	promClient, err := tools.NewPrometheusClient(cfg.Tools.Prometheus)
	if err != nil {
		return fmt.Errorf("server: init prometheus client: %w", err)
	}
	if err := promClient.RegisterTools(registry); err != nil {
		return err
	}
	// Loki 是可选的历史日志：未配置时只剩 docker_logs 看当前容器实例。
	if strings.TrimSpace(cfg.Tools.Loki.BaseURL) != "" {
		lokiClient, err := tools.NewLokiClient(cfg.Tools.Loki)
		if err != nil {
			return fmt.Errorf("server: init loki client: %w", err)
		}
		if err := lokiClient.RegisterTools(registry); err != nil {
			return err
		}
	}
	// sub2api's admin key has no read-only scope: every caller gets a client
	// built with exactly the endpoints it may call.
	var opsReader *sub2api.Client
	if strings.TrimSpace(cfg.Service.AdminAPIKey) != "" {
		opsReader, err = sub2api.New(cfg.Service.BaseURL, cfg.Service.AdminAPIKey, time.Duration(cfg.Diagnose.Evidence.TimeoutSeconds)*time.Second, sub2api.ReadOnlyOps...)
		if err != nil {
			return fmt.Errorf("server: init sub2api ops client: %w", err)
		}
		upstreamAdmin, err := sub2api.New(cfg.Service.BaseURL, cfg.Service.AdminAPIKey, 10*time.Second, tools.UpstreamEndpoints...)
		if err != nil {
			return fmt.Errorf("server: init sub2api admin client: %w", err)
		}
		for _, action := range tools.NewUpstreamActions(upstreamAdmin) {
			if err := registry.RegisterAction(action); err != nil {
				return err
			}
		}
	}
	if dockerClient, err := tools.NewDockerClient(cfg.Diagnose.Evidence.DockerSocket); err != nil {
		log.Printf("server: docker socket unavailable, docker evidence and container actions disabled: %v", err)
	} else {
		if err := dockerClient.RegisterTools(registry, cfg.Diagnose.Evidence.LogMaxLines); err != nil {
			return err
		}
		if err := registry.RegisterAction(tools.NewRestartAction(dockerClient, cfg.Service)); err != nil {
			return err
		}
		if len(cfg.Service.Release.Command) > 0 {
			if err := registry.RegisterAction(tools.NewRollbackAction(dockerClient, releaseSource(db, cfg.Service.Name), cfg.Service)); err != nil {
				return err
			}
		}
	}
	var upstreamEvidence diagnose.Collector = diagnose.NewUpstreamAccountsCollector(nil)
	var verifierAccounts diagnose.AccountReader
	if opsReader != nil {
		upstreamEvidence, verifierAccounts = diagnose.NewUpstreamAccountsCollector(opsReader), opsReader
	}
	collectors := []diagnose.Collector{
		diagnose.NewSnapshotCollector(),
		diagnose.NewPromReplayCollector(registry, cfg.Tools.Prometheus.RangeMinutes),
		diagnose.NewGoldenMetricsCollector(registry),
		diagnose.NewSub2APIHealthCollector(cfg.Service, cfg.Diagnose.Evidence),
		diagnose.NewSub2APIMetricsCollector(registry),
		upstreamEvidence,
		diagnose.NewRecentChangesCollector(db, cfg.Service),
		diagnose.NewPostgresCollector(cfg.Service, cfg.Diagnose.Evidence),
		diagnose.NewRedisCollector(cfg.Service, cfg.Diagnose.Evidence),
		diagnose.NewDockerInspectCollector(cfg.Service, registry),
		diagnose.NewDockerLogsCollector(cfg.Service, cfg.Diagnose.Evidence, registry),
	}
	evidenceBuilder := diagnose.NewEvidenceBuilder(db, collectors)

	// 处置规则是唯一的执行授权来源；规则引用未启用的动作时拒绝启动。
	authority, err := approval.NewAuthority(cfg.Service, cfg.Remediation, registry)
	if err != nil {
		return fmt.Errorf("server: %w", err)
	}
	if err := db.RecordRulesRelease(ctx, authority.Release(), cfg.Remediation.Rules, time.Now().UTC()); err != nil {
		return fmt.Errorf("server: audit rules release: %w", err)
	}
	policy := approval.NewPolicy(authority, registry, time.Duration(cfg.Approval.TTLMinutes)*time.Minute, db)
	approvalSvc := approval.NewService(db)
	expiryWorker = approval.NewExpiryWorker(db, log.Default())
	expiryWorker.Start(ctx)

	// D11：执行器消费 approved 审批单。Provider selection is explicit: the
	// enterprise Feishu app never falls back to a legacy webhook or Noop.
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
	if _, disabled := notifier.(notify.NoopNotifier); !disabled {
		delivery := notify.NewWorker(db, notifier)
		if err := delivery.Start(ctx); err != nil {
			return fmt.Errorf("server: start notification worker: %w", err)
		}
		notificationWorker = delivery
	} else {
		for _, rule := range cfg.Remediation.Rules {
			if rule.Mode == incident.ModeAuto {
				return errors.New("server: automatic remediation requires a notification provider")
			}
		}
	}
	faultMemory := memory.NewStore(db, cfg.Memory.TTLSeconds, cfg.Memory.OnlyHighConfidence)
	execution := approval.NewExecutor(db, registry, authority, log.Default())
	if err := execution.Start(ctx); err != nil {
		return fmt.Errorf("server: start executor: %w", err)
	}
	executor = execution
	var probe *sub2api.Probe
	if p := cfg.Service.Probe; strings.TrimSpace(p.APIKey) != "" {
		if probe, err = sub2api.NewProbe(cfg.Service.BaseURL, p.Path, p.APIKey, p.Model, 20*time.Second); err != nil {
			return fmt.Errorf("server: init business probe: %w", err)
		}
	}
	verification := diagnose.NewVerificationWorker(db, diagnose.NewVerifier(registry, verifierAccounts, probe), cfg.Memory.TTLSeconds, notifier, log.Default(), authority.Binding())
	if err := verification.Start(ctx); err != nil {
		return fmt.Errorf("server: start verification worker: %w", err)
	}
	verificationWorker = verification

	pendingRaw, pendingRuns := &atomic.Int64{}, &atomic.Int64{}
	metrics.RegisterGauge(metrics.PendingRawEvents, pendingRaw.Load)
	metrics.RegisterGauge(metrics.PendingAgentRuns, pendingRuns.Load)
	// Refresh queue delay outside the metrics request: a stalled DB cannot hold a scrape.
	queueAges := map[string]*atomic.Int64{}
	for _, name := range []string{"raw_event", "diagnosis", "verification", "notification"} {
		age := &atomic.Int64{}
		age.Store(-1)
		queueAges[name] = age
		metrics.RegisterGauge("oldest_"+name+"_seconds", age.Load)
	}
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			queryCtx, queryCancel := context.WithTimeout(ctx, time.Second)
			rawCount, rawErr := db.CountRawEventsByStatus(queryCtx, "pending")
			runCount, runErr := db.CountAgentRunsByStatus(queryCtx, "pending")
			queryCancel()
			if rawErr != nil {
				rawCount = -1
			}
			if runErr != nil {
				runCount = -1
			}
			pendingRaw.Store(rawCount)
			pendingRuns.Store(runCount)
			ages, err := db.QueueAges(ctx)
			for name, value := range queueAges {
				if err != nil {
					value.Store(-1)
				} else {
					value.Store(ages[name])
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	// Every API resolves a server-side identity: the shared machine token, or a
	// configured operator (personal Bearer token or the session cookie issued
	// for it). Console routes are bound only when the console is enabled, and
	// config validation refuses a console without operators.
	webEnabled := cfg.ConsoleEnabled()
	auth, err := api.NewAuth(cfg.Server.AuthToken, cfg.Web.Operators, strings.HasPrefix(strings.ToLower(notificationWebURL(cfg)), "https://"))
	if err != nil {
		return fmt.Errorf("server: init auth: %w", err)
	}
	var conversationService conversation.Service
	if webEnabled {
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
		diagnosis := diagnose.NewWorker(db, pipeline, log.Default())
		if err := diagnosis.Start(ctx); err != nil {
			return fmt.Errorf("server: start diagnose worker: %w", err)
		}
		diagnoseWorker = diagnosis
	}
	if conversationService != nil {
		questioner := llm.NewQuestioner(llmFactory, registry, cfg.Diagnose.Budget)
		questions := conversation.NewWorker(db, questioner, log.Default())
		if feishuClient != nil {
			questions.SetThreadReply(feishuThreadReply{client: feishuClient}, "feishu_app")
		}
		if err := questions.Start(ctx); err != nil {
			return fmt.Errorf("server: start conversation worker: %w", err)
		}
		conversationWorker = questions
	}

	server := g.Server()
	server.SetAddr(net.JoinHostPort(cfg.Server.ListenAddr, strconv.Itoa(cfg.Server.Port)))
	server.SetGraceful(true)
	server.BindHandler("/webhook/alertmanager", api.NewAlertmanagerWebhook(db, worker, auth).Handle)
	incidentAPI := api.NewIncidentAPI(db, auth, cfg.Diagnose.SeverityRoute)
	server.BindHandler("/api/v1/incidents", incidentAPI.Handle)
	server.BindHandler("/api/v1/incidents/:id", incidentAPI.Handle)
	server.BindHandler("/api/v1/incidents/:id/diagnose", incidentAPI.Handle)
	server.BindHandler("/metrics", api.MetricsHandler)
	server.BindHandler("/debug/evidence/:id", api.NewEvidenceDebugAPI(evidenceBuilder, auth).Handle)
	approvalAPI := api.NewApprovalAPI(approvalSvc, auth)
	server.BindHandler("/api/v1/approvals", approvalAPI.Handle)
	server.BindHandler("/api/v1/approvals/:id", approvalAPI.Handle)
	server.BindHandler("/api/v1/approvals/:id/approve", approvalAPI.Handle)
	server.BindHandler("/api/v1/approvals/:id/deny", approvalAPI.Handle)
	modelAPI := api.NewModelAPI(modelSwitcher, auth)
	server.BindHandler("/api/v1/admin/model", modelAPI.Handle)
	remediationAPI := api.NewRemediationAPI(db, authority, auth)
	for _, route := range []string{"/api/v1/remediation", "/api/v1/remediation/report", "/api/v1/remediation/stop", "/api/v1/remediation/resume",
		"/api/v1/remediation/rules/:rule/reset", "/api/v1/incidents/:id/reviews", "/api/v1/changes", "/api/v1/changes/:id/verify"} {
		server.BindHandler(route, remediationAPI.Handle)
	}

	if webEnabled {
		server.BindHandler("/api/v1/session", api.NewSessionAPI(auth).Handle)
		controlRoomAPI := api.NewControlRoomAPI(db, auth)
		server.BindHandler("/api/v1/incidents/:id/control-room", controlRoomAPI.Handle)
		server.BindHandler("/api/v1/control-room/incidents", controlRoomAPI.Handle)
		server.BindHandler("/api/v1/control-room/model", modelAPI.Handle)
		server.BindHandler("/api/v1/incidents/:id/events", controlRoomAPI.Handle)
		server.BindHandler("/api/v1/incidents/:id/problems", controlRoomAPI.Handle)
		runAPI := api.NewRunAPI(db, auth)
		server.BindHandler("/api/v1/incidents/:id/runs", runAPI.Handle)
		server.BindHandler("/api/v1/runs/:id/steps", runAPI.Handle)
		server.BindHandler("/api/v1/incidents/:id/runs/:run_id/steps", runAPI.Handle)
		streamAPI := api.NewStreamAPI(db, auth)
		server.BindHandler("/api/v1/incidents/:id/stream", streamAPI.Handle)
		actionService := &incidentActionService{db: db, severityRoute: cfg.Diagnose.SeverityRoute, questions: conversationService}
		conversationAPI := api.NewConversationAPI(conversationService, auth, actionService)
		server.BindHandler("/api/v1/incidents/:id/conversation", conversationAPI.Handle)
		server.BindHandler("/api/v1/incidents/:id/questions", conversationAPI.Handle)
		server.BindHandler("/api/v1/incidents/:id/rediagnose", conversationAPI.Handle)
		server.BindHandler("/api/v1/incidents/:id/request-evidence", conversationAPI.Handle)
		dashboards, err := grafana.Load()
		if err != nil {
			return fmt.Errorf("server: %w", err)
		}
		observabilityAPI := api.NewObservabilityAPI(promClient, dashboards, auth)
		for _, route := range []string{"/api/v1/prometheus/query_range", "/api/v1/observability/dashboards", "/api/v1/observability/dashboards/:uid"} {
			server.BindHandler(route, observabilityAPI.Handle)
		}
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
	shutdownErr := server.Shutdown()
	select {
	case err := <-leaseFailure:
		return errors.Join(err, shutdownErr)
	default:
		return shutdownErr
	}
}

// releaseSource reads what ran over time for the rollback action: release
// records and the rollbacks that returned to them, newest first.
func releaseSource(db *store.DB, service string) tools.ReleaseSource {
	return func(ctx context.Context) ([]tools.Release, error) {
		rows, err := db.ListReleases(ctx, service, 20)
		if err != nil {
			return nil, err
		}
		releases := make([]tools.Release, 0, len(rows))
		for _, row := range rows {
			if row.ReleaseID == nil || row.ImageRef == nil {
				continue
			}
			releases = append(releases, tools.Release{ID: *row.ReleaseID, ImageRef: *row.ImageRef, Migration: row.DBMigration, VerifiedAt: row.VerifiedAt, OccurredAt: row.OccurredAt})
		}
		return releases, nil
	}
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
	run, _, err := s.db.RequestRun(ctx, store.RunRequest{
		IncidentID: incidentID, Mode: mode, Trigger: store.RunTriggerManual, RequestedAt: time.Now().UTC(),
	})
	return run, err
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

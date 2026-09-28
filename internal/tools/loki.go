package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"oncall-agent/internal/config"
)

// ToolLokiQuery 查 Loki 里的历史日志：跨容器重启、跨服务，docker_logs 只看当前容器实例。
const ToolLokiQuery = "loki_query"

const (
	lokiToolTimeout   = 10 * time.Second
	lokiDefaultWindow = 30 * time.Minute
	lokiDefaultLimit  = 100
	// lokiMaxContains 限制行过滤串长度：它是模型给的字面量，不需要写成长段文本。
	lokiMaxContains = 200
)

var lokiHTTPClient = &http.Client{Timeout: lokiToolTimeout + 5*time.Second}

// lokiServicePattern 是采集端写入的 service 标签值（Compose 服务名或 systemd 服务名）。
// 只允许名字字符，拼进 selector 时不存在需要转义的内容。
var lokiServicePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)

// LokiClient 只做一种查询：按 service 取一个时间窗内的日志，可选字面量过滤。
// 模型不能提交任意 LogQL：selector 和过滤表达式都由这里拼出，
// 能读到的范围就是采集端（deploy/monitoring/config.alloy）送进 Loki 的服务。
type LokiClient struct {
	base       *url.URL
	maxLines   int
	maxWindow  time.Duration
	httpClient *http.Client
	now        func() time.Time
}

// NewLokiClient 校验 base URL 并剥掉 userinfo，与 Prometheus 客户端同一纪律。
func NewLokiClient(cfg config.LokiConfig) (*LokiClient, error) {
	base, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, errors.New("tools: loki base_url must be an http(s) URL")
	}
	base.User = nil
	if cfg.MaxLines <= 0 || cfg.MaxWindowMinutes <= 0 {
		return nil, errors.New("tools: loki max_lines and max_window_minutes must be positive")
	}
	return &LokiClient{
		base:       base,
		maxLines:   cfg.MaxLines,
		maxWindow:  time.Duration(cfg.MaxWindowMinutes) * time.Minute,
		httpClient: lokiHTTPClient,
		now:        time.Now,
	}, nil
}

// RegisterTools 登记 loki_query。
func (c *LokiClient) RegisterTools(registry *Registry) error {
	return registry.Register(ToolSpec{
		Name: ToolLokiQuery,
		Description: "Search historical logs stored in Loki for one service, including lines from before container restarts and from other services such as postgres or redis. " +
			"Lines are aggregated by pattern with count and first/last time, most recent first. Use docker_logs for the current container instance only.",
		Timeout:   lokiToolTimeout,
		MaxOutput: defaultMaxOutput,
		Params: []ParamSpec{
			{Name: "service", Description: "service label: Compose service name (e.g. sub2api, postgres) or oncall-agent", Required: true},
			{Name: "contains", Description: "case-sensitive literal substring the line must contain, optional"},
			{Name: "since", Description: "RFC3339 window start, optional; default 30 minutes before until"},
			{Name: "until", Description: "RFC3339 window end, optional; default now"},
			{Name: "limit", Description: "max log lines, optional"},
		},
		Handler: c.query,
	})
}

type lokiQueryArgs struct {
	Service  string          `json:"service"`
	Contains string          `json:"contains"`
	Since    string          `json:"since"`
	Until    string          `json:"until"`
	Limit    json.RawMessage `json:"limit"`
}

func (c *LokiClient) query(ctx context.Context, raw json.RawMessage) (string, error) {
	var args lokiQueryArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if !lokiServicePattern.MatchString(args.Service) {
		return "", fmt.Errorf("invalid service %q", args.Service)
	}
	if len([]rune(args.Contains)) > lokiMaxContains {
		return "", fmt.Errorf("contains must be at most %d characters", lokiMaxContains)
	}
	until := c.now().UTC()
	if strings.TrimSpace(args.Until) != "" {
		parsed, err := time.Parse(time.RFC3339, args.Until)
		if err != nil {
			return "", fmt.Errorf("until must be RFC3339: %w", err)
		}
		until = parsed.UTC()
	}
	since := until.Add(-lokiDefaultWindow)
	if strings.TrimSpace(args.Since) != "" {
		parsed, err := time.Parse(time.RFC3339, args.Since)
		if err != nil {
			return "", fmt.Errorf("since must be RFC3339: %w", err)
		}
		since = parsed.UTC()
	}
	if !until.After(since) {
		return "", errors.New("until must be after since")
	}
	// 窗口封顶时保留靠近 until 的一段：故障排查关心的是故障时刻之前的日志。
	clamped := until.Sub(since) > c.maxWindow
	if clamped {
		since = until.Add(-c.maxWindow)
	}
	limit, err := optionalInt(args.Limit)
	if err != nil {
		return "", fmt.Errorf("limit must be an integer: %w", err)
	}
	if limit == 0 {
		limit = lokiDefaultLimit
	}
	if limit < 0 || limit > c.maxLines {
		limit = c.maxLines
	}

	logQL := `{service="` + args.Service + `"}`
	if args.Contains != "" {
		logQL += " |= " + strconv.Quote(args.Contains)
	}
	query := url.Values{
		"query":     {logQL},
		"start":     {strconv.FormatInt(since.UnixNano(), 10)},
		"end":       {strconv.FormatInt(until.UnixNano(), 10)},
		"limit":     {strconv.Itoa(limit)},
		"direction": {"backward"},
	}
	lines, err := c.queryRange(ctx, query)
	if err != nil {
		return "", err
	}

	var header strings.Builder
	fmt.Fprintf(&header, "service=%s window=%s..%s", args.Service, since.Format(time.RFC3339), until.Format(time.RFC3339))
	if clamped {
		fmt.Fprintf(&header, " (window capped to %s)", c.maxWindow)
	}
	if len(lines) >= limit {
		fmt.Fprintf(&header, " limit=%d reached: older lines omitted", limit)
	}
	header.WriteByte('\n')
	return header.String() + aggregateLogLines(strings.Join(lines, "\n")), nil
}

// optionalInt 接受 JSON 数字或数字字符串：工具 schema 把参数都声明成字符串，
// 模型两种写法都会出现。缺省、null 和空串返回 0。
func optionalInt(raw json.RawMessage) (int, error) {
	text := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if text == "" || text == "null" {
		return 0, nil
	}
	return strconv.Atoi(text)
}

type lokiEntry struct {
	at   time.Time
	line string
}

// queryRange 调 /loki/api/v1/query_range，把所有 stream 的条目按时间升序
// 排成 "RFC3339Nano 原文" 的行 —— 与 docker_logs 的时间戳格式一致，共用模式聚合。
func (c *LokiClient) queryRange(ctx context.Context, query url.Values) ([]string, error) {
	endpoint := c.base.ResolveReference(&url.URL{Path: "/loki/api/v1/query_range", RawQuery: query.Encode()})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("response exceeds %d bytes", maxResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		// Loki 的错误体是纯文本（如 LogQL 解析错误），不含 URL；截短后带回给调用方。
		return nil, fmt.Errorf("loki returned HTTP %d: %s", resp.StatusCode, Truncate(strings.TrimSpace(string(body)), 200))
	}
	var envelope struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Values [][2]string `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if envelope.Status != "success" || envelope.Data.ResultType != "streams" {
		return nil, fmt.Errorf("unexpected loki response status=%q type=%q", envelope.Status, envelope.Data.ResultType)
	}
	var entries []lokiEntry
	for _, stream := range envelope.Data.Result {
		for _, value := range stream.Values {
			nanos, err := strconv.ParseInt(value[0], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("decode log timestamp: %w", err)
			}
			entries = append(entries, lokiEntry{at: time.Unix(0, nanos).UTC(), line: value[1]})
		}
	}
	// 模式聚合按出现顺序记录首次/末次时间，输入必须是时间升序。
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].at.Before(entries[j].at) })
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		// 一条日志内的换行会被聚合拆成多行，这里压成一行。
		line := strings.ReplaceAll(strings.TrimRight(entry.line, "\r\n"), "\n", " ")
		lines = append(lines, entry.at.Format(time.RFC3339Nano)+" "+line)
	}
	return lines, nil
}

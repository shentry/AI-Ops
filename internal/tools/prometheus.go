package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"oncall-agent/internal/config"
)

// Prometheus 工具名。注册表之外不允许出现第二套名字（GC-12）。
const (
	ToolPromInstantQuery = "prom_instant_query"
	ToolPromRangeQuery   = "prom_range_query"
	ToolPromSeriesMeta   = "prom_series_meta"
)

// promToolTimeout 是三个只读查询共用的单次调用上限。
const promToolTimeout = 10 * time.Second

// promHTTPClient 进程级复用：连接池跨工具调用共享，
// 禁止每次调用新建 client（那会每次新建连接池，耗尽 fd）。
var promHTTPClient = &http.Client{Timeout: promToolTimeout + 5*time.Second}

// maxResponseBytes 是单次查询响应的内存兜底。合法的 matrix 响应可达
// 数百 KB（多 series × max_points 样本），兜底必须远高于该量级；
// 超过即报错，而不是截断 —— 截断会把 JSON 切坏，伪装成解析错误。
const maxResponseBytes = 4 << 20 // 4 MiB

// PrometheusClient 只封装只读查询路径。它不保存凭据，也不把
// base URL 写进错误文本 —— DSN/URL 里的 userinfo 不进日志（GC-19）。
type PrometheusClient struct {
	base       *url.URL
	rangeMax   time.Duration
	maxPoints  int
	httpClient *http.Client
}

// NewPrometheusClient 校验 base URL 并剥掉 userinfo。
func NewPrometheusClient(cfg config.PrometheusConfig) (*PrometheusClient, error) {
	raw := strings.TrimSpace(cfg.BaseURL)
	if raw == "" {
		return nil, errors.New("tools: prometheus base_url is required")
	}
	base, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("tools: prometheus base_url is invalid")
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("tools: prometheus base_url must be http(s)")
	}
	base.User = nil // 凭据只用于连接，不留存在结构里，更不进错误
	if cfg.RangeMinutes <= 0 {
		return nil, fmt.Errorf("tools: prometheus range_minutes must be positive")
	}
	if cfg.MaxPoints <= 0 {
		return nil, fmt.Errorf("tools: prometheus max_points must be positive")
	}
	return &PrometheusClient{
		base:       base,
		rangeMax:   time.Duration(cfg.RangeMinutes) * time.Minute,
		maxPoints:  cfg.MaxPoints,
		httpClient: promHTTPClient,
	}, nil
}

// RegisterTools 把三个 Prometheus 只读工具登记进注册表。
func (c *PrometheusClient) RegisterTools(registry *Registry) error {
	for _, spec := range []ToolSpec{
		{
			Name:        ToolPromInstantQuery,
			Description: "Run a PromQL instant query. Returns the Prometheus result JSON.",
			Timeout:     promToolTimeout,
			MaxOutput:   defaultMaxOutput,
			Params: []ParamSpec{
				{Name: "query", Description: "PromQL expression", Required: true},
				{Name: "time", Description: "RFC3339 evaluation time, optional"},
			},
			Handler: c.instantQuery,
		},
		{
			Name:        ToolPromRangeQuery,
			Description: "Run a PromQL range query. Range is capped and step adapts to max_points.",
			Timeout:     promToolTimeout,
			MaxOutput:   defaultMaxOutput,
			Params: []ParamSpec{
				{Name: "query", Description: "PromQL expression", Required: true},
				{Name: "start", Description: "RFC3339 range start", Required: true},
				{Name: "end", Description: "RFC3339 range end", Required: true},
			},
			Handler: c.rangeQuery,
		},
		{
			Name:        ToolPromSeriesMeta,
			Description: "List series metadata. Use this before writing PromQL against unknown labels.",
			Timeout:     promToolTimeout,
			MaxOutput:   defaultMaxOutput,
			Params: []ParamSpec{
				{Name: "match", Description: "series selector, e.g. up{job=\"x\"}", Required: true},
				{Name: "limit", Description: "max series to return, optional"},
			},
			Handler: c.seriesMeta,
		},
	} {
		if err := registry.Register(spec); err != nil {
			return err
		}
	}
	return nil
}

// promEnvelope 是 Prometheus HTTP API 的统一外壳。
type promEnvelope struct {
	Status    string          `json:"status"`
	Data      json.RawMessage `json:"data"`
	ErrorType string          `json:"errorType"`
	Error     string          `json:"error"`
}

// do 执行一次 GET 并解包 envelope。错误摘要只带 Prometheus 返回的
// errorType/error 文本和 HTTP 状态码，不带 URL（GC-19）。
func (c *PrometheusClient) do(ctx context.Context, path string, query url.Values) (json.RawMessage, error) {
	endpoint := c.base.ResolveReference(&url.URL{Path: path, RawQuery: query.Encode()})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	// 响应体限量是内存兜底（对端发狂时进程不被拖死），与给 LLM 的文本
	// 截断（Registry 的 MaxOutput）是两件事：前者必须大到容得下任何合法
	// 响应，读 limit+1 显式判溢出，绝不允许把 body 切在 JSON 中间。
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("response exceeds %d bytes", maxResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus returned HTTP %d", resp.StatusCode)
	}
	var envelope promEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if envelope.Status != "success" {
		return nil, fmt.Errorf("prometheus %s: %s", envelope.ErrorType, envelope.Error)
	}
	return envelope.Data, nil
}

type instantArgs struct {
	Query string `json:"query"`
	Time  string `json:"time"`
}

func (c *PrometheusClient) instantQuery(ctx context.Context, raw json.RawMessage) (string, error) {
	var args instantArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if strings.TrimSpace(args.Query) == "" {
		return "", errors.New("query is required")
	}
	query := url.Values{"query": {args.Query}}
	if strings.TrimSpace(args.Time) != "" {
		// 只接受 RFC3339，拒绝透传任意字符串，避免参数注入进 URL。
		parsed, err := time.Parse(time.RFC3339, args.Time)
		if err != nil {
			return "", fmt.Errorf("time must be RFC3339: %w", err)
		}
		query.Set("time", strconv.FormatFloat(float64(parsed.UnixMilli())/1000, 'f', 3, 64))
	}
	data, err := c.do(ctx, "/api/v1/query", query)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

type rangeArgs struct {
	Query string `json:"query"`
	Start string `json:"start"`
	End   string `json:"end"`
}

func (c *PrometheusClient) rangeQuery(ctx context.Context, raw json.RawMessage) (string, error) {
	var args rangeArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if strings.TrimSpace(args.Query) == "" {
		return "", errors.New("query is required")
	}
	start, err := time.Parse(time.RFC3339, args.Start)
	if err != nil {
		return "", fmt.Errorf("start must be RFC3339: %w", err)
	}
	end, err := time.Parse(time.RFC3339, args.End)
	if err != nil {
		return "", fmt.Errorf("end must be RFC3339: %w", err)
	}
	if !end.After(start) {
		return "", errors.New("end must be after start")
	}
	// 时间窗封顶：range 查询不允许产生无限时间范围（验收清单）。
	if end.Sub(start) > c.rangeMax {
		end = start.Add(c.rangeMax)
	}
	// step 自适应：Prometheus 在 start、start+step、… ≤ end 处采样，点数含两端，
	// 即 1 + floor(range/step)。要保证点数 ≤ max_points，step = ceil(range/(maxPoints-1))；
	// maxPoints=1 时只剩起点一个采样。
	rangeSeconds := int64(end.Sub(start).Seconds())
	stepSeconds := rangeSeconds
	if c.maxPoints > 1 {
		divisor := int64(c.maxPoints - 1)
		stepSeconds = (rangeSeconds + divisor - 1) / divisor
	}
	if stepSeconds < 1 {
		stepSeconds = 1
	}
	query := url.Values{
		"query": {args.Query},
		"start": {strconv.FormatFloat(float64(start.UnixMilli())/1000, 'f', 3, 64)},
		"end":   {strconv.FormatFloat(float64(end.UnixMilli())/1000, 'f', 3, 64)},
		"step":  {strconv.FormatInt(stepSeconds, 10) + "s"},
	}
	data, err := c.do(ctx, "/api/v1/query_range", query)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// QueryRange runs a caller-bounded range query for the console's Monitor page
// and returns Prometheus' result type and raw result. Unlike the model-facing
// prom_range_query it does not cap the window: the HTTP handler owns those
// limits for human users.
func (c *PrometheusClient) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) (string, json.RawMessage, error) {
	query := url.Values{
		"query": {expr},
		"start": {strconv.FormatFloat(float64(start.UnixMilli())/1000, 'f', 3, 64)},
		"end":   {strconv.FormatFloat(float64(end.UnixMilli())/1000, 'f', 3, 64)},
		"step":  {strconv.FormatFloat(step.Seconds(), 'f', -1, 64)},
	}
	data, err := c.do(ctx, "/api/v1/query_range", query)
	if err != nil {
		return "", nil, err
	}
	var result struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", nil, fmt.Errorf("decode range result: %w", err)
	}
	return result.ResultType, result.Result, nil
}

type seriesArgs struct {
	// LLM 可能给字符串也可能给数组，两种都接。
	Match json.RawMessage `json:"match"`
	Limit json.RawMessage `json:"limit"`
}

// matchSelectors 把 match 参数归一成 selector 列表。
func matchSelectors(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, errors.New("at least one match selector is required")
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return []string{single}, nil
	}
	var multiple []string
	if err := json.Unmarshal(raw, &multiple); err != nil {
		return nil, fmt.Errorf("match must be a string or string array: %w", err)
	}
	if len(multiple) == 0 {
		return nil, errors.New("at least one match selector is required")
	}
	return multiple, nil
}

// seriesMeta 查 /api/v1/series。Prometheus 的 series 接口没有 limit 参数，
// limit 在客户端裁剪，防止把全量 series 塞给 LLM。
func (c *PrometheusClient) seriesMeta(ctx context.Context, raw json.RawMessage) (string, error) {
	var args seriesArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	selectors, err := matchSelectors(args.Match)
	if err != nil {
		return "", err
	}
	limit, err := IntArg(args.Limit)
	if err != nil {
		return "", fmt.Errorf("limit must be an integer: %w", err)
	}
	if limit <= 0 {
		limit = 50
	}
	query := url.Values{}
	for _, selector := range selectors {
		if strings.TrimSpace(selector) == "" {
			return "", errors.New("match selector must not be empty")
		}
		query.Add("match[]", selector)
	}
	data, err := c.do(ctx, "/api/v1/series", query)
	if err != nil {
		return "", err
	}
	var series []json.RawMessage
	if err := json.Unmarshal(data, &series); err != nil {
		return "", fmt.Errorf("decode series data: %w", err)
	}
	truncated := false
	if len(series) > limit {
		series = series[:limit]
		truncated = true
	}
	out, err := json.Marshal(map[string]any{"series": series, "truncated": truncated, "returned": len(series)})
	if err != nil {
		return "", fmt.Errorf("encode series result: %w", err)
	}
	return string(out), nil
}

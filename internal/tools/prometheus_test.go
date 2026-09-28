package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"oncall-agent/internal/config"
)

// fakePrometheus 按路径返回预置 envelope，并记录收到的查询参数供断言。
func fakePrometheus(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *PrometheusClient) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(server.Close)
	client, err := NewPrometheusClient(config.PrometheusConfig{BaseURL: server.URL, RangeMinutes: 15, MaxPoints: 3})
	if err != nil {
		t.Fatalf("NewPrometheusClient() error = %v", err)
	}
	return server, client
}

func promSuccess(data string) string {
	return fmt.Sprintf(`{"status":"success","data":%s}`, data)
}

func TestNewPrometheusClientValidation(t *testing.T) {
	for _, cfg := range []config.PrometheusConfig{
		{BaseURL: "", RangeMinutes: 15, MaxPoints: 300},
		{BaseURL: "ftp://x", RangeMinutes: 15, MaxPoints: 300},
		{BaseURL: "http://x", RangeMinutes: 0, MaxPoints: 300},
		{BaseURL: "http://x", RangeMinutes: 15, MaxPoints: 0},
	} {
		if _, err := NewPrometheusClient(cfg); err == nil {
			t.Fatalf("NewPrometheusClient(%+v) error = nil, want failure", cfg)
		}
	}
}

func TestNewPrometheusClientStripsUserinfo(t *testing.T) {
	client, err := NewPrometheusClient(config.PrometheusConfig{BaseURL: "http://user:secret@127.0.0.1:9090", RangeMinutes: 15, MaxPoints: 300})
	if err != nil {
		t.Fatal(err)
	}
	if client.base.User != nil || strings.Contains(client.base.String(), "secret") {
		t.Fatalf("base URL keeps credentials: %s", client.base)
	}
}

func TestInstantQuery(t *testing.T) {
	_, client := fakePrometheus(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.URL.Query().Get("query") != "up" {
			t.Errorf("query = %s", r.URL.Query().Get("query"))
		}
		if got := r.URL.Query().Get("time"); got != "1700000000.000" {
			t.Errorf("time = %s", got)
		}
		fmt.Fprint(w, promSuccess(`{"resultType":"vector","result":[]}`))
	})
	out, err := client.instantQuery(context.Background(), json.RawMessage(`{"query":"up","time":"2023-11-14T22:13:20Z"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"resultType":"vector"`) {
		t.Fatalf("output = %s", out)
	}

	if _, err := client.instantQuery(context.Background(), json.RawMessage(`{"query":""}`)); err == nil {
		t.Fatal("instantQuery(empty query) error = nil")
	}
	if _, err := client.instantQuery(context.Background(), json.RawMessage(`{"query":"up","time":"not-a-time"}`)); err == nil {
		t.Fatal("instantQuery(bad time) error = nil")
	}
}

func TestRangeQueryCapsWindowAndAdaptsStep(t *testing.T) {
	_, client := fakePrometheus(t, func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		start, _ := strconv.ParseFloat(query.Get("start"), 64)
		end, _ := strconv.ParseFloat(query.Get("end"), 64)
		// 请求 2 小时窗口，配置 range_minutes=15：必须被裁到 900 秒。
		if got := end - start; got != 900 {
			t.Errorf("range window = %vs, want capped 900s", got)
		}
		// max_points=3 → 点数含两端，step = ceil(900/(3-1)) = 450s → 0/450/900 共 3 点。
		if got := query.Get("step"); got != "450s" {
			t.Errorf("step = %s, want 450s", got)
		}
		fmt.Fprint(w, promSuccess(`{"resultType":"matrix","result":[]}`))
	})
	start := time.Unix(1700000000, 0).UTC()
	end := start.Add(2 * time.Hour)
	args := fmt.Sprintf(`{"query":"up","start":%q,"end":%q}`, start.Format(time.RFC3339), end.Format(time.RFC3339))
	if _, err := client.rangeQuery(context.Background(), json.RawMessage(args)); err != nil {
		t.Fatal(err)
	}

	// 反向窗口和缺参数直接拒绝。
	bad := fmt.Sprintf(`{"query":"up","start":%q,"end":%q}`, end.Format(time.RFC3339), start.Format(time.RFC3339))
	if _, err := client.rangeQuery(context.Background(), json.RawMessage(bad)); err == nil {
		t.Fatal("rangeQuery(reversed) error = nil")
	}
}

func TestSeriesMetaAppliesLimit(t *testing.T) {
	_, client := fakePrometheus(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/series" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query()["match[]"]; len(got) != 1 || got[0] != "up" {
			t.Errorf("match[] = %v", got)
		}
		fmt.Fprint(w, promSuccess(`[{"__name__":"up","instance":"a"},{"__name__":"up","instance":"b"},{"__name__":"up","instance":"c"}]`))
	})
	out, err := client.seriesMeta(context.Background(), json.RawMessage(`{"match":["up"],"limit":2}`))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Series    []map[string]string `json:"series"`
		Truncated bool                `json:"truncated"`
		Returned  int                 `json:"returned"`
	}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Returned != 2 || !decoded.Truncated || len(decoded.Series) != 2 {
		t.Fatalf("series result = %+v", decoded)
	}

	if _, err := client.seriesMeta(context.Background(), json.RawMessage(`{"match":[]}`)); err == nil {
		t.Fatal("seriesMeta(no match) error = nil")
	}
}

func TestPrometheusResponseOverflowIsExplicitError(t *testing.T) {
	// 构造一个无法按 JSON 解析完的大 body（超过 4MiB 内存兜底）：
	// 必须得到显式的溢出错误，而不是"切在 JSON 中间"的 decode 错误。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"success","data":{"padding":"`)
		chunk := strings.Repeat("x", 1<<20)
		for range 5 {
			fmt.Fprint(w, chunk)
		}
		fmt.Fprint(w, `"}}`)
	}))
	defer server.Close()
	client, err := NewPrometheusClient(config.PrometheusConfig{BaseURL: server.URL, RangeMinutes: 15, MaxPoints: 3})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.instantQuery(context.Background(), json.RawMessage(`{"query":"up"}`))
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error = %v, want explicit overflow error", err)
	}
}

func TestPrometheusErrorIsSanitized(t *testing.T) {
	// 假 Prometheus 返回业务错误；错误摘要只带 errorType/error，不带 URL。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"error","errorType":"bad_data","error":"invalid parameter"}`)
	}))
	defer server.Close()
	client, err := NewPrometheusClient(config.PrometheusConfig{BaseURL: "http://user:secret@" + strings.TrimPrefix(server.URL, "http://"), RangeMinutes: 15, MaxPoints: 3})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.instantQuery(context.Background(), json.RawMessage(`{"query":"up"}`))
	if err == nil || !strings.Contains(err.Error(), "bad_data") {
		t.Fatalf("error = %v, want prometheus error summary", err)
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), server.URL) {
		t.Fatalf("error leaks URL or credentials: %v", err)
	}
}

func TestRegisterToolsRegistersThreeL1Tools(t *testing.T) {
	client, err := NewPrometheusClient(config.PrometheusConfig{BaseURL: "http://127.0.0.1:9090", RangeMinutes: 15, MaxPoints: 300})
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry()
	if err := client.RegisterTools(registry); err != nil {
		t.Fatal(err)
	}
	exposed := registry.ForLLM()
	if len(exposed) != 3 {
		t.Fatalf("ForLLM() = %d tools, want 3", len(exposed))
	}
	for i, name := range []string{ToolPromInstantQuery, ToolPromRangeQuery, ToolPromSeriesMeta} {
		if exposed[i].Name != name {
			t.Fatalf("ForLLM()[%d] = %s, want %s", i, exposed[i].Name, name)
		}
	}
}

// TestPrometheusToolsAgainstRealServer 需要本地 Prometheus（docker compose）。
// 设置 TEST_PROMETHEUS_URL 才运行，对三个接口各做一次真实查询。
func TestPrometheusToolsAgainstRealServer(t *testing.T) {
	baseURL := prometheusTestURL(t)
	client, err := NewPrometheusClient(config.PrometheusConfig{BaseURL: baseURL, RangeMinutes: 15, MaxPoints: 60})
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry()
	if err := client.RegisterTools(registry); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	instant, err := registry.Execute(ctx, ToolPromInstantQuery, json.RawMessage(`{"query":"up"}`))
	if err != nil {
		t.Fatalf("instant query: %v", err)
	}
	if !strings.Contains(instant, "resultType") {
		t.Fatalf("instant output = %s", instant)
	}

	now := time.Now().UTC()
	rangeArgs := fmt.Sprintf(`{"query":"up","start":%q,"end":%q}`, now.Add(-10*time.Minute).Format(time.RFC3339), now.Format(time.RFC3339))
	rangeOut, err := registry.Execute(ctx, ToolPromRangeQuery, json.RawMessage(rangeArgs))
	if err != nil {
		t.Fatalf("range query: %v", err)
	}
	if !strings.Contains(rangeOut, "resultType") {
		t.Fatalf("range output = %s", rangeOut)
	}

	meta, err := registry.Execute(ctx, ToolPromSeriesMeta, json.RawMessage(`{"match":["up"],"limit":5}`))
	if err != nil {
		t.Fatalf("series meta: %v", err)
	}
	if !strings.Contains(meta, "series") {
		t.Fatalf("series meta output = %s", meta)
	}
}

func prometheusTestURL(t *testing.T) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv("TEST_PROMETHEUS_URL"))
	if value == "" {
		t.Skip("TEST_PROMETHEUS_URL is not set")
	}
	if _, err := url.Parse(value); err != nil {
		t.Fatalf("TEST_PROMETHEUS_URL invalid: %v", err)
	}
	return value
}

func TestQueryRangeForConsoleKeepsCallerWindow(t *testing.T) {
	var got url.Values
	_, client := fakePrometheus(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		_, _ = w.Write([]byte(promSuccess(`{"resultType":"matrix","result":[{"metric":{"job":"a"},"values":[[1,"1"]]}]}`)))
	})
	start := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	end := start.Add(7 * 24 * time.Hour)
	resultType, result, err := client.QueryRange(context.Background(), "up", start, end, 28*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// 七天窗口原样下发：prom_range_query 的 15 分钟封顶只约束模型。
	if got.Get("start") != "1789992000.000" || got.Get("end") != "1790596800.000" || got.Get("step") != "1680" {
		t.Fatalf("params = %v", got)
	}
	if resultType != "matrix" || !strings.Contains(string(result), `"job":"a"`) {
		t.Fatalf("result = %s %s", resultType, result)
	}
}

package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"oncall-agent/internal/config"
)

var lokiTestNow = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

// fakeLoki 记录收到的查询参数，并按 handler 返回预置响应。
func fakeLoki(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *LokiClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(server.Close)
	client, err := NewLokiClient(config.LokiConfig{BaseURL: server.URL, MaxLines: 50, MaxWindowMinutes: 60})
	if err != nil {
		t.Fatalf("NewLokiClient() error = %v", err)
	}
	client.now = func() time.Time { return lokiTestNow }
	return client
}

func lokiStreams(streams ...string) string {
	return `{"status":"success","data":{"resultType":"streams","result":[` + strings.Join(streams, ",") + `]}}`
}

func TestNewLokiClientValidation(t *testing.T) {
	for _, cfg := range []config.LokiConfig{
		{BaseURL: "", MaxLines: 10, MaxWindowMinutes: 10},
		{BaseURL: "ftp://x", MaxLines: 10, MaxWindowMinutes: 10},
		{BaseURL: "http://", MaxLines: 10, MaxWindowMinutes: 10},
		{BaseURL: "http://x", MaxLines: 0, MaxWindowMinutes: 10},
		{BaseURL: "http://x", MaxLines: 10, MaxWindowMinutes: 0},
	} {
		if _, err := NewLokiClient(cfg); err == nil {
			t.Fatalf("NewLokiClient(%+v) error = nil, want failure", cfg)
		}
	}
	client, err := NewLokiClient(config.LokiConfig{BaseURL: "http://user:secret@127.0.0.1:3100", MaxLines: 10, MaxWindowMinutes: 10})
	if err != nil {
		t.Fatal(err)
	}
	if client.base.User != nil {
		t.Fatalf("base URL keeps credentials: %s", client.base)
	}
}

func TestLokiQueryBuildsBoundedSelector(t *testing.T) {
	var got url.Values
	client := fakeLoki(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/query_range" {
			t.Errorf("path = %s", r.URL.Path)
		}
		got = r.URL.Query()
		_, _ = w.Write([]byte(lokiStreams()))
	})
	out, err := client.query(context.Background(), json.RawMessage(`{"service":"sub2api","contains":"say \"hi\" 连接","limit":"20"}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{service="sub2api"} |= "say \"hi\" 连接"`; got.Get("query") != want {
		t.Fatalf("query = %s, want %s", got.Get("query"), want)
	}
	// 默认窗口：until=now，since=until-30m。
	if got.Get("end") != "1790596800000000000" || got.Get("start") != "1790595000000000000" {
		t.Fatalf("window = %s..%s", got.Get("start"), got.Get("end"))
	}
	if got.Get("limit") != "20" || got.Get("direction") != "backward" {
		t.Fatalf("limit/direction = %s/%s", got.Get("limit"), got.Get("direction"))
	}
	if !strings.HasPrefix(out, "service=sub2api window=2026-09-28T11:30:00Z..2026-09-28T12:00:00Z\nlines=0") {
		t.Fatalf("output = %q", out)
	}
}

func TestLokiQueryRejectsUnsafeArguments(t *testing.T) {
	client := fakeLoki(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("an invalid request must not reach Loki")
		w.WriteHeader(http.StatusInternalServerError)
	})
	for _, raw := range []string{
		`{"service":""}`,
		`{"service":"sub2api\"} or {service=~\".+"}`,
		`{"service":"a b"}`,
		`{"service":"sub2api","since":"yesterday"}`,
		`{"service":"sub2api","since":"2026-09-28T12:00:00Z","until":"2026-09-28T11:00:00Z"}`,
		`{"service":"sub2api","limit":"ten"}`,
		`{"service":"sub2api","contains":"` + strings.Repeat("x", lokiMaxContains+1) + `"}`,
	} {
		if _, err := client.query(context.Background(), json.RawMessage(raw)); err == nil {
			t.Fatalf("query(%s) error = nil", raw)
		}
	}
}

func TestLokiQueryCapsWindowAndLimit(t *testing.T) {
	var got url.Values
	client := fakeLoki(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		_, _ = w.Write([]byte(lokiStreams()))
	})
	// 3 小时窗口封顶到 60 分钟，保留靠近 until 的一段；limit 超上限回落到 max_lines。
	out, err := client.query(context.Background(), json.RawMessage(`{"service":"postgres","since":"2026-09-28T08:00:00Z","until":"2026-09-28T11:00:00Z","limit":500}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.Get("start") != "1790589600000000000" || got.Get("end") != "1790593200000000000" || got.Get("limit") != "50" {
		t.Fatalf("start/end/limit = %s/%s/%s", got.Get("start"), got.Get("end"), got.Get("limit"))
	}
	if !strings.Contains(out, "window capped to 1h0m0s") {
		t.Fatalf("output does not report the cap: %q", out)
	}
	// 空串是"未填"：用默认条数，再受 max_lines 约束。
	if _, err := client.query(context.Background(), json.RawMessage(`{"service":"postgres","limit":""}`)); err != nil || got.Get("limit") != "50" {
		t.Fatalf("empty limit: err=%v limit=%s", err, got.Get("limit"))
	}
}

func TestLokiQueryMergesStreamsInTimeOrder(t *testing.T) {
	client := fakeLoki(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(lokiStreams(
			`{"stream":{"service":"sub2api","stream":"stderr"},"values":[["1790596500000000000","db error: too many connections 7"],["1790596200000000000","db error: too many connections 3"]]}`,
			`{"stream":{"service":"sub2api","stream":"stdout"},"values":[["1790596300000000000","request done\n"]]}`,
		)))
	})
	out, err := client.query(context.Background(), json.RawMessage(`{"service":"sub2api","limit":3}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"limit=3 reached: older lines omitted",
		"lines=3 patterns=2",
		"count=2 first=2026-09-28T11:50:00Z last=2026-09-28T11:55:00Z | db error: too many connections 7",
		"count=1 first=2026-09-28T11:51:40Z last=2026-09-28T11:51:40Z | request done",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	// 最新的模式排在前面，输出预算截断的是旧模式。
	if strings.Index(out, "too many connections") > strings.Index(out, "request done") {
		t.Fatalf("most recent pattern is not first:\n%s", out)
	}
}

func TestLokiQueryReportsBackendErrors(t *testing.T) {
	client := fakeLoki(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "parse error at line 1", http.StatusBadRequest)
	})
	_, err := client.query(context.Background(), json.RawMessage(`{"service":"sub2api"}`))
	if err == nil || !strings.Contains(err.Error(), "HTTP 400: parse error at line 1") {
		t.Fatalf("error = %v", err)
	}
	client = fakeLoki(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	})
	if _, err := client.query(context.Background(), json.RawMessage(`{"service":"sub2api"}`)); err == nil {
		t.Fatal("a non-stream result must be rejected")
	}
}

func TestLokiToolRegistersThroughRegistry(t *testing.T) {
	client := fakeLoki(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(lokiStreams(`{"stream":{},"values":[["1790596500000000000","token=abcd1234secret failed"]]}`)))
	})
	registry := NewRegistry()
	if err := client.RegisterTools(registry); err != nil {
		t.Fatal(err)
	}
	out, err := registry.Execute(context.Background(), ToolLokiQuery, json.RawMessage(`{"service":"sub2api"}`))
	if err != nil {
		t.Fatal(err)
	}
	// Registry 统一脱敏：日志里的凭据不能原样交给模型。
	if strings.Contains(out, "abcd1234secret") {
		t.Fatalf("output is not sanitized: %q", out)
	}
}
